package heartbeat

import (
	"context"
	"sync/atomic"
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

func TestUnregister_removesStream(t *testing.T) {
	mon := NewMonitor(30 * time.Second)
	mon.Register("lidar", 10)

	mon.Unregister("lidar")

	_, ok := mon.streams.Load("lidar")
	require.False(t, ok, "unregistered stream must not be stored")
}

func TestUnregister_unknownStream_isNoOp(t *testing.T) {
	mon := NewMonitor(30 * time.Second)
	require.NotPanics(t, func() { mon.Unregister("never-registered") })
}

func TestUnregister_thenRegister_resetsGracePeriod(t *testing.T) {
	mon := NewMonitor(5 * time.Millisecond)
	mon.Register("lidar", 10)
	time.Sleep(20 * time.Millisecond)
	mon.check() // would warn if left registered

	mon.Unregister("lidar")
	mon.Register("lidar", 10) // simulates a restart after stop

	mon.check() // immediately after registering, still within grace period

	v, ok := mon.streams.Load("lidar")
	require.True(t, ok)
	require.False(t, v.(*state).warned, "freshly re-registered stream should not warn immediately")
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

// Reproduces the real gap this closes: a DDS reader that never received the
// writer's post-restart discovery burst just sits there silently forever.
func TestRegisterRecoverable_callsReconnectWhenBroken(t *testing.T) {
	mon := NewMonitor(5 * time.Millisecond)
	called := make(chan struct{}, 1)
	mon.RegisterRecoverable("pointcloud", 10, func() {
		select {
		case called <- struct{}{}:
		default:
		}
	})

	time.Sleep(20 * time.Millisecond)
	mon.check()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect was not called for a stream that never received a message")
	}
}

func TestRegisterRecoverable_doesNotCallReconnectWhileHealthy(t *testing.T) {
	mon := NewMonitor(5 * time.Millisecond)
	called := make(chan struct{}, 1)
	mon.RegisterRecoverable("lidar", 10, func() {
		select {
		case called <- struct{}{}:
		default:
		}
	})

	for range 100 {
		mon.Tick("lidar")
	}
	time.Sleep(20 * time.Millisecond)
	mon.check()

	select {
	case <-called:
		t.Fatal("reconnect must not be called for a healthy stream")
	case <-time.After(50 * time.Millisecond):
	}
}

// check() must not pile up a new reconnect goroutine on top of one still running.
func TestRegisterRecoverable_doesNotOverlapReconnects(t *testing.T) {
	mon := NewMonitor(5 * time.Millisecond)
	var calls atomic.Int32
	release := make(chan struct{})
	mon.RegisterRecoverable("odom", 30, func() {
		calls.Add(1)
		<-release
	})

	time.Sleep(20 * time.Millisecond)
	mon.check()
	mon.check()

	time.Sleep(20 * time.Millisecond)
	require.Equal(t, int32(1), calls.Load(), "a reconnect already in flight must not be duplicated")
	close(release)
}

// Reader-level reconnects can loop forever without ever restoring data if
// the real fault is upstream of the reader, so a stream that worked and
// then got stuck must escalate exactly once, not on every remaining bad check.
func TestSetStuckThreshold_escalatesOnceAtThreshold(t *testing.T) {
	mon := NewMonitor(5 * time.Millisecond)
	mon.RegisterRecoverable("pointcloud", 10, func() {})
	mon.Tick("pointcloud") // it worked once, then got stuck

	var stuckCalls int
	var lastStream string
	mon.SetStuckThreshold(2, func(stream string) {
		stuckCalls++
		lastStream = stream
	})

	time.Sleep(20 * time.Millisecond)
	mon.check() // consumes the initial tick; establishes the baseline
	require.Zero(t, stuckCalls)

	mon.check()
	require.Zero(t, stuckCalls, "must not escalate before reaching the threshold")

	mon.check()
	require.Equal(t, 1, stuckCalls, "must escalate once the threshold is reached")
	require.Equal(t, "pointcloud", lastStream)

	mon.check()
	require.Equal(t, 1, stuckCalls, "must not escalate again while still broken")
}

// A stream with no hardware ever attached (e.g. no camera connected) never
// ticks at all; restarting it can never help, so it must never escalate no
// matter how long it stays "broken" -- otherwise the process would restart
// itself forever because of a permanently, expectedly absent device.
func TestSetStuckThreshold_neverEscalatesAStreamThatNeverWorked(t *testing.T) {
	mon := NewMonitor(5 * time.Millisecond)
	mon.RegisterRecoverable("depth", 10, func() {})

	var stuckCalls int
	mon.SetStuckThreshold(2, func(string) { stuckCalls++ })

	time.Sleep(20 * time.Millisecond)
	for range 20 {
		mon.check()
	}
	require.Zero(t, stuckCalls, "a stream that has never ticked must never escalate")
}

func TestSetStuckThreshold_recoveryResetsTheCount(t *testing.T) {
	mon := NewMonitor(5 * time.Millisecond)
	mon.RegisterRecoverable("odom", 10, func() {})

	var stuckCalls int
	mon.SetStuckThreshold(2, func(string) { stuckCalls++ })

	for range 50 {
		mon.Tick("odom")
	}
	time.Sleep(20 * time.Millisecond)
	mon.check() // consumes the ticks; establishes the baseline

	mon.check()
	require.Zero(t, stuckCalls, "one failure right after recovering must not immediately re-escalate")
}

// Session rotation re-registers every stream every few minutes; that must
// not reset a stream's progress toward the stuck threshold, or a stream
// broken for hours would never escalate.
func TestSetStuckThreshold_survivesReRegistration(t *testing.T) {
	mon := NewMonitor(5 * time.Millisecond)
	mon.RegisterRecoverable("pointcloud", 10, func() {})
	mon.Tick("pointcloud")

	var stuckCalls int
	mon.SetStuckThreshold(3, func(string) { stuckCalls++ })

	time.Sleep(20 * time.Millisecond)
	mon.check() // consumes the initial tick
	mon.check() // consecutiveBad=1
	require.Zero(t, stuckCalls, "must not escalate before reaching the threshold")

	mon.RegisterRecoverable("pointcloud", 10, func() {})
	time.Sleep(20 * time.Millisecond)
	mon.check() // consecutiveBad=2 despite the re-registration in between
	require.Zero(t, stuckCalls)
	mon.check() // consecutiveBad=3
	require.Equal(t, 1, stuckCalls, "re-registration must not reset progress toward the stuck threshold")
}

func TestSetStuckThreshold_disabledByDefault(t *testing.T) {
	mon := NewMonitor(5 * time.Millisecond)
	mon.RegisterRecoverable("lidar", 10, func() {})

	time.Sleep(20 * time.Millisecond)
	require.NotPanics(t, func() {
		for range 10 {
			mon.check()
		}
	})
}
