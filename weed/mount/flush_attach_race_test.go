//go:build !freebsd

package mount

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/protobuf/proto"
)

// recordingFiler captures every CreateEntry a flush sends, so the test can
// assert that a chunk uploaded while the flush was transforming its snapshot
// still lands in the committed entry.
type recordingFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	mu       sync.Mutex
	requests []*filer_pb.CreateEntryRequest
}

func (s *recordingFiler) CreateEntry(ctx context.Context, req *filer_pb.CreateEntryRequest) (*filer_pb.CreateEntryResponse, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	return &filer_pb.CreateEntryResponse{
		MetadataEvent: &filer_pb.SubscribeMetadataResponse{
			Directory: req.Directory,
			TsNs:      3000,
			EventNotification: &filer_pb.EventNotification{
				NewEntry:      req.Entry,
				NewParentPath: req.Directory,
			},
		},
	}, nil
}

func (s *recordingFiler) LookupDirectoryEntry(context.Context, *filer_pb.LookupDirectoryEntryRequest) (*filer_pb.LookupDirectoryEntryResponse, error) {
	return &filer_pb.LookupDirectoryEntryResponse{}, nil
}

func attachRaceChunks(n int) (chunks []*filer_pb.FileChunk) {
	for i := 0; i < n; i++ {
		chunks = append(chunks, &filer_pb.FileChunk{
			Fid:          &filer_pb.FileId{VolumeId: 1, FileKey: uint64(100 + i), Cookie: 1},
			Offset:       int64(i * 1024),
			Size:         1024,
			ModifiedTsNs: int64(1000 + i),
		})
	}
	return
}

// A flush takes its chunk snapshot, then spends time compacting and folding
// the chunks. An upload that attaches its chunk to the entry in that window
// takes only the entry lock, so the flush's write-back must fold it in:
// replacing the chunks wholesale dropped the late reference, the filer stored
// a version without it, and the range it covered read back as zeros (the
// attach race behind the 10G rsync corruption; master #12722).
func TestFlushKeepsChunkAttachedDuringTransform(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	recorder := &recordingFiler{}
	startFakeFiler(t, wfs, recorder)

	wfs.inodeToPath.Lookup(util.FullPath("/dir"), 1, true, false, 0, false)
	inode := wfs.inodeToPath.Lookup(util.FullPath("/dir/file"), 1, false, false, 0, false)
	base := &filer_pb.Entry{
		Name:       "file",
		Attributes: &filer_pb.FuseAttributes{FileSize: 0},
		Chunks:     attachRaceChunks(5000),
	}
	fh, _ := wfs.fhMap.AcquireFileHandle(wfs, inode, proto.Clone(base).(*filer_pb.Entry), 0, 0)
	fh.dirtyMetadata = true

	late := &filer_pb.FileChunk{
		Fid:          &filer_pb.FileId{VolumeId: 1, FileKey: 99999, Cookie: 1},
		Offset:       5000 * 1024,
		Size:         1024,
		ModifiedTsNs: 999999,
	}

	// The flush snapshots immediately; the attach lands a little later, while
	// the flush is still inside the transforms (5000 chunks keep compaction
	// busy well past the attach).
	flushed := make(chan error, 1)
	go func() {
		flushed <- wfs.flushMetadataToFiler(context.Background(), fh, "/dir", "file", 0, 0, 0)
	}()
	time.Sleep(200 * time.Microsecond)
	fh.AddChunks([]*filer_pb.FileChunk{proto.Clone(late).(*filer_pb.FileChunk)})
	if err := <-flushed; err != nil {
		t.Fatalf("flushMetadataToFiler: %v", err)
	}

	hasLate := func(chunks []*filer_pb.FileChunk) bool {
		for _, c := range chunks {
			if c.Fid != nil && c.Fid.FileKey == late.Fid.FileKey {
				return true
			}
		}
		return false
	}
	if !hasLate(fh.GetEntry().GetEntry().GetChunks()) {
		t.Fatalf("handle entry lost the chunk attached during the flush transform")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.requests) == 0 {
		t.Fatalf("no CreateEntry reached the filer")
	}
	for i, req := range recorder.requests {
		if !hasLate(req.Entry.Chunks) {
			t.Fatalf("CreateEntry %d dropped the chunk attached during the flush transform", i)
		}
	}
}

func TestWithChunksAttachedSince(t *testing.T) {
	snapshot := attachRaceChunks(3)
	processed := []*filer_pb.FileChunk{snapshot[0], snapshot[1]} // as if compaction dropped snapshot[2]
	late := &filer_pb.FileChunk{
		Fid:          &filer_pb.FileId{VolumeId: 1, FileKey: 42, Cookie: 7},
		Offset:       4096,
		Size:         1024,
		ModifiedTsNs: 2,
	}
	current := append(append([]*filer_pb.FileChunk{}, snapshot...), late)

	got := withChunksAttachedSince(snapshot, processed, current)
	if len(got) != len(processed)+1 {
		t.Fatalf("got %d chunks, want %d+1 (processed plus the late attach)", len(got), len(processed))
	}
	last := got[len(got)-1]
	if last.Fid.FileKey != late.Fid.FileKey {
		t.Fatalf("last chunk key = %d, want the late attach %d", last.Fid.FileKey, late.Fid.FileKey)
	}

	// Both file-id encodings identify the same chunk: an attach recorded via
	// FileId string must not be re-appended when it is already in the snapshot.
	strChunk := &filer_pb.FileChunk{FileId: fmt.Sprintf("%d,%x,%x", 1, 100, 1), Offset: 0, Size: 1024}
	inSnapshot := withChunksAttachedSince(snapshot, processed, append(current, strChunk))
	if len(inSnapshot) != len(processed)+1 {
		t.Fatalf("string-encoded duplicate appended: %d chunks, want %d+1", len(inSnapshot), len(processed))
	}
}
