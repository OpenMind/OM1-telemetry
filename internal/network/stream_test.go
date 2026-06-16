package network

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testConfig(t *testing.T) Config {
	return Config{
		PingHost:     "192.0.2.1",
		PingTimeout:  1 * time.Second,
		PollInterval: 50 * time.Millisecond,
		DataFile:     filepath.Join(t.TempDir(), "network_status.csv"),
	}
}

func TestNew_returnsNonNilStream(t *testing.T) {
	require.NotNil(t, New(testConfig(t)), "New() returned nil")
}

func TestStartStop_cleanLifecycle(t *testing.T) {
	stream := New(testConfig(t))
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
	stream := New(testConfig(t))
	stream.Start()
	stream.Start()
	stream.Stop()
}

func TestStop_beforeStart_isNoOp(t *testing.T) {
	require.NotPanics(t, func() { New(testConfig(t)).Stop() })
}

func TestParsePing_reachable(t *testing.T) {
	out := `PING 8.8.8.8 (8.8.8.8) 56(84) bytes of data.
64 bytes from 8.8.8.8: icmp_seq=1 ttl=116 time=12.3 ms

--- 8.8.8.8 ping statistics ---
1 packets transmitted, 1 received, 0% packet loss, time 0ms
rtt min/avg/max/mdev = 12.345/12.345/12.345/0.000 ms`

	res := parsePing(out)
	require.True(t, res.reachable)
	require.InDelta(t, 12.345, res.rttMs, 0.001)
	require.InDelta(t, 0, res.lossPct, 0.001)
}

func TestParsePing_unreachable(t *testing.T) {
	out := `PING 192.0.2.1 (192.0.2.1) 56(84) bytes of data.

--- 192.0.2.1 ping statistics ---
1 packets transmitted, 0 received, 100% packet loss, time 0ms`

	res := parsePing(out)
	require.False(t, res.reachable)
	require.InDelta(t, 100, res.lossPct, 0.001)
}

func TestParsePing_emptyOutput(t *testing.T) {
	res := parsePing("")
	require.False(t, res.reachable)
	require.InDelta(t, 100, res.lossPct, 0.001)
}

func TestParsePing_macOSFormat(t *testing.T) {
	// macOS ping uses "round-trip" instead of "rtt" and a slightly different layout.
	out := `PING 8.8.8.8 (8.8.8.8): 56 data bytes
64 bytes from 8.8.8.8: icmp_seq=0 ttl=116 time=11.234 ms

--- 8.8.8.8 ping statistics ---
1 packets transmitted, 1 packets received, 0.0% packet loss
round-trip min/avg/max/stddev = 11.234/11.234/11.234/0.000 ms`

	res := parsePing(out)
	require.True(t, res.reachable)
	require.InDelta(t, 11.234, res.rttMs, 0.001)
	require.InDelta(t, 0, res.lossPct, 0.01)
}

func TestStop_idempotent(t *testing.T) {
	stream := New(testConfig(t))
	stream.Start()
	stream.Stop()
	stream.Stop() // second call must be a no-op
}

func TestFormatFloat_validValue(t *testing.T) {
	require.Equal(t, "12.345", formatFloat(12.345, true))
	require.Equal(t, "0.000", formatFloat(0, true))
}

func TestFormatFloat_invalidValue(t *testing.T) {
	require.Equal(t, "", formatFloat(99.9, false), "invalid flag must return empty string")
}
