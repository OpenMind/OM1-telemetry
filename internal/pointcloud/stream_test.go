package pointcloud

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestEncodeFrame_compressesAndRoundTrips(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err, "create encoder")
	defer func() { require.NoError(t, encoder.Close()) }()

	decoder, err := zstd.NewReader(nil)
	require.NoError(t, err, "create decoder")
	defer decoder.Close()

	raw := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 1024)

	data, method := encodeFrame(encoder, raw)

	require.Equal(t, "zstd", method, "expected compressible payload to use zstd")
	require.Less(t, len(data), len(raw), "compressed frame should be smaller than raw")

	decoded, err := decoder.DecodeAll(data, nil)
	require.NoError(t, err, "decode frame")
	require.Equal(t, raw, decoded, "round-trip must be lossless")
}

func TestEncodeFrame_fallsBackToRawWhenNotSmaller(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err, "create encoder")
	defer func() { require.NoError(t, encoder.Close()) }()

	raw := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	data, method := encodeFrame(encoder, raw)

	require.Equal(t, "raw", method, "expected incompressible payload to fall back to raw")
	require.Equal(t, raw, data, "raw fallback must store the payload verbatim")
}

func TestNew_returnsNonNilStream(t *testing.T) {
	stream := New(Config{
		ZenohEndpoint:  "tcp/192.0.2.1:7447",
		ZenohTopic:     "rt/utlidar/cloud_livox_mid360",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})
	require.NotNil(t, stream, "New() returned nil")
}

func TestStartStop_cleanLifecycle(t *testing.T) {
	stream := New(Config{
		ZenohEndpoint:  "tcp/192.0.2.1:7447",
		ZenohTopic:     "pointcloud/unreachable",
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
		ZenohEndpoint:  "tcp/192.0.2.1:7447",
		ZenohTopic:     "pointcloud/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})

	stream.Start()
	stream.Start()

	stream.Stop()
}

func TestStop_beforeStart_isNoOp(t *testing.T) {
	stream := New(Config{
		ZenohEndpoint:  "tcp/192.0.2.1:7447",
		ZenohTopic:     "pointcloud/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})
	stream.Stop()
}

func TestStop_idempotent(t *testing.T) {
	stream := New(Config{
		ZenohEndpoint:  "tcp/192.0.2.1:7447",
		ZenohTopic:     "pointcloud/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})

	stream.Start()
	stream.Stop()
	stream.Stop()
}
