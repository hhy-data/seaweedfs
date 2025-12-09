package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

var (
	logDir         = flag.String("logdir", "", "log directory path (e.g., /topics/.system/log)")
	outputMetaFile = flag.String("output", "", "output meta file path (default: auto-generated)")
	verbose        = flag.Bool("v", false, "verbose output")
	skipDeleted    = flag.Bool("skip-deleted", true, "skip recovering deleted files")
	batchSize      = flag.Int("batch-size", 10000, "batch size for processing events")
	includeUploads = flag.Bool("include-uploads", false, "include .uploads directory files (internal multipart operations)")
	maxMemoryMB    = flag.Int("max-memory", 1024, "maximum memory usage in MB (approximate)")
	flushThreshold = flag.Int("flush-threshold", 5000, "number of files to accumulate before flushing to disk")
)

// EventType represents the type of file event
type EventType int

const (
	EventCreate EventType = iota
	EventUpdate
	EventDelete
)

func (e EventType) String() string {
	switch e {
	case EventCreate:
		return "CREATE"
	case EventUpdate:
		return "UPDATE"
	case EventDelete:
		return "DELETE"
	default:
		return "UNKNOWN"
	}
}

// LogFile represents a single log file with metadata
type LogFile struct {
	Path    string
	Name    string
	ModTime time.Time
	Size    int64
}

// CompleteFileState tracks the complete state of a file across all log files
type CompleteFileState struct {
	Path            string
	FirstCreateTime int64
	LastUpdateTime  int64
	FinalEntry      *filer_pb.Entry
	AllChunks       map[string]*filer_pb.FileChunk // key: chunk unique identifier
	TotalSize       uint64
	IsDeleted       bool
	EventCount      int
	LastSeenLogFile string
}

// CompactFileState represents a memory-efficient file state
type CompactFileState struct {
	Path            string
	FirstCreateTime int64
	LastUpdateTime  int64
	TotalSize       uint64
	IsDeleted       bool
	IsDirectory     bool
	EventCount      int
	LastSeenLogFile string
	// Essential attributes only
	FileMode uint32
	Uid      uint32
	Gid      uint32
	Crtime   int64
	Mtime    int64
	// Compact chunk representation - just file IDs and sizes
	ChunkCount   int
	ChunkFileIds []string // simplified chunk tracking
	ChunkSizes   []uint64
	ChunkOffsets []int64
}

// MemoryManager tracks memory usage and manages flushing
type MemoryManager struct {
	CurrentFiles     int
	MaxFiles         int
	TotalMemoryMB    int
	EstimatedUsageMB int
}

// EventWithSource represents an event with its source log file
type EventWithSource struct {
	Event     *filer_pb.SubscribeMetadataResponse
	Timestamp int64
	LogFile   string
	FilePath  string
}

// isUploadPath checks if a path is related to .uploads directory (multipart upload operations)
// Matches paths like: /buckets/bucket1/.uploads, /buckets/bucket1/.uploads/xxx, etc.
func isUploadPath(path string) bool {
	return strings.Contains(path, "/.uploads/") || strings.HasSuffix(path, "/.uploads")
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Recover files from SeaweedFS filer log files and generate meta file for restoration.\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  # Basic recovery (skip deleted files, exclude .uploads)\n")
		fmt.Fprintf(os.Stderr, "  %s -logdir=/path/to/logs\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Include deleted files but exclude .uploads\n")
		fmt.Fprintf(os.Stderr, "  %s -logdir=/path/to/logs -skip-deleted=false\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Include both deleted files and .uploads internal operations\n")
		fmt.Fprintf(os.Stderr, "  %s -logdir=/path/to/logs -skip-deleted=false -include-uploads\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Control memory usage for large datasets (outputs multiple batch files)\n")
		fmt.Fprintf(os.Stderr, "  %s -logdir=/path/to/logs -max-memory=512 -flush-threshold=2000\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Process large datasets with aggressive memory limits\n")
		fmt.Fprintf(os.Stderr, "  %s -logdir=/path/to/logs -max-memory=256 -flush-threshold=1000\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	if *logDir == "" {
		log.Fatal("log directory is required")
	}

	if *verbose {
		fmt.Printf("Starting log recovery from directory: %s\n", *logDir)
		fmt.Printf("Batch size: %d events\n", *batchSize)
	}

	// Discover and sort log files
	logFiles, err := discoverLogFiles(*logDir)
	if err != nil {
		log.Fatalf("failed to discover log files: %v", err)
	}

	if len(logFiles) == 0 {
		log.Fatalf("no log files found in directory: %s", *logDir)
	}

	if *verbose {
		fmt.Printf("Found %d log files\n", len(logFiles))
		for i, lf := range logFiles {
			fmt.Printf("  %d. %s (size: %d bytes, mtime: %s)\n",
				i+1, lf.Name, lf.Size, lf.ModTime.Format(time.RFC3339))
		}
	}

	// Process all log files to build complete file states
	completeStates, err := processAllLogFiles(logFiles)
	if err != nil {
		log.Fatalf("failed to process log files: %v", err)
	}

	if *verbose {
		fmt.Printf("Processed %d unique files\n", len(completeStates))
	}

	// Build final filer entries from remaining states
	filerEntries, err := buildCompleteEntries(completeStates)
	if err != nil {
		log.Fatalf("failed to build complete entries: %v", err)
	}

	if *verbose {
		fmt.Printf("Final entries to save: %d\n", len(filerEntries))
	}

	// Generate output meta file for final batch
	var outputFileName string
	var totalEntries int
	
	if len(filerEntries) > 0 {
		outputFileName = *outputMetaFile
		if outputFileName == "" {
			t := time.Now()
			outputFileName = fmt.Sprintf("log-recover-final-%04d%02d%02d-%02d%02d%02d.meta",
				t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())
		}

		err = saveMetaFile(outputFileName, filerEntries)
		if err != nil {
			log.Fatalf("failed to save meta file %s: %v", outputFileName, err)
		}
		totalEntries = len(filerEntries)
		fmt.Printf("Successfully saved final batch with %d entries to %s\n", len(filerEntries), outputFileName)
	} else {
		fmt.Printf("No final entries to save\n")
	}

	// Summary of all processing
	fmt.Printf("\n=== Recovery Complete ===\n")
	fmt.Printf("Processed %d log files\n", len(logFiles))
	if totalEntries > 0 {
		fmt.Printf("Total entries recovered: %d\n", totalEntries)
		if outputFileName != "" {
			fmt.Printf("Final output file: %s\n", outputFileName)
		}
	}

	// Print summary statistics for final batch only
	if len(filerEntries) > 0 {
		printSummaryStats(completeStates, filerEntries)
	}
}

// discoverLogFiles finds and sorts all log files in the directory
func discoverLogFiles(logDir string) ([]*LogFile, error) {
	var logFiles []*LogFile

	err := filepath.Walk(logDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories and hidden files
		if info.IsDir() || strings.HasPrefix(info.Name(), ".") {
			return nil
		}

		// Skip files that are clearly not log files (e.g., .go files)
		if strings.HasSuffix(info.Name(), ".go") ||
			strings.HasSuffix(info.Name(), ".md") ||
			strings.HasSuffix(info.Name(), ".meta") {
			return nil
		}

		logFile := &LogFile{
			Path:    path,
			Name:    info.Name(),
			ModTime: info.ModTime(),
			Size:    info.Size(),
		}

		logFiles = append(logFiles, logFile)
		return nil
	})

	if err != nil {
		return nil, err
	}

	// Sort log files by modification time to ensure chronological processing
	sort.Slice(logFiles, func(i, j int) bool {
		return logFiles[i].ModTime.Before(logFiles[j].ModTime)
	})

	return logFiles, nil
}

func processAllLogFiles(logFiles []*LogFile) (map[string]*CompleteFileState, error) {
	// Initialize memory management
	estimatedBytesPerFile := 500 // rough estimate per complete file state
	maxFiles := (*maxMemoryMB * 1024 * 1024) / estimatedBytesPerFile
	if maxFiles < *flushThreshold {
		maxFiles = *flushThreshold
	}

	if *verbose {
		fmt.Printf("Memory limit: %d MB, max tracked files: %d\n", *maxMemoryMB, maxFiles)
	}

	completeStates := make(map[string]*CompleteFileState)
	totalEvents := 0
	processedFiles := 0
	totalOutputFiles := 0

	for _, logFile := range logFiles {
		if *verbose {
			fmt.Printf("Processing log file: %s\n", logFile.Name)
		}

		events, err := readEventsFromLogFile(logFile.Path)
		if err != nil {
			log.Printf("Warning: failed to read events from %s: %v", logFile.Path, err)
			continue
		}

		if *verbose {
			fmt.Printf("  Found %d events\n", len(events))
		}

		// Process events in batches to manage memory
		err = processEventsInBatches(events, logFile.Name, completeStates)
		if err != nil {
			return nil, fmt.Errorf("failed to process events from %s: %v", logFile.Name, err)
		}

		totalEvents += len(events)
		processedFiles++

		// Check if we need to flush results to manage memory
		if len(completeStates) >= *flushThreshold {
			if *verbose {
				fmt.Printf("Memory threshold reached (%d files), flushing batch %d...\n", len(completeStates), totalOutputFiles+1)
			}

			// Build and save batch entries
			batchEntries, err := buildCompleteEntries(completeStates)
			if err != nil {
				return nil, fmt.Errorf("failed to build batch entries: %v", err)
			}

			if len(batchEntries) > 0 {
				// Generate batch output file name
				t := time.Now()
				batchFileName := fmt.Sprintf("log-recover-batch-%d-%04d%02d%02d-%02d%02d%02d.meta",
					totalOutputFiles+1, t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())

				err = saveMetaFile(batchFileName, batchEntries)
				if err != nil {
					return nil, fmt.Errorf("failed to save batch meta file %s: %v", batchFileName, err)
				}

				totalOutputFiles++
				if *verbose {
					fmt.Printf("Saved batch %d with %d entries to %s\n", totalOutputFiles, len(batchEntries), batchFileName)
				}
			}

			// Clear processed states to free memory
			completeStates = make(map[string]*CompleteFileState)
		}
	}

	if *verbose {
		fmt.Printf("Total events processed: %d from %d log files\n", totalEvents, processedFiles)
		if totalOutputFiles > 0 {
			fmt.Printf("Generated %d batch files\n", totalOutputFiles)
		}
	}

	return completeStates, nil
}

func readEventsFromLogFile(filePath string) ([]*EventWithSource, error) {
	dst, err := os.OpenFile(filePath, os.O_RDONLY, 0644)
	if err != nil {
		return nil, err
	}
	defer dst.Close()

	var events []*EventWithSource
	sizeBuf := make([]byte, 4)

	for {
		if _, err := io.ReadFull(dst, sizeBuf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}

		size := util.BytesToUint32(sizeBuf)
		data := make([]byte, int(size))

		if _, err := io.ReadFull(dst, data); err != nil {
			return nil, err
		}

		logEntry := &filer_pb.LogEntry{}
		err := proto.Unmarshal(data, logEntry)
		if err != nil {
			log.Printf("Warning: failed to unmarshal LogEntry: %v", err)
			continue
		}

		event := &filer_pb.SubscribeMetadataResponse{}
		err = proto.Unmarshal(logEntry.Data, event)
		if err != nil {
			log.Printf("Warning: failed to unmarshal SubscribeMetadataResponse: %v", err)
			continue
		}

		// Extract file path from the event
		filePathFromEvent := extractFilePathFromEvent(event)
		if filePathFromEvent == "" {
			continue // Skip events without valid file paths
		}
		eventWithSource := &EventWithSource{
			Event:     event,
			Timestamp: logEntry.TsNs,
			LogFile:   filepath.Base(filePath),
			FilePath:  filePathFromEvent,
		}

		events = append(events, eventWithSource)
	}

	return events, nil
}

// extractFilePathFromEvent extracts the file path from an event
func extractFilePathFromEvent(event *filer_pb.SubscribeMetadataResponse) string {
	var fileName string

	if event.EventNotification.NewEntry != nil {
		fileName = event.EventNotification.NewEntry.Name
	} else if event.EventNotification.OldEntry != nil {
		fileName = event.EventNotification.OldEntry.Name
	} else {
		return ""
	}

	// Construct full path
	dir := strings.TrimSuffix(event.Directory, "/")
	if dir == "" {
		dir = "/"
	} else if !strings.HasPrefix(dir, "/") {
		dir = "/" + dir
	}

	return filepath.Join(dir, fileName)
}

// processEventsInBatches processes events in batches to manage memory usage
func processEventsInBatches(events []*EventWithSource, logFileName string, completeStates map[string]*CompleteFileState) error {
	for i := 0; i < len(events); i += *batchSize {
		end := i + *batchSize
		if end > len(events) {
			end = len(events)
		}

		batch := events[i:end]
		err := processBatchEvents(batch, logFileName, completeStates)
		if err != nil {
			return err
		}

		if i > 0 && i%(*batchSize*10) == 0 {
			if *verbose {
				fmt.Printf("  Processed %d events from %s\n", i, logFileName)
			}
		}
	}

	return nil
}

// processBatchEvents processes a batch of events
func processBatchEvents(events []*EventWithSource, logFileName string, completeStates map[string]*CompleteFileState) error {
	for _, eventWithSource := range events {
		err := processEvent(eventWithSource, logFileName, completeStates)
		if err != nil {
			log.Printf("Warning: failed to process event: %v", err)
			continue
		}
	}
	return nil
}

// processEvent processes a single event and updates the complete file state
func processEvent(eventWithSource *EventWithSource, logFileName string, completeStates map[string]*CompleteFileState) error {
	event := eventWithSource.Event
	filePath := eventWithSource.FilePath

	// Get or create complete file state
	if completeStates[filePath] == nil {
		completeStates[filePath] = &CompleteFileState{
			Path:            filePath,
			AllChunks:       make(map[string]*filer_pb.FileChunk),
			FirstCreateTime: eventWithSource.Timestamp,
		}
	}

	state := completeStates[filePath]
	state.EventCount++
	state.LastSeenLogFile = logFileName
	state.LastUpdateTime = eventWithSource.Timestamp

	var eventType EventType
	var entry *filer_pb.Entry

	switch {
	case event.EventNotification.NewEntry != nil && event.EventNotification.OldEntry == nil:
		// File create
		eventType = EventCreate
		entry = event.EventNotification.NewEntry
		if state.FirstCreateTime > eventWithSource.Timestamp {
			state.FirstCreateTime = eventWithSource.Timestamp
		}

	case event.EventNotification.NewEntry != nil && event.EventNotification.OldEntry != nil:
		// File update
		eventType = EventUpdate
		entry = event.EventNotification.NewEntry

	case event.EventNotification.NewEntry == nil && event.EventNotification.OldEntry != nil:
		// File delete
		eventType = EventDelete
		entry = event.EventNotification.OldEntry
		state.IsDeleted = true
		if *verbose {
			fmt.Printf("[DELETE] %s: %s (from %s)\n",
				time.Unix(0, eventWithSource.Timestamp).Format(time.RFC3339),
				filePath, logFileName)
		}
		return nil

	default:
		// Unknown event, skip
		return nil
	}

	// Update the final entry state
	state.FinalEntry = cloneEntry(entry)

	// Merge chunks from this event
	err := mergeChunksIntoState(state, entry.Chunks, eventWithSource.Timestamp)
	if err != nil {
		return err
	}

	if *verbose && eventType == EventCreate {
		fmt.Printf("[CREATE] %s: %s (from %s)\n",
			time.Unix(0, eventWithSource.Timestamp).Format(time.RFC3339),
			filePath, logFileName)
	} else if *verbose && eventType == EventUpdate {
		fmt.Printf("[UPDATE] %s: %s (%d chunks from %s)\n",
			time.Unix(0, eventWithSource.Timestamp).Format(time.RFC3339),
			filePath, len(entry.Chunks), logFileName)
	}

	return nil
}

func mergeChunksIntoState(state *CompleteFileState, chunks []*filer_pb.FileChunk, timestamp int64) error {
	for _, chunk := range chunks {
		// Create a unique key for this chunk based on offset
		chunkKey := fmt.Sprintf("offset_%d", chunk.Offset)

		// Clone the chunk to avoid reference issues
		clonedChunk := cloneChunk(chunk)
		if clonedChunk.ModifiedTsNs == 0 {
			clonedChunk.ModifiedTsNs = timestamp
		}

		// If we already have a chunk at this offset, use the newer one
		if existingChunk, exists := state.AllChunks[chunkKey]; exists {
			if clonedChunk.ModifiedTsNs >= existingChunk.ModifiedTsNs {
				state.AllChunks[chunkKey] = clonedChunk
			}
		} else {
			state.AllChunks[chunkKey] = clonedChunk
		}
	}

	// Recalculate total size
	state.TotalSize = 0
	for _, chunk := range state.AllChunks {
		if chunk.Offset+int64(chunk.Size) > int64(state.TotalSize) {
			state.TotalSize = uint64(chunk.Offset + int64(chunk.Size))
		}
	}

	return nil
}

// cloneEntry creates a deep copy of an entry
func cloneEntry(original *filer_pb.Entry) *filer_pb.Entry {
	if original == nil {
		return nil
	}
	cloned := proto.Clone(original).(*filer_pb.Entry)
	// Note: Chunks will be set separately from the merged state
	cloned.Chunks = nil
	return cloned
}

func cloneChunk(original *filer_pb.FileChunk) *filer_pb.FileChunk {
	return proto.Clone(original).(*filer_pb.FileChunk)
}

func buildCompleteEntries(completeStates map[string]*CompleteFileState) ([]*filer_pb.FullEntry, error) {
	var filerEntries []*filer_pb.FullEntry

	for filePath, state := range completeStates {
		// Skip deleted files if requested
		if *skipDeleted && state.IsDeleted {
			if *verbose {
				fmt.Printf("Skipping deleted file: %s\n", filePath)
			}
			continue
		}

		// Filter out .uploads directory files unless explicitly requested
		if !*includeUploads && isUploadPath(filePath) {
			if *verbose {
				fmt.Printf("Skipping .uploads path: %s\n", filePath)
			}
			continue
		}

		if state.FinalEntry == nil {
			if *verbose {
				fmt.Printf("Warning: no final entry for %s, skipping\n", filePath)
			}
			continue
		}

		// Build the complete entry with all chunks
		completeEntry := cloneEntry(state.FinalEntry)

		// Set chunks from the merged state
		var allChunks []*filer_pb.FileChunk
		for _, chunk := range state.AllChunks {
			allChunks = append(allChunks, chunk)
		}

		// Sort chunks by offset
		sort.Slice(allChunks, func(i, j int) bool {
			return allChunks[i].Offset < allChunks[j].Offset
		})

		completeEntry.Chunks = allChunks

		// Update file size in attributes to match actual chunks
		if completeEntry.Attributes != nil {
			completeEntry.Attributes.FileSize = state.TotalSize
		}

		// Extract directory and file name from path
		dir := "/"
		fileName := strings.TrimPrefix(filePath, "/")

		if lastSlash := strings.LastIndex(fileName, "/"); lastSlash >= 0 {
			dir = "/" + fileName[:lastSlash]
			fileName = fileName[lastSlash+1:]
		}

		completeEntry.Name = fileName

		fullEntry := &filer_pb.FullEntry{
			Dir:   dir,
			Entry: completeEntry,
		}

		filerEntries = append(filerEntries, fullEntry)
	}

	return filerEntries, nil
}

// saveMetaFile saves filer entries to meta file format
func saveMetaFile(fileName string, filerEntries []*filer_pb.FullEntry) error {
	dst, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %v", fileName, err)
	}
	defer dst.Close()

	sizeBuf := make([]byte, 4)

	for _, fullEntry := range filerEntries {
		bytes, err := proto.Marshal(fullEntry)
		if err != nil {
			return fmt.Errorf("marshal error for entry %s/%s: %v", fullEntry.Dir, fullEntry.Entry.Name, err)
		}

		util.Uint32toBytes(sizeBuf, uint32(len(bytes)))
		if _, err := dst.Write(sizeBuf); err != nil {
			return err
		}
		if _, err := dst.Write(bytes); err != nil {
			return err
		}
	}

	return nil
}

// printSummaryStats prints summary statistics
func printSummaryStats(completeStates map[string]*CompleteFileState, filerEntries []*filer_pb.FullEntry) {
	fmt.Printf("\n=== Recovery Summary ===\n")

	var totalEvents, deletedFiles, activeFiles, uploadsFiles int
	var totalSize uint64
	var dirCount, fileCount int

	for filePath, state := range completeStates {
		totalEvents += state.EventCount
		if isUploadPath(filePath) {
			uploadsFiles++
		} else if state.IsDeleted {
			deletedFiles++
		} else {
			activeFiles++
			totalSize += state.TotalSize
		}
	}

	for _, entry := range filerEntries {
		if entry.Entry.IsDirectory {
			dirCount++
		} else {
			fileCount++
		}
	}

	fmt.Printf("Total unique files tracked: %d\n", len(completeStates))
	fmt.Printf("Total events processed: %d\n", totalEvents)
	fmt.Printf("Active files: %d\n", activeFiles)
	fmt.Printf("Deleted files: %d\n", deletedFiles)
	fmt.Printf(".uploads files: %d\n", uploadsFiles)
	fmt.Printf("Final entries saved: %d (directories: %d, files: %d)\n", len(filerEntries), dirCount, fileCount)
	fmt.Printf("Total recovered data size: %.2f MB\n", float64(totalSize)/(1024*1024))

	if uploadsFiles > 0 && !*includeUploads {
		fmt.Printf("\nNote: %d .uploads files were excluded (use -include-uploads to include them)\n", uploadsFiles)
	}
}
