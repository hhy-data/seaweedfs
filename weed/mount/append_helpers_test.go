//go:build !freebsd

package mount

import (
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// attributeAppendEntry builds a file entry whose first `records` 256-byte
// blocks are covered by one chunk each. Ported from master's test helpers;
// 4.48 entries have no CommitTsNs, so the base version lives in the fake.
func attributeAppendEntry(records int) *filer_pb.Entry {
	e := &filer_pb.Entry{Name: "file", Attributes: &filer_pb.FuseAttributes{
		Inode: 42, Uid: 2000, Gid: 20, FileMode: 0644, FileSize: uint64(records * 256), Mtime: 10,
	}, Extended: map[string][]byte{"xattr-user.other": []byte("keep"), "xattr-user.remove": []byte("remove")}}
	for i := 0; i < records; i++ {
		e.Chunks = append(e.Chunks, &filer_pb.FileChunk{Fid: &filer_pb.FileId{VolumeId: 1, FileKey: uint64(i + 1), Cookie: 1}, Offset: int64(i * 256), Size: 256, ModifiedTsNs: int64(i + 1)})
	}
	return e
}
