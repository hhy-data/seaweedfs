package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/storage/backend"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

var (
	volumeDir        = flag.String("dir", "/tmp", "data directory")
	volumeCollection = flag.String("collection", "", "volume collection name")
	volumeId         = flag.Int("volumeId", -1, "volume id")
	targetFileKey    = flag.Uint64("fileKey", 0, "needle file key to recover (decimal)")
	targetCookie     = flag.Uint64("cookie", 0, "needle cookie (decimal)")
	dryRun           = flag.Bool("dry-run", false, "only scan and print, do not write idx")
)

/*

Recover a lost chunk by scanning the .dat file for a needle with matching
fileKey+cookie and appending the correct entry to the .idx file.

Usage:

	# 1. Dry-run: scan and verify the needle exists
	go run recover_chunk.go -dir=/data -volumeId=2191276 -fileKey=646160384 -cookie=4116679282 -dry-run

	# 2. Recover: write found entry to .idx_recovered
	go run recover_chunk.go -dir=/data -volumeId=2191276 -fileKey=646160384 -cookie=4116679282

	# 3. Replace original .idx after verification
	mv 2191276.idx_recovered 2191276.idx
*/
func main() {
	flag.Parse()
	util_http.InitGlobalHttpClient()

	if *volumeId < 0 || *targetFileKey == 0 {
		glog.Fatalf("Usage: recover_chunk -dir=/path -volumeId=N -fileKey=KEY -cookie=COOKIE")
	}

	fileName := fmt.Sprintf("%d", *volumeId)
	if *volumeCollection != "" {
		fileName = *volumeCollection + "_" + fileName
	}

	datPath := path.Join(*volumeDir, fileName+".dat")
	idxPath := path.Join(*volumeDir, fileName+".idx")

	datFile, err := os.OpenFile(datPath, os.O_RDONLY, 0644)
	if err != nil {
		glog.Fatalf("Open .dat %s: %v", datPath, err)
	}
	defer datFile.Close()

	datBackend := backend.NewDiskFile(datFile)
	defer datBackend.Close()

	// Read superblock
	superBlock, err := super_block.ReadSuperBlock(datBackend)
	if err != nil {
		glog.Fatalf("Read superblock: %v", err)
	}
	fmt.Printf("Volume %d, version=%d, replica=%v, ttl=%v\n",
		*volumeId, superBlock.Version, superBlock.ReplicaPlacement, superBlock.Ttl)

	version := superBlock.Version
	targetId := types.NeedleId(*targetFileKey)
	targetCook := types.Cookie(uint32(*targetCookie))

	// Scan .dat file for the target needle
	offset := int64(superBlock.BlockSize())
	totalNeedles := 0
	found := false

	for {
		n, _, bodyLen, err := needle.ReadNeedleHeader(datBackend, version, offset)
		if err != nil {
			if err == io.EOF {
				break
			}
			fmt.Printf("ReadNeedleHeader error at offset %d: %v\n", offset, err)
			break
		}
		if n == nil || bodyLen < 0 {
			break
		}

		totalNeedles++

		if n.Id == targetId && n.Cookie == targetCook {
			// Found the needle
			found = true
			fmt.Printf("\n=== FOUND ===\n")
			fmt.Printf("  NeedleId: %d (0x%x)\n", n.Id, n.Id)
			fmt.Printf("  Cookie:   %d (0x%08x)\n", n.Cookie, n.Cookie)
			fmt.Printf("  Offset:   %d (actual), %d (idx)\n", offset, offset/int64(types.NeedlePaddingSize))
			fmt.Printf("  Size:     %d (%s)\n", n.Size, util.BytesToHumanReadable(uint64(n.Size)))

			// Try to read body for more info
			if n.Size > 0 && n.Size != types.TombstoneFileSize {
				n.ReadNeedleBody(datBackend, version, offset+int64(types.NeedleHeaderSize), bodyLen)
				fmt.Printf("  DataSize: %d\n", n.DataSize)
				fmt.Printf("  Name:     %s\n", string(n.Name))
				if n.AppendAtNs > 0 {
					t := time.Unix(0, int64(n.AppendAtNs))
					fmt.Printf("  Appended: %v\n", t)
				}
				fmt.Printf("  Checksum: %d\n", n.Checksum)
			}
			fmt.Printf("============\n")

			if !*dryRun {
				if err := appendIdxEntry(idxPath, n.Id, offset, n.Size); err != nil {
					glog.Fatalf("Write idx entry: %v", err)
				}
			}
			break
		}

		offset += int64(types.NeedleHeaderSize) + bodyLen
	}

	fmt.Printf("\nScan complete: %d needles scanned\n", totalNeedles)
	if !found {
		fmt.Printf("ERROR: needle fileKey=%d cookie=%d NOT FOUND in .dat\n", targetId, targetCook)
		os.Exit(1)
	}
	if *dryRun {
		fmt.Println("Dry-run: no changes written")
	}
}

func appendIdxEntry(idxPath string, needleId types.NeedleId, actualOffset int64, size types.Size) error {
	outPath := idxPath + "_recovered"

	// Copy existing .idx to .idx_recovered
	src, err := os.Open(idxPath)
	if err != nil {
		return fmt.Errorf("open src idx: %v", err)
	}
	defer src.Close()

	dst, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %v", outPath, err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy idx: %v", err)
	}

	// Append new entry
	entry := make([]byte, types.NeedleIdSize+types.OffsetSize+types.SizeSize)
	types.NeedleIdToBytes(entry[0:types.NeedleIdSize], needleId)
	idxOffset := types.ToOffset(actualOffset)
	types.OffsetToBytes(entry[types.NeedleIdSize:types.NeedleIdSize+types.OffsetSize], idxOffset)
	types.SizeToBytes(entry[types.NeedleIdSize+types.OffsetSize:], size)

	// Verify by reading back
	key2 := types.BytesToNeedleId(entry[0:types.NeedleIdSize])
	offset2 := types.BytesToOffset(entry[types.NeedleIdSize : types.NeedleIdSize+types.OffsetSize])
	size2 := types.BytesToSize(entry[types.NeedleIdSize+types.OffsetSize:])
	fmt.Printf("  Writing idx entry: key=%d offset=%d(actual=%d) size=%d\n",
		key2, offset2.ToActualOffset(), offset2.ToActualOffset(), size2)

	// Double check actual offset matches
	if offset2.ToActualOffset() != actualOffset {
		return fmt.Errorf("offset mismatch: encoded %d != actual %d", offset2.ToActualOffset(), actualOffset)
	}

	// Check size is not negative (tombstone)
	if binary.BigEndian.Uint32(entry[types.NeedleIdSize+types.OffsetSize:]) != uint32(size) {
		fmt.Printf("  Warning: size encoding check: expected %d, got encoded bytes that decode back correctly\n", size)
	}

	if _, err := dst.Write(entry); err != nil {
		return fmt.Errorf("write entry: %v", err)
	}

	fmt.Printf("  Appended to %s\n", outPath)
	return nil
}
