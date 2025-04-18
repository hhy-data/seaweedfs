package udm

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/backend"
)

const (
	superBlockSize  = 8
	volumeCachePath = ".udm_cache"
	separator       = "::"
)

func init() {
	backend.BackendStorageFactories[storageType] = &backendFactory{}
}

const storageType = "udm"

type backendFactory struct {
}

func (factory *backendFactory) StorageType() backend.StorageType {
	return storageType
}
func (factory *backendFactory) BuildStorage(configuration backend.StringProperties, configPrefix string, id string) (backend.BackendStorage, error) {
	return newBackendStorage(configuration, configPrefix, id)
}

type BackendStorage struct {
	id           string
	grpcServer   string
	readDisabled bool
	client       *ClientSet
}

func newBackendStorage(configuration backend.StringProperties, configPrefix string, id string) (*BackendStorage, error) {
	grpcServer := configuration.GetString(configPrefix + "grpc_server")
	readDisabled, _ := strconv.ParseBool(configuration.GetString(configPrefix + "read_disabled"))

	cl, err := NewClient(grpcServer, readDisabled)
	if err != nil {
		return nil, err
	}

	glog.V(1).Infof("Adding backend storage: %s.%s", storageType, id)

	return &BackendStorage{
		id:           id,
		client:       cl,
		grpcServer:   grpcServer,
		readDisabled: readDisabled,
	}, nil
}

func (s *BackendStorage) ToProperties() map[string]string {
	return map[string]string{
		"grpc_server": s.grpcServer,
	}
}

func (s *BackendStorage) NewStorageFile(key string, tierInfo *volume_server_pb.VolumeInfo) backend.BackendStorageFile {
	f := &backendStorageFile{
		backendStorage: s,
		key:            key,
		readDisabled:   s.readDisabled,
		tierInfo:       tierInfo,
	}

	return f
}

func (s *BackendStorage) CopyFile(f *os.File, _ func(progressed int64, percentage float32) error) (key string, size int64, err error) {
	superblock, size, err := moveFileToInternalCache(f.Name())
	if err != nil {
		glog.V(1).Infof("failed to copy file: %v", err)
		return
	}

	key = fmt.Sprintf("%s%s%s", f.Name(), separator, string(superblock))

	glog.V(1).Infof("copying dat file of %s to remote udm.%s as %s", f.Name(), s.id, key)

	return
}

func (s *BackendStorage) DownloadFile(fileName string, key string, _ func(progressed int64, percentage float32) error) (size int64, err error) {
	size, err = moveFileFromInternalCache(fileName)
	if err != nil {
		glog.V(1).Infof("failed to download file: %v", err)
		return
	}

	glog.V(1).Infof("download dat file of %s from remote udm.%s as %s", fileName, s.id, key)

	return
}

func (s *BackendStorage) DeleteFile(key string) (err error) {

	glog.V(1).Infof("delete dat file %s from remote", key)

	_ = deleteFileInInternalCache(key)

	return
}

type backendStorageFile struct {
	backendStorage *BackendStorage
	key            string
	readDisabled   bool
	tierInfo       *volume_server_pb.VolumeInfo
}

func (f *backendStorageFile) ReadAt(p []byte, off int64) (n int, err error) {
	length := len(p)
	var data []byte
	if isSuperBlock(off, length) {
		data = []byte(strings.SplitN(f.key, separator, 2)[1])
	} else {
		if f.readDisabled {
			return 0, fmt.Errorf("can not read %s at %d with length %d: read is disabled", f.key, off, length)
		}

		// TODO: download to cache and read
	}

	n = len(data)

	copy(p, data)
	if length > n {
		for i := n; i < length; i++ {
			p[i] = 0
		}
	}

	return n, nil
}

func (f *backendStorageFile) WriteAt(p []byte, off int64) (n int, err error) {
	panic(fmt.Sprintf("Can not write %s at %d with length %d: not implemented", f.key, off, len(p)))
}

func (f *backendStorageFile) Truncate(off int64) error {
	panic("not implemented")
}

func (f *backendStorageFile) Close() error {
	return nil
}

func (f *backendStorageFile) GetStat() (datSize int64, modTime time.Time, err error) {
	return
}

func (f *backendStorageFile) Name() string {
	return f.key
}

func (f *backendStorageFile) Sync() error {
	return nil
}

func moveFileToInternalCache(path string) (superBlock []byte, size int64, err error) {
	cacheFile := buildInternalCacheFilePath(path)
	err = os.MkdirAll(filepath.Dir(cacheFile), 0777)
	if err != nil {
		glog.V(1).Infof("Failed to create cache dir for file %s, err: %v", cacheFile, err)
		return nil, 0, err
	}

	fileInfo, err := os.Stat(cacheFile)
	if err != nil {
		if os.IsNotExist(err) {
			err = os.Rename(path, cacheFile)
			if err != nil {
				glog.V(1).Infof("Failed to rename file from %s to %s, err: %s", path, cacheFile, err)
				return nil, 0, err
			}
		} else {
			glog.V(1).Infof("Can not stat file %s", cacheFile)
			return nil, 0, err
		}
	}

	size = fileInfo.Size()
	superBlock, err = readSuperBlock(cacheFile)
	if err != nil {
		glog.V(1).Infof("Failed to read super block for file %s, err: %s", cacheFile, err)
		return nil, 0, err
	}

	return
}

func moveFileFromInternalCache(path string) (int64, error) {
	f, err := os.Stat(path)
	if err == nil {
		// already exists
		return f.Size(), nil
	}

	cacheFile := buildInternalCacheFilePath(path)
	fileInfo, err := os.Stat(cacheFile)
	if err != nil {
		glog.V(1).Infof("Can not stat file %s", cacheFile)
		return 0, err
	}

	err = os.Rename(cacheFile, path)
	if err != nil {
		glog.V(1).Infof("Failed to rename file from %s to %s, err: %s", cacheFile, path, err)
		return 0, err
	}

	return fileInfo.Size(), nil
}

func deleteFileInInternalCache(key string) error {
	path := strings.SplitN(key, separator, 2)
	cacheFile := buildInternalCacheFilePath(path[0])
	return os.Remove(cacheFile)
}

func buildInternalCacheFilePath(path string) string {
	filePath, fileName := filepath.Dir(path), filepath.Base(path)
	return filepath.Join(filePath, volumeCachePath, fileName)
}

func readSuperBlock(filePath string) ([]byte, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data := make([]byte, superBlockSize)
	n, err := f.ReadAt(data, 0)
	if err != nil {
		return nil, err
	} else if n != superBlockSize {
		return nil, fmt.Errorf("read super block size %d not equal to %d", n, superBlockSize)
	}

	return data, nil
}

func isSuperBlock(offset int64, length int) bool {
	return offset == 0 && length == superBlockSize
}
