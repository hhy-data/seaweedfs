package udm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/backend"
)

// readDisabled backend must classify its error as ErrTierBackendUnavailable
// so the volume server can route the read to a peer replica.
func TestReadAt_ReadDisabled_WrapsSentinel(t *testing.T) {
	dir := t.TempDir()

	// Construct the file directly with a nil client: the readDisabled
	// path returns before touching the grpc client, so a real dial is
	// unnecessary and would slow the test by 30+ seconds.
	f := &backendStorageFile{
		backendStorage: &BackendStorage{},
		key:            filepath.Join(dir, "x.dat") + "::" + "deadbeef",
		readDisabled:   true,
	}

	buf := make([]byte, 16)
	n, err := f.ReadAt(buf, 0)
	if err == nil {
		t.Fatalf("expected error from readDisabled backend, got n=%d", n)
	}
	if !errors.Is(err, backend.ErrTierBackendUnavailable) {
		t.Fatalf("readDisabled error should wrap ErrTierBackendUnavailable, got %v", err)
	}
}

// When the download from the remote udm fails, the read must also be
// classified as ErrTierBackendUnavailable so the volume server can fall
// back to a peer that may hold a hot local .dat.
func TestReadAt_DownloadFailure_WrapsSentinel(t *testing.T) {
	dir := t.TempDir()

	// Simulate a long path so buildInternalCacheFilePath resolves below
	// the per-volume dir and the readAtInternalCache call returns ENOENT
	// for the udm cache, forcing the download branch.
	keyPath := filepath.Join(dir, "v1", "needle.dat")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &props{
		values: map[string]string{
			"grpc_server":   "127.0.0.1:0",
			"read_disabled": "false",
		},
	}

	s, err := newBackendStorage(cfg, "", "test")
	if err != nil {
		t.Skipf("cannot construct udm backend in this env: %v", err)
	}
	// Close the gRPC client so the download RPC fails immediately,
	// exercising the wrapped-error branch.
	_ = s.client.Close()
	t.Cleanup(func() { _ = s.client.Close() })

	f := &backendStorageFile{
		backendStorage: s,
		key:            keyPath + "::" + "deadbeef",
		readDisabled:   false,
	}

	buf := make([]byte, 16)
	_, err = f.ReadAt(buf, 0)
	if err == nil {
		t.Fatalf("expected error from download failure, got nil")
	}
	if !errors.Is(err, backend.ErrTierBackendUnavailable) {
		t.Fatalf("download failure should wrap ErrTierBackendUnavailable, got %v", err)
	}
}

// props is a minimal in-memory StringProperties for the test.
type props struct {
	values map[string]string
}

func (p *props) GetString(key string) string { return p.values[key] }
