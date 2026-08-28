package lidar

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNew_returnsNonNilStream(t *testing.T) {
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "rt/utlidar/cloud",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})
	require.NotNil(t, stream, "New() returned nil")
}

func TestStartStop_cleanLifecycle(t *testing.T) {
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "rt/utlidar/cloud/unreachable",
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
		DDSTopic:       "rt/utlidar/cloud/unreachable",
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
		DDSTopic:       "rt/utlidar/cloud/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})
	stream.Stop()
}

func TestStop_idempotent(t *testing.T) {
	stream := New(Config{
		DDSDomainID:    0,
		DDSTopic:       "rt/utlidar/cloud/unreachable",
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
		DDSTopic:       "rt/utlidar/cloud/unreachable",
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
		DDSTopic:       "rt/utlidar/cloud/unreachable",
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
