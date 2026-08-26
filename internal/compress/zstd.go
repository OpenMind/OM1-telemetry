package compress

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// wholeFileTargets are session files compressed as a single opaque zstd blob. Lossless, so the
// original is deleted once compression succeeds -- see Pointcloud for a step that keeps one.
var wholeFileTargets = []string{
	"lowstate_frames.bin",
	"odom_frames.bin",
	"lidar_scans.bin",
}

// WholeFiles replaces each of wholeFileTargets with a zstd-compressed copy.
func WholeFiles(localDir string) error {
	for _, name := range wholeFileTargets {
		if err := compressWholeFile(localDir, name); err != nil {
			return fmt.Errorf("preprocess: compress %s: %w", name, err)
		}
	}
	return nil
}

// compressWholeFile is also Depth's fallback for a non-RVL frame; kept generic over name.
func compressWholeFile(localDir, name string) error {
	dstName := zstdName(name)
	if _, err := os.Stat(filepath.Join(localDir, dstName)); err == nil {
		return removeIfExists(localDir, name)
	} else if !os.IsNotExist(err) {
		return err
	}

	raw, err := os.ReadFile(filepath.Join(localDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	compressed, err := Compress(raw)
	if err != nil {
		return fmt.Errorf("zstd compress: %w", err)
	}
	if err := writeAtomic(localDir, dstName, compressed); err != nil {
		return err
	}
	return removeIfExists(localDir, name)
}

// zstdName turns "foo.bin" into "foo.zstd".
func zstdName(name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return stem + ".zstd"
}

// Compress zstd-compresses raw.
func Compress(raw []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = enc.Close() }()
	return enc.EncodeAll(raw, make([]byte, 0, len(raw))), nil
}

// Decompress reverses Compress.
func Decompress(compressed []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(compressed, nil)
}
