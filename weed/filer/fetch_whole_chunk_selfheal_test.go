package filer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"

	"google.golang.org/protobuf/proto"
)

// staleTrackingInvalidator records invalidation calls and lets the test
// observe whether fetchWholeChunk triggered a cache drop after a fetch failure.
type staleTrackingInvalidator struct {
	invalidations atomic.Int32
}

func (inv *staleTrackingInvalidator) InvalidateCache(fileId string) {
	inv.invalidations.Add(1)
}

// twoStageLookupFn returns staleUrls on the first call and freshUrls on every
// subsequent call. Mirrors what a vidMapClient does after InvalidateCache: the
// next LookupVolumeIdsWithFallback queries master and returns current locations.
type twoStageLookupFn struct {
	staleUrls []string
	freshUrls []string
	calls     atomic.Int32
}

func (l *twoStageLookupFn) lookup(ctx context.Context, fileId string) ([]string, error) {
	n := l.calls.Add(1)
	if n == 1 {
		return l.staleUrls, nil
	}
	return l.freshUrls, nil
}

// TestFetchWholeChunkRetriesFreshLocations covers the mount reading a multipart
// file whose manifest volume has moved: the cached location is dead, so the
// fetch has to drop it and come back with what the master knows now. The stale
// server streams a prefix before dying, which the retry must not keep - an HTTP
// error status returns before ReadUrlAsStream ever calls the writer, so a 500
// never exercises the Reset the retry path relies on.
func TestFetchWholeChunkRetriesFreshLocations(t *testing.T) {
	manifest := &filer_pb.FileChunkManifest{
		Chunks: []*filer_pb.FileChunk{
			{FileId: "100,abc", Offset: 0, Size: 8},
			{FileId: "101,def", Offset: 8, Size: 8},
		},
	}
	manifestBytes, err := proto.Marshal(manifest)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	// Stale endpoint: stream a short prefix and then panic mid-body so the
	// client sees a real read failure after partial bytes are appended. The
	// panic lets net/http close the connection without sending a status code.
	staleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(manifestBytes)+64))
		w.Write([]byte("garbage-from-stale-server"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer staleSrv.Close()

	// Fresh endpoint: full manifest, served normally.
	freshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(manifestBytes)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(manifestBytes)
	}))
	defer freshSrv.Close()

	lookup := &twoStageLookupFn{
		staleUrls: []string{staleSrv.URL + "/5,stale"},
		freshUrls: []string{freshSrv.URL + "/5,stale"},
	}
	inv := &staleTrackingInvalidator{}

	bytesBuffer := bytesBufferPool.Get().(*bytes.Buffer)
	bytesBuffer.Reset()
	defer bytesBufferPool.Put(bytesBuffer)

	if err := fetchWholeChunk(context.Background(), bytesBuffer, lookup.lookup, "5,stale", nil, false, inv); err != nil {
		t.Fatalf("fetchWholeChunk returned error after self-heal: %v", err)
	}
	if got := inv.invalidations.Load(); got != 1 {
		t.Errorf("expected exactly 1 InvalidateCache call, got %d", got)
	}
	if got := lookup.calls.Load(); got != 2 {
		t.Errorf("expected exactly 2 lookup calls (initial + re-lookup), got %d", got)
	}

	// The buffer must hold the fresh manifest alone, not the stale prefix that
	// streamed ahead of it. Without bytesBuffer.Reset() before retry the
	// garbage prefix would survive and proto.Unmarshal would fail.
	got := bytesBuffer.Bytes()
	if bytes.Contains(got, []byte("garbage-from-stale-server")) {
		t.Errorf("bytesBuffer still contains stale prefix; Reset() before retry is missing")
	}

	decoded := &filer_pb.FileChunkManifest{}
	if err := proto.Unmarshal(got, decoded); err != nil {
		t.Fatalf("proto.Unmarshal of fetched buffer: %v", err)
	}
	if len(decoded.Chunks) != 2 {
		t.Fatalf("expected 2 manifest chunks, got %d", len(decoded.Chunks))
	}
	if decoded.Chunks[0].FileId != "100,abc" || decoded.Chunks[1].FileId != "101,def" {
		t.Errorf("unexpected manifest chunks: %+v", decoded.Chunks)
	}
}

// TestFetchWholeChunkNoInvalidatorSkipsRetry verifies that with a nil
// invalidator, fetchWholeChunk returns the original fetch error without
// retrying - preserving the existing semantics for non-mount callers that
// don't participate in cache invalidation.
func TestFetchWholeChunkNoInvalidatorSkipsRetry(t *testing.T) {
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	lookup := &twoStageLookupFn{
		staleUrls: []string{failSrv.URL + "/5,abc"},
		freshUrls: []string{"http://unused:8080/5,abc"},
	}

	bytesBuffer := bytesBufferPool.Get().(*bytes.Buffer)
	bytesBuffer.Reset()
	defer bytesBufferPool.Put(bytesBuffer)

	err := fetchWholeChunk(context.Background(), bytesBuffer, lookup.lookup, "5,abc", nil, false, nil)
	if err == nil {
		t.Fatal("expected fetchWholeChunk to return the original error when invalidator is nil")
	}
	if got := lookup.calls.Load(); got != 1 {
		t.Errorf("expected only the initial lookup, got %d calls", got)
	}
}

// TestFetchWholeChunkSameUrlsSkipsRetry verifies that when invalidation leads
// to a re-lookup that returns the same stale URLs, fetchWholeChunk does not
// retry (avoiding infinite retry loops against the same broken servers).
func TestFetchWholeChunkSameUrlsSkipsRetry(t *testing.T) {
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	// Both stale and fresh lookups return the same URL (the stale endpoint).
	// This is what happens when the cache had the wrong info AND the master
	// still returns the same wrong info - we must not loop.
	lookup := &twoStageLookupFn{
		staleUrls: []string{failSrv.URL + "/5,abc"},
		freshUrls: []string{failSrv.URL + "/5,abc"},
	}
	inv := &staleTrackingInvalidator{}

	bytesBuffer := bytesBufferPool.Get().(*bytes.Buffer)
	bytesBuffer.Reset()
	defer bytesBufferPool.Put(bytesBuffer)

	err := fetchWholeChunk(context.Background(), bytesBuffer, lookup.lookup, "5,abc", nil, false, inv)
	if err == nil {
		t.Fatal("expected fetchWholeChunk to return error when locations unchanged after re-lookup")
	}
	if got := lookup.calls.Load(); got != 2 {
		t.Errorf("expected initial + re-lookup (2 calls), got %d", got)
	}
	if got := inv.invalidations.Load(); got != 1 {
		t.Errorf("expected exactly 1 InvalidateCache call, got %d", got)
	}
}

// TestFetchWholeChunkCancelledKeepsLocations checks that a caller walking away
// mid-read does not cost every other reader a master round trip. A cancelled
// read is not evidence that the locations are wrong, so the cache must stay put
// and the cancellation must surface to the caller so they can tell their own
// abort apart from a manifest that the network actually dropped.
func TestFetchWholeChunkCancelledKeepsLocations(t *testing.T) {
	lookup := &twoStageLookupFn{
		staleUrls: []string{"http://unused:8080/5,abc"},
		freshUrls: []string{"http://elsewhere:8080/5,abc"},
	}
	inv := &staleTrackingInvalidator{}

	bytesBuffer := bytesBufferPool.Get().(*bytes.Buffer)
	bytesBuffer.Reset()
	defer bytesBufferPool.Put(bytesBuffer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := fetchWholeChunk(ctx, bytesBuffer, lookup.lookup, "5,abc", nil, false, inv)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if got := inv.invalidations.Load(); got != 0 {
		t.Errorf("expected no InvalidateCache calls on cancelled read, got %d", got)
	}
	if got := lookup.calls.Load(); got != 1 {
		t.Errorf("expected exactly the initial lookup, got %d", got)
	}

	// The cancellation must survive the wrapping ResolveOneChunkManifest does,
	// or volume.fsck cannot tell its own abort from a corrupt manifest.
	manifestChunk := &filer_pb.FileChunk{FileId: "5,abc", IsChunkManifest: true}
	if _, resolveErr := ResolveOneChunkManifest(ctx, lookup.lookup, manifestChunk, inv); !errors.Is(resolveErr, context.Canceled) {
		t.Fatalf("ResolveOneChunkManifest should wrap context.Canceled, got %v", resolveErr)
	}

	// The nil-invalidator path also has to short-circuit on cancellation:
	// volume.fsck resolves manifests with no invalidator and still needs to
	// distinguish a caller-driven abort from real corruption.
	refetchRan := false
	noInvalidator := retryFetchWithFreshLocations(ctx, nil, lookup.lookup, "5,abc", nil, fmt.Errorf("stale server said no"), func([]string) error {
		refetchRan = true
		return nil
	})
	if !errors.Is(noInvalidator, context.Canceled) {
		t.Fatalf("retryFetchWithFreshLocations should surface context.Canceled even with nil invalidator, got %v", noInvalidator)
	}
	if refetchRan {
		t.Fatal("refetch must not run on a cancelled read")
	}
}