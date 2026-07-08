// filer_log_recover downloads filer metadata logs by date range and
// traces the full event history of one or more file paths.
//
// Usage:
//
//	# 1. download one day's worth of metadata log (skipped if file already exists)
//	go run ./unmaintained/filer_log_recover download \
//	    -filer=filer:8888 -date=2025-09-25 -out=/tmp/logs/2025-09-25.bin
//
//	# 2. trace every event that touched two paths
//	go run ./unmaintained/filer_log_recover trace \
//	    -in=/tmp/logs/2025-09-25.bin \
//	    -path=/buckets/a.txt,/buckets/b.txt
//
// The download command persists events as [4B big-endian size][LogEntry protobuf],
// the same layout weed/filer uses on disk so the output is also readable by
// unmaintained/see_log_entry.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "download":
		cmdDownload(os.Args[2:])
	case "trace":
		cmdTrace(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `filer_log_recover — download and trace filer metadata logs

subcommands:
  download   pull events in a time range from a filer to a local file
  trace      read a downloaded file and print events matching one or more paths

examples:
  filer_log_recover download -filer=filer:8888 -date=2025-09-25
  filer_log_recover trace -in=logs-2025-09-25.bin -path=/a/b.txt,/c/d.txt`)
}

// ---------------------------------------------------------------------------
// download
// ---------------------------------------------------------------------------

func cmdDownload(args []string) {
	fs := flag.NewFlagSet("download", flag.ExitOnError)
	filer := fs.String("filer", "localhost:8888", "filer address host:port")
	dateStr := fs.String("date", "", "date YYYY-MM-DD in UTC")
	startStr := fs.String("start", "", "start time RFC3339 (overrides -date)")
	endStr := fs.String("end", "", "end time RFC3339 (overrides -date)")
	out := fs.String("out", "", "output file (default ./logs-<date>.bin)")
	fs.Parse(args)

	startNs, endNs, label, err := resolveRange(*dateStr, *startStr, *endStr)
	if err != nil {
		glog.Fatal(err)
	}
	if *out == "" {
		*out = fmt.Sprintf("logs-%s.bin", label)
	}

	// Skip if already downloaded (the "if not present" requirement).
	if info, statErr := os.Stat(*out); statErr == nil && info.Size() > 0 {
		fmt.Printf("skip: %s already exists (%d bytes)\n", *out, info.Size())
		return
	}
	if dir := filepath.Dir(*out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			glog.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		glog.Fatalf("open out %s: %v", *out, err)
	}
	defer f.Close()

	var count int64
	var bytes int64
	option := &pb.MetadataFollowOption{
		ClientName:     "log-recover-dl",
		PathPrefix:     "", // pull everything
		StartTsNs:      startNs,
		StopTsNs:       endNs,
		EventErrorType: pb.FatalOnError,
	}

	started := time.Now()
	// The server ends the stream after UntilNs (see SubscribeMetadata in
	// weed/server/filer_grpc_server_sub_meta.go), so we don't need to cancel
	// from the client side for normal termination.
	procErr := pb.FollowMetadata(pb.ServerAddress(*filer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		option,
		func(resp *filer_pb.SubscribeMetadataResponse) error {
			// Skip empty markers / heartbeats.
			if resp.EventNotification == nil || filer_pb.IsEmpty(resp) {
				return nil
			}
			if err := writeEvent(f, resp); err != nil {
				return err
			}
			count++
			if count%20000 == 0 {
				fmt.Printf("\r%-12s events=%-10d size=%s",
					time.Since(started).Truncate(time.Second),
					count, humanBytes(bytes))
			}
			return nil
		})
	// io.EOF from cancel is the clean path; anything else is real.
	if procErr != nil && procErr != io.EOF && procErr != context.Canceled {
		glog.Fatalf("subscribe failed: %v", procErr)
	}

	fmt.Printf("\rdownloaded %d events (%s) in %s -> %s\n",
		count, humanBytes(fileSizeOrZero(f)), time.Since(started).Truncate(time.Second), *out)
}

// writeEvent encodes one event as [4B size][LogEntry protobuf], where
// LogEntry.Data holds the marshaled SubscribeMetadataResponse. This is the
// exact layout weed/filer uses on disk.
func writeEvent(w io.Writer, resp *filer_pb.SubscribeMetadataResponse) error {
	data, err := proto.Marshal(resp)
	if err != nil {
		return err
	}
	le := &filer_pb.LogEntry{TsNs: resp.TsNs, Data: data}
	leData, err := proto.Marshal(le)
	if err != nil {
		return err
	}
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(leData)))
	if _, err := w.Write(sz[:]); err != nil {
		return err
	}
	_, err = w.Write(leData)
	return err
}

// resolveRange turns -date or -start/-end into a [startNs, endNs] window
// (inclusive on both ends) plus a short label for default file naming.
func resolveRange(dateStr, startStr, endStr string) (startNs, endNs int64, label string, err error) {
	switch {
	case startStr != "" && endStr != "":
		t1, e1 := time.Parse(time.RFC3339, startStr)
		if e1 != nil {
			return 0, 0, "", fmt.Errorf("-start: %v", e1)
		}
		t2, e2 := time.Parse(time.RFC3339, endStr)
		if e2 != nil {
			return 0, 0, "", fmt.Errorf("-end: %v", e2)
		}
		if !t2.After(t1) {
			return 0, 0, "", fmt.Errorf("-end must be after -start")
		}
		return t1.UnixNano(), t2.UnixNano(), t1.UTC().Format("2006-01-02_1504"), nil
	case dateStr != "":
		t, e := time.Parse("2006-01-02", dateStr)
		if e != nil {
			return 0, 0, "", fmt.Errorf("-date: %v", e)
		}
		t = t.UTC()
		return t.UnixNano(), t.Add(24*time.Hour - time.Nanosecond).UnixNano(), dateStr, nil
	default:
		return 0, 0, "", fmt.Errorf("need -date YYYY-MM-DD or -start/-end RFC3339")
	}
}

// ---------------------------------------------------------------------------
// trace
// ---------------------------------------------------------------------------

func cmdTrace(args []string) {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	in := fs.String("in", "", "input file produced by 'download'")
	pathList := fs.String("path", "", "comma-separated absolute paths to trace")
	resolveManifest := fs.Bool("resolve-manifest", false,
		"recursively fetch IsChunkManifest chunks via the filer (slow, needs -filer)")
	filer := fs.String("filer", "localhost:8888", "filer address for -resolve-manifest")
	fs.Parse(args)

	if *in == "" || *pathList == "" {
		glog.Fatal("need -in and -path")
	}

	want := make(map[string]struct{}, 4)
	for _, p := range strings.Split(*pathList, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			want[normalizePath(p)] = struct{}{}
		}
	}

	f, err := os.Open(*in)
	if err != nil {
		glog.Fatalf("open %s: %v", *in, err)
	}
	defer f.Close()

	var hits []*eventRow
	var scanned int64
	szBuf := make([]byte, 4)
	for {
		if _, err := io.ReadFull(f, szBuf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			glog.Fatalf("read size: %v", err)
		}
		sz := binary.BigEndian.Uint32(szBuf)
		body := make([]byte, sz)
		if _, err := io.ReadFull(f, body); err != nil {
			glog.Fatalf("read body: %v", err)
		}
		scanned++
		le := &filer_pb.LogEntry{}
		if err := proto.Unmarshal(body, le); err != nil {
			glog.V(1).Infof("skip: unmarshal LogEntry: %v", err)
			continue
		}
		ev := &filer_pb.SubscribeMetadataResponse{}
		if err := proto.Unmarshal(le.Data, ev); err != nil {
			glog.V(1).Infof("skip: unmarshal event: %v", err)
			continue
		}
		if eventMatchesPaths(ev, want) {
			hits = append(hits, &eventRow{ts: ev.TsNs, ev: ev})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].ts < hits[j].ts })

	var resolver manifestResolver
	if *resolveManifest {
		resolver = manifestResolver{filer: *filer, client: http.DefaultClient}
	}

	for i, row := range hits {
		fmt.Printf("=== [%d/%d] ===\n", i+1, len(hits))
		printEvent(os.Stdout, row, resolver)
	}

	fmt.Printf("\nscanned %d events, %d matched paths\n", scanned, len(hits))
}

type eventRow struct {
	ts int64
	ev *filer_pb.SubscribeMetadataResponse
}

// normalizePath enforces a leading slash and drops trailing slashes (except
// for the root).
func normalizePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	return p
}

// joinPath combines a directory and entry name into an absolute path,
// treating empty or "/" directory as root.
func joinPath(dir, name string) string {
	if dir == "" || dir == "/" {
		return "/" + name
	}
	if !strings.HasPrefix(dir, "/") {
		dir = "/" + dir
	}
	for len(dir) > 1 && strings.HasSuffix(dir, "/") {
		dir = dir[:len(dir)-1]
	}
	return dir + "/" + name
}

// eventMatchesPaths reports whether the event touches any wanted path.
// A create/update touches directory + NewEntry.Name.
// A delete   touches directory + OldEntry.Name.
// A rename   additionally considers OldParent/OldEntry.Name.
func eventMatchesPaths(ev *filer_pb.SubscribeMetadataResponse, want map[string]struct{}) bool {
	en := ev.EventNotification
	if en == nil {
		return false
	}
	dir := ev.Directory
	if en.NewEntry != nil {
		if _, ok := want[joinPath(dir, en.NewEntry.Name)]; ok {
			return true
		}
	}
	if en.OldEntry != nil {
		if _, ok := want[joinPath(dir, en.OldEntry.Name)]; ok {
			return true
		}
		// rename: the old path lives under OldParent, not Directory
		if en.NewParentPath != "" {
			if _, ok := want[joinPath(en.NewParentPath, en.OldEntry.Name)]; ok {
				return true
			}
		}
	}
	return false
}

func printEvent(w io.Writer, row *eventRow, resolver manifestResolver) {
	ev := row.ev
	en := ev.EventNotification
	t := time.Unix(0, row.ts).UTC()

	fmt.Fprintf(w, "  time: %s (ts_ns=%d)\n", t.Format("2006-01-02T15:04:05.000000000Z"), row.ts)
	fmt.Fprintf(w, "  dir : %s\n", ev.Directory)

	var op string
	switch {
	case filer_pb.IsCreate(ev):
		op = "CREATE"
	case filer_pb.IsUpdate(ev):
		op = "UPDATE"
	case filer_pb.IsDelete(ev):
		op = "DELETE"
	case filer_pb.IsRename(ev):
		op = "RENAME"
	default:
		op = "OTHER"
	}
	fmt.Fprintf(w, "  op  : %s\n", op)

	if en == nil {
		fmt.Fprintln(w, "  (nil event notification — heartbeat/marker)")
		return
	}

	if en.OldEntry != nil {
		fmt.Fprintf(w, "  old : %q", en.OldEntry.Name)
		if en.NewParentPath != "" {
			fmt.Fprintf(w, " (was at %s)", joinPath(en.NewParentPath, en.OldEntry.Name))
		}
		fmt.Fprintln(w)
		printChunks(w, "old chunks", en.OldEntry.Chunks, resolver)
	}
	if en.NewEntry != nil {
		fmt.Fprintf(w, "  new : %q\n", en.NewEntry.Name)
		printChunks(w, "new chunks", en.NewEntry.Chunks, resolver)
	}
	fmt.Fprintln(w)
}

// printChunks prints a chunk list, expanding manifest chunks when a resolver
// is configured.
func printChunks(w io.Writer, header string, chunks []*filer_pb.FileChunk, resolver manifestResolver) {
	if len(chunks) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s (%d):\n", header, len(chunks))
	for _, c := range chunks {
		fmt.Fprintf(w, "    - fid=%s offset=%d size=%d mtime_ns=%d",
			c.GetFileIdString(), c.Offset, c.Size, c.ModifiedTsNs)
		if c.IsChunkManifest {
			fmt.Fprint(w, " [manifest]")
		}
		if c.SourceFileId != "" {
			fmt.Fprintf(w, " src=%s", c.SourceFileId)
		}
		fmt.Fprintln(w)
		if c.IsChunkManifest && resolver.client != nil {
			if subs, err := resolver.resolve(context.Background(), c); err == nil {
				for _, sc := range subs {
					fmt.Fprintf(w, "        * fid=%s offset=%d size=%d\n",
						sc.GetFileIdString(), sc.Offset, sc.Size)
				}
			} else {
				fmt.Fprintf(w, "        (resolve failed: %v)\n", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// manifest resolver — only used when -resolve-manifest is set
// ---------------------------------------------------------------------------

type manifestResolver struct {
	filer  string
	client *http.Client
}

// resolve fetches a manifest chunk's body via the filer and decodes the
// nested FileChunkManifest. It does NOT recurse into sub-manifests — those
// would need another round trip per nested chunk, which trace mode is not
// designed for. Use unmaintained/see_chunk_manifest for deep inspection.
func (r manifestResolver) resolve(ctx context.Context, c *filer_pb.FileChunk) ([]*filer_pb.FileChunk, error) {
	if r.client == nil {
		return nil, fmt.Errorf("no http client")
	}
	fidStr := c.GetFileIdString()
	// Read through the filer so we don't have to do volume-id -> URL
	// lookups ourselves.
	url := strings.TrimRight(r.filer, "/") + "/" + fidStr
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	m := &filer_pb.FileChunkManifest{}
	if err := proto.Unmarshal(body, m); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}
	filer_pb.AfterEntryDeserialization(m.Chunks)
	return m.Chunks, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func fileSizeOrZero(f *os.File) int64 {
	if st, err := f.Stat(); err == nil {
		return st.Size()
	}
	return 0
}
