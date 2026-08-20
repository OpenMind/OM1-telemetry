package upload

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompressWholeFiles_roundTripsAndArchivesOriginal(t *testing.T) {
	dir := t.TempDir()
	original := []byte("some lowstate bytes, repeated repeated repeated repeated")
	writeFile(t, dir, "lowstate_frames.bin", original)

	require.NoError(t, compressWholeFiles(dir, Options{}))

	require.NoFileExists(t, filepath.Join(dir, "lowstate_frames.bin"),
		"the original must not be uploaded -- see raw/ instead")
	require.FileExists(t, filepath.Join(dir, rawDirName, "lowstate_frames.bin"),
		"but it must still exist locally")

	archived, err := os.ReadFile(filepath.Join(dir, rawDirName, "lowstate_frames.bin"))
	require.NoError(t, err)
	require.Equal(t, original, archived, "archiving must not alter the original bytes")

	compressed, err := os.ReadFile(filepath.Join(dir, "lowstate_frames.zstd.bin"))
	require.NoError(t, err)
	decompressed, err := zstdDecompress(compressed)
	require.NoError(t, err)
	require.Equal(t, original, decompressed)
}

func TestCompressWholeFiles_compressesAllThreeTargets(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lowstate_frames.bin", []byte("aaaaaaaaaaaaaaaa"))
	writeFile(t, dir, "odom_frames.bin", []byte("bbbbbbbbbbbbbbbb"))
	writeFile(t, dir, "lidar_scans.bin", []byte("cccccccccccccccc"))

	require.NoError(t, compressWholeFiles(dir, Options{}))

	for _, name := range []string{"lowstate_frames.zstd.bin", "odom_frames.zstd.bin", "lidar_scans.zstd.bin"} {
		require.FileExists(t, filepath.Join(dir, name))
	}
}

func TestCompressWholeFiles_missingFileIsANoop(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{}`))

	require.NoError(t, compressWholeFiles(dir, Options{}))

	require.NoFileExists(t, filepath.Join(dir, "lowstate_frames.zstd.bin"))
}

func TestCompressWholeFile_isIdempotentOnRetry(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "odom_frames.bin", []byte("odometry payload"))

	require.NoError(t, compressWholeFile(dir, "odom_frames.bin"))
	firstCompressed, err := os.ReadFile(filepath.Join(dir, "odom_frames.zstd.bin"))
	require.NoError(t, err)

	require.NoError(t, compressWholeFile(dir, "odom_frames.bin"),
		"a retry after the original was already archived must not error")

	secondCompressed, err := os.ReadFile(filepath.Join(dir, "odom_frames.zstd.bin"))
	require.NoError(t, err)
	require.Equal(t, firstCompressed, secondCompressed, "a retry must not recompress or otherwise change the output")
	require.FileExists(t, filepath.Join(dir, rawDirName, "odom_frames.bin"))
}

func TestZstdName(t *testing.T) {
	require.Equal(t, "lowstate_frames.zstd.bin", zstdName("lowstate_frames.bin"))
}
