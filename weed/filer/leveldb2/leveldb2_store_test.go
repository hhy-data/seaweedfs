package leveldb

import (
	"context"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func TestCreateAndFind(t *testing.T) {
	testFiler := filer.NewFiler(pb.ServerDiscovery{}, nil, "", "", "", "", "", 255, nil)
	dir := t.TempDir()
	store := &LevelDB2Store{}
	store.initialize(dir, 2)
	testFiler.SetStore(store)

	fullpath := util.FullPath("/home/chris/this/is/one/file1.jpg")

	ctx := context.Background()

	entry1 := &filer.Entry{
		FullPath: fullpath,
		Attr: filer.Attr{
			Mode: 0440,
			Uid:  1234,
			Gid:  5678,
		},
	}

	if err := testFiler.CreateEntry(ctx, entry1, false, false, nil, false, testFiler.MaxFilenameLength); err != nil {
		t.Errorf("create entry %v: %v", entry1.FullPath, err)
		return
	}

	entry, err := testFiler.FindEntry(ctx, fullpath)

	if err != nil {
		t.Errorf("find entry: %v", err)
		return
	}

	if entry.FullPath != entry1.FullPath {
		t.Errorf("find wrong entry: %v", entry.FullPath)
		return
	}

	// checking one upper directory
	entries, _, _ := testFiler.ListDirectoryEntries(ctx, util.FullPath("/home/chris/this/is/one"), "", false, 100, "", "", "")
	if len(entries) != 1 {
		t.Errorf("list entries count: %v", len(entries))
		return
	}

	// checking one upper directory
	entries, _, _ = testFiler.ListDirectoryEntries(ctx, util.FullPath("/"), "", false, 100, "", "", "")
	if len(entries) != 1 {
		t.Errorf("list entries count: %v", len(entries))
		return
	}

}

func TestEmptyRoot(t *testing.T) {
	testFiler := filer.NewFiler(pb.ServerDiscovery{}, nil, "", "", "", "", "", 255, nil)
	dir := t.TempDir()
	store := &LevelDB2Store{}
	store.initialize(dir, 2)
	testFiler.SetStore(store)

	ctx := context.Background()

	// checking one upper directory
	entries, _, err := testFiler.ListDirectoryEntries(ctx, util.FullPath("/"), "", false, 100, "", "", "")
	if err != nil {
		t.Errorf("list entries: %v", err)
		return
	}
	if len(entries) != 0 {
		t.Errorf("list entries count: %v", len(entries))
		return
	}

}

func TestListDirectoryWithPrefix(t *testing.T) {
	testFiler := filer.NewFiler(pb.ServerDiscovery{}, nil, "", "", "", "", "", 255, nil)
	//dir := t.TempDir()
	dir := t.TempDir()
	store := &LevelDB2Store{}
	store.initialize(dir, 2)
	testFiler.SetStore(store)

	ctx := context.Background()

	//basePath := util.FullPath("/bucket1/test-prefix1")
	files := []string{
		"test-prefix1/file1.txt",
		"test-prefix1/file2.txt",
		"test-prefix1-extra.txt", // Should match prefix=test-prefix1, not test-prefix1/
	}

	// Create test entries
	for _, file := range files {
		fullpath := util.FullPath("/bucket1/" + file)
		entry := &filer.Entry{
			FullPath: fullpath,
			Attr: filer.Attr{
				Mode: 0644,
				Uid:  1,
				Gid:  1,
			},
		}
		if err := testFiler.CreateEntry(ctx, entry, false, false, nil, false, testFiler.MaxFilenameLength); err != nil {
			t.Fatalf("Failed to create entry %s: %v", fullpath, err)
		}
	}

	prefix1 := "test-prefix1"
	entries1, _, err := testFiler.ListDirectoryEntries(ctx, util.FullPath("/bucket1"), "", false, 100, prefix1, "", "")
	if err != nil {
		t.Fatalf("Failed to list entries with prefix %s: %v", prefix1, err)
	}
	if len(entries1) != 2 {
		t.Errorf("Expected 2 entries with prefix %s, got %d", prefix1, len(entries1))
	} else {
		t.Logf("Found %d entries with prefix %s", len(entries1), prefix1)
	}
	for _, entry := range entries1 {
		t.Logf("Entry: %s", entry.FullPath)
	}
	entries11, _, err := testFiler.ListDirectoryEntries(ctx, util.FullPath("/bucket1/test-prefix1"), "", false, 100, "", "", "")
	if err != nil {
		t.Fatalf("Failed to list entries with prefix %s: %v", prefix1, err)
	}
	t.Logf("Entry 11 len: %d", len(entries11))
	for _, entry := range entries11 {
		t.Logf("Entry: %s", entry.FullPath)
	}

}
