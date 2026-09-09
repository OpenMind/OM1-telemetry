package heartbeat

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Monitor struct {
	streams  sync.Map
	interval time.Duration

	stuckThreshold int
	stuckHandler   func(stream string)
}

type state struct {
	expectedHz float64
	ticks      atomic.Int64

	lastTicks  int64
	lastTime   time.Time
	registered time.Time
	warned     bool

	// reconnect, if set, is called (in its own goroutine) every time a check
	// finds the stream still broken -- see RegisterRecoverable.
	reconnect    func()
	reconnecting atomic.Bool

	// consecutiveBad counts checks in a row that found the stream broken;
	// reset the moment it recovers. Drives the stuck-handler escalation.
	consecutiveBad int
	escalated      bool

	// everWorked latches true on the first tick ever; only a stream that
	// worked and got stuck is eligible to escalate.
	everWorked bool
}

func NewMonitor(checkInterval time.Duration) *Monitor {
	return &Monitor{interval: checkInterval}
}

// SetStuckThreshold calls handler once, from check's goroutine, if a
// recoverable stream is still broken after threshold consecutive checks.
func (m *Monitor) SetStuckThreshold(threshold int, handler func(stream string)) {
	m.stuckThreshold = threshold
	m.stuckHandler = handler
}

func (m *Monitor) Register(name string, expectedHz float64) {
	m.register(name, expectedHz, nil)
}

// RegisterRecoverable is like Register, but reconnect is called once per
// check interval for as long as the stream stays broken.
func (m *Monitor) RegisterRecoverable(name string, expectedHz float64, reconnect func()) {
	m.register(name, expectedHz, reconnect)
}

// register (re)creates name's tracking state, carrying consecutiveBad and
// escalated over from any existing entry so periodic re-registration can't reset them.
func (m *Monitor) register(name string, expectedHz float64, reconnect func()) {
	now := time.Now()
	s := &state{
		expectedHz: expectedHz,
		lastTime:   now,
		registered: now,
		reconnect:  reconnect,
	}
	if prev, ok := m.streams.Load(name); ok {
		p := prev.(*state)
		s.consecutiveBad = p.consecutiveBad
		s.escalated = p.escalated
		s.everWorked = p.everWorked
	}
	m.streams.Store(name, s)
}

// Unregister stops tracking name, so a deliberately stopped stream isn't flagged NOT WORKING.
func (m *Monitor) Unregister(name string) {
	m.streams.Delete(name)
}

func (m *Monitor) Tick(name string) {
	if m == nil {
		return
	}
	if v, ok := m.streams.Load(name); ok {
		v.(*state).ticks.Add(1)
	}
}

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
		if current > 0 {
			s.everWorked = true
		}
		delta := current - s.lastTicks
		elapsed := now.Sub(s.lastTime).Seconds()
		var rate float64
		if elapsed > 0 {
			rate = float64(delta) / elapsed
		}

		inGrace := current == 0 && now.Sub(s.registered) < 2*m.interval
		if inGrace {
			s.lastTicks = current
			s.lastTime = now
			return true
		}

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

		switch {
		case bad && !s.warned:
			slog.Warn("recorder NOT WORKING",
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

		if bad && s.reconnect != nil && s.reconnecting.CompareAndSwap(false, true) {
			reconnect := s.reconnect
			go func() {
				defer s.reconnecting.Store(false)
				reconnect()
			}()
		}

		if bad && s.reconnect != nil && s.everWorked {
			s.consecutiveBad++
			if m.stuckThreshold > 0 && s.consecutiveBad >= m.stuckThreshold && !s.escalated {
				s.escalated = true
				slog.Error("recorder still broken after repeated reconnects; escalating",
					"stream", name, "consecutive_failures", s.consecutiveBad)
				m.stuckHandler(name)
			}
		} else {
			s.consecutiveBad = 0
			s.escalated = false
		}

		s.lastTicks = current
		s.lastTime = now
		return true
	})
}
