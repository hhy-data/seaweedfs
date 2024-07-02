package udm

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/backend"
)

const (
	superBlockSize       = 8
	volumeCachePath      = ".udm_cache"
	transRecallCachePath = "trans_recall_cache"
	separator            = "::"
	cacheCleanupInterval = 5 * time.Minute
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
	ctx          context.Context
	id           string
	grpcServer   string
	readDisabled bool
	client       *ClientSet

	cache *lruCache
}

func newBackendStorage(configuration backend.StringProperties, configPrefix string, id string) (*BackendStorage, error) {
	grpcServer := configuration.GetString(configPrefix + "grpc_server")
	readDisabled, _ := strconv.ParseBool(configuration.GetString(configPrefix + "read_disabled"))

	cache, err := newLRUCache(findTransRecallCachePath())
	if err != nil {
		return nil, err
	}

	cl, err := NewClient(grpcServer)
	if err != nil {
		return nil, err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-ctx.Done()
		stop()
		_ = cl.Close()
	}()

	glog.V(0).Infof("Adding backend storage: %s.%s", storageType, id)

	res := &BackendStorage{
		ctx:          ctx,
		id:           id,
		client:       cl,
		grpcServer:   grpcServer,
		readDisabled: readDisabled,
		cache:        cache,
	}

	go res.cleanUpCache()

	return res, nil
}

func (s *BackendStorage) cleanUpCache() {
	ticker := time.NewTicker(cacheCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.cache.Cleanup()
		case <-s.ctx.Done():
			glog.V(0).Infof("Stopping periodic cache cleanup for backend storage: %s.%s", storageType, s.id)
			return
		}
	}
}

func (s *BackendStorage) ToProperties() map[string]string {
	return map[string]string{
		"grpc_server":   s.grpcServer,
		"read_disabled": strconv.FormatBool(s.readDisabled),
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
		glog.V(0).Infof("failed to copy file: %v", err)
		return
	}

	key = generateFileKey(f.Name(), superblock)

	glog.V(0).Infof("copying dat file of %s to remote udm.%s as %s", f.Name(), s.id, key)

	return
}

func (s *BackendStorage) DownloadFile(fileName string, key string, _ func(progressed int64, percentage float32) error) (size int64, err error) {
	size, err = moveFileFromInternalCache(fileName)
	if err != nil {
		glog.V(0).Infof("failed to download file: %v", err)
		return
	}

	glog.V(0).Infof("download dat file of %s from remote udm.%s as %s", fileName, s.id, key)

	return
}

func (s *BackendStorage) DeleteFile(key string) (err error) {

	glog.V(0).Infof("delete dat file %s from remote", key)

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
		data = getSuperBlockFromKey(f.key)
		copy(p, data)
		return length, nil
	}

	path := getPathFromKey(f.key)

	// First try UDM cache to avoid unnecessary downloads.
	udmCacheFile := buildInternalCacheFilePath(path)
	cacheN, cacheErr := f.readAtInternalCache(udmCacheFile, p, off)
	if cacheErr != nil {
		if !os.IsNotExist(cacheErr) {
			glog.Warningf("failed to read file in internal cache %s, err: %v", udmCacheFile, cacheErr)
			return cacheN, cacheErr
		}
	} else {
		return cacheN, nil
	}

	if f.readDisabled {
		return 0, fmt.Errorf("can not read %s at %d with length %d: read is disabled", f.key, off, length)
	}

	cacheFile, subPathInVolumeCache := buildTransRecallCacheFilePath(path)
	_, err = os.Stat(cacheFile)
	if err != nil {
		if os.IsNotExist(err) {
			glog.Warningf("file %s does not exist in trans recall cache, downloading from remote", path)
			shortName := filepath.Base(path)
			err = f.backendStorage.client.DownloadFile(f.backendStorage.ctx, subPathInVolumeCache, strings.TrimSuffix(shortName, filepath.Ext(shortName)))
			if err != nil {
				glog.Errorf("failed to download file %s, err: %v", path, err)
				return 0, fmt.Errorf("failed to download file %s, err: %w", path, err)
			}
		} else {
			return 0, fmt.Errorf("failed to stat file %s, err: %w", path, err)
		}
	}

	defer f.backendStorage.cache.UpdateAccess(cacheFile)

	return f.readAtInternalCache(cacheFile, p, off)
}

func (f *backendStorageFile) readAtInternalCache(path string, p []byte, off int64) (n int, err error) {
	file, err := os.Open(path)
	if err != nil {
		return
	}

	defer file.Close()

	n, err = file.ReadAt(p, off)
	if err == io.EOF {
		err = nil
	}

	// p might be reused by previous call
	if len(p) > n {
		for i := n; i < len(p); i++ {
			p[i] = 0
		}
	}

	return
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
	files := f.tierInfo.GetFiles()

	if len(files) == 0 {
		err = fmt.Errorf("remote file info not found")
		return
	}

	datSize = int64(files[0].FileSize)
	modTime = time.Unix(int64(files[0].ModifiedTime), 0)

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
		glog.V(0).Infof("Failed to create cache dir for file %s, err: %v", cacheFile, err)
		return nil, 0, err
	}

	fileInfo, err := os.Stat(cacheFile)
	if err != nil {
		if os.IsNotExist(err) {
			err = os.Rename(path, cacheFile)
			if err != nil {
				glog.V(0).Infof("Failed to rename file from %s to %s, err: %s", path, cacheFile, err)
				return nil, 0, err
			}

			fileInfo, err = os.Stat(cacheFile)
			if err != nil {
				glog.V(0).Infof("Can not stat file after rename %s", cacheFile)
				return nil, 0, err
			}

			size = fileInfo.Size()
		} else {
			glog.V(0).Infof("Can not stat file %s", cacheFile)
			return nil, 0, err
		}
	} else {
		size = fileInfo.Size()
	}

	superBlock, err = readSuperBlock(cacheFile)
	if err != nil {
		glog.V(0).Infof("Failed to read super block for file %s, err: %s", cacheFile, err)
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
		glog.V(0).Infof("Can not stat file %s", cacheFile)
		return 0, err
	}

	err = os.Rename(cacheFile, path)
	if err != nil {
		glog.V(0).Infof("Failed to rename file from %s to %s, err: %s", cacheFile, path, err)
		return 0, err
	}

	return fileInfo.Size(), nil
}

func generateFileKey(path string, superBlock []byte) string {
	encodedSuperBlock := hex.EncodeToString(superBlock)
	return strings.Join([]string{path, encodedSuperBlock}, separator)
}

func getPathFromKey(key string) string {
	parts := strings.SplitN(key, separator, 2)
	return parts[0]
}

func getSuperBlockFromKey(key string) []byte {
	parts := strings.SplitN(key, separator, 2)
	superBlock, err := hex.DecodeString(parts[1])
	if err != nil {
		superBlock = []byte(parts[1])
	}

	return superBlock
}

func deleteFileInInternalCache(key string) error {
	path := getPathFromKey(key)
	cacheFile := buildInternalCacheFilePath(path)
	return os.Remove(cacheFile)
}

func buildInternalCacheFilePath(path string) string {
	filePath, fileName := filepath.Dir(path), filepath.Base(path)
	return filepath.Join(filePath, volumeCachePath, fileName)
}

func buildTransRecallCacheFilePath(path string) (string, string) {
	filePath, fileName := filepath.Dir(path), filepath.Base(path)
	subPath := filepath.Join(transRecallCachePath, fileName)
	return filepath.Join(filePath, volumeCachePath, subPath), subPath
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

func findTransRecallCachePath() string {
	var foundPath string

	_ = filepath.WalkDir("/", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Ignore access errors and continue traversal
			return nil
		}

		if !d.IsDir() {
			return nil
		}

		if d.Name() == transRecallCachePath {
			foundPath = path
			// Stop traversal after finding the first match
			return filepath.SkipAll
		}

		return nil
	})

	return foundPath
}
