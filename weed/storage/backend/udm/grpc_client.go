package udm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/storage/backend/udm/util/hash"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/seaweedfs/seaweedfs/weed/storage/backend/udm/api/private/v1"
)

const (
	HeaderChecksum = "x-checksum"
)

type ClientSet struct {
	conn *grpc.ClientConn

	tapeIOClient pb.TapeIOClient
}

func NewClient(target string) (*ClientSet, error) {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}

	return &ClientSet{
		conn:         conn,
		tapeIOClient: pb.NewTapeIOClient(conn),
	}, nil
}

func (cs *ClientSet) Close() error {
	if cs.conn != nil {
		return cs.conn.Close()
	}
	return nil
}

//func (cs *ClientSet) DownloadFile(ctx context.Context, volumeShortName string) error {
//	_, err := cs.tapeIOClient.ReadFromTape(ctx, &pb.ReadFromTapeRequest{
//		Files: []*pb.ReadFromTapeFileInfo{
//			{
//				Id: strings.TrimSuffix(volumeShortName, filepath.Ext(volumeShortName)),
//				WriteTo: &pb.DataLocation{
//					Host:          "",
//					TransportType: pb.TransportType_volumeInternalCache,
//					SubPath:       volumeShortName,
//				},
//			},
//		},
//	})
//
//	return err
//}

func (cs *ClientSet) DownloadFile(ctx context.Context, targetPath, volumeShortName string) (err error) {
	defer func() {
		if err != nil {
			_ = os.Remove(targetPath)
		}
	}()

	rc, err := cs.downloadFile(ctx, strings.TrimSuffix(volumeShortName, filepath.Ext(volumeShortName)), 0, pb.CheckSumAlgorithm_md5)
	if err != nil {
		return fmt.Errorf("failed to download file: %w", err)
	}

	// write the file content to a new file
	outFile, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", targetPath, err)
	}

	defer outFile.Close()

	// Write the file content
	_, err = io.Copy(outFile, rc)
	if err != nil {
		return fmt.Errorf("failed to write file %s: %w", targetPath, err)
	}

	// seek to the beginning of the file
	_, err = outFile.Seek(0, 0)
	if err != nil {
		return fmt.Errorf("failed to seek to beginning of downloaded file %s: %w", targetPath, err)
	}

	// Calculate MD5 checksum
	checksum, err := hash.Md5(outFile)
	if err != nil {
		return fmt.Errorf("failed to calculate md5 of %s: %w", targetPath, err)
	}

	// Compare the checksum with the original checksum
	if checksum != rc.Checksum() {
		return fmt.Errorf("file %s checksum mismatch: %s != %s", targetPath, checksum, rc.Checksum())
	}

	return nil
}

type ReaderWithChecksum interface {
	io.Reader
	Checksum() string
}

type downloadStream struct {
	stream   grpc.ServerStreamingClient[pb.DownloadFileResponse]
	buffer   []byte
	checksum string
}

func (cs *ClientSet) downloadFile(ctx context.Context, key string, chunkSize uint64, checksumAlg pb.CheckSumAlgorithm) (ReaderWithChecksum, error) {
	stream, err := cs.tapeIOClient.DownFromTape(ctx, &pb.DownFromTapeRequest{
		Id:          key,
		ChunkSize:   chunkSize,
		ChecksumAlg: checksumAlg,
	})
	if err != nil {
		return nil, err
	}

	return &downloadStream{
		stream: stream,
	}, nil
}

func (d *downloadStream) Read(p []byte) (n int, err error) {
	if d.stream == nil {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	if len(d.buffer) == 0 {
		resp, err := d.stream.Recv()
		if err != nil {
			if err == io.EOF {
				ts := d.stream.Trailer()
				if ts != nil && len(ts[HeaderChecksum]) > 0 {
					d.checksum = ts[HeaderChecksum][0]
				}
			}
			d.stream = nil
			return 0, err
		}
		d.buffer = resp.Data
	}
	n = copy(p, d.buffer)
	d.buffer = d.buffer[n:]
	return n, nil
}

func (d *downloadStream) Checksum() string {
	return d.checksum
}
