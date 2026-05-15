package video

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNew_returnsNonNilStream(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://localhost:8554/live",
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
	})
	require.NotNil(t, stream, "New() returned nil")
}

func TestStartStop_cleanLifecycle(t *testing.T) {

	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
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
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
	})

	stream.Start()
	stream.Start()

	stream.Stop()
}

func TestStop_beforeStart_isNoOp(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
	})
	stream.Stop()
}

func TestStop_idempotent(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
	})

	stream.Start()
	stream.Stop()
	stream.Stop()
}

func TestRecord_createsOutputFileDirectory(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "nested", "session")
	outputFile := filepath.Join(outputDir, "video.mp4")

	err := os.MkdirAll(outputDir, 0o755)
	require.NoError(t, err, "could not create output dir")

	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: outputFile,
	})

	stream.Start()
	time.Sleep(50 * time.Millisecond)
	stream.Stop()

	_, err = os.Stat(outputDir)
	require.False(t, os.IsNotExist(err), "output directory was unexpectedly removed")
}
