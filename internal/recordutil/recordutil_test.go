package recordutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenForAppend_createsNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.csv")

	result, err := OpenForAppend(path)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NoError(t, result.File.Close())

	require.Zero(t, result.PrevSize, "new file must report PrevSize 0")
	_, err = os.Stat(path)
	require.NoError(t, err, "file should exist after open")
}

func TestOpenForAppend_prevSizeReflectsExistingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.csv")
	content := []byte("unix_ns,seq\n1000,0\n")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	result, err := OpenForAppend(path)
	require.NoError(t, err)
	require.NoError(t, result.File.Close())

	require.Equal(t, int64(len(content)), result.PrevSize)
}

func TestOpenForAppend_appendsWithoutTruncating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.csv")
	require.NoError(t, os.WriteFile(path, []byte("line1\n"), 0o644))

	result, err := OpenForAppend(path)
	require.NoError(t, err)

	_, err = fmt.Fprintln(result.File, "line2")
	require.NoError(t, err)
	require.NoError(t, result.File.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "line1\nline2\n", string(data), "existing content must not be truncated")
}

func TestOpenForAppend_createsMissingDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deep", "data.csv")

	result, err := OpenForAppend(path)
	require.NoError(t, err)
	require.NoError(t, result.File.Close())

	_, err = os.Stat(filepath.Dir(path))
	require.NoError(t, err, "parent directories must be created")
}

func TestReadLastSeq_nonExistentFile(t *testing.T) {
	seq, err := ReadLastSeq("/nonexistent/path/timestamps.csv")
	require.NoError(t, err)
	require.Equal(t, int64(-1), seq)
}

func TestReadLastSeq_emptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ts.csv")
	require.NoError(t, os.WriteFile(path, nil, 0o644))

	seq, err := ReadLastSeq(path)
	require.NoError(t, err)
	require.Equal(t, int64(-1), seq)
}

func TestReadLastSeq_headerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ts.csv")
	require.NoError(t, os.WriteFile(path, []byte("unix_ns,seq,byte_offset\n"), 0o644))

	seq, err := ReadLastSeq(path)
	require.NoError(t, err)
	require.Equal(t, int64(-1), seq)
}

func TestReadLastSeq_singleDataRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ts.csv")
	require.NoError(t, os.WriteFile(path, []byte("unix_ns,seq,byte_offset\n1000000,42,0\n"), 0o644))

	seq, err := ReadLastSeq(path)
	require.NoError(t, err)
	require.Equal(t, int64(42), seq)
}

func TestReadLastSeq_returnsLastRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ts.csv")
	content := "unix_ns,seq,byte_offset\n1000000,0,0\n2000000,1,128\n3000000,2,256\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	seq, err := ReadLastSeq(path)
	require.NoError(t, err)
	require.Equal(t, int64(2), seq)
}

func TestReadLastSeq_skipsBlankLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ts.csv")
	content := "unix_ns,seq,byte_offset\n1000000,0,0\n\n2000000,5,128\n\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	seq, err := ReadLastSeq(path)
	require.NoError(t, err)
	require.Equal(t, int64(5), seq)
}

func TestUniqueSegmentFile_insertsTimestampBeforeExtension(t *testing.T) {
	base := "/data/top_camera.mp4"
	ts := time.Date(2026, 6, 12, 16, 46, 29, 876543210, time.UTC)

	result := UniqueSegmentFile(base, ts)

	require.Contains(t, result, "20260612T164629")
	require.Contains(t, result, "876543210Z")
	require.Equal(t, ".mp4", filepath.Ext(result), "must keep original extension")
	require.True(t, strings.HasPrefix(filepath.Base(result), "top_camera_"))
}

func TestUniqueSegmentFile_keepsAudioExtension(t *testing.T) {
	result := UniqueSegmentFile("/data/audio.ogg", time.Now().UTC())
	require.Equal(t, ".ogg", filepath.Ext(result))
}

func TestUniqueSegmentFile_differentTimesProduceDifferentNames(t *testing.T) {
	base := "/data/video.mp4"
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	require.NotEqual(t, UniqueSegmentFile(base, t1), UniqueSegmentFile(base, t2))
}

func TestNewFrameCSVWriter_emptyPath(t *testing.T) {
	w := NewFrameCSVWriter("")
	require.NotNil(t, w)
}

func TestExtractAndAppend_nilWriter_isNoOp(t *testing.T) {
	var w *FrameCSVWriter
	err := w.ExtractAndAppend("/some/file.mp4", "v:0", 0)
	require.NoError(t, err)
}

func TestExtractAndAppend_emptyPath_isNoOp(t *testing.T) {
	w := NewFrameCSVWriter("")
	err := w.ExtractAndAppend("/some/file.mp4", "v:0", 0)
	require.NoError(t, err)
}
