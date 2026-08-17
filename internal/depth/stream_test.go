package depth

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNew_returnsNonNilStream(t *testing.T) {
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "rt/depth/test",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})
	require.NotNil(t, stream, "New() returned nil")
}

func TestStartStop_cleanLifecycle(t *testing.T) {
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "rt/depth/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
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
		DDSDomainID:    0,
		DDSTopic:       "rt/depth/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})

	stream.Start()
	stream.Start()

	stream.Stop()
}

func TestStop_beforeStart_isNoOp(t *testing.T) {
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "rt/depth/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})
	stream.Stop()
}

func TestStop_idempotent(t *testing.T) {
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "rt/depth/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})

	stream.Start()
	stream.Stop()
	stream.Stop()
}
