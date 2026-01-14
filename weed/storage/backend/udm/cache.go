package udm

import (
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

const (
	cacheThresholdHigh = 0.8 // Start cleanup when cache usage reaches 80%
	cacheThresholdLow  = 0.6 // Cleanup target is 60% of capacity
	cacheRatio         = 0.3 // Cache uses 30% of total available space
)

// LRU Cache structure
type lruCacheEntry struct {
	key  string
	size int64
	time time.Time
}

// lruCache Thread-safe LRU cache structure
type lruCache struct {
	list    *list.List               // LRU linked list
	mapping map[string]*list.Element // Fast lookup mapping

	mutex sync.RWMutex
}

func newLRUCache(basePath string) (*lruCache, error) {
	res := &lruCache{
		list:    list.New(),
		mapping: make(map[string]*list.Element),
	}

	err := res.initializeWithMTime(basePath)

	return res, err
}

// UpdateAccess Updates file access record
func (c *lruCache) UpdateAccess(filePath string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// Get file information
	info, err := os.Stat(filePath)
	if err != nil {
		glog.Errorf("Failed to stat file in cache: %v", err)
		return
	}

	// Check if already exists in LRU cache
	if elem, exists := c.mapping[filePath]; exists {
		// Update existing entry's time and size
		entry := elem.Value.(*lruCacheEntry)
		entry.time = time.Now()
		entry.size = info.Size()

		// Move element to front of list (most recently used)
		c.list.MoveToFront(elem)
	} else {
		// Add new entry to LRU cache
		entry := &lruCacheEntry{
			key:  filePath,
			size: info.Size(),
			time: time.Now(),
		}

		elem = c.list.PushFront(entry)
		c.mapping[filePath] = elem
	}
}

// Remove Removes specified file's cache record
//func (c *lruCache) Remove(filePath string) {
//	c.mutex.Lock()
//	defer c.mutex.Unlock()
//
//	if elem, exists := c.mapping[filePath]; exists {
//		c.list.Remove(elem)
//		delete(c.mapping, filePath)
//	}
//}

// GetTotalSize Gets total size of all files in cache
func (c *lruCache) getTotalSize() int64 {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	var totalSize int64
	for _, elem := range c.mapping {
		entry := elem.Value.(*lruCacheEntry)
		totalSize += entry.size
	}
	return totalSize
}

// getCacheCapacity Gets cache capacity limit
func (c *lruCache) getCacheCapacity() (int64, error) {
	elem := c.list.Front()
	if elem == nil {
		return -1, nil
	}

	availableSpace, err := getAvailableSpace(elem.Value.(*lruCacheEntry).key)
	if err != nil {
		return 0, err
	}

	// Cache capacity is 30% of available space
	return int64(float64(availableSpace) * cacheRatio), nil
}

// Cleanup Performs LRU cleanup operation
func (c *lruCache) Cleanup() {
	// Get cache capacity limit
	cacheCapacity, err := c.getCacheCapacity()
	if err != nil {
		glog.V(0).Infof("Failed to get cache capacity for cleanup: %v", err)
		return
	} else if cacheCapacity < 0 {
		return
	}

	// Calculate current cache usage
	currentCacheSize := c.getTotalSize()

	// If current usage is less than threshold, no need to clean up
	if float64(currentCacheSize) < float64(cacheCapacity)*cacheThresholdHigh {
		return
	}

	glog.V(0).Infof("Starting cache cleanup. Current size: %d, Capacity: %d", currentCacheSize, cacheCapacity)

	// Target cleanup to 60% of capacity
	targetSize := int64(float64(cacheCapacity) * cacheThresholdLow)

	c.mutex.Lock()
	defer c.mutex.Unlock()

	// Delete from tail of list (least recently used files)
	for currentCacheSize > targetSize && c.list.Len() > 0 {
		// Get least recently used element
		elem := c.list.Back()
		if elem == nil {
			break
		}

		entry := elem.Value.(*lruCacheEntry)

		// Delete file
		if err = os.Remove(entry.key); err != nil {
			glog.Errorf("Failed to remove file from cache: %s, err: %v", entry.key, err)

			// Even if deletion fails, remove from LRU cache anyway
			c.list.Remove(elem)
			delete(c.mapping, entry.key)
			continue
		}

		// Update cache size
		currentCacheSize -= entry.size

		// Remove from LRU cache
		c.list.Remove(elem)
		delete(c.mapping, entry.key)

		glog.V(1).Infof("Removed file from cache: %s, size: %d", entry.key, entry.size)
	}

	glog.V(0).Infof("Cache cleanup completed. Final size: %d", currentCacheSize)
}

// getAvailableSpace Gets available disk space for specified path
func getAvailableSpace(path string) (int64, error) {
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	if err != nil {
		return 0, err
	}

	// Available space = number of blocks * block size
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

// initializeWithMTime scans directory and initializes LRU cache ordered by modification time
func (c *lruCache) initializeWithMTime(path string) error {
	// Check if directory exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// If directory doesn't exist, no need to initialize
		return nil
	}

	// Read all files in the directory
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("failed to read directory %s: %w", path, err)
	}

	// Collect file information and sort by modification time
	var files []*lruCacheEntry
	for _, entry := range entries {
		// Only process files, skip subdirectories
		if entry.IsDir() {
			continue
		}

		fullPath := filepath.Join(path, entry.Name())

		// Get file information
		fileInfo, err := entry.Info()
		if err != nil {
			glog.V(2).Infof("Failed to get file info for %s: %v", fullPath, err)
			continue
		}

		// Create LRU cache entry
		entryObj := &lruCacheEntry{
			key:  fullPath,
			size: fileInfo.Size(),
			time: fileInfo.ModTime(),
		}
		files = append(files, entryObj)
	}

	// Sort by modification time in ascending order (oldest first, newest last)
	sort.Slice(files, func(i, j int) bool {
		return files[i].time.Before(files[j].time)
	})

	// Lock and populate cache
	c.mutex.Lock()
	defer c.mutex.Unlock()

	for _, fileEntry := range files {
		// Add to LRU cache ordered by modification time
		elem := c.list.PushFront(fileEntry)
		c.mapping[fileEntry.key] = elem
	}

	glog.V(0).Infof("Initialized LRU cache with %d files from %s, ordered by modification time",
		len(files), path)
	return nil
}
