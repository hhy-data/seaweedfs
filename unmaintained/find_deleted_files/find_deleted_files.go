package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

var (
	logDir         = flag.String("logdir", "", "directory containing log files (e.g., downloaded_logs)")
	logFile        = flag.String("logfile", "", "single log file to analyze")
	outputFile     = flag.String("output", "", "output file (default: stdout)")
	verbose        = flag.Bool("v", false, "verbose output")
	showDetails    = flag.Bool("details", false, "show detailed file information (chunks, attributes)")
	pathFilter     = flag.String("path", "", "filter by path prefix (e.g., /bucket/subfolder/)")
	timeFrom       = flag.String("time-from", "", "filter by time from (RFC3339 format, e.g., 2025-09-03T00:00:00Z)")
	timeTo         = flag.String("time-to", "", "filter by time to (RFC3339 format, e.g., 2025-09-03T23:59:59Z)")
	includeUploads = flag.Bool("include-uploads", false, "include .uploads directory deletions (internal multipart operations)")
	batchSize      = flag.Int("batch-size", 1000, "number of files to track in memory at once")
	maxMemoryMB    = flag.Int("max-memory", 500, "maximum memory usage in MB (approximate)")
)

// FileLifecycleSummary represents a lightweight file lifecycle summary
type FileLifecycleSummary struct {
	Path            string
	Name            string
	Directory       string
	FirstCreateTime *time.Time
	LastDeleteTime  *time.Time
	LastEventType   string // "create", "update", "delete"
	LastEventTime   time.Time
	CreateTimestamp int64 // from attributes
	FileSize        uint64
	IsDirectory     bool
	EventCount      int
}

// MemoryTracker tracks memory usage
type MemoryTracker struct {
	TrackedFiles int
	MaxFiles     int
}

// DeletedFileRecord represents a permanently deleted file record
type DeletedFileRecord struct {
	Path        string
	Name        string
	Directory   string
	DeleteTime  time.Time  // When the file was deleted
	CreateTime  *time.Time // When the file was created (may be nil)
	Entry       *filer_pb.Entry
}

// Regex pattern for log files: XX-XX.XXXXXXXX (time.8-char-hex)
var logFilePattern = regexp.MustCompile(`^\d{2}-\d{2}\.[a-f0-9]{8}$`)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Find deleted files from SeaweedFS filer log files.\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  # Analyze all log files in directory\n")
		fmt.Fprintf(os.Stderr, "  %s -logdir=downloaded_logs\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Analyze single log file\n")
		fmt.Fprintf(os.Stderr, "  %s -logfile=downloaded_logs/2025-09-03/09-32.6e72fd9a\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Filter by path and show details\n")
		fmt.Fprintf(os.Stderr, "  %s -logdir=downloaded_logs -path=/bucket/docs/ -details\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Filter by time range\n")
		fmt.Fprintf(os.Stderr, "  %s -logdir=downloaded_logs -time-from=2025-09-03T10:00:00Z -time-to=2025-09-03T12:00:00Z\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Include .uploads directory deletions (normally filtered out)\n")
		fmt.Fprintf(os.Stderr, "  %s -logdir=downloaded_logs -include-uploads\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Control memory usage for large datasets\n")
		fmt.Fprintf(os.Stderr, "  %s -logdir=downloaded_logs -batch-size=500 -max-memory=200\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}

	flag.Parse()
	util_http.InitGlobalHttpClient()

	if *logDir == "" && *logFile == "" {
		fmt.Fprintf(os.Stderr, "Error: either -logdir or -logfile must be specified\n")
		flag.Usage()
		os.Exit(1)
	}

	if *logDir != "" && *logFile != "" {
		fmt.Fprintf(os.Stderr, "Error: cannot use both -logdir and -logfile at the same time\n")
		os.Exit(1)
	}

	// Parse time filters
	var fromTime, toTime *time.Time
	if *timeFrom != "" {
		t, err := time.Parse(time.RFC3339, *timeFrom)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid time-from format. Use RFC3339 format (e.g., 2025-09-03T10:00:00Z)\n")
			os.Exit(1)
		}
		fromTime = &t
	}

	if *timeTo != "" {
		t, err := time.Parse(time.RFC3339, *timeTo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid time-to format. Use RFC3339 format (e.g., 2025-09-03T23:59:59Z)\n")
			os.Exit(1)
		}
		toTime = &t
	}

	fmt.Printf("SeaweedFS Deleted Files Finder\n")

	var deletedFiles []DeletedFileRecord
	var err error

	if *logFile != "" {
		fmt.Printf("Analyzing single log file: %s\n", *logFile)
		deletedFiles, err = analyzeLogFile(*logFile, fromTime, toTime)
		if err != nil {
			log.Fatalf("failed to analyze log file %s: %v", *logFile, err)
		}
	} else {
		fmt.Printf("Analyzing log directory: %s\n", *logDir)
		deletedFiles, err = analyzeLogDirectory(*logDir, fromTime, toTime)
		if err != nil {
			log.Fatalf("failed to analyze log directory %s: %v", *logDir, err)
		}
	}

	// Apply path filter
	if *pathFilter != "" {
		filtered := make([]DeletedFileRecord, 0)
		for _, record := range deletedFiles {
			fullPath := filepath.Join(record.Directory, record.Name)
			if strings.HasPrefix(fullPath, *pathFilter) {
				filtered = append(filtered, record)
			}
		}
		deletedFiles = filtered
		fmt.Printf("Path filter: %s\n", *pathFilter)
	}

	// Sort by delete timestamp
	sort.Slice(deletedFiles, func(i, j int) bool {
		return deletedFiles[i].DeleteTime.Before(deletedFiles[j].DeleteTime)
	})

	fmt.Printf("\n=== Found %d deleted files ===\n", len(deletedFiles))

	// Setup output writer
	var writer *os.File
	if *outputFile != "" {
		writer, err = os.Create(*outputFile)
		if err != nil {
			log.Fatalf("failed to create output file: %v", err)
		}
		defer writer.Close()
		fmt.Printf("Writing results to: %s\n\n", *outputFile)
	} else {
		writer = os.Stdout
	}

	// Output results
	outputResults(writer, deletedFiles)

	if len(deletedFiles) > 0 {
		fmt.Printf("\n✅ Analysis complete. Found %d deleted files.\n", len(deletedFiles))
	} else {
		fmt.Printf("\n📝 No deleted files found matching the criteria.\n")
	}
}

func isLogFile(filename string) bool {
	return logFilePattern.MatchString(filename)
}

// isUploadPath checks if a path is related to .uploads directory (multipart upload operations)
// Matches paths like: /buckets/bucket1/.uploads, /buckets/bucket1/.uploads/xxx, etc.
func isUploadPath(path string) bool {
	return strings.Contains(path, "/.uploads/") || strings.HasSuffix(path, "/.uploads")
}

func analyzeLogDirectory(logDir string, fromTime, toTime *time.Time) ([]DeletedFileRecord, error) {
	// Calculate memory limits
	estimatedBytesPerFile := 200 // rough estimate per file lifecycle
	maxFiles := (*maxMemoryMB * 1024 * 1024) / estimatedBytesPerFile
	if maxFiles < *batchSize {
		maxFiles = *batchSize
	}

	if *verbose {
		fmt.Printf("Memory limit: %d MB, max tracked files: %d\n", *maxMemoryMB, maxFiles)
	}

	// Collect log files first
	var logFiles []string
	err := filepath.Walk(logDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		// Check if file matches log file pattern (XX-XX.XXXXXXXX)
		if !isLogFile(info.Name()) {
			if *verbose {
				fmt.Printf("Skipping non-log file: %s\n", info.Name())
			}
			return nil
		}

		logFiles = append(logFiles, path)
		return nil
	})

	if err != nil {
		return nil, err
	}

	if *verbose {
		fmt.Printf("Found %d log files to process\n", len(logFiles))
	}

	// Process log files in streaming fashion
	return processLogFilesStreaming(logFiles, maxFiles, fromTime, toTime)
}

func analyzeLogFile(filePath string, fromTime, toTime *time.Time) ([]DeletedFileRecord, error) {
	// For single file analysis, use simplified processing
	maxFiles := (*maxMemoryMB * 1024 * 1024) / 200 // estimate 200 bytes per file
	if maxFiles < *batchSize {
		maxFiles = *batchSize
	}
	
	return processLogFilesStreaming([]string{filePath}, maxFiles, fromTime, toTime)
}

// processLogFilesStreaming processes log files in a memory-efficient streaming manner
func processLogFilesStreaming(logFiles []string, maxFiles int, fromTime, toTime *time.Time) ([]DeletedFileRecord, error) {
	var allDeletedFiles []DeletedFileRecord
	fileLifecycles := make(map[string]*FileLifecycleSummary)
	memTracker := &MemoryTracker{MaxFiles: maxFiles}

	for _, logFile := range logFiles {
		if *verbose {
			fmt.Printf("Processing: %s\n", logFile)
		}

		err := processLogFileEventsStreaming(logFile, fileLifecycles, memTracker)
		if err != nil {
			if *verbose {
				fmt.Printf("Warning: failed to analyze %s: %v\n", logFile, err)
			}
			continue
		}

		// Check if we need to flush some results to manage memory
		if memTracker.TrackedFiles > maxFiles {
			if *verbose {
				fmt.Printf("Memory limit reached (%d files), flushing batch...\n", memTracker.TrackedFiles)
			}
			
			batch := findPermanentlyDeletedFilesStreaming(fileLifecycles, fromTime, toTime)
			allDeletedFiles = append(allDeletedFiles, batch...)
			
			// Clear processed lifecycles to free memory
			fileLifecycles = make(map[string]*FileLifecycleSummary)
			memTracker.TrackedFiles = 0
		}
	}

	// Process final batch
	if len(fileLifecycles) > 0 {
		batch := findPermanentlyDeletedFilesStreaming(fileLifecycles, fromTime, toTime)
		allDeletedFiles = append(allDeletedFiles, batch...)
	}

	return allDeletedFiles, nil
}

// processLogFileEventsStreaming processes all events from a single log file with memory efficiency
func processLogFileEventsStreaming(filePath string, fileLifecycles map[string]*FileLifecycleSummary, memTracker *MemoryTracker) error {
	file, err := os.OpenFile(filePath, os.O_RDONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	sizeBuf := make([]byte, 4)

	for {
		if n, err := file.Read(sizeBuf); n != 4 {
			if err == io.EOF {
				break
			}
			return err
		}

		size := util.BytesToUint32(sizeBuf)
		data := make([]byte, int(size))

		if n, err := file.Read(data); n != len(data) {
			return err
		}

		logEntry := &filer_pb.LogEntry{}
		err := proto.Unmarshal(data, logEntry)
		if err != nil {
			if *verbose {
				log.Printf("Warning: unexpected unmarshal filer_pb.LogEntry: %v", err)
			}
			continue
		}

		event := &filer_pb.SubscribeMetadataResponse{}
		err = proto.Unmarshal(logEntry.Data, event)
		if err != nil {
			if *verbose {
				log.Printf("Warning: unexpected unmarshal filer_pb.SubscribeMetadataResponse: %v", err)
			}
			continue
		}

		timestamp := time.Unix(0, int64(logEntry.TsNs))
		
		// Process different event types
		var eventType string
		var entry *filer_pb.Entry
		var fullPath string

		switch {
		case event.EventNotification.NewEntry != nil && event.EventNotification.OldEntry == nil:
			// File create
			eventType = "create"
			entry = event.EventNotification.NewEntry
			fullPath = filepath.Join(event.Directory, entry.Name)
			
		case event.EventNotification.NewEntry != nil && event.EventNotification.OldEntry != nil:
			// File update
			eventType = "update"
			entry = event.EventNotification.NewEntry
			fullPath = filepath.Join(event.Directory, entry.Name)
			
		case event.EventNotification.NewEntry == nil && event.EventNotification.OldEntry != nil:
			// File delete
			eventType = "delete"
			entry = event.EventNotification.OldEntry
			fullPath = filepath.Join(event.Directory, entry.Name)
			
		default:
			// Unknown event, skip
			continue
		}

		// Get or create file lifecycle summary
		if fileLifecycles[fullPath] == nil {
			fileLifecycles[fullPath] = &FileLifecycleSummary{
				Path:      fullPath,
				Name:      entry.Name,
				Directory: event.Directory,
			}
			memTracker.TrackedFiles++
		}

		lifecycle := fileLifecycles[fullPath]
		lifecycle.EventCount++
		
		// Update lifecycle state (only keep essential info)
		if eventType == "create" && lifecycle.FirstCreateTime == nil {
			lifecycle.FirstCreateTime = &timestamp
		}
		if eventType == "delete" {
			lifecycle.LastDeleteTime = &timestamp
		}
		
		// Always update last event info
		lifecycle.LastEventType = eventType
		lifecycle.LastEventTime = timestamp
		
		// Extract essential info from entry (avoid storing full entry)
		if entry.Attributes != nil {
			if entry.Attributes.Crtime > 0 {
				lifecycle.CreateTimestamp = entry.Attributes.Crtime
			}
			lifecycle.FileSize = entry.Attributes.FileSize
		}
		lifecycle.IsDirectory = entry.IsDirectory
	}

	return nil
}

// findPermanentlyDeletedFilesStreaming analyzes lightweight file lifecycles to find truly deleted files
func findPermanentlyDeletedFilesStreaming(fileLifecycles map[string]*FileLifecycleSummary, fromTime, toTime *time.Time) []DeletedFileRecord {
	var records []DeletedFileRecord

	for fullPath, lifecycle := range fileLifecycles {
		// Only include files that are finally deleted
		if lifecycle.LastEventType != "delete" {
			continue
		}

		// Apply time filters to delete time
		if lifecycle.LastDeleteTime == nil {
			continue
		}
		
		if fromTime != nil && lifecycle.LastDeleteTime.Before(*fromTime) {
			continue
		}
		if toTime != nil && lifecycle.LastDeleteTime.After(*toTime) {
			continue
		}

		// Filter out .uploads directory deletions unless explicitly requested
		if !*includeUploads && isUploadPath(fullPath) {
			if *verbose {
				fmt.Printf("Skipping .uploads path: %s\n", fullPath)
			}
			continue
		}

		// Extract creation time
		var createTime *time.Time
		if lifecycle.CreateTimestamp > 0 {
			ct := time.Unix(lifecycle.CreateTimestamp, 0)
			createTime = &ct
		} else if lifecycle.FirstCreateTime != nil {
			createTime = lifecycle.FirstCreateTime
		}

		// Create a minimal entry for compatibility
		entry := &filer_pb.Entry{
			Name:        lifecycle.Name,
			IsDirectory: lifecycle.IsDirectory,
		}
		if lifecycle.FileSize > 0 || lifecycle.CreateTimestamp > 0 {
			entry.Attributes = &filer_pb.FuseAttributes{
				FileSize: lifecycle.FileSize,
				Crtime:   lifecycle.CreateTimestamp,
			}
		}

		record := DeletedFileRecord{
			Path:       fullPath,
			Name:       lifecycle.Name,
			Directory:  lifecycle.Directory,
			DeleteTime: *lifecycle.LastDeleteTime,
			CreateTime: createTime,
			Entry:      entry,
		}

		records = append(records, record)
	}

	return records
}

func outputResults(writer *os.File, records []DeletedFileRecord) {
	if len(records) == 0 {
		return
	}

	// Output header
	if *showDetails {
		fmt.Fprintf(writer, "%-30s %-30s %-50s %-8s %-12s %s\n", "DELETE_TIME", "CREATE_TIME", "PATH", "IS_DIR", "SIZE", "DETAILS")
		fmt.Fprintf(writer, "%s\n", strings.Repeat("-", 150))
	} else {
		fmt.Fprintf(writer, "%-30s %-30s %s\n", "DELETE_TIME", "CREATE_TIME", "PATH")
		fmt.Fprintf(writer, "%s\n", strings.Repeat("-", 110))
	}

	for _, record := range records {
		deleteTimeStr := record.DeleteTime.Format(time.RFC3339)
		
		var createTimeStr string
		if record.CreateTime != nil {
			createTimeStr = record.CreateTime.Format(time.RFC3339)
		} else {
			createTimeStr = "N/A"
		}

		if *showDetails {
			isDir := "false"
			if record.Entry.IsDirectory {
				isDir = "true"
			}

			var size int64
			for _, chunk := range record.Entry.Chunks {
				size += int64(chunk.Size)
			}

			sizeStr := fmt.Sprintf("%d", size)
			if size == 0 && !record.Entry.IsDirectory {
				sizeStr = "0"
			} else if record.Entry.IsDirectory {
				sizeStr = "-"
			}

			details := fmt.Sprintf("chunks:%d", len(record.Entry.Chunks))
			if record.Entry.Attributes != nil {
				details += fmt.Sprintf(" mode:%o", record.Entry.Attributes.FileMode)
				if record.Entry.Attributes.Uid > 0 {
					details += fmt.Sprintf(" uid:%d", record.Entry.Attributes.Uid)
				}
				if record.Entry.Attributes.Gid > 0 {
					details += fmt.Sprintf(" gid:%d", record.Entry.Attributes.Gid)
				}
			}

			fmt.Fprintf(writer, "%-30s %-30s %-50s %-8s %-12s %s\n",
				deleteTimeStr, createTimeStr, record.Path, isDir, sizeStr, details)

			// Show chunk details if requested and verbose
			if *verbose && len(record.Entry.Chunks) > 0 {
				for _, chunk := range record.Entry.Chunks {
					fmt.Fprintf(writer, "    chunk: fileId=%s offset=%d size=%d\n",
						chunk.FileId, chunk.Offset, chunk.Size)
				}
			}
		} else {
			fmt.Fprintf(writer, "%-30s %-30s %s\n", deleteTimeStr, createTimeStr, record.Path)
		}
	}
}
