package lidar

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNew_returnsNonNilStream(t *testing.T) {
	stream := New(Config{
		ZenohEndpoint:  "tcp/192.0.2.1:7447",
		ZenohTopic:     "lidar/test",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})
	require.NotNil(t, stream, "New() returned nil")
}

func TestStartStop_cleanLifecycle(t *testing.T) {
	stream := New(Config{
		ZenohEndpoint:  "tcp/192.0.2.1:7447",
		ZenohTopic:     "lidar/unreachable",
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
		ZenohTopic:     "lidar/unreachable",
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
		ZenohTopic:     "lidar/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})
	stream.Stop()
}

func TestStop_idempotent(t *testing.T) {
	stream := New(Config{
		ZenohEndpoint:  "tcp/192.0.2.1:7447",
		ZenohTopic:     "lidar/unreachable",
		TimestampsFile: filepath.Join(t.TempDir(), "timestamps.csv"),
		DataFile:       filepath.Join(t.TempDir(), "data.bin"),
	})

	stream.Start()
	stream.Stop()
	stream.Stop()
}

func TestNtpTimeToUnixNs(t *testing.T) {
	tests := []struct {
		name     string
		ntpTime  uint64
		expected int64
	}{
		{
			name:     "epoch_time",
			ntpTime:  2208988800 << 32,
			expected: 0,
		},
		{
			name:     "with_fraction",
			ntpTime:  (2208988801 << 32) | 0x80000000,
			expected: 1_500_000_000,
		},
		{
			name:     "recent_time",
			ntpTime:  (2208988800 + 1000000) << 32,
			expected: 1000000 * 1e9,
		},
		{
			name:     "time_with_precise_fraction",
			ntpTime:  (2208988801 << 32) | 0x40000000,
			expected: 1_250_000_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ntpTimeToUnixNs(tt.ntpTime)
			require.Equal(t, tt.expected, result, "timestamp conversion mismatch")
		})
	}
}
