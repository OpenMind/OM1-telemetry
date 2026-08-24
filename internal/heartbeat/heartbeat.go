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
}

type state struct {
	expectedHz float64
	ticks      atomic.Int64

	lastTicks  int64
	lastTime   time.Time
	registered time.Time
	warned     bool
}

func NewMonitor(checkInterval time.Duration) *Monitor {
	return &Monitor{interval: checkInterval}
}

func (m *Monitor) Register(name string, expectedHz float64) {
	now := time.Now()
	m.streams.Store(name, &state{
		expectedHz: expectedHz,
		lastTime:   now,
		registered: now,
	})
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

		s.lastTicks = current
		s.lastTime = now
		return true
	})
}
