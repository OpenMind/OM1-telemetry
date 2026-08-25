package upload

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// zstdWholeFileTargets are session files compressed as a single opaque zstd blob.
var zstdWholeFileTargets = []string{
	"lowstate_frames.bin",
	"odom_frames.bin",
	"lidar_scans.bin",
}

// compressWholeFiles replaces each of zstdWholeFileTargets with a zstd-compressed copy; original kept under raw/.
func compressWholeFiles(localDir string, opts Options) error {
	for _, name := range zstdWholeFileTargets {
		if err := compressWholeFile(localDir, name); err != nil {
			return fmt.Errorf("preprocess: compress %s: %w", name, err)
		}
	}
	return nil
}

// compressWholeFile is also depth's fallback for a non-RVL frame; kept generic over name.
func compressWholeFile(localDir, name string) error {
	dstName := zstdName(name)
	if _, err := os.Stat(filepath.Join(localDir, dstName)); err == nil {
		return archiveOriginal(localDir, name)
	} else if !os.IsNotExist(err) {
		return err
	}

	src, err := findSource(localDir, name)
	if err != nil {
		return err
	}
	if src == "" {
		return nil
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

// zstdName turns "foo.bin" into "foo.zstd".
func zstdName(name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return stem + ".zstd"
}

func zstdCompress(raw []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = enc.Close() }()
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
