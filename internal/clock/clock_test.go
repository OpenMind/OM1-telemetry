package clock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMonoNs_isNonZeroAndMonotonic(t *testing.T) {
	a := MonoNs()
	require.NotZero(t, a, "CLOCK_BOOTTIME should be readable on Linux")
	time.Sleep(2 * time.Millisecond)
	require.Greater(t, MonoNs(), a)
}

func TestSyncState_trusted(t *testing.T) {
	require.True(t, SyncYes.Trusted())
	require.True(t, SyncUnknown.Trusted(),
		"no evidence must not be read as bad evidence, or every session lands in pending/")
	require.False(t, SyncNo.Trusted())
}

// StartWallNsNow carries the session start forward along the monotonic
// timeline, which is what makes a corrected date recoverable.
func TestStartWallNsNow_tracksTheMonotonicTimeline(t *testing.T) {
	c := NewWithSync(SyncYes)
	time.Sleep(5 * time.Millisecond)

	got := c.StartWallNsNow()
	require.InDelta(t, c.StartWallNs(), got, float64(50*time.Millisecond),
		"with a stable clock the recomputed start equals the recorded one")
}

func readJournal(t *testing.T, path string) []Record {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var out []Record
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec Record
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		out = append(out, rec)
	}
	return out
}

// Every session gets an anchor immediately, even a wrong one: without a start
// record there is nothing to carry a later correction back to.
func TestWatcher_writesStartRecordImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), TimebaseName)
	c := NewWithSync(SyncNo)
	w := NewWatcher(c, path, nil)
	w.syncFn = func() SyncState { return SyncNo }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()

	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(path)
		return err == nil && len(raw) > 0
	}, time.Second, 10*time.Millisecond)

	cancel()
	<-done

	recs := readJournal(t, path)
	require.NotEmpty(t, recs)
	require.Equal(t, "start", recs[0].Kind)
	require.NotZero(t, recs[0].MonoNs)
	require.NotZero(t, recs[0].UTCNs)
	require.False(t, recs[0].Synced)
}

// A wall clock that moves further than the monotonic clock over the same
// interval was stepped. That is the NTP correction this whole package exists to
// capture.
func TestWatcher_detectsAStepAndJournalsTheOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), TimebaseName)
	c := NewWithSync(SyncNo)
	w := NewWatcher(c, path, nil)
	w.syncFn = func() SyncState { return SyncNo }

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	w.file = f
	defer func() { _ = f.Close() }()

	// Pretend the previous sample was taken with a wall clock four days behind:
	// the next tick sees wall advance far more than monotonic did.
	wall, mono := c.Now()
	const backBy = 4 * 24 * time.Hour
	w.lastWall = wall - int64(backBy) - int64(time.Second)
	w.lastMono = mono - int64(time.Second)

	w.tick(false)

	recs := readJournal(t, path)
	require.Len(t, recs, 1)
	require.Equal(t, "step", recs[0].Kind)
	require.InDelta(t, float64(backBy), float64(recs[0].StepNs), float64(time.Second),
		"the journaled step is the offset every earlier row needs")
	require.NotZero(t, recs[0].UTCBeforeNs)
}

// NTP's ordinary frequency discipline is parts-per-million. Journaling that as
// a step would litter the file and make corrections ambiguous.
func TestWatcher_ignoresSlew(t *testing.T) {
	path := filepath.Join(t.TempDir(), TimebaseName)
	c := NewWithSync(SyncNo)
	w := NewWatcher(c, path, nil)
	w.syncFn = func() SyncState { return SyncNo }

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	w.file = f
	defer func() { _ = f.Close() }()

	wall, mono := c.Now()
	w.lastWall = wall - int64(time.Second) - int64(time.Millisecond)
	w.lastMono = mono - int64(time.Second)

	w.tick(false)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(string(raw)), "a 1 ms divergence is drift, not a step")
}

// A periodic anchor lets a reader interpolate across the session instead of
// assuming a single offset holds for hours.
func TestWatcher_forceSampleWritesAnAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), TimebaseName)
	c := NewWithSync(SyncYes)
	w := NewWatcher(c, path, nil)
	w.syncFn = func() SyncState { return SyncNo }

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	w.file = f
	defer func() { _ = f.Close() }()

	w.lastWall, w.lastMono = c.Now()
	w.tick(true)

	recs := readJournal(t, path)
	require.Len(t, recs, 1)
	require.Equal(t, "sample", recs[0].Kind)
}

// A step with no NTP confirmation -- someone ran `date` -- is worth recording
// but must not trigger a rename to a date nothing has vouched for.
func TestWatcher_unconfirmedStepDoesNotTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), TimebaseName)
	c := NewWithSync(SyncNo)

	var fired int
	w := NewWatcher(c, path, func(Record) { fired++ })
	w.syncFn = func() SyncState { return SyncNo }

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	w.file = f
	defer func() { _ = f.Close() }()

	w.maybeTrust(Record{Kind: "step", Synced: false})
	require.Zero(t, fired, "an unconfirmed step must not date a session")

	w.maybeTrust(Record{Kind: "step", Synced: true})
	require.Equal(t, 1, fired)

	w.maybeTrust(Record{Kind: "step", Synced: true})
	require.Equal(t, 1, fired, "the callback fires once per session")
}

// The clock can become trustworthy without stepping, when it was already close
// enough that NTP only slewed it. The session still needs dating.
func TestWatcher_syncWithoutAStepStillTrusts(t *testing.T) {
	path := filepath.Join(t.TempDir(), TimebaseName)
	c := NewWithSync(SyncNo)

	var fired int
	w := NewWatcher(c, path, func(Record) { fired++ })
	w.syncFn = func() SyncState { return SyncYes }

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	w.file = f
	defer func() { _ = f.Close() }()

	w.lastWall, w.lastMono = c.Now()
	w.tick(false)

	require.Equal(t, 1, fired)
	recs := readJournal(t, path)
	require.Len(t, recs, 1)
	require.Equal(t, "sync", recs[0].Kind)
	require.True(t, recs[0].Synced)
}

func TestShortBootID(t *testing.T) {
	c := NewWithSync(SyncYes)
	id := c.ShortBootID()
	require.NotEmpty(t, id)
	require.NotContains(t, id, "-", "the id goes into a directory name")
}
