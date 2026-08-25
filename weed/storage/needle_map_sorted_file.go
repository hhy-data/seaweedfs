package storage

import (
	"fmt"
	"io"
	"os"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/storage/erasure_coding"
	"github.com/seaweedfs/seaweedfs/weed/storage/idx"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle_map"
	. "github.com/seaweedfs/seaweedfs/weed/storage/types"
)

// SortedFileNeedleMap serves read-only and remote-tiered volumes. To keep
// the per-volume file descriptor cost at zero, the .idx and .sdx handles are
// released right after construction and re-opened on demand in Get()/
// Delete()/ReadIndexEntry(). Servers hosting hundreds of thousands of
// read-only volumes otherwise hold 2 fds per volume for the process
// lifetime and exhaust the fd limit ("too many open files"), which drops
// client connections.
type SortedFileNeedleMap struct {
	baseNeedleMapper
	baseFileName string
	dbFileSize   int64
}

func NewSortedFileNeedleMap(indexBaseFileName string, indexFile *os.File, version needle.Version) (m *SortedFileNeedleMap, err error) {
	m = &SortedFileNeedleMap{baseFileName: indexBaseFileName}
	m.indexFile = indexFile
	fileName := indexBaseFileName + ".sdx"
	if !isSortedFileFresh(fileName, indexFile) {
		glog.V(0).Infof("Start to Generate %s from %s", fileName, indexFile.Name())
		erasure_coding.WriteSortedFileFromIdx(indexBaseFileName, ".sdx")
		glog.V(0).Infof("Finished Generating %s from %s", fileName, indexFile.Name())
	}
	glog.V(1).Infof("Opening %s...", fileName)

	sdxFile, err := os.OpenFile(indexBaseFileName+".sdx", os.O_RDWR, 0)
	if err != nil {
		return
	}
	// only needed during construction; Get()/Delete() re-open it on demand
	defer sdxFile.Close()
	sdxStat, _ := sdxFile.Stat()
	m.dbFileSize = sdxStat.Size()
	// Seed indexFileOffset so Delete() appends tombstones to the tail of
	// .idx instead of overwriting from offset 0 and clobbering existing
	// records with tombstones for unrelated keys.
	indexStat, statErr := indexFile.Stat()
	if statErr != nil {
		return nil, fmt.Errorf("stat %s: %v", indexFile.Name(), statErr)
	}
	m.indexFileOffset = indexStat.Size()
	glog.V(1).Infof("Loading %s...", indexFile.Name())
	mm, indexLoadError := newNeedleMapMetricFromIndexFile(indexFile, version)
	if indexLoadError != nil {
		return nil, indexLoadError
	}
	m.mapMetric = *mm

	// release the .idx fd; Delete()/ReadIndexEntry() re-open it on demand
	_ = m.indexFile.Close()
	m.indexFile = nil

	return
}

func isSortedFileFresh(dbFileName string, indexFile *os.File) bool {
	// normally we always write to index file first
	dbFile, err := os.Open(dbFileName)
	if err != nil {
		return false
	}
	defer dbFile.Close()
	dbStat, dbStatErr := dbFile.Stat()
	indexStat, indexStatErr := indexFile.Stat()
	if dbStatErr != nil || indexStatErr != nil {
		glog.V(0).Infof("Can not stat file: %v and %v", dbStatErr, indexStatErr)
		return false
	}

	return dbStat.ModTime().After(indexStat.ModTime())
}

func (m *SortedFileNeedleMap) Get(key NeedleId) (element *needle_map.NeedleValue, ok bool) {
	sdxFile, err := os.Open(m.baseFileName + ".sdx")
	if err != nil {
		glog.V(1).Infof("open %s.sdx: %v", m.baseFileName, err)
		return nil, false
	}
	defer sdxFile.Close()
	offset, size, err := erasure_coding.SearchNeedleFromSortedIndex(sdxFile, m.dbFileSize, key, nil)
	ok = err == nil
	return &needle_map.NeedleValue{Key: key, Offset: offset, Size: size}, ok

}

func (m *SortedFileNeedleMap) Put(key NeedleId, offset Offset, size Size) error {
	return os.ErrInvalid
}

func (m *SortedFileNeedleMap) Delete(key NeedleId, offset Offset) error {
	sdxFile, err := os.OpenFile(m.baseFileName+".sdx", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer sdxFile.Close()

	_, size, err := erasure_coding.SearchNeedleFromSortedIndex(sdxFile, m.dbFileSize, key, nil)

	if err != nil {
		if err == erasure_coding.NotFoundError {
			return nil
		}
		return err
	}

	if size.IsDeleted() {
		return nil
	}

	if err := m.ensureIndexFile(); err != nil {
		return err
	}
	// write to index file first
	if err := m.appendToIndexFile(key, offset, TombstoneFileSize); err != nil {
		return err
	}
	_, _, err = erasure_coding.SearchNeedleFromSortedIndex(sdxFile, m.dbFileSize, key, erasure_coding.MarkNeedleDeleted)

	return err
}

// ensureIndexFile lazily re-opens the .idx fd released after construction.
// The fd is kept open for subsequent appends (Delete) or walks
// (ReadIndexEntry); only volumes that were actually written to pay for it.
func (m *SortedFileNeedleMap) ensureIndexFile() error {
	m.indexFileAccessLock.Lock()
	defer m.indexFileAccessLock.Unlock()
	if m.indexFile != nil {
		return nil
	}
	f, err := os.OpenFile(m.baseFileName+".idx", os.O_RDWR, 0644)
	if err != nil {
		// fall back for read-only mounts or restricted permissions
		if f, err = os.Open(m.baseFileName + ".idx"); err != nil {
			return fmt.Errorf("reopen %s.idx: %v", m.baseFileName, err)
		}
	}
	m.indexFile = f
	return nil
}

// IndexFileSize stats the .idx by name instead of keeping its fd open.
func (m *SortedFileNeedleMap) IndexFileSize() uint64 {
	stat, err := os.Stat(m.baseFileName + ".idx")
	if err == nil {
		return uint64(stat.Size())
	}
	return 0
}

// Sync fsyncs the .idx only when it was re-opened by Delete(); with no fd
// there is nothing buffered to flush.
func (m *SortedFileNeedleMap) Sync() error {
	if m.indexFile == nil {
		return nil
	}
	return m.indexFile.Sync()
}

// ReadIndexEntry lazily re-opens the .idx; the fd stays open for the whole
// walk so entry-by-entry scans (volume backup) do not re-open per entry.
func (m *SortedFileNeedleMap) ReadIndexEntry(n int64) (key NeedleId, offset Offset, size Size, err error) {
	if err = m.ensureIndexFile(); err != nil {
		return
	}
	bytes := make([]byte, NeedleMapEntrySize)
	var readCount int
	if readCount, err = m.indexFile.ReadAt(bytes, n*NeedleMapEntrySize); err != nil {
		if err == io.EOF {
			if readCount == NeedleMapEntrySize {
				err = nil
			}
		}
		if err != nil {
			return
		}
	}
	key, offset, size = idx.IdxFileEntry(bytes)
	return
}

func (m *SortedFileNeedleMap) Close() {
	if m == nil {
		return
	}
	if m.indexFile != nil {
		m.indexFile.Close()
	}
}

func (m *SortedFileNeedleMap) Destroy() error {
	m.Close()
	os.Remove(m.baseFileName + ".idx")
	return os.Remove(m.baseFileName + ".sdx")
}
