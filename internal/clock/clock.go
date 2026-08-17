// Package clock provides a boot-anchored monotonic timeline alongside wall
// time, and detects the moment a wrong wall clock is corrected.
//
// Why this exists: a robot that boots without a network has no way to know the
// time. Its RTC is not battery-backed (the G1's is the PMIC's), so the clock
// comes up at whatever was last persisted -- days stale in the worst case. The
// recorder starts anyway, because the data collected on that trip is exactly
// the data worth having, and every timestamp it writes is wrong by a constant
// offset nobody has measured yet.
//
// Recording a monotonic reading next to every wall-clock timestamp turns that
// from data loss into arithmetic. CLOCK_BOOTTIME does not jump when NTP steps
// the clock and does not stop across suspend, so it orders the session
// correctly no matter what the wall clock says. When NTP finally lands, one
// (mono, utc) pair is enough to recover true UTC for every row recorded before
// it -- see script/fix_session_time.py.
package clock

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// pollInterval is how often the watcher samples both clocks. A step is
	// detected within one tick of it happening.
	pollInterval = time.Second

	// stepThreshold separates an NTP step from NTP's ordinary slew. Slew is
	// parts-per-million; a step that matters here is seconds to days.
	stepThreshold = time.Second

	// sampleInterval is the cadence for routine timebase records. Steps alone
	// are not enough: after synchronizing, NTP disciplines the clock frequency,
	// so wall and monotonic drift apart slowly. Periodic anchors let a reader
	// interpolate instead of assuming one constant offset for hours.
	sampleInterval = 60 * time.Second

	// defaultSyncMarkerPath is created by systemd-timesyncd once NTP has
	// actually synchronized. This is deliberately not the same as
	// time-set.target, which docker.service orders itself after: "the clock has
	// been set to something" is true of a stale RTC value too, and that is
	// precisely the failure this package exists to survive.
	defaultSyncMarkerPath = "/run/systemd/timesync/synchronized"

	// SyncMarkerEnv points the sync check at a different file, for a deployment
	// that establishes NTP state some other way -- and to exercise the
	// unsynced-boot path without waiting for one.
	SyncMarkerEnv = "CLOCK_SYNC_MARKER"

	bootIDPath = "/proc/sys/kernel/random/boot_id"

	// TimebaseName is the journal file written into each session directory.
	TimebaseName = "clock_timebase.jsonl"
)

// SyncState is what we can say about whether the wall clock is trustworthy.
type SyncState int

const (
	// SyncUnknown means the sync marker's directory is not visible, so there is
	// no evidence either way. Treated as trusted: a deployment that does not
	// mount /run/systemd/timesync should behave exactly as it did before this
	// package existed, rather than filing every session under pending/.
	SyncUnknown SyncState = iota
	// SyncNo means the marker directory exists and the marker does not: NTP has
	// definitively not synchronized yet.
	SyncNo
	// SyncYes means NTP has synchronized.
	SyncYes
)

func (s SyncState) String() string {
	switch s {
	case SyncYes:
		return "synced"
	case SyncNo:
		return "unsynced"
	default:
		return "unknown"
	}
}

// Trusted reports whether the wall clock may be used to name things.
func (s SyncState) Trusted() bool { return s != SyncNo }

// Record is one entry in clock_timebase.jsonl: a pairing of the monotonic
// timeline with what the wall clock believed at that instant.
type Record struct {
	// Kind is "start", "sample", "step", or "sync".
	Kind string `json:"kind"`
	// MonoNs is CLOCK_BOOTTIME, which never jumps.
	MonoNs int64 `json:"mono_ns"`
	// UTCNs is the wall clock after any step that this record reports.
	UTCNs int64 `json:"utc_ns"`
	// Synced is the sync state at the time of the record.
	Synced bool `json:"synced"`
	// UTCBeforeNs and StepNs are set on "step" records: what the wall clock
	// read immediately before the jump, and how far it moved.
	UTCBeforeNs int64 `json:"utc_before_ns,omitempty"`
	StepNs      int64 `json:"step_ns,omitempty"`
	// BootID identifies the boot this monotonic timeline belongs to. Monotonic
	// readings from different boots are not comparable.
	BootID string `json:"boot_id,omitempty"`
}

// Clock reads the two timelines.
type Clock struct {
	startMonoNs int64
	startWallNs int64
	startSync   SyncState
	bootID      string
}

// New samples both clocks and the sync state once, at startup.
func New() *Clock { return NewWithSync(Sync()) }

// NewWithSync is New for a caller that already knows whether the wall clock can
// be believed -- a deployment that determines it some other way, or a test.
func NewWithSync(sync SyncState) *Clock {
	c := &Clock{
		startMonoNs: MonoNs(),
		startWallNs: time.Now().UnixNano(),
		startSync:   sync,
		bootID:      readBootID(),
	}
	if c.startSync == SyncUnknown {
		slog.Info("clock sync state unknown; treating the wall clock as trusted",
			"marker", syncMarkerPath(),
			"hint", "mount /run/systemd/timesync:ro to detect an unsynced boot")
	}
	return c
}

// MonoNs returns CLOCK_BOOTTIME in nanoseconds: monotonic across NTP steps and
// across suspend. It is zero-based at boot, not at any wall-clock epoch.
func MonoNs() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		// Every Linux since 2.6.39 has CLOCK_BOOTTIME. If it is somehow absent,
		// a monotonic-but-suspend-blind reading still beats returning zero.
		if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
			return 0
		}
	}
	return ts.Nano()
}

// Now returns the wall clock and the monotonic reading, sampled together.
func (c *Clock) Now() (unixNs, monoNs int64) {
	return time.Now().UnixNano(), MonoNs()
}

// MonoNs returns the current monotonic reading.
func (c *Clock) MonoNs() int64 { return MonoNs() }

// StartMonoNs is the monotonic reading when the recorder started.
func (c *Clock) StartMonoNs() int64 { return c.startMonoNs }

// StartWallNs is the wall clock when the recorder started -- wrong, if the
// robot booted offline.
func (c *Clock) StartWallNs() int64 { return c.startWallNs }

// StartSync is the sync state observed at startup.
func (c *Clock) StartSync() SyncState { return c.startSync }

// BootID identifies this boot's monotonic timeline.
func (c *Clock) BootID() string { return c.bootID }

// StartWallNsNow back-computes when the session started, on the wall clock as
// it reads now. Once NTP has corrected a stale clock this is the true session
// start; before that it equals StartWallNs.
func (c *Clock) StartWallNsNow() int64 {
	wall, mono := c.Now()
	return wall - (mono - c.startMonoNs)
}

// syncMarkerPath is the file whose existence means "NTP has synchronized".
func syncMarkerPath() string {
	if p := os.Getenv(SyncMarkerEnv); p != "" {
		return p
	}
	return defaultSyncMarkerPath
}

// Sync reports what can be determined about NTP synchronization.
func Sync() SyncState {
	marker := syncMarkerPath()
	if _, err := os.Stat(marker); err == nil {
		return SyncYes
	}
	// The marker's absence only means something if we can see its directory.
	if _, err := os.Stat(filepath.Dir(marker)); err == nil {
		return SyncNo
	}
	return SyncUnknown
}

func readBootID() string {
	raw, err := os.ReadFile(bootIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// ShortBootID is the boot id truncated for use in a directory name.
func (c *Clock) ShortBootID() string {
	id := strings.ReplaceAll(c.bootID, "-", "")
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "unknownboot"
	}
	return id
}

// Watcher journals the monotonic-to-UTC mapping for a session and reports the
// moment the wall clock becomes trustworthy.
type Watcher struct {
	clk  *Clock
	path string
	// syncFn probes NTP state. Held as a field rather than calling Sync()
	// directly so a test can drive the transition it is asserting about.
	syncFn func() SyncState

	mu       sync.Mutex
	file     *os.File
	trusted  bool
	onTrust  func(Record)
	lastWall int64
	lastMono int64
}

// NewWatcher journals to path. onTrusted fires at most once, on the first
// evidence that the wall clock can be believed -- either NTP's marker appearing
// or a step large enough to only be an NTP correction. It runs on the watcher's
// goroutine, so it should not block for long.
func NewWatcher(clk *Clock, path string, onTrusted func(Record)) *Watcher {
	return &Watcher{
		clk:     clk,
		path:    path,
		syncFn:  Sync,
		onTrust: onTrusted,
		trusted: clk.StartSync() == SyncYes,
	}
}

// Run journals until ctx is cancelled. It writes a "start" record immediately
// so a session always has at least one anchor, even if it is a wrong one.
func (w *Watcher) Run(ctx context.Context) {
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Error("clock: cannot open timebase journal; corrections will not be recoverable",
			"path", w.path, "err", err)
		return
	}
	w.mu.Lock()
	w.file = f
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if err := w.file.Sync(); err != nil {
			slog.Warn("clock: timebase journal sync failed", "err", err)
		}
		if err := w.file.Close(); err != nil {
			slog.Warn("clock: timebase journal close failed", "err", err)
		}
		w.file = nil
	}()

	wall, mono := w.clk.Now()
	w.lastWall, w.lastMono = wall, mono
	w.write(Record{
		Kind:   "start",
		MonoNs: mono,
		UTCNs:  wall,
		Synced: w.clk.StartSync() == SyncYes,
		BootID: w.clk.BootID(),
	})

	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	sample := time.NewTicker(sampleInterval)
	defer sample.Stop()

	for {
		select {
		case <-ctx.Done():
			w.tick(false)
			return
		case <-poll.C:
			w.tick(false)
		case <-sample.C:
			w.tick(true)
		}
	}
}

// tick samples both clocks, journals a step if one happened, and journals a
// routine anchor when forceSample is set.
func (w *Watcher) tick(forceSample bool) {
	wall, mono := w.clk.Now()
	synced := w.syncFn() == SyncYes

	dWall := wall - w.lastWall
	dMono := mono - w.lastMono
	drift := dWall - dMono

	prevWall := w.lastWall
	w.lastWall, w.lastMono = wall, mono

	stepped := drift > int64(stepThreshold) || drift < -int64(stepThreshold)

	switch {
	case stepped:
		rec := Record{
			Kind:        "step",
			MonoNs:      mono,
			UTCNs:       wall,
			Synced:      synced,
			UTCBeforeNs: prevWall + dMono, // what the old clock would have read
			StepNs:      drift,
			BootID:      w.clk.BootID(),
		}
		w.write(rec)
		slog.Info("clock stepped; earlier timestamps in this session are correctable",
			"step", time.Duration(drift).String(),
			"now", time.Unix(0, wall).UTC().Format(time.RFC3339Nano),
			"synced", synced)
		w.maybeTrust(rec)
	case forceSample:
		w.write(Record{
			Kind:   "sample",
			MonoNs: mono,
			UTCNs:  wall,
			Synced: synced,
			BootID: w.clk.BootID(),
		})
	}

	// The marker can appear without a step, when the clock was already close.
	if synced && !stepped {
		w.maybeTrust(Record{
			Kind:   "sync",
			MonoNs: mono,
			UTCNs:  wall,
			Synced: true,
			BootID: w.clk.BootID(),
		})
	}
}

// maybeTrust fires the onTrusted callback the first time the clock becomes
// believable. A step is only taken as evidence once NTP confirms it; a step
// with no marker (someone ran `date`) is journaled but does not rename anything.
func (w *Watcher) maybeTrust(rec Record) {
	w.mu.Lock()
	if w.trusted || !rec.Synced {
		w.mu.Unlock()
		return
	}
	w.trusted = true
	cb := w.onTrust
	w.mu.Unlock()

	if rec.Kind == "sync" {
		w.write(rec)
	}
	if cb != nil {
		cb(rec)
	}
}

func (w *Watcher) write(rec Record) {
	line, err := json.Marshal(rec)
	if err != nil {
		slog.Warn("clock: cannot marshal timebase record", "err", err)
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return
	}
	if _, err := fmt.Fprintf(w.file, "%s\n", line); err != nil {
		slog.Warn("clock: timebase journal write failed", "err", err)
		return
	}
	// Synced per record: this file is worthless if it is lost to a power cut,
	// and it is a handful of bytes a minute.
	if err := w.file.Sync(); err != nil {
		slog.Warn("clock: timebase journal sync failed", "err", err)
	}
}
