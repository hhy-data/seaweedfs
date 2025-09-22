package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

var (
	filerURL    = flag.String("filer", "", "filer URL (e.g., http://192.168.2.11:8888)")
	logPath     = flag.String("path", "/topics/.system/log", "log path on filer (default: /topics/.system/log)")
	outputDir   = flag.String("output", "downloaded_logs", "local output directory")
	concurrency = flag.Int("concurrency", 4, "number of concurrent downloads")
	verbose     = flag.Bool("v", false, "verbose output")
	retryCount  = flag.Int("retry", 3, "number of retries for failed downloads")
	timeout     = flag.Int("timeout", 30, "timeout in seconds for each download")
)

// FilerEntry represents a file or directory entry from filer API
type FilerEntry struct {
	FullPath string `json:"FullPath"`
	Mode     uint32 `json:"Mode"`
	FileSize int64  `json:"FileSize"`
	ModTime  string `json:"Mtime"`
}

// FilerListResponse represents the response from filer list API
type FilerListResponse struct {
	Path    string       `json:"Path"`
	Entries []FilerEntry `json:"Entries"`
}

// DownloadTask represents a single file download task
type DownloadTask struct {
	RemotePath string
	LocalPath  string
	Size       int64
	ModTime    string
}

// DownloadStats tracks download statistics
type DownloadStats struct {
	TotalFiles     int
	TotalSize      int64
	DownloadedSize int64
	CompletedFiles int
	FailedFiles    int
	StartTime      time.Time
	mu             sync.Mutex
}

func main() {
	flag.Parse()
	util_http.InitGlobalHttpClient()

	if *filerURL == "" {
		fmt.Fprintf(os.Stderr, "Error: filer URL is required\n")
		flag.Usage()
		os.Exit(1)
	}

	// Validate and normalize filer URL
	if !strings.HasPrefix(*filerURL, "http://") && !strings.HasPrefix(*filerURL, "https://") {
		*filerURL = "http://" + *filerURL
	}
	*filerURL = strings.TrimSuffix(*filerURL, "/")

	fmt.Printf("SeaweedFS Log Downloader\n")
	fmt.Printf("Filer URL: %s\n", *filerURL)
	fmt.Printf("Log path: %s\n", *logPath)
	fmt.Printf("Output directory: %s\n", *outputDir)

	// Discover all log files
	if *verbose {
		fmt.Printf("\n=== Discovering log files ===\n")
	}

	downloadTasks, err := discoverLogFiles(*filerURL, *logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error discovering log files: %v\n", err)
		os.Exit(1)
	}

	if len(downloadTasks) == 0 {
		fmt.Printf("No log files found in %s\n", *logPath)
		return
	}

	// Calculate total size
	var totalSize int64
	for _, task := range downloadTasks {
		totalSize += task.Size
	}

	fmt.Printf("\n=== Download Summary ===\n")
	fmt.Printf("Found %d log files\n", len(downloadTasks))
	fmt.Printf("Total size: %.2f MB\n", float64(totalSize)/(1024*1024))

	// Create output directory
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating output directory: %v\n", err)
		os.Exit(1)
	}

	// Download files
	stats := &DownloadStats{
		TotalFiles: len(downloadTasks),
		TotalSize:  totalSize,
		StartTime:  time.Now(),
	}

	fmt.Printf("\n=== Starting Downloads ===\n")
	err = downloadFiles(downloadTasks, stats)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error during download: %v\n", err)
		os.Exit(1)
	}

	// Print final statistics
	elapsed := time.Since(stats.StartTime)
	fmt.Printf("\n=== Download Complete ===\n")
	fmt.Printf("Successfully downloaded: %d/%d files\n", stats.CompletedFiles, stats.TotalFiles)
	fmt.Printf("Failed downloads: %d\n", stats.FailedFiles)
	fmt.Printf("Total size downloaded: %.2f MB\n", float64(stats.DownloadedSize)/(1024*1024))
	fmt.Printf("Total time: %v\n", elapsed)
	if elapsed.Seconds() > 0 {
		fmt.Printf("Average speed: %.2f MB/s\n",
			float64(stats.DownloadedSize)/(1024*1024)/elapsed.Seconds())
	}

	if stats.FailedFiles == 0 {
		fmt.Printf("\n✅ All files downloaded successfully!\n")
		fmt.Printf("You can now run: ./log_recover_all -logdir=%s\n", *outputDir)
	} else {
		fmt.Printf("\n⚠️  Some downloads failed. Check the output for details.\n")
	}
}

// discoverLogFiles recursively discovers all log files in the given path
func discoverLogFiles(filerURL, logPath string) ([]*DownloadTask, error) {
	var allTasks []*DownloadTask
	visited := make(map[string]bool)

	err := walkDirectory(filerURL, logPath, *outputDir, &allTasks, visited)
	if err != nil {
		return nil, err
	}

	// Sort by remote path for consistent ordering
	sort.Slice(allTasks, func(i, j int) bool {
		return allTasks[i].RemotePath < allTasks[j].RemotePath
	})

	return allTasks, nil
}

// walkDirectory recursively walks a directory and collects download tasks
func walkDirectory(filerURL, remotePath, localBasePath string, tasks *[]*DownloadTask, visited map[string]bool) error {
	// Prevent infinite loops
	if visited[remotePath] {
		return nil
	}
	visited[remotePath] = true

	if *verbose {
		fmt.Printf("Scanning: %s\n", remotePath)
	}

	entries, err := listDirectory(filerURL, remotePath)
	if err != nil {
		return fmt.Errorf("failed to list directory %s: %v", remotePath, err)
	}
	fmt.Printf("Found %d entries, in %s\n", len(entries), filerURL)

	for _, entry := range entries {
		fmt.Printf("entry name: %s, file size: %d, mod time: %s\n", entry.FullPath, entry.FileSize, entry.ModTime)

		// filer REST API does not return if an entry is a directory, here assume
		// if the file size is 0, and name like yyyy-mm-dd, it is a log directory
		baseName := filepath.Base(entry.FullPath)

		if entry.FileSize == 0 && isValidDateStr(baseName) {
			// scan log files inside
			subRemotePath := filepath.Join(remotePath, baseName)
			err := walkDirectory(filerURL, subRemotePath, localBasePath, tasks, visited)
			if err != nil {
				if *verbose {
					fmt.Printf("Warning: failed to scan directory %s: %v\n", subRemotePath, err)
				}
				continue
			}
		} else {
			// Add file to download tasks
			remoteFilePath := filepath.Join(remotePath, filepath.Base(entry.FullPath))

			// Calculate local path - remove the log path prefix and add to output dir
			relPath := strings.TrimPrefix(remoteFilePath, *logPath)
			relPath = strings.TrimPrefix(relPath, "/")
			localFilePath := filepath.Join(localBasePath, relPath)

			task := &DownloadTask{
				RemotePath: remoteFilePath,
				LocalPath:  localFilePath,
				Size:       entry.FileSize,
				ModTime:    entry.ModTime,
			}

			*tasks = append(*tasks, task)

			if *verbose {
				fmt.Printf("  File: %s (%d bytes)\n", entry.FullPath, entry.FileSize)
			}
		}
	}

	return nil
}

func listDirectory(filerURL, path string) ([]FilerEntry, error) {
	apiURL := fmt.Sprintf("%s%s?pretty=y", filerURL, path)

	if *verbose {
		fmt.Printf("GET %s\n", apiURL)
	}

	// Create HTTP request with proper headers
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	// Set Accept header for JSON response
	req.Header.Set("Accept", "application/json")

	// Make HTTP request
	client := &http.Client{
		Timeout: time.Duration(*timeout) * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Parse JSON response
	var listResp FilerListResponse
	decoder := json.NewDecoder(resp.Body)
	err = decoder.Decode(&listResp)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JSON response: %v", err)
	}

	return listResp.Entries, nil
}

// downloadFiles downloads all files with concurrent workers
func downloadFiles(tasks []*DownloadTask, stats *DownloadStats) error {
	taskChan := make(chan *DownloadTask, len(tasks))
	var wg sync.WaitGroup

	// Start progress reporter
	stopProgress := make(chan bool)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				stats.mu.Lock()
				if stats.TotalSize > 0 {
					percent := float64(stats.DownloadedSize) * 100 / float64(stats.TotalSize)
					fmt.Printf("\rProgress: %.1f%% (%d/%d files, %.2f MB/%.2f MB)",
						percent, stats.CompletedFiles, stats.TotalFiles,
						float64(stats.DownloadedSize)/(1024*1024),
						float64(stats.TotalSize)/(1024*1024))
				}
				stats.mu.Unlock()
			case <-stopProgress:
				return
			}
		}
	}()

	// Start worker goroutines
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for task := range taskChan {
				err := downloadFile(task, stats, workerID)
				if err != nil {
					fmt.Printf("\nWorker %d: Failed to download %s: %v\n",
						workerID, task.RemotePath, err)
					stats.mu.Lock()
					stats.FailedFiles++
					stats.mu.Unlock()
				}
			}
		}(i)
	}

	// Send tasks to workers
	for _, task := range tasks {
		taskChan <- task
	}
	close(taskChan)

	// Wait for completion
	wg.Wait()
	stopProgress <- true

	fmt.Printf("\n") // New line after progress

	return nil
}

// downloadFile downloads a single file with retries
func downloadFile(task *DownloadTask, stats *DownloadStats, workerID int) error {
	var lastErr error

	for attempt := 1; attempt <= *retryCount; attempt++ {
		err := downloadFileAttempt(task, stats, workerID)
		if err == nil {
			return nil
		}

		lastErr = err
		if attempt < *retryCount {
			if *verbose {
				fmt.Printf("Worker %d: Retry %d/%d for %s: %v\n",
					workerID, attempt, *retryCount, task.RemotePath, err)
			}
			time.Sleep(time.Duration(attempt) * time.Second) // Exponential backoff
		}
	}

	return lastErr
}

// downloadFileAttempt performs a single download attempt
func downloadFileAttempt(task *DownloadTask, stats *DownloadStats, workerID int) error {
	// Create local directory if needed
	localDir := filepath.Dir(task.LocalPath)
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %v", localDir, err)
	}

	// Check if file already exists and has the same size
	if info, err := os.Stat(task.LocalPath); err == nil {
		if info.Size() == task.Size {
			if *verbose {
				fmt.Printf("Worker %d: Skipping %s (already exists)\n",
					workerID, task.RemotePath)
			}
			stats.mu.Lock()
			stats.CompletedFiles++
			stats.DownloadedSize += task.Size
			stats.mu.Unlock()
			return nil
		}
	}

	// Construct download URL
	downloadURL := fmt.Sprintf("%s%s", *filerURL, task.RemotePath)

	if *verbose {
		fmt.Printf("Worker %d: Downloading %s\n", workerID, task.RemotePath)
	}

	// Create HTTP request
	req, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// Make HTTP request
	client := &http.Client{
		Timeout: time.Duration(*timeout) * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Create temporary file
	tmpPath := task.LocalPath + ".tmp"
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}

	// Copy data
	written, err := io.Copy(tmpFile, resp.Body)
	tmpFile.Close()

	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to copy data: %v", err)
	}

	// Verify size
	if written != task.Size {
		os.Remove(tmpPath)
		return fmt.Errorf("size mismatch: got %d, expected %d", written, task.Size)
	}

	// Rename to final path
	if err := os.Rename(tmpPath, task.LocalPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename file: %v", err)
	}

	// Set modification time
	if task.ModTime != "" {
		// convert task.ModTime from string to time.Time
		modTime, err := time.Parse(time.RFC3339, task.ModTime)
		if err != nil {
			return fmt.Errorf("failed to parse modTime: %v", err)
		}
		os.Chtimes(task.LocalPath, modTime, modTime)
	}

	// Update statistics
	stats.mu.Lock()
	stats.CompletedFiles++
	stats.DownloadedSize += written
	stats.mu.Unlock()

	return nil
}

func isValidDateStr(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}
