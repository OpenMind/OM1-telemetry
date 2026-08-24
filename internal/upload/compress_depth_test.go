package upload

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"om1-telemetry/internal/rvl"
)

const (
	testDepthWidth  = 4
	testDepthHeight = 3
)

// writeSyntheticDepth writes an RVL-encoded depth pair and returns the original pixel values per frame.
func writeSyntheticDepth(t *testing.T, dir string, numFrames int) [][]uint16 {
	t.Helper()
	n := testDepthWidth * testDepthHeight
	var bin []byte
	csvBody := "unix_ns,seq,byte_offset,byte_length,method,width,height,encoding,mono_ns\n"
	var frames [][]uint16

	for i := 0; i < numFrames; i++ {
		pixels := make([]uint16, n)
		for p := range pixels {
			pixels[p] = uint16((i*n + p) % 5000)
		}
		frames = append(frames, pixels)

		encoded := rvl.Encode(pixels)
		offset := len(bin)
		bin = append(bin, encoded...)

		csvBody += fmt.Sprintf("%d,%d,%d,%d,rvl,%d,%d,16UC1,%d\n",
			1000+i, i, offset, len(encoded), testDepthWidth, testDepthHeight, 2000+i)
	}

	writeFile(t, dir, depthFramesName, bin)
	writeFile(t, dir, depthTimestampsName, []byte(csvBody))
	return frames
}

func decodeRawU16LE(t *testing.T, raw []byte) []uint16 {
	t.Helper()
	require.Zero(t, len(raw)%2)
	out := make([]uint16, len(raw)/2)
	for i := range out {
		out[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	return out
}

func TestCompressDepth_decodesRVLAndZstdCompressesLosslessly(t *testing.T) {
	dir := t.TempDir()
	frames := writeSyntheticDepth(t, dir, 3)

	require.NoError(t, compressDepth(dir, Options{}))

	require.NoFileExists(t, filepath.Join(dir, depthFramesName))
	require.FileExists(t, filepath.Join(dir, rawDirName, depthFramesName))
	require.FileExists(t, filepath.Join(dir, rawDirName, depthTimestampsName),
		"the pre-rewrite csv must be preserved alongside the original bin")

	compressed, err := os.ReadFile(filepath.Join(dir, "depth_frames.zstd"))
	require.NoError(t, err)
	raw, err := zstdDecompress(compressed)
	require.NoError(t, err)

	got := decodeRawU16LE(t, raw)
	var want []uint16
	for _, f := range frames {
		want = append(want, f...)
	}
	require.Equal(t, want, got, "decoded pixels must exactly match what was recorded, losslessly")

	newCSVRaw, err := os.ReadFile(filepath.Join(dir, depthTimestampsName))
	require.NoError(t, err)
	newRecords, err := parseDepthCSV(newCSVRaw)
	require.NoError(t, err)
	require.Len(t, newRecords, 3)
	frameBytes := int64(testDepthWidth * testDepthHeight * 2)
	for i, r := range newRecords {
		require.Equal(t, "raw_u16le", r.method)
		require.Equal(t, int64(i)*frameBytes, r.byteOffset)
		require.Equal(t, frameBytes, r.byteLength)
	}
}

func TestCompressDepth_fallsBackToWholeFileOnNonRVLFrame(t *testing.T) {
	dir := t.TempDir()
	writeSyntheticDepth(t, dir, 2)

	csvRaw, err := os.ReadFile(filepath.Join(dir, depthTimestampsName))
	require.NoError(t, err)
	bin, err := os.ReadFile(filepath.Join(dir, depthFramesName))
	require.NoError(t, err)
	bin = append(bin, []byte("not rvl data")...)
	newCSV := string(csvRaw) + fmt.Sprintf("%d,%d,%d,%d,raw,%d,%d,mono8,%d\n",
		9999, 2, len(bin)-12, 12, testDepthWidth, testDepthHeight, 9999)
	writeFile(t, dir, depthFramesName, bin)
	writeFile(t, dir, depthTimestampsName, []byte(newCSV))

	require.NoError(t, compressDepth(dir, Options{}))

	require.NoFileExists(t, filepath.Join(dir, depthFramesName))
	compressed, err := os.ReadFile(filepath.Join(dir, "depth_frames.zstd"))
	require.NoError(t, err)
	decompressed, err := zstdDecompress(compressed)
	require.NoError(t, err)
	require.Equal(t, bin, decompressed, "fallback must preserve the original bytes exactly")

	stillOriginalCSV, err := os.ReadFile(filepath.Join(dir, depthTimestampsName))
	require.NoError(t, err)
	require.Equal(t, newCSV, string(stillOriginalCSV), "csv must be untouched in the fallback path")
}

func TestCompressDepth_isIdempotentOnRetry(t *testing.T) {
	dir := t.TempDir()
	writeSyntheticDepth(t, dir, 2)

	require.NoError(t, compressDepth(dir, Options{}))
	first, err := os.ReadFile(filepath.Join(dir, "depth_frames.zstd"))
	require.NoError(t, err)

	require.NoError(t, compressDepth(dir, Options{}))
	second, err := os.ReadFile(filepath.Join(dir, "depth_frames.zstd"))
	require.NoError(t, err)

	require.Equal(t, first, second)
}

func TestCompressDepth_missingFileIsANoop(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{}`))

	require.NoError(t, compressDepth(dir, Options{}))

	require.NoFileExists(t, filepath.Join(dir, "depth_frames.zstd"))
}
