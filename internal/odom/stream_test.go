package odom

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNew_returnsNonNilStream(t *testing.T) {
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "odom/test",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})
	require.NotNil(t, stream, "New() returned nil")
}

func TestStartStop_cleanLifecycle(t *testing.T) {
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "odom/unreachable",
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
		DDSTopic:       "odom/unreachable",
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
		DDSTopic:       "odom/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})
	stream.Stop()
}

func TestStop_idempotent(t *testing.T) {
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "odom/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})

	stream.Start()
	stream.Stop()
	stream.Stop()
}

func TestRotate_opensNewFilesAndPreservesOldOnes(t *testing.T) {
	dir := t.TempDir()
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "odom/unreachable",
		TimestampsFile: filepath.Join(dir, "a_timestamps.csv"),
		DataFile:       filepath.Join(dir, "a_data.bin"),
	})

	stream.Start()
	defer stream.Stop()
	time.Sleep(20 * time.Millisecond) // give loop() time to open the initial files

	newData := filepath.Join(dir, "b_data.bin")
	newTS := filepath.Join(dir, "b_timestamps.csv")
	require.NoError(t, stream.Rotate(newData, newTS))

	require.FileExists(t, filepath.Join(dir, "a_data.bin"), "the pre-rotation data file must not be deleted")
	require.FileExists(t, filepath.Join(dir, "a_timestamps.csv"), "the pre-rotation timestamps file must not be deleted")
	require.FileExists(t, newData, "rotate must create the new data file")
	require.FileExists(t, newTS, "rotate must create the new timestamps file")
}

func TestRotate_beforeStart_stillOpensFiles(t *testing.T) {
	dir := t.TempDir()
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "odom/unreachable",
		TimestampsFile: filepath.Join(dir, "a_timestamps.csv"),
		DataFile:       filepath.Join(dir, "a_data.bin"),
	})

	newData := filepath.Join(dir, "b_data.bin")
	newTS := filepath.Join(dir, "b_timestamps.csv")
	require.NoError(t, stream.Rotate(newData, newTS),
		"a rotate racing ahead of the stream's own initial file open must still succeed")

	stream.Start()
	defer stream.Stop()
	time.Sleep(20 * time.Millisecond)

	require.FileExists(t, newData)
	require.FileExists(t, newTS)
}

// TestRecord_resubscribesAfterProlongedSilence guards the bug where a wedged
// DDS subscription (subscribed successfully, but the publisher side stopped
// delivering samples) left the receiver channel blocked forever: no error,
// no data, and nothing to make the stream try again. Every closed session
// during the real outage this reproduces ended up with an empty data file.
func TestRecord_resubscribesAfterProlongedSilence(t *testing.T) {
	old := staleTimeout
	staleTimeout = 50 * time.Millisecond
	defer func() { staleTimeout = old }()

	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prevLogger)

	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "odom/unreachable", // subscribes fine; no publisher ever sends a sample
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})

	stream.Start()
	defer stream.Stop()

	require.Eventually(t, func() bool {
		return strings.Count(buf.String(), "odom recorder started") >= 2
	}, 3*time.Second, 10*time.Millisecond,
		"a stale subscription with zero samples must force a resubscribe, not sit blocked forever")
}
