// Package heartbeat provides centralized health monitoring for all recorders.
//
// Design principle: SILENT when everything works.  The log only contains
// heartbeat messages when something is wrong (recorder stopped receiving,
// or rate dropped below expected).  Each transition (healthy → broken,
// broken → recovered) is logged exactly once, so you don't get spam.
//
// Why this matters: during an 8-hour road trip recording session, you do
// NOT want "all recorders healthy" messages every 30 seconds.  You want
// the log to be EMPTY unless something needs attention.  This module
// achieves that.
//
// USAGE in main.go:
//
//	mon := heartbeat.NewMonitor(30 * time.Second)
//	mon.Register("lidar",      10)    // expected 10 Hz
//	mon.Register("lowstate",   1000)  // expected ~1 kHz on G1, ~500 Hz on Go2
//	mon.Register("network",    0)     // 0 = just check it's alive at all
//	go mon.Run(ctx)
//
//	// Pass mon to each recorder's Config:
//	lidarCfg.Monitor = mon
//
// USAGE in each recorder, after receiving a message:
//
//	l.cfg.Monitor.Tick("lidar")   // safe even if Monitor is nil
package heartbeat

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Monitor tracks per-stream tick counters and emits warnings when streams
// stop ticking or fall below their expected rate.
type Monitor struct {
	streams  sync.Map // name (string) → *state
	interval time.Duration
}

type state struct {
	expectedHz float64
	ticks      atomic.Int64 // total ticks since registration

	// The following fields are accessed ONLY by the checker goroutine,
	// so no mutex is needed.
	lastTicks  int64
	lastTime   time.Time
	registered time.Time
	warned     bool
}

// NewMonitor creates a monitor that checks all registered streams every
// checkInterval.  30 seconds is a good default — long enough to filter
// transient hiccups, short enough that you notice problems quickly.
func NewMonitor(checkInterval time.Duration) *Monitor {
	return &Monitor{interval: checkInterval}
}

// Register adds a stream to monitor.
//
// expectedHz is the rate this stream should be running at.  The monitor
// warns if the actual rate falls below HALF of this.  Pass 0 to disable
// rate checking entirely; the monitor will then only warn if the stream
// goes completely silent.
//
// Common values:
//
//	LiDAR cloud      10
//	RealSense depth  15
//	RealSense color  30
//	odom             50
//	lowstate Go2     500
//	lowstate G1      1000
//	network ping     0 (or 0.2 if you want to enforce ~5s polling)
//
// Use lower-bound estimates — better to under-warn than spam false positives.
func (m *Monitor) Register(name string, expectedHz float64) {
	now := time.Now()
	m.streams.Store(name, &state{
		expectedHz: expectedHz,
		lastTime:   now,
		registered: now,
	})
}

// Tick records that the named stream just received a message.  Call this
// once per received message from inside the recorder.
//
// nil-safe: calling Tick on a nil *Monitor is a no-op.  This means
// recorders can hold a *Monitor in their config without nil-checking at
// every call site, and unit tests / standalone runs can pass nil to
// disable monitoring entirely.
//
// Hot path: a single atomic.Int64 add.  Safe to call from any goroutine.
func (m *Monitor) Tick(name string) {
	if m == nil {
		return
	}
	if v, ok := m.streams.Load(name); ok {
		v.(*state).ticks.Add(1)
	}
}

// Run blocks until ctx is done, periodically checking all registered
// streams.  Launch in a goroutine from main:
//
//	go mon.Run(ctx)
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.check()
		}
	}
}

func (m *Monitor) check() {
	now := time.Now()

	m.streams.Range(func(k, v any) bool {
		name := k.(string)
		s := v.(*state)

		current := s.ticks.Load()
		delta := current - s.lastTicks
		elapsed := now.Sub(s.lastTime).Seconds()
		var rate float64
		if elapsed > 0 {
			rate = float64(delta) / elapsed
		}

		// Grace period: don't warn during the first 2× interval after
		// registration if we haven't yet received any message.  This
		// prevents spurious "never received" warnings during startup.
		inGrace := current == 0 && now.Sub(s.registered) < 2*m.interval
		if inGrace {
			s.lastTicks = current
			s.lastTime = now
			return true
		}

		// Health classification
		bad := false
		var reason string
		switch {
		case delta == 0 && current == 0:
			bad = true
			reason = "never received any message"
		case delta == 0:
			bad = true
			reason = "stopped receiving messages"
		case s.expectedHz > 0 && rate < s.expectedHz*0.5:
			bad = true
			reason = "rate dropped below half of expected"
		}

		// Log ONLY on state transitions.  This is what keeps the log
		// silent during normal operation.
		switch {
		case bad && !s.warned:
			slog.Warn("⚠️  recorder NOT WORKING",
				"stream", name,
				"reason", reason,
				"actual_hz", strconv.FormatFloat(rate, 'f', 1, 64),
				"expected_hz", strconv.FormatFloat(s.expectedHz, 'f', 0, 64),
				"total_msgs_since_start", current)
			s.warned = true

		case !bad && s.warned:
			slog.Info("✅ recorder recovered",
				"stream", name,
				"actual_hz", strconv.FormatFloat(rate, 'f', 1, 64))
			s.warned = false
		}

		s.lastTicks = current
		s.lastTime = now
		return true
	})
}