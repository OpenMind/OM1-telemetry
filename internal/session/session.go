package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"om1-telemetry/internal/clock"
)

const (
	// PendingDirName holds sessions whose true start time is not yet known. It
	// deliberately does not look like a date directory, so anything scanning
	// for YYYY-MM-DD (retention, upload) skips it until it has been promoted.
	PendingDirName = "pending"

	// CurrentLinkName is the symlink recorders write through while a session is
	// pending, so the directory underneath can be renamed without them noticing.
	CurrentLinkName = ".current"

	// TimebaseFileName is the clock journal, read by Janitor and by
	// script/fix_session_time.py.
	TimebaseFileName = clock.TimebaseName

	metaFileName = "meta.json"

	dateLayout    = "2006-01-02"
	sessionLayout = "2006-01-02_15-04-05"
)

// Session is one recording's directory.
type Session struct {
	root string
	clk  *clock.Clock

	mu        sync.Mutex
	robotType string
	realDir   string // where the files actually are
	dir       string // what recorders were handed; a symlink while pending
	pending   bool
	promoted  bool
}

// Meta is the session's meta.json.
type Meta struct {
	SessionDir         string `json:"session_dir"`
	SessionStartUnixNs int64  `json:"session_start_unix_ns"`
	SessionStartMonoNs int64  `json:"session_start_mono_ns"`
	BootID             string `json:"boot_id,omitempty"`
	// RobotType names the profile this session was recorded with, so an
	// analysis tool can look up the expected publish rates without being told.
	RobotType             string `json:"robot_type,omitempty"`
	ClockSyncStateAtStart string `json:"clock_sync_state_at_start"`
	ClockTrustedAtStart   bool   `json:"clock_trusted_at_start"`
	// StartTimeCorrected is set when the session was promoted out of pending/,
	// meaning SessionStartUnixNs was recomputed from the monotonic timeline.
	StartTimeCorrected bool   `json:"start_time_corrected,omitempty"`
	PromotedFrom       string `json:"promoted_from,omitempty"`
}

// Open creates the session directory. When the clock is trusted this is the
// familiar root/YYYY-MM-DD/YYYY-MM-DD_HH-MM-SS and no symlink is involved --
// identical to the behaviour before this package existed.
func Open(root string, clk *clock.Clock) (*Session, error) {
	s := &Session{root: root, clk: clk}

	if clk.StartSync().Trusted() {
		start := time.Unix(0, clk.StartWallNs())
		s.realDir = datedDir(root, start)
		s.dir = s.realDir
		if err := os.MkdirAll(s.realDir, 0o755); err != nil {
			return nil, fmt.Errorf("create session dir: %w", err)
		}
		if err := s.writeMeta(); err != nil {
			return nil, err
		}
		return s, nil
	}

	// Untrusted: name the directory from things that are true anyway.
	s.pending = true
	name := fmt.Sprintf("%s_%012d", clk.ShortBootID(), clk.StartMonoNs()/int64(time.Millisecond))
	s.realDir = filepath.Join(root, PendingDirName, name)
	if err := os.MkdirAll(s.realDir, 0o755); err != nil {
		return nil, fmt.Errorf("create pending session dir: %w", err)
	}

	link := filepath.Join(root, CurrentLinkName)
	if err := repointSymlink(link, s.realDir); err != nil {
		// Without the symlink the recorders would hold a path that a later
		// rename invalidates, so stay put in pending/ instead. Janitor renames
		// it on the next start.
		slog.Warn("session: cannot create current-session symlink; session will stay in pending/ until the next start",
			"link", link, "err", err)
		s.dir = s.realDir
	} else {
		s.dir = link
	}

	slog.Warn("clock not synchronized; recording to a pending session directory",
		"dir", s.realDir,
		"note", "renamed to its true date automatically once NTP synchronizes")

	if err := s.writeMeta(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir is the path recorders write through. It does not change for the life of
// the session, even when the directory underneath is renamed.
func (s *Session) Dir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dir
}

// RealDir is the current true location of the files.
func (s *Session) RealDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.realDir
}

// Pending reports whether the session is still waiting for a trustworthy clock.
func (s *Session) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending && !s.promoted
}

// TimebasePath is where the clock watcher journals.
func (s *Session) TimebasePath() string {
	return filepath.Join(s.Dir(), TimebaseFileName)
}

// Promote moves a pending session to its true date, computed by carrying the
// now-correct wall clock back along the monotonic timeline. Open files are
// unaffected: their descriptors follow the inode, and new files are opened
// through the symlink, which is repointed in the same operation.
func (s *Session) Promote() error {
	s.mu.Lock()
	if !s.pending || s.promoted {
		s.mu.Unlock()
		return nil
	}
	from := s.realDir
	s.mu.Unlock()

	trueStart := time.Unix(0, s.clk.StartWallNsNow())
	to := datedDir(s.root, trueStart)

	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fmt.Errorf("create date dir: %w", err)
	}
	to = uniqueDir(to)
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", from, to, err)
	}

	s.mu.Lock()
	s.realDir = to
	s.promoted = true
	usingLink := s.dir != from
	s.mu.Unlock()

	if usingLink {
		if err := repointSymlink(filepath.Join(s.root, CurrentLinkName), to); err != nil {
			// The rename already happened; recorders holding open files are
			// fine, but anything opening a new file now would fail. Loud.
			slog.Error("session: promoted but could not repoint the current symlink",
				"dir", to, "err", err)
		}
	}

	if err := s.writeMetaCorrected(trueStart, from); err != nil {
		slog.Warn("session: could not rewrite meta.json after promotion", "err", err)
	}

	slog.Info("session promoted to its true date",
		"from", from, "to", to,
		"start", trueStart.UTC().Format(time.RFC3339Nano))
	return nil
}

func (s *Session) writeMeta() error {
	m := Meta{
		SessionDir:            s.realDir,
		SessionStartUnixNs:    s.clk.StartWallNs(),
		SessionStartMonoNs:    s.clk.StartMonoNs(),
		BootID:                s.clk.BootID(),
		ClockSyncStateAtStart: s.clk.StartSync().String(),
		ClockTrustedAtStart:   s.clk.StartSync().Trusted(),
		RobotType:             s.robotType,
	}
	return writeMetaTo(s.Dir(), m)
}

// SetRobotType records which profile this session used. Called once the
// configuration has resolved, which happens after the directory exists.
func (s *Session) SetRobotType(rt string) {
	s.mu.Lock()
	s.robotType = rt
	s.mu.Unlock()
	if err := s.writeMeta(); err != nil {
		slog.Warn("session: could not record robot type in meta.json", "err", err)
	}
}

func (s *Session) writeMetaCorrected(trueStart time.Time, from string) error {
	m := Meta{
		SessionDir:            s.RealDir(),
		SessionStartUnixNs:    trueStart.UnixNano(),
		SessionStartMonoNs:    s.clk.StartMonoNs(),
		BootID:                s.clk.BootID(),
		ClockSyncStateAtStart: s.clk.StartSync().String(),
		ClockTrustedAtStart:   s.clk.StartSync().Trusted(),
		RobotType:             s.robotType,
		StartTimeCorrected:    true,
		PromotedFrom:          filepath.Base(from),
	}
	return writeMetaTo(s.Dir(), m)
}

func writeMetaTo(dir string, m Meta) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	path := filepath.Join(dir, metaFileName)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write meta: %w", err)
	}
	return nil
}

// Janitor promotes pending sessions left behind by an earlier run -- a crash, a
// power cut, or a shutdown that happened before NTP ever synchronized.
//
// A leftover session is recoverable when its own timebase journal contains a
// synchronized record: that record pairs the monotonic timeline with real UTC,
// which is all that is needed to back-compute when the session started. One
// that never saw a synchronized clock is left alone, because the information
// required to date it does not exist anywhere -- its monotonic timeline died
// with that boot. Guessing would be worse than leaving it honestly undated.
func Janitor(root string, skip string) {
	pendingRoot := filepath.Join(root, PendingDirName)
	entries, err := os.ReadDir(pendingRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("session: cannot scan pending sessions", "dir", pendingRoot, "err", err)
		}
		return
	}

	var promoted, stranded int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(pendingRoot, e.Name())
		if skip != "" && sameDir(dir, skip) {
			continue
		}

		start, ok := recoverStart(dir)
		if !ok {
			stranded++
			continue
		}

		to := uniqueDir(datedDir(root, start))
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			slog.Warn("session: cannot create date dir for recovered session",
				"dir", to, "err", err)
			continue
		}
		if err := os.Rename(dir, to); err != nil {
			slog.Warn("session: cannot promote recovered session",
				"from", dir, "to", to, "err", err)
			continue
		}
		if err := patchMetaStart(to, start, filepath.Base(dir)); err != nil {
			slog.Warn("session: promoted but could not update meta.json",
				"dir", to, "err", err)
		}
		slog.Info("recovered a pending session from an earlier run",
			"from", filepath.Base(dir), "to", to,
			"start", start.UTC().Format(time.RFC3339Nano))
		promoted++
	}

	if stranded > 0 {
		slog.Warn("pending sessions left undated: they never saw a synchronized clock",
			"count", stranded, "dir", pendingRoot,
			"note", "their monotonic timeline ended with that boot, so no later sync can date them")
	}
	if promoted > 0 {
		slog.Info("pending sessions recovered", "count", promoted)
	}
}

// recoverStart reads a session's timebase journal and works out when it began.
func recoverStart(dir string) (time.Time, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, TimebaseFileName))
	if err != nil {
		return time.Time{}, false
	}

	var startMono int64
	var haveStart bool
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec clock.Record
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.Kind == "start" && !haveStart {
			startMono, haveStart = rec.MonoNs, true
		}
		// The first synchronized record is the anchor. Later ones would work
		// equally well; the first is the closest to the data that needs dating.
		if haveStart && rec.Synced && rec.UTCNs > 0 {
			return time.Unix(0, rec.UTCNs-(rec.MonoNs-startMono)), true
		}
	}
	return time.Time{}, false
}

func patchMetaStart(dir string, start time.Time, from string) error {
	path := filepath.Join(dir, metaFileName)
	var m Meta
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	m.SessionDir = dir
	m.SessionStartUnixNs = start.UnixNano()
	m.StartTimeCorrected = true
	m.PromotedFrom = from
	return writeMetaTo(dir, m)
}

func datedDir(root string, t time.Time) string {
	return filepath.Join(root, t.Format(dateLayout), t.Format(sessionLayout))
}

// uniqueDir avoids colliding with an existing directory, which two sessions
// promoted to the same second would otherwise do.
func uniqueDir(base string) string {
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base
	}
	for i := 2; i < 100; i++ {
		candidate := fmt.Sprintf("%s_%d", base, i)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return base
}

// repointSymlink atomically makes link point at target. A plain Remove+Symlink
// would leave a window where the path does not resolve, and a recorder opening
// its next segment in that window would fail.
func repointSymlink(link, target string) error {
	tmp := link + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear temp symlink: %w", err)
	}
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("create temp symlink: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("swap symlink: %w", err)
	}
	return nil
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.Abs(a)
	rb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return ra == rb
}

// SortedNames is a small helper for tests and logging.
func SortedNames(dirs []string) []string {
	out := append([]string(nil), dirs...)
	sort.Strings(out)
	return out
}
