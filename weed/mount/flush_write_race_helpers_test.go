//go:build !freebsd

package mount

import (
	"bytes"
	"context"
	"sync"
	"syscall"
	"testing"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// errG6Rejected mimics a filer WriteCondition refusal: FailedPrecondition is
// non-retryable, matching the 4.48 error classifier.
var errG6Rejected = status.Error(codes.FailedPrecondition, "entry changed")

// These helpers port the minimal fake-filer plumbing the flush/write race
// regression test needs from master's content_gap_test.go. 4.48 has no
// CommitTsNs on Entry and no saveDataAsChunk field on ChunkedDirtyPages, so
// the uploader is intercepted at the WFS level and file versions are tracked
// with the fake's own clock.

// g6AppendHandle builds a WFS whose handle for /dir/file (inode 42) is
// opened for append against the fake peer, with uploads intercepted.
func g6AppendHandle(t *testing.T, peer *g6ConditionalPeer, base *filer_pb.Entry, key uint64, writeback ...bool) (*WFS, uint64) {
	t.Helper()
	w := newInvalidateTestWFS(t)
	w.option.CacheDirForWrite = t.TempDir()
	w.option.DefaultPermissions = true
	w.option.WritebackCache = len(writeback) > 0 && writeback[0]
	w.posixLocks = NewPosixLockTable()
	w.option.ConcurrentWriters = 1
	w.concurrentWriters = util.NewLimitedConcurrentExecutor(1)
	startFakeFiler(t, w, peer)
	w.inodeToPath.Lookup("/dir", 1, true, false, 0, false)
	w.inodeToPath.Lookup("/dir/file", 1, false, false, 42, true)
	w.inodeToPath.MarkChildrenCached("/dir")
	if err := w.metaCache.InsertEntry(context.Background(), filer.FromPbEntry("/dir", proto.Clone(base).(*filer_pb.Entry)), 1); err != nil {
		t.Fatal(err)
	}
	out := &fuse.OpenOut{}
	if code := w.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: 42}, Flags: syscall.O_WRONLY | syscall.O_APPEND}, out); code != fuse.OK {
		t.Fatal(code)
	}
	fh := w.GetHandle(FileHandleId(out.Fh))
	peer.mu.Lock()
	if peer.blobs == nil {
		peer.blobs = make(map[string][]byte)
	}
	peer.blobs[base.Chunks[0].GetFileIdString()] = bytes.Repeat([]byte{'O'}, 256)
	peer.mu.Unlock()
	g6AppendUploader(t, fh, key, peer)
	return w, out.Fh
}

// g6AppendUploader routes this handle's chunk uploads into the fake peer.
// 4.48's ChunkedDirtyPages has no injectable saveDataAsChunk field, so the
// interception happens through the upload hook seam in upload_hook.go.
func g6AppendUploader(t *testing.T, fh *FileHandle, key uint64, peer *g6ConditionalPeer) {
	t.Helper()
	t.Cleanup(fh.dirtyPages.Destroy)
	g6RegisterHook(fh.fh, &g6UploadHook{key: key, peer: peer})
	t.Cleanup(func() { g6UnregisterHook(fh.fh) })
}

// storeBlob implements g6UploadPeer.
func (s *g6ConditionalPeer) storeBlob(fileId string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blobs == nil {
		s.blobs = make(map[string][]byte)
	}
	s.blobs[fileId] = append([]byte(nil), body...)
}

type g6ConditionalPeer struct {
	filer_pb.UnimplementedSeaweedFilerServer
	mu           sync.Mutex
	entry        *filer_pb.Entry
	rejected     int
	refuse       bool
	blobs        map[string][]byte
	clock        int64
	beforeCreate func(context.Context)
}

func g6EntryEqual(current, expected *filer_pb.Entry) bool {
	actual := filer.FromPbEntry("", proto.Clone(current).(*filer_pb.Entry)).ToProtoEntry()
	want := proto.Clone(expected).(*filer_pb.Entry)
	filer_pb.BeforeEntrySerialization(actual.Chunks)
	filer_pb.BeforeEntrySerialization(want.Chunks)
	return proto.Equal(actual, filer.FromPbEntry("", want).ToProtoEntry())
}

func g6StoredBytes(t *testing.T, s *g6ConditionalPeer) []byte {
	t.Helper()
	result := make([]byte, s.entry.Attributes.FileSize)
	for _, c := range s.entry.Chunks {
		body, ok := s.blobs[c.GetFileIdString()]
		if !ok || uint64(len(body)) != c.Size || c.Offset < 0 || uint64(c.Offset)+c.Size > uint64(len(result)) {
			t.Fatalf("invalid stored reference %v", c)
		}
		copy(result[c.Offset:], body)
	}
	return result
}

func (s *g6ConditionalPeer) LookupDirectoryEntry(context.Context, *filer_pb.LookupDirectoryEntryRequest) (*filer_pb.LookupDirectoryEntryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &filer_pb.LookupDirectoryEntryResponse{Entry: proto.Clone(s.entry).(*filer_pb.Entry)}, nil
}

func (s *g6ConditionalPeer) UpdateEntry(_ context.Context, r *filer_pb.UpdateEntryRequest) (*filer_pb.UpdateEntryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range r.GetCondition().GetClauses() {
		if c.Kind != filer_pb.WriteCondition_IF_ENTRY_EQUAL || !g6EntryEqual(s.entry, c.ExpectedEntry) {
			return nil, errG6Rejected
		}
	}
	s.entry = proto.Clone(r.Entry).(*filer_pb.Entry)
	s.clock = max(max(s.clock, s.entry.Attributes.Mtime), 10000) + 1
	return &filer_pb.UpdateEntryResponse{LogTsNs: s.clock, MetadataEvent: &filer_pb.SubscribeMetadataResponse{Directory: r.Directory, TsNs: s.clock, EventNotification: &filer_pb.EventNotification{NewEntry: proto.Clone(s.entry).(*filer_pb.Entry)}}}, nil
}

func (s *g6ConditionalPeer) CreateEntry(ctx context.Context, r *filer_pb.CreateEntryRequest) (*filer_pb.CreateEntryResponse, error) {
	if s.beforeCreate != nil {
		s.beforeCreate(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse {
		s.rejected++
		return nil, errG6Rejected
	}
	for _, c := range r.GetCondition().GetClauses() {
		match := c.Kind == filer_pb.WriteCondition_IF_NOT_EXISTS && s.entry == nil || c.Kind == filer_pb.WriteCondition_IF_ENTRY_EQUAL && s.entry != nil && c.ExpectedEntry != nil && g6EntryEqual(s.entry, c.ExpectedEntry)
		if !match {
			s.rejected++
			return nil, errG6Rejected
		}
	}
	s.entry = proto.Clone(r.Entry).(*filer_pb.Entry)
	s.clock = max(max(s.clock, s.entry.Attributes.Mtime), 10000) + 1
	return &filer_pb.CreateEntryResponse{LogTsNs: s.clock, MetadataEvent: &filer_pb.SubscribeMetadataResponse{Directory: r.Directory, TsNs: s.clock, EventNotification: &filer_pb.EventNotification{NewEntry: proto.Clone(s.entry).(*filer_pb.Entry)}}}, nil
}
