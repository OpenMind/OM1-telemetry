package heartbeat

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewMonitor_returnsNonNil(t *testing.T) {
	mon := NewMonitor(30 * time.Second)
	require.NotNil(t, mon)
}

func TestTick_nilMonitor_isNoOp(t *testing.T) {
	var mon *Monitor
	require.NotPanics(t, func() { mon.Tick("anything") })
}

func TestTick_unregisteredStream_isNoOp(t *testing.T) {
	mon := NewMonitor(30 * time.Second)
	require.NotPanics(t, func() { mon.Tick("not-registered") })
}

func TestRegister_thenTick_incrementsCounter(t *testing.T) {
	mon := NewMonitor(30 * time.Second)
	mon.Register("lidar", 10)

	for range 7 {
		mon.Tick("lidar")
	}

	v, ok := mon.streams.Load("lidar")
	require.True(t, ok, "registered stream must be stored")
	require.Equal(t, int64(7), v.(*state).ticks.Load())
}

func TestTick_multipleStreams_independent(t *testing.T) {
	mon := NewMonitor(30 * time.Second)
	mon.Register("lidar", 10)
	mon.Register("depth", 15)

	mon.Tick("lidar")
	mon.Tick("lidar")
	mon.Tick("depth")

	vLidar, _ := mon.streams.Load("lidar")
	vDepth, _ := mon.streams.Load("depth")
	require.Equal(t, int64(2), vLidar.(*state).ticks.Load())
	require.Equal(t, int64(1), vDepth.(*state).ticks.Load())
}

func TestRun_exitsOnContextCancel(t *testing.T) {
	mon := NewMonitor(30 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		mon.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}

func TestCheck_gracePeriod_noWarn(t *testing.T) {
	// Very short interval so check() fires quickly.
	mon := NewMonitor(5 * time.Millisecond)
	mon.Register("camera", 30)

	// Call check() immediately — we're within the 2× interval grace period
	// so the stream should NOT be marked as warned.
	mon.check()

	v, ok := mon.streams.Load("camera")
	require.True(t, ok)
	require.False(t, v.(*state).warned, "should not warn within grace period")
}

func TestCheck_warnsAfterGracePeriodExpires(t *testing.T) {
	mon := NewMonitor(5 * time.Millisecond)
	mon.Register("camera", 30)

	// Wait longer than 2× interval so grace period expires.
	time.Sleep(20 * time.Millisecond)
	mon.check()

	v, ok := mon.streams.Load("camera")
	require.True(t, ok)
	require.True(t, v.(*state).warned, "should warn when stream never received a message past grace period")
}

func TestCheck_noWarnWhenTicksArrive(t *testing.T) {
	mon := NewMonitor(5 * time.Millisecond)
	mon.Register("lidar", 10)

	// Tick enough to be above half-rate threshold.
	for range 100 {
		mon.Tick("lidar")
	}

	time.Sleep(20 * time.Millisecond)
	mon.check()

	v, _ := mon.streams.Load("lidar")
	require.False(t, v.(*state).warned, "should not warn when stream is ticking at expected rate")
}
