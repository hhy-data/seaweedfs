package mount

import (
	"io"
	"sync"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// Test seam for the flush/write race regression test: when a hook is
// registered for the handle that is saving, the chunk "upload" routes
// through it instead of a real volume server. Production builds never
// register hooks, so g6LookupHook always returns nil.
type g6UploadHook struct {
	key   uint64
	peer  g6UploadPeer
	onUp  func(offset int64)
	block func(*g6UploadHook, func())
}

// g6UploadPeer is the slice of the fake filer the hook writes into.
type g6UploadPeer interface {
	storeBlob(fileId string, body []byte)
}

var (
	g6Mu    sync.Mutex
	g6Hooks = map[FileHandleId]*g6UploadHook{}
)

func g6LookupHook(fh FileHandleId) *g6UploadHook {
	g6Mu.Lock()
	defer g6Mu.Unlock()
	return g6Hooks[fh]
}

func g6RegisterHook(fh FileHandleId, hook *g6UploadHook) {
	g6Mu.Lock()
	g6Hooks[fh] = hook
	g6Mu.Unlock()
}

func g6UnregisterHook(fh FileHandleId) {
	g6Mu.Lock()
	delete(g6Hooks, fh)
	g6Mu.Unlock()
}

// g6HookUpload stores the blob through the hook's peer and mints a chunk.
func g6HookUpload(hook *g6UploadHook, r io.Reader, _ string, offset, ts int64, _ uint64) (*filer_pb.FileChunk, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if hook.onUp != nil {
		hook.onUp(offset)
	}
	if hook.block != nil {
		release := make(chan struct{})
		hook.block(hook, func() { close(release) })
		<-release
	}
	g6Mu.Lock()
	hook.key++
	key := hook.key
	g6Mu.Unlock()
	chunk := &filer_pb.FileChunk{Fid: &filer_pb.FileId{VolumeId: 1, FileKey: key, Cookie: 1}, Offset: offset, Size: uint64(len(body)), ModifiedTsNs: ts}
	hook.peer.storeBlob(chunk.GetFileIdString(), body)
	return chunk, nil
}
