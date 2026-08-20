package upload

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// zstdWholeFileTargets are session files compressed as a single opaque zstd
// blob rather than reprocessed frame by frame: each is a fixed-schema
// binary stream (see internal/lowstate, internal/odom, internal/lidar) with
// a timestamps CSV alongside it whose byte_offset column already points
// into the file's *decompressed* bytes -- nothing about per-record framing
// changes, only the bytes on the wire, so that CSV needs no rewrite.
var zstdWholeFileTargets = []string{
	"lowstate_frames.bin",
	"odom_frames.bin",
	"lidar_scans.bin",
}

// compressWholeFiles replaces each of zstdWholeFileTargets with a zstd-
// compressed copy under a new name, e.g. lowstate_frames.bin ->
// lowstate_frames.zstd.bin. The original is kept locally under raw/ (see
// rawstore.go) -- only the compressed copy is ever uploaded.
func compressWholeFiles(localDir string, opts Options) error {
	for _, name := range zstdWholeFileTargets {
		if err := compressWholeFile(localDir, name); err != nil {
			return fmt.Errorf("preprocess: compress %s: %w", name, err)
		}
	}
	return nil
}

// compressWholeFile is also depth's fallback (see compress_depth.go) for a
// session that recorded at least one non-RVL depth frame, so it's kept
// generic over name rather than only used via zstdWholeFileTargets.
func compressWholeFile(localDir, name string) error {
	dstName := zstdName(name)
	if _, err := os.Stat(filepath.Join(localDir, dstName)); err == nil {
		return archiveOriginal(localDir, name) // already compressed; just make sure the original is archived too
	} else if !os.IsNotExist(err) {
		return err
	}

	src, err := findSource(localDir, name)
	if err != nil {
		return err
	}
	if src == "" {
		return nil // that sensor is disabled; nothing to compress
	}

	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	compressed, err := zstdCompress(raw)
	if err != nil {
		return fmt.Errorf("zstd compress: %w", err)
	}
	if err := writeAtomic(localDir, dstName, compressed); err != nil {
		return err
	}
	return archiveOriginal(localDir, name)
}

// zstdName turns "foo.bin" into "foo.zstd.bin" -- the compressed form keeps
// the .bin suffix openmind-api's bucket policy already allows (the same
// trick convertJSONLToJSON uses for .jsonl -> .json), while the ".zstd"
// segment marks it as not being the file's original format.
func zstdName(name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return stem + ".zstd" + ext
}

func zstdCompress(raw []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(raw, make([]byte, 0, len(raw))), nil
}

func zstdDecompress(compressed []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(compressed, nil)
}
