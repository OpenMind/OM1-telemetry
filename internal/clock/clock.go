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
	pollInterval          = time.Second
	stepThreshold         = time.Second
	sampleInterval        = 60 * time.Second
	defaultSyncMarkerPath = "/run/systemd/timesync/synchronized"
	SyncMarkerEnv         = "CLOCK_SYNC_MARKER"
	bootIDPath            = "/proc/sys/kernel/random/boot_id"
	TimebaseName          = "clock_timebase.jsonl"
)

type SyncState int

const (
	SyncUnknown SyncState = iota
	SyncNo
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

type Record struct {
	Kind        string `json:"kind"`
	MonoNs      int64  `json:"mono_ns"`
	UTCNs       int64  `json:"utc_ns"`
	Synced      bool   `json:"synced"`
	UTCBeforeNs int64  `json:"utc_before_ns,omitempty"`
	StepNs      int64  `json:"step_ns,omitempty"`
	BootID      string `json:"boot_id,omitempty"`
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

func MonoNs() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
			return 0
		}
	}
	return ts.Nano()
}

func (c *Clock) Now() (unixNs, monoNs int64) {
	return time.Now().UnixNano(), MonoNs()
}

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

func (c *Clock) StartWallNsNow() int64 {
	wall, mono := c.Now()
	return wall - (mono - c.startMonoNs)
}

func syncMarkerPath() string {
	if p := os.Getenv(SyncMarkerEnv); p != "" {
		return p
	}
	return defaultSyncMarkerPath
}

func Sync() SyncState {
	marker := syncMarkerPath()
	if _, err := os.Stat(marker); err == nil {
		return SyncYes
	}
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

type Watcher struct {
	clk    *Clock
	path   string
	syncFn func() SyncState

	mu       sync.Mutex
	file     *os.File
	trusted  bool
	onTrust  func(Record)
	lastWall int64
	lastMono int64
}

func NewWatcher(clk *Clock, path string, onTrusted func(Record)) *Watcher {
	return &Watcher{
		clk:     clk,
		path:    path,
		syncFn:  Sync,
		onTrust: onTrusted,
		trusted: clk.StartSync() == SyncYes,
	}
}

// Trusted reports whether the wall clock has synchronized at some point during this run.
func (w *Watcher) Trusted() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.trusted
}

// SyncState summarizes the watcher's current best knowledge of clock trust, in Sync()'s terms.
func (w *Watcher) SyncState() SyncState {
	if w.Trusted() {
		return SyncYes
	}
	return SyncNo
}

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
	if err := w.file.Sync(); err != nil {
		slog.Warn("clock: timebase journal sync failed", "err", err)
	}
}
