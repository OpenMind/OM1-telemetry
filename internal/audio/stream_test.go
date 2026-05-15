package audio

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNew_returnsNonNilStream(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://localhost:8554/audio",
		OutputFile: filepath.Join(t.TempDir(), "audio.wav"),
	})
	if stream == nil {
		t.Fatal("New() returned nil")
	}
}

func TestStartStop_cleanLifecycle(t *testing.T) {
	// Use an unreachable URL so ffmpeg exits immediately.
	// Stop() must still return cleanly.
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable", // TEST-NET — no route
		OutputFile: filepath.Join(t.TempDir(), "audio.wav"),
	})

	stream.Start()

	// Give the goroutine a moment to enter the reconnect loop.
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		stream.Stop()
		close(done)
	}()

	select {
	case <-done:
		// expected
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return within 5 s")
	}
}

func TestStart_idempotent(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "audio.wav"),
	})

	stream.Start()
	stream.Start() // second call must be a no-op, not a panic

	stream.Stop()
}

func TestStop_beforeStart_isNoOp(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "audio.wav"),
	})
	// Stop without a preceding Start must not block or panic.
	stream.Stop()
}

func TestStop_idempotent(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "audio.wav"),
	})

	stream.Start()
	stream.Stop()
	stream.Stop() // second call must be a no-op
}
