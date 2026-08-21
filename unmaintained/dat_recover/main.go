// dat_recover recovers filer metadata from a volume .dat file that stored
// the filer system log (topics/.system/log) needles.
//
// Each needle body is one filer log flush batch, in the same format as the
// persisted log files:
//
//	repeated [4-byte big-endian size][filer_pb.LogEntry protobuf]
//
// where LogEntry.Data contains a serialized filer_pb.SubscribeMetadataResponse.
//
// Usage: dat_recover -dat=/path/to/3.dat -volumeId=3 -output=3.meta
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
	"github.com/seaweedfs/seaweedfs/weed/util"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

var (
	datFile        = flag.String("dat", "", "path to a single .dat file")
	datList        = flag.String("datList", "", "file listing .dat paths, one per line (# comments and blank lines ignored); volume ids are parsed from file names")
	volumeId       = flag.Int("volumeId", -1, "volume id (required with -dat, ignored with -datList)")
	outputFile     = flag.String("output", "", "output meta file path (default: auto-generated)")
	verbose        = flag.Bool("v", false, "verbose output")
	skipDeleted    = flag.Bool("skip-deleted", true, "skip recovering deleted files")
	includeUploads = flag.Bool("include-uploads", false, "include .uploads directory files (internal multipart operations)")
)

// CompleteFileState tracks the latest state of one file path across all events.
type CompleteFileState struct {
	Path         string
	AllChunks    map[string]*filer_pb.FileChunk
	FinalEntry   *filer_pb.Entry
	IsDeleted    bool
	FirstSeenNs  int64
	LastUpdateNs int64
	EventCount   int
	TotalSize    uint64
}

type DatRecoverScanner struct {
	verbose    bool
	states     map[string]*CompleteFileState
	eventCount int
	skipped    int
}

func (s *DatRecoverScanner) VisitSuperBlock(sb super_block.SuperBlock) error {
	glog.V(0).Infof("Volume version: %d, BlockSize: %d", sb.Version, sb.BlockSize())
	return nil
}

func (s *DatRecoverScanner) ReadNeedleBody() bool {
	return true
}

// VisitNeedle parses one needle as a LogBuffer flush batch:
// repeated [4-byte big-endian size][LogEntry protobuf].
// Needles are visited in append order, and records inside a batch are also
// in append order, so events are processed chronologically.
func (s *DatRecoverScanner) VisitNeedle(n *needle.Needle, offset int64, needleHeader, needleBody []byte) error {
	if n.Size.IsDeleted() || !n.Size.IsValid() {
		return nil
	}
	if len(n.Data) == 0 {
		return nil
	}

	buf := n.Data
	for len(buf) >= 4 {
		size := int(util.BytesToUint32(buf[:4]))
		if size <= 0 || 4+size > len(buf) {
			break // truncated record at batch tail
		}
		record := buf[4 : 4+size]
		buf = buf[4+size:]

		logEntry := &filer_pb.LogEntry{}
		if err := proto.Unmarshal(record, logEntry); err != nil {
			s.skipped++
			if s.verbose && s.skipped <= 10 {
				glog.V(0).Infof("  [SKIP] bad LogEntry at offset %d id=%x: %v", offset, n.Id, err)
			}
			continue
		}

		event := &filer_pb.SubscribeMetadataResponse{}
		if err := proto.Unmarshal(logEntry.Data, event); err != nil {
			s.skipped++
			if s.verbose && s.skipped <= 10 {
				glog.V(0).Infof("  [SKIP] bad event at offset %d id=%x: %v", offset, n.Id, err)
			}
			continue
		}

		s.processEvent(event, logEntry.TsNs)
	}
	return nil
}

// processEvent applies one filer event to the path states.
func (s *DatRecoverScanner) processEvent(event *filer_pb.SubscribeMetadataResponse, tsNs int64) {
	en := event.EventNotification
	if en == nil {
		s.skipped++
		return
	}

	// Rename/move: new path lives under NewParentPath, old path is gone.
	if en.NewParentPath != "" && en.NewEntry != nil && en.OldEntry != nil {
		newPath := joinPath(en.NewParentPath, en.NewEntry.Name)
		oldPath := joinPath(event.Directory, en.OldEntry.Name)
		s.applyEntry(newPath, en.NewEntry, tsNs)
		s.markDeleted(oldPath, tsNs)
		if s.verbose {
			glog.V(0).Infof("  [RENAME] %s -> %s %s", oldPath, newPath,
				time.Unix(0, tsNs).Format(time.RFC3339))
		}
		return
	}

	switch {
	case en.NewEntry != nil && en.OldEntry == nil:
		// create
		s.applyEntry(joinPath(event.Directory, en.NewEntry.Name), en.NewEntry, tsNs)
	case en.NewEntry != nil && en.OldEntry != nil:
		// update in the same directory
		s.applyEntry(joinPath(event.Directory, en.NewEntry.Name), en.NewEntry, tsNs)
	case en.NewEntry == nil && en.OldEntry != nil:
		// delete: mark the state so the path is filtered out at output time
		filePath := joinPath(event.Directory, en.OldEntry.Name)
		s.markDeleted(filePath, tsNs)
		if s.verbose {
			glog.V(0).Infof("  [DELETE] %s %s", time.Unix(0, tsNs).Format(time.RFC3339), filePath)
		}
	default:
		s.skipped++
	}
}

func (s *DatRecoverScanner) getOrCreate(filePath string, tsNs int64) *CompleteFileState {
	st, ok := s.states[filePath]
	if !ok {
		st = &CompleteFileState{
			Path:        filePath,
			AllChunks:   make(map[string]*filer_pb.FileChunk),
			FirstSeenNs: tsNs,
		}
		s.states[filePath] = st
	} else if tsNs < st.FirstSeenNs {
		st.FirstSeenNs = tsNs
	}
	return st
}

// markDeleted marks a path as deleted if the event is not older than the
// latest state seen so far (volumes are scanned in arbitrary order).
func (s *DatRecoverScanner) markDeleted(filePath string, tsNs int64) *CompleteFileState {
	st := s.getOrCreate(filePath, tsNs)
	st.EventCount++
	if tsNs >= st.LastUpdateNs {
		st.LastUpdateNs = tsNs
		st.IsDeleted = true
	}
	return st
}

// applyEntry records a create/update/rename-target state and merges chunks.
// Events may carry incremental chunks (e.g. S3 multipart, appends), so chunks
// are merged by offset: same offset keeps the newer ModifiedTsNs.
// State fields are only overwritten when the event is not older than what has
// been seen, so scanning volumes in any order yields the same result.
func (s *DatRecoverScanner) applyEntry(filePath string, entry *filer_pb.Entry, tsNs int64) {
	st := s.getOrCreate(filePath, tsNs)
	st.EventCount++
	if tsNs >= st.LastUpdateNs {
		st.LastUpdateNs = tsNs
		st.IsDeleted = false
		st.FinalEntry = proto.Clone(entry).(*filer_pb.Entry)
	}

	for _, chunk := range entry.Chunks {
		cloned := proto.Clone(chunk).(*filer_pb.FileChunk)
		if cloned.ModifiedTsNs == 0 {
			cloned.ModifiedTsNs = tsNs
		}
		chunkKey := fmt.Sprintf("offset_%d", chunk.Offset)
		if existing, exists := st.AllChunks[chunkKey]; !exists || cloned.ModifiedTsNs >= existing.ModifiedTsNs {
			st.AllChunks[chunkKey] = cloned
		}
	}
	st.TotalSize = 0
	for _, chunk := range st.AllChunks {
		if end := chunk.Offset + int64(chunk.Size); end > int64(st.TotalSize) {
			st.TotalSize = uint64(end)
		}
	}

	s.eventCount++
	if s.verbose && s.eventCount <= 20 {
		kind := "CREATE"
		if len(st.AllChunks) > 0 && st.EventCount > 1 {
			kind = "UPDATE"
		}
		glog.V(0).Infof("  [%s] %s ts=%d", kind, filePath, tsNs)
	}
}

func joinPath(dir, name string) string {
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "/"
	} else if !strings.HasPrefix(dir, "/") {
		dir = "/" + dir
	}
	return path.Join(dir, name)
}

// isUploadPath reports whether any path segment is ".uploads",
// matching the directory itself and everything under it.
func isUploadPath(p string) bool {
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == ".uploads" {
			return true
		}
	}
	return false
}

// readDatList reads a list file with one .dat path per line.
func readDatList(listFile string) ([]string, error) {
	data, err := os.ReadFile(listFile)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		paths = append(paths, line)
	}
	return paths, nil
}

// scanOneDat scans one volume .dat file; the volume id is parsed from the
// file name (e.g. /data/3.dat -> volume id 3).
func (s *DatRecoverScanner) scanOneDat(datPath string) error {
	vidStr := strings.TrimSuffix(filepath.Base(datPath), ".dat")
	vid, err := strconv.Atoi(vidStr)
	if err != nil {
		return fmt.Errorf("cannot extract volume id from file name")
	}
	dir := filepath.Dir(datPath)
	glog.V(0).Infof("Scanning %s (dir=%s volumeId=%d)", datPath, dir, vid)
	if err := storage.ScanVolumeFile(dir, "", needle.VolumeId(vid), storage.NeedleMapInMemory, s); err != nil {
		return fmt.Errorf("ScanVolumeFile error (may be partial): %v", err)
	}
	return nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Recover filer metadata from .dat files that stored topics/.system/log needles.\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  %s -dat=/path/to/3.dat -volumeId=3 -output=3.meta\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -datList=/path/to/list.txt -output=all.meta   # one .dat path per line\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -dat=/path/to/3.dat -volumeId=3 -v\n\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	if *datList == "" && *datFile == "" {
		flag.Usage()
		log.Fatal("either -datList or -dat is required")
	}
	if *datList == "" && *volumeId < 0 {
		flag.Usage()
		log.Fatal("volumeId is required when using -dat")
	}

	util_http.InitGlobalHttpClient()

	scanner := &DatRecoverScanner{
		verbose: *verbose,
		states:  make(map[string]*CompleteFileState),
	}

	var datPaths []string
	if *datList != "" {
		paths, err := readDatList(*datList)
		if err != nil {
			log.Fatalf("failed to read dat list %s: %v", *datList, err)
		}
		if len(paths) == 0 {
			log.Fatalf("no .dat files listed in %s", *datList)
		}
		datPaths = paths
	} else {
		datPaths = []string{*datFile}
	}

	for _, datPath := range datPaths {
		if err := scanner.scanOneDat(datPath); err != nil {
			// keep going: one bad volume must not abort the whole recovery
			glog.V(0).Infof("Skip %s: %v", datPath, err)
		}
	}

	filerEntries := buildCompleteEntries(scanner)

	fmt.Printf("\n=== Recovery Summary ===\n")
	fmt.Printf("Events processed: %d, skipped: %d\n", scanner.eventCount, scanner.skipped)
	var dirCount, fileCount, deleted, uploads int
	for _, st := range scanner.states {
		if isUploadPath(st.Path) {
			uploads++
		} else if st.IsDeleted {
			deleted++
		}
	}
	for _, fe := range filerEntries {
		if fe.Entry.IsDirectory {
			dirCount++
		} else {
			fileCount++
		}
	}
	fmt.Printf("Unique paths tracked: %d (deleted: %d, .uploads: %d)\n", len(scanner.states), deleted, uploads)
	fmt.Printf("Entries to save: %d (directories: %d, files: %d)\n", len(filerEntries), dirCount, fileCount)
	if uploads > 0 && !*includeUploads {
		fmt.Printf("Note: %d .uploads paths excluded (use -include-uploads to include)\n", uploads)
	}

	if len(filerEntries) > 0 {
		outputPath := *outputFile
		if outputPath == "" {
			t := time.Now()
			outputPath = fmt.Sprintf("dat-recover-%04d%02d%02d-%02d%02d%02d.meta",
				t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())
		}
		if err := saveEvents(outputPath, filerEntries); err != nil {
			log.Fatalf("failed to save events: %v", err)
		}
		fmt.Printf("Saved %d entries to %s\n", len(filerEntries), outputPath)
	}
}

// buildCompleteEntries turns path states into fs.meta.load compatible entries.
func buildCompleteEntries(s *DatRecoverScanner) []*filer_pb.FullEntry {
	var filerEntries []*filer_pb.FullEntry

	for _, st := range s.states {
		if *skipDeleted && st.IsDeleted {
			continue
		}
		if !*includeUploads && isUploadPath(st.Path) {
			continue
		}
		if st.FinalEntry == nil {
			// only delete events seen for this path
			continue
		}

		completeEntry := proto.Clone(st.FinalEntry).(*filer_pb.Entry)

		var allChunks []*filer_pb.FileChunk
		for _, chunk := range st.AllChunks {
			allChunks = append(allChunks, chunk)
		}
		sort.Slice(allChunks, func(i, j int) bool {
			return allChunks[i].Offset < allChunks[j].Offset
		})
		completeEntry.Chunks = allChunks

		if completeEntry.Attributes != nil {
			completeEntry.Attributes.FileSize = uint64(st.TotalSize)
		}

		dir := "/"
		fileName := strings.TrimPrefix(st.Path, "/")
		if lastSlash := strings.LastIndex(fileName, "/"); lastSlash >= 0 {
			dir = "/" + fileName[:lastSlash]
			fileName = fileName[lastSlash+1:]
		}
		completeEntry.Name = fileName

		filerEntries = append(filerEntries, &filer_pb.FullEntry{
			Dir:   dir,
			Entry: completeEntry,
		})
	}

	// Directories first (sorted), so parents exist before their children are
	// loaded, then files sorted by path.
	sort.Slice(filerEntries, func(i, j int) bool {
		if filerEntries[i].Entry.IsDirectory != filerEntries[j].Entry.IsDirectory {
			return filerEntries[i].Entry.IsDirectory
		}
		return filerEntries[i].Dir < filerEntries[j].Dir
	})

	return filerEntries
}

// saveEvents writes [4-byte big-endian size][FullEntry protobuf] records,
// compatible with `weed shell` fs.meta.load.
func saveEvents(filePath string, filerEntries []*filer_pb.FullEntry) error {
	dst, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %v", filePath, err)
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
