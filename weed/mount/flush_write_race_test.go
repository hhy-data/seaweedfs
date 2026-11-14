//go:build !freebsd

package mount

import (
	"bytes"
	"testing"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/protobuf/proto"
)

// A FLUSH from another fd of the open file uploads its pages, then a WRITE
// lands before it commits. The commit carries the new size but not the
// write's chunk, so the next flush must still publish that chunk.
//
// Ported from master 53377bcb5d (#12652), adapted to 4.48: uploads are
// intercepted through the g6 hook registered on the handle instead of the
// ChunkedDirtyPages.saveDataAsChunk field master grew later.
func TestFlushRacingWriteKeepsHandleDirty(t *testing.T) {
	base := attributeAppendEntry(1)
	peer := &g6ConditionalPeer{entry: proto.Clone(base).(*filer_pb.Entry)}
	w, handle := g6AppendHandle(t, peer, base, 100)
	write := func(offset uint64, value byte) {
		t.Helper()
		n, code := w.Write(nil, &fuse.WriteIn{InHeader: fuse.InHeader{NodeId: 42}, Fh: handle, Offset: offset, Size: 256}, bytes.Repeat([]byte{value}, 256))
		if n != 256 || code != fuse.OK {
			t.Fatalf("write at %d: %d %v", offset, n, code)
		}
	}
	flush := func() fuse.Status {
		return w.Flush(nil, &fuse.FlushIn{InHeader: fuse.InHeader{NodeId: 42}, Fh: handle})
	}

	uploading, resume := make(chan struct{}), make(chan struct{})
	hook := g6LookupHook(FileHandleId(handle))
	if hook == nil {
		t.Fatal("no upload hook registered for the handle")
	}
	first := true
	hook.block = func(_ *g6UploadHook, done func()) {
		if first {
			first = false
			close(uploading)
			<-resume
		}
		done()
	}

	write(256, 'A')
	racing := make(chan fuse.Status, 1)
	go func() { racing <- flush() }()
	<-uploading
	write(512, 'B')
	close(resume)
	if code := <-racing; code != fuse.OK {
		t.Fatalf("racing flush: %v", code)
	}
	if code := flush(); code != fuse.OK {
		t.Fatalf("owner flush: %v", code)
	}

	peer.mu.Lock()
	defer peer.mu.Unlock()
	want := append(bytes.Repeat([]byte{'O'}, 256), bytes.Repeat([]byte{'A'}, 256)...)
	want = append(want, bytes.Repeat([]byte{'B'}, 256)...)
	if got := g6StoredBytes(t, peer); !bytes.Equal(got, want) {
		t.Fatalf("stored size %d chunks %v: the racing write's bytes were not published", peer.entry.Attributes.FileSize, peer.entry.Chunks)
	}
}
