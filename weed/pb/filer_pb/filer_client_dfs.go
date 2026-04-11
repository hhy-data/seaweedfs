package filer_pb

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"

	"github.com/seaweedfs/seaweedfs/weed/util"
)

func TraverseDfs(filerClient FilerClient, parentPath util.FullPath, fn func(parentPath util.FullPath, entry *Entry)) (err error) {
	return processOneDirectoryDfs(filerClient, parentPath, fn)
}

func processOneDirectoryDfs(filerClient FilerClient, parentPath util.FullPath, fn func(parentPath util.FullPath, entry *Entry)) (err error) {
	return ReadDirAllEntries(filerClient, parentPath, "", func(entry *Entry, isLast bool) error {
		fn(parentPath, entry)

		if !entry.IsDirectory {
			return nil
		}

		subDir := fmt.Sprintf("%s/%s", parentPath, entry.Name)
		if parentPath == "/" {
			subDir = "/" + entry.Name
		}
		return processOneDirectoryDfs(filerClient, util.FullPath(subDir), fn)
	})
}

func StreamDfs(client SeaweedFilerClient, dir util.FullPath, olderThanTsNs int64, fn func(parentPath util.FullPath, entry *Entry) error) (err error) {
	glog.V(0).Infof("TraverseDfsMetadata %v if before %v", dir, time.Unix(0, olderThanTsNs))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.TraverseDfsMetadata(ctx, &TraverseDfsMetadataRequest{
		Directory: string(dir),
	})
	if err != nil {
		return fmt.Errorf("traverse dfs metadata: %v", err)
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("traverse dfs metadata: %v", err)
		}
		if err := fn(util.FullPath(resp.Directory), resp.Entry); err != nil {
			return err
		}
	}
	return nil
}
