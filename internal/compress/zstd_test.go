package compress

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
}

func TestWholeFiles_roundTripsAndDeletesOriginal(t *testing.T) {
	dir := t.TempDir()
	original := []byte("some lowstate bytes, repeated repeated repeated repeated")
	writeFile(t, dir, "lowstate_frames.bin", original)

	require.NoError(t, WholeFiles(dir))

	require.NoFileExists(t, filepath.Join(dir, "lowstate_frames.bin"),
		"the original must not be uploaded, and zstd is lossless so no local backup is needed either")
	require.NoDirExists(t, filepath.Join(dir, rawDirName))

	compressed, err := os.ReadFile(filepath.Join(dir, "lowstate_frames.zstd"))
	require.NoError(t, err)
	decompressed, err := Decompress(compressed)
	require.NoError(t, err)
	require.Equal(t, original, decompressed)
}

func TestWholeFiles_compressesAllThreeTargets(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lowstate_frames.bin", []byte("aaaaaaaaaaaaaaaa"))
	writeFile(t, dir, "odom_frames.bin", []byte("bbbbbbbbbbbbbbbb"))
	writeFile(t, dir, "lidar_scans.bin", []byte("cccccccccccccccc"))

	require.NoError(t, WholeFiles(dir))

	for _, name := range []string{"lowstate_frames.zstd", "odom_frames.zstd", "lidar_scans.zstd"} {
		require.FileExists(t, filepath.Join(dir, name))
	}
}

func TestWholeFiles_missingFileIsANoop(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{}`))

	require.NoError(t, WholeFiles(dir))

	require.NoFileExists(t, filepath.Join(dir, "lowstate_frames.zstd"))
}

func TestCompressWholeFile_isIdempotentOnRetry(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "odom_frames.bin", []byte("odometry payload"))

	require.NoError(t, compressWholeFile(dir, "odom_frames.bin"))
	firstCompressed, err := os.ReadFile(filepath.Join(dir, "odom_frames.zstd"))
	require.NoError(t, err)

	require.NoError(t, compressWholeFile(dir, "odom_frames.bin"),
		"a retry after the original was already deleted must not error")

	secondCompressed, err := os.ReadFile(filepath.Join(dir, "odom_frames.zstd"))
	require.NoError(t, err)
	require.Equal(t, firstCompressed, secondCompressed, "a retry must not recompress or otherwise change the output")
	require.NoFileExists(t, filepath.Join(dir, "odom_frames.bin"))
}

func TestZstdName(t *testing.T) {
	require.Equal(t, "lowstate_frames.zstd", zstdName("lowstate_frames.bin"))
}
