package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"om1-telemetry/internal/clock"
)

var datedPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}(_\d+)?$`)

// A robot whose clock is fine must behave exactly as it did before pending/
// existed: a plain dated directory, no symlink, nothing to promote.
func TestOpen_trustedClockUsesDatedDirectory(t *testing.T) {
	root := t.TempDir()
	clk := clock.NewWithSync(clock.SyncYes)

	s, err := Open(root, clk)
	require.NoError(t, err)

	require.False(t, s.Pending())
	require.Equal(t, s.RealDir(), s.Dir(), "no symlink indirection when nothing will be renamed")
	require.True(t, datedPattern.MatchString(filepath.Base(s.RealDir())), s.RealDir())

	_, err = os.Stat(filepath.Join(root, CurrentLinkName))
	require.True(t, os.IsNotExist(err), "no symlink should be created")
}

// SyncUnknown means the marker is not visible, which is what a deployment that
// does not mount /run/systemd/timesync looks like. It must not be treated as a
// bad clock, or every such session lands in pending/.
func TestOpen_unknownSyncIsTreatedAsTrusted(t *testing.T) {
	root := t.TempDir()

	s, err := Open(root, clock.NewWithSync(clock.SyncUnknown))
	require.NoError(t, err)

	require.False(t, s.Pending())
	require.True(t, datedPattern.MatchString(filepath.Base(s.RealDir())))
}

func TestOpen_untrustedClockUsesPendingDirAndSymlink(t *testing.T) {
	root := t.TempDir()

	s, err := Open(root, clock.NewWithSync(clock.SyncNo))
	require.NoError(t, err)

	require.True(t, s.Pending())
	require.Equal(t, filepath.Join(root, CurrentLinkName), s.Dir(),
		"recorders write through the symlink so the directory can move under them")
	require.Equal(t, PendingDirName, filepath.Base(filepath.Dir(s.RealDir())))
	require.False(t, datedPattern.MatchString(filepath.Base(s.RealDir())),
		"a pending directory must not be named from a clock nobody believes")

	target, err := os.Readlink(filepath.Join(root, CurrentLinkName))
	require.NoError(t, err)
	require.Equal(t, s.RealDir(), target)

	var meta Meta
	raw, err := os.ReadFile(filepath.Join(s.Dir(), metaFileName))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &meta))
	require.False(t, meta.ClockTrustedAtStart)
	require.Equal(t, "unsynced", meta.ClockSyncStateAtStart)
	require.NotZero(t, meta.SessionStartMonoNs)
}

// The whole point of the symlink: a recorder that opened a file before the
// rename keeps writing to it, and a recorder that opens one after the rename
// lands in the new directory. Neither notices.
func TestPromote_doesNotDisturbOpenOrFutureWriters(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, clock.NewWithSync(clock.SyncNo))
	require.NoError(t, err)
	pendingDir := s.RealDir()

	// A recorder that is already running.
	open, err := os.Create(filepath.Join(s.Dir(), "lidar_timestamps.csv"))
	require.NoError(t, err)
	defer func() { _ = open.Close() }()
	_, err = open.WriteString("before\n")
	require.NoError(t, err)

	require.NoError(t, s.Promote())

	require.False(t, s.Pending())
	require.NotEqual(t, pendingDir, s.RealDir())
	require.True(t, datedPattern.MatchString(filepath.Base(s.RealDir())), s.RealDir())

	// The already-open descriptor follows the inode.
	_, err = open.WriteString("after\n")
	require.NoError(t, err)
	require.NoError(t, open.Sync())

	content, err := os.ReadFile(filepath.Join(s.RealDir(), "lidar_timestamps.csv"))
	require.NoError(t, err)
	require.Equal(t, "before\nafter\n", string(content),
		"writes from both sides of the rename must land in the same file")

	// A new segment opened through the unchanged Dir() lands in the new place.
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir(), "segment2.mp4"), []byte("x"), 0o644))
	_, err = os.Stat(filepath.Join(s.RealDir(), "segment2.mp4"))
	require.NoError(t, err)

	_, err = os.Stat(pendingDir)
	require.True(t, os.IsNotExist(err), "the pending directory should be gone, not copied")

	var meta Meta
	raw, err := os.ReadFile(filepath.Join(s.Dir(), metaFileName))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &meta))
	require.True(t, meta.StartTimeCorrected)
	require.Equal(t, filepath.Base(pendingDir), meta.PromotedFrom)
}

func TestPromote_isIdempotent(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, clock.NewWithSync(clock.SyncNo))
	require.NoError(t, err)

	require.NoError(t, s.Promote())
	first := s.RealDir()
	require.NoError(t, s.Promote())
	require.Equal(t, first, s.RealDir())
}

// writePendingSession fakes a session an earlier run left behind.
func writePendingSession(t *testing.T, root, name string, journal []clock.Record) string {
	t.Helper()
	dir := filepath.Join(root, PendingDirName, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lidar_scans.bin"), []byte("data"), 0o644))

	f, err := os.Create(filepath.Join(dir, TimebaseFileName))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	for _, rec := range journal {
		raw, err := json.Marshal(rec)
		require.NoError(t, err)
		_, err = fmt.Fprintf(f, "%s\n", raw)
		require.NoError(t, err)
	}
	return dir
}

// The crash case: the clock synced and was journaled, but the process died
// before it could promote. The journal is enough to date it afterwards.
func TestJanitor_recoversSessionUsingItsJournal(t *testing.T) {
	root := t.TempDir()

	// Started 2 hours (on the monotonic clock) before NTP landed on a known
	// wall time, so the true start is that wall time minus 2 hours.
	syncWall := time.Date(2026, 7, 14, 9, 30, 0, 0, time.UTC)
	startMono := int64(30 * time.Second)
	syncMono := startMono + int64(2*time.Hour)

	dir := writePendingSession(t, root, "abcd1234_000030000", []clock.Record{
		{Kind: "start", MonoNs: startMono, UTCNs: time.Date(2026, 7, 10, 21, 52, 9, 0, time.UTC).UnixNano()},
		{Kind: "step", MonoNs: syncMono, UTCNs: syncWall.UnixNano(), Synced: true},
	})

	Janitor(root, "")

	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err), "the recovered session should have moved out of pending/")

	// Directory names are local time, as they have always been; the recovered
	// instant is what matters and is asserted below.
	trueStart := syncWall.Add(-2 * time.Hour).Local()
	recovered := filepath.Join(root, trueStart.Format(dateLayout), trueStart.Format(sessionLayout))
	data, err := os.ReadFile(filepath.Join(recovered, "lidar_scans.bin"))
	require.NoError(t, err, "session should be dated two hours before the sync")
	require.Equal(t, "data", string(data))

	var meta Meta
	raw, err := os.ReadFile(filepath.Join(recovered, metaFileName))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &meta))
	require.True(t, meta.StartTimeCorrected)
	require.Equal(t, syncWall.Add(-2*time.Hour).UnixNano(), meta.SessionStartUnixNs)
}

// A session that never saw a synchronized clock cannot be dated by anything --
// its monotonic timeline ended with that boot. Leaving it undated is the honest
// outcome; inventing a date would be worse.
func TestJanitor_leavesUndatableSessionAlone(t *testing.T) {
	root := t.TempDir()
	dir := writePendingSession(t, root, "beef5678_000030000", []clock.Record{
		{Kind: "start", MonoNs: 30, UTCNs: 1, Synced: false},
		{Kind: "sample", MonoNs: 60, UTCNs: 31, Synced: false},
	})

	Janitor(root, "")

	_, err := os.Stat(dir)
	require.NoError(t, err, "an undatable session must stay in pending/, not be guessed at")
}

func TestJanitor_skipsTheLiveSession(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, clock.NewWithSync(clock.SyncNo))
	require.NoError(t, err)

	// Give the live session a journal that would otherwise make it recoverable.
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir(), TimebaseFileName),
		[]byte(`{"kind":"start","mono_ns":1,"utc_ns":1}`+"\n"+
			`{"kind":"step","mono_ns":2,"utc_ns":1000000000,"synced":true}`+"\n"), 0o644))

	Janitor(root, s.RealDir())

	_, err = os.Stat(s.RealDir())
	require.NoError(t, err, "the janitor must not move the session being written right now")
}

func TestJanitor_noPendingDirIsNotAnError(t *testing.T) {
	require.NotPanics(t, func() { Janitor(t.TempDir(), "") })
}

// Two sessions promoted into the same second must not collide.
func TestUniqueDir_avoidsCollision(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "2026-07-14_07-30-00")
	require.Equal(t, base, uniqueDir(base))

	require.NoError(t, os.MkdirAll(base, 0o755))
	require.Equal(t, base+"_2", uniqueDir(base))
}

func TestRepointSymlink_isAtomicAndRepeatable(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	require.NoError(t, os.MkdirAll(a, 0o755))
	require.NoError(t, os.MkdirAll(b, 0o755))
	link := filepath.Join(root, CurrentLinkName)

	require.NoError(t, repointSymlink(link, a))
	target, err := os.Readlink(link)
	require.NoError(t, err)
	require.Equal(t, a, target)

	require.NoError(t, repointSymlink(link, b))
	target, err = os.Readlink(link)
	require.NoError(t, err)
	require.Equal(t, b, target)
}

// Rotation opens several trusted segments back to back; each must land in
// its own dated directory rather than colliding.
func TestOpenNext_trustedClockOpensDistinctDirectories(t *testing.T) {
	root := t.TempDir()
	clk := clock.NewWithSync(clock.SyncYes)

	first, err := Open(root, clk)
	require.NoError(t, err)
	time.Sleep(1100 * time.Millisecond) // dated dirs are second-granularity

	second, err := OpenNext(root, clk, clock.SyncYes)
	require.NoError(t, err)

	require.NotEqual(t, first.RealDir(), second.RealDir())
	require.True(t, datedPattern.MatchString(filepath.Base(second.RealDir())), second.RealDir())
}

// OpenNext must not reuse the same pending directory name across repeated
// calls in one run -- the naming used to be keyed on the process's fixed
// StartMonoNs, which every rotated segment would have shared.
func TestOpenNext_untrustedClockOpensDistinctPendingDirectories(t *testing.T) {
	root := t.TempDir()
	clk := clock.NewWithSync(clock.SyncNo)

	first, err := Open(root, clk)
	require.NoError(t, err)
	require.True(t, first.Pending())

	second, err := OpenNext(root, clk, clock.SyncNo)
	require.NoError(t, err)
	require.True(t, second.Pending())

	require.NotEqual(t, first.RealDir(), second.RealDir())
}

// Once the watcher has seen a sync, OpenNext must date the next segment
// directly even though the process itself booted untrusted.
func TestOpenNext_syncedAfterBootUsesDatedDirectory(t *testing.T) {
	root := t.TempDir()
	clk := clock.NewWithSync(clock.SyncNo)

	pending, err := Open(root, clk)
	require.NoError(t, err)
	require.True(t, pending.Pending())

	next, err := OpenNext(root, clk, clock.SyncYes)
	require.NoError(t, err)
	require.False(t, next.Pending())
	require.True(t, datedPattern.MatchString(filepath.Base(next.RealDir())), next.RealDir())
}

// Close records an end time distinct from the start, so a segment's true
// duration can be read back out of meta.json.
func TestClose_recordsEndTime(t *testing.T) {
	root := t.TempDir()
	clk := clock.NewWithSync(clock.SyncYes)

	s, err := Open(root, clk)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	require.NoError(t, s.Close())

	raw, err := os.ReadFile(filepath.Join(s.RealDir(), metaFileName))
	require.NoError(t, err)
	var m Meta
	require.NoError(t, json.Unmarshal(raw, &m))

	require.NotZero(t, m.SessionEndUnixNs)
	require.Greater(t, m.SessionEndUnixNs, m.SessionStartUnixNs)
	require.NotZero(t, m.SessionEndMonoNs)
}

// A session promoted after being rotated away from (no longer "current")
// must date itself from when *it* started, not from process boot -- the bug
// this guards against: reusing clk.StartWallNsNow, which always answers for
// process boot, would misdate every segment after the first.
func TestPromote_datesFromThisSessionsOwnStart(t *testing.T) {
	root := t.TempDir()
	clk := clock.NewWithSync(clock.SyncNo)

	first, err := Open(root, clk)
	require.NoError(t, err)
	require.True(t, first.Pending())

	time.Sleep(50 * time.Millisecond)

	second, err := OpenNext(root, clk, clock.SyncNo)
	require.NoError(t, err)
	require.True(t, second.Pending())
	secondPending := second.RealDir()

	// first stays pending forever (nothing promotes it -- see OpenNext's doc
	// comment); only second, the "current" segment, is promoted here.
	require.NoError(t, second.Promote())
	require.False(t, second.Pending())
	require.NotEqual(t, secondPending, second.RealDir())

	raw, err := os.ReadFile(filepath.Join(second.RealDir(), metaFileName))
	require.NoError(t, err)
	var m Meta
	require.NoError(t, json.Unmarshal(raw, &m))
	require.True(t, m.StartTimeCorrected)

	// The written mono anchor must be *second's own* startMonoNs, not
	// first's/the process's -- this is the precise, deterministic form of the
	// bug this test guards against (the wall-clock value alone is too close
	// to "now" either way, over a 50ms gap, to tell the two formulas apart).
	require.Equal(t, second.startMonoNs, m.SessionStartMonoNs)
	require.NotEqual(t, first.startMonoNs, second.startMonoNs)

	gotStart := time.Unix(0, m.SessionStartUnixNs)
	require.WithinDuration(t, time.Now(), gotStart, 2*time.Second)
}
