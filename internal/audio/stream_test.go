package audio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNew_returnsNonNilStream(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://localhost:8554/audio",
		OutputFile: filepath.Join(t.TempDir(), "audio.ogg"),
	})
	require.NotNil(t, stream, "New() returned nil")
}

func TestStartStop_cleanLifecycle(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "audio.ogg"),
	})

	stream.Start()

	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		stream.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "Stop() did not return within 5 s")
	}
}

func TestStart_idempotent(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "audio.ogg"),
	})

	stream.Start()
	stream.Start()

	stream.Stop()
}

func TestStop_beforeStart_isNoOp(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "audio.ogg"),
	})
	stream.Stop()
}

func TestStop_idempotent(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "audio.ogg"),
	})

	stream.Start()
	stream.Stop()
	stream.Stop() // second call must be a no-op
}

func TestAppendSegmentEntry_emptyPath_isNoOp(t *testing.T) {
	err := appendSegmentEntry("", time.Now(), 0, "/data/audio.ogg")
	require.NoError(t, err)
}

func TestAppendSegmentEntry_writesHeaderAndEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audio_timestamps.csv")
	start := time.Unix(1_000_000_000, 123_456_789)
	segFile := "/data/audio_20260612T164629_876543210Z.ogg"

	err := appendSegmentEntry(path, start, 12345, segFile)
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	require.Contains(t, content, "recording_start_unix_ns,segment_file,mono_ns")
	require.Contains(t, content, fmt.Sprintf("%d", start.UnixNano()))
	require.Contains(t, content, filepath.Base(segFile))
}

func TestAppendSegmentEntry_headerWrittenOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audio_timestamps.csv")
	seg1 := "/data/audio_seg1.ogg"
	seg2 := "/data/audio_seg2.ogg"

	require.NoError(t, appendSegmentEntry(path, time.Unix(1_000_000_000, 0), 1, seg1))
	require.NoError(t, appendSegmentEntry(path, time.Unix(2_000_000_000, 0), 2, seg2))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)

	require.Equal(t, 1, strings.Count(content, "recording_start_unix_ns"), "header must appear exactly once")
	require.Contains(t, content, filepath.Base(seg1))
	require.Contains(t, content, filepath.Base(seg2))
}
