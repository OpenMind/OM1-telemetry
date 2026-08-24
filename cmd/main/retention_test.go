package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"om1-telemetry/config"
	"om1-telemetry/internal/control"
	"om1-telemetry/internal/upload"
)

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
}

func TestMarkUploaded_isUploadedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	require.False(t, isUploaded(dir))

	markUploaded(dir)
	require.True(t, isUploaded(dir))
}

func TestDirSize_sumsFilesRecursively(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.bin", make([]byte, 10))
	writeFile(t, filepath.Join(dir, "sub"), "b.bin", make([]byte, 5))

	got, err := dirSize(dir)
	require.NoError(t, err)
	require.EqualValues(t, 15, got)
}

func TestEnforceRetentionCap_deletesOldestUploadedFirstUntilUnderCap(t *testing.T) {
	root := t.TempDir()
	oldest := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	middle := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")
	newest := filepath.Join(root, "2026-08-16", "2026-08-16_00-00-00")

	writeFile(t, oldest, "a.bin", make([]byte, 100))
	markUploaded(oldest)
	writeFile(t, middle, "b.bin", make([]byte, 100))
	markUploaded(middle)
	writeFile(t, newest, "c.bin", make([]byte, 100))
	markUploaded(newest)

	// Exactly enough room for middle+newest, so deleting just oldest must
	// satisfy the cap -- computed rather than hand-counted so the marker
	// file's own (small, incidental) size can't throw the arithmetic off.
	middleSize, err := dirSize(middle)
	require.NoError(t, err)
	newestSize, err := dirSize(newest)
	require.NoError(t, err)

	enforceRetentionCap(root, []string{oldest, middle, newest}, func(string) bool { return false }, middleSize+newestSize)

	require.NoDirExists(t, oldest, "oldest uploaded dir must go first to free space")
	require.DirExists(t, middle, "stops as soon as the cap is satisfied")
	require.DirExists(t, newest)
}

// The 100GB cap is a hard requirement: if there's nothing already-uploaded
// left to delete, the oldest directory must still go, even unuploaded, so
// the recorder never fills the disk and stops recording entirely.
func TestEnforceRetentionCap_fallsThroughToUnuploadedWhenNothingElseCanFreeEnough(t *testing.T) {
	root := t.TempDir()
	notUploaded := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")

	writeFile(t, notUploaded, "a.bin", make([]byte, 200))
	// deliberately not marked uploaded -- and nothing else exists to delete instead

	enforceRetentionCap(root, []string{notUploaded}, func(string) bool { return false }, 10)

	require.NoDirExists(t, notUploaded, "the cap must be honored even with no uploaded data to fall back on")
}

func TestEnforceRetentionCap_neverDeletesProtected(t *testing.T) {
	root := t.TempDir()
	protectedDir := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")

	writeFile(t, protectedDir, "b.bin", make([]byte, 200))
	markUploaded(protectedDir)

	enforceRetentionCap(root, []string{protectedDir}, func(dir string) bool { return true }, 10)

	require.DirExists(t, protectedDir, "must never delete a directory the caller marked protected, cap or no cap")
}

// When an uploaded directory alone is enough to satisfy the cap, it is
// deleted in preference to an older not-yet-uploaded one -- upload status
// beats raw age as long as it's sufficient to stay under the cap.
func TestEnforceRetentionCap_prefersUploadedOverOlderUnuploadedWhenSufficient(t *testing.T) {
	root := t.TempDir()
	olderUnuploaded := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	newerUploaded := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")

	writeFile(t, olderUnuploaded, "a.bin", make([]byte, 200))
	writeFile(t, newerUploaded, "b.bin", make([]byte, 200))
	markUploaded(newerUploaded)

	newerSize, err := dirSize(newerUploaded)
	require.NoError(t, err)

	enforceRetentionCap(root, []string{olderUnuploaded, newerUploaded},
		func(string) bool { return false }, newerSize) // deleting only the uploaded one is enough

	require.NoDirExists(t, newerUploaded, "the uploaded directory should be freed first")
	require.DirExists(t, olderUnuploaded, "the older but unconfirmed directory is spared once the cap is satisfied")
}

// Uploaded directories are preferred, but only when they're enough on their
// own to satisfy the cap -- age still wins once upload status can't decide.
func TestEnforceRetentionCap_prefersUploadedButDeletesOldestOverallIfNeeded(t *testing.T) {
	root := t.TempDir()
	oldestUnuploaded := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	newerUploaded := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")

	writeFile(t, oldestUnuploaded, "a.bin", make([]byte, 200))
	writeFile(t, newerUploaded, "b.bin", make([]byte, 200))
	markUploaded(newerUploaded)

	// Cap tight enough that deleting either alone isn't enough -- both must go.
	enforceRetentionCap(root, []string{oldestUnuploaded, newerUploaded}, func(string) bool { return false }, 10)

	require.NoDirExists(t, oldestUnuploaded)
	require.NoDirExists(t, newerUploaded)
}

func TestEnforceRetentionCap_zeroMaxBytesDisablesEnforcement(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	writeFile(t, dir, "a.bin", make([]byte, 1000))
	markUploaded(dir)

	enforceRetentionCap(root, []string{dir}, func(string) bool { return false }, 0)

	require.DirExists(t, dir)
}

func TestRetentionSweep_nilUploaderSkipsCatchUpButStillEnforcesCap(t *testing.T) {
	root := t.TempDir()
	underCap := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	writeFile(t, underCap, "a.bin", []byte("data"))

	retentionSweep(nil, root, filepath.Join(root, "boot_timebase.jsonl"), "", 100)

	require.False(t, isUploaded(underCap), "without an uploader nothing can be marked uploaded")
	require.DirExists(t, underCap, "comfortably under the cap, so nothing needs to be deleted")

	overCap := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")
	writeFile(t, overCap, "b.bin", make([]byte, 200))

	retentionSweep(nil, root, filepath.Join(root, "boot_timebase.jsonl"), "", 100)

	require.NoDirExists(t, underCap,
		"with no uploader configured, cap enforcement must still delete the oldest directory rather than let the disk fill up")
}

// minimalFakeAPI is a smaller stand-in than internal/upload's own test fake:
// just enough of the openmind-api surface for a small, single-file session
// upload to succeed, so retentionSweep's catch-up/cap-enforcement behavior
// can be exercised end to end without duplicating internal/upload's whole
// suite here.
type minimalFakeAPI struct {
	mu          sync.Mutex
	createCalls map[string]int // session_dir -> number of "create session" calls
}

func newMinimalFakeAPI(t *testing.T) (*minimalFakeAPI, string) {
	api := &minimalFakeAPI{createCalls: map[string]int{}}
	mux := http.NewServeMux()

	var s3URL string
	mux.HandleFunc("/data/collection/sessions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionDir string `json:"session_dir"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		api.mu.Lock()
		api.createCalls[body.SessionDir]++
		api.mu.Unlock()

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": "sess-" + body.SessionDir,
			"s3_prefix":  "prefix/",
			"status":     "uploading",
			"upload": map[string]any{
				"url":    s3URL + "/",
				"fields": map[string]string{},
			},
		})
	})
	mux.HandleFunc("/data/collection/sessions/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "ok"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s3URL = srv.URL
	return api, srv.URL
}

func TestRetentionSweep_uploadsOldestUnmarkedFirstAndSkipsProtectedAndAlreadyUploaded(t *testing.T) {
	api, apiURL := newMinimalFakeAPI(t)
	client := upload.New(upload.Config{BaseURL: apiURL, APIKey: "k"})

	root := t.TempDir()
	oldest := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	alreadyDone := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")
	live := filepath.Join(root, "2026-08-16", "2026-08-16_00-00-00")

	writeFile(t, oldest, "meta.json", []byte(`{}`))
	writeFile(t, alreadyDone, "meta.json", []byte(`{}`))
	markUploaded(alreadyDone)
	writeFile(t, live, "meta.json", []byte(`{}`))

	bootTimebasePath := filepath.Join(root, "does-not-exist.jsonl") // no boot session protection in play

	retentionSweep(client, root, bootTimebasePath, live, 0)

	require.True(t, isUploaded(oldest), "the oldest not-yet-uploaded closed dir must get uploaded")
	require.False(t, isUploaded(live), "the currently-open session must never be swept")

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Equal(t, 1, api.createCalls[apiSessionDir(root, oldest)])
	require.Zero(t, api.createCalls[apiSessionDir(root, alreadyDone)],
		"a session already marked uploaded must not be re-uploaded")
	require.Zero(t, api.createCalls[apiSessionDir(root, live)])
}

// TestRunRetentionSweeps_capEnforcementNotBlockedByStuckCatchUpUpload
// reproduces the scenario that let the recordings cap go unenforced for
// hours in production: a large catch-up backlog plus a bad network, where
// retentionSweep's single combined call could spend its whole time stuck in
// the upload loop and never reach enforceRetentionCap. runRetentionSweeps
// now ticks the two on separate goroutines specifically so this can't
// happen -- this test makes the create-session call for the oldest
// directory slow (longer than several sweep intervals, so catch-up can't
// even finish its first attempt in time) and asserts a different, over-cap
// directory still gets deleted promptly regardless.
//
// The delay is real wall-clock time, not context cancellation: uploadSession
// runs each attempt on its own independent timeout unrelated to
// runRetentionSweeps' ctx (see uploadSession in main.go), so this test can't
// rely on canceling ctx to unblock a hung HTTP call -- it has to actually
// let the slow response land.
func TestRunRetentionSweeps_capEnforcementNotBlockedByStuckCatchUpUpload(t *testing.T) {
	const sweepInterval = 20 * time.Millisecond
	const slowUpload = 300 * time.Millisecond // several sweep intervals

	mux := http.NewServeMux()
	mux.HandleFunc("/data/collection/sessions", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(slowUpload)
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := upload.New(upload.Config{BaseURL: srv.URL, APIKey: "k"})

	root := t.TempDir()
	slow := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	writeFile(t, slow, "meta.json", []byte(`{}`))
	overCap := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")
	writeFile(t, overCap, "b.bin", make([]byte, 200))
	// Neither directory is marked uploaded or protected: catch-up will pick
	// up "slow" first (oldest first) and be stuck on its slow response for
	// several sweep intervals, never reaching "overCap" in that time -- so
	// if cap enforcement deletes "overCap" before "slow"'s attempt even
	// returns, it can only be the independent enforcement ticker doing it.

	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.RetentionConfig{MaxBytes: 10, SweepInterval: sweepInterval}

	done := make(chan struct{})
	go func() {
		runRetentionSweeps(ctx, client, root, filepath.Join(root, "missing-boot.jsonl"), func() string { return "" }, cfg, control.New())
		close(done)
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(overCap)
		return os.IsNotExist(err)
	}, slowUpload/2, 5*time.Millisecond,
		"cap enforcement must delete the over-cap directory well before the stuck catch-up upload even returns")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRetentionSweeps did not shut down after ctx cancellation")
	}
}

// Disabling uploading must not stop RETENTION_MAX_BYTES enforcement.
func TestRunRetentionSweeps_uploadDisabled_skipsCatchUpButStillEnforcesCap(t *testing.T) {
	const sweepInterval = 20 * time.Millisecond

	var hit atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/data/collection/sessions", func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := upload.New(upload.Config{BaseURL: srv.URL, APIKey: "k"})

	root := t.TempDir()
	notUploaded := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	writeFile(t, notUploaded, "meta.json", []byte(`{}`))
	overCap := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")
	writeFile(t, overCap, "b.bin", make([]byte, 200))

	ctl := control.New()
	ctl.SetUploading(false)

	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.RetentionConfig{MaxBytes: 10, SweepInterval: sweepInterval}

	done := make(chan struct{})
	go func() {
		runRetentionSweeps(ctx, client, root, filepath.Join(root, "missing-boot.jsonl"), func() string { return "" }, cfg, ctl)
		close(done)
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(overCap)
		return os.IsNotExist(err)
	}, time.Second, 5*time.Millisecond,
		"cap enforcement must still delete over-cap directories while uploading is disabled")

	time.Sleep(5 * sweepInterval) // several ticks' worth of chances to (wrongly) call out
	require.False(t, hit.Load(), "catch-up uploads must not run while uploading is disabled")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRetentionSweeps did not shut down after ctx cancellation")
	}
}

// POST /upload/start must trigger an immediate sweep, not wait for the next tick.
func TestRunRetentionSweeps_uploadTrigger_runsImmediateSweep(t *testing.T) {
	const longSweepInterval = 2 * time.Second // must not fire on its own during this test

	var hit atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/data/collection/sessions", func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := upload.New(upload.Config{BaseURL: srv.URL, APIKey: "k"})

	root := t.TempDir()
	notUploaded := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	writeFile(t, notUploaded, "meta.json", []byte(`{}`))

	ctl := control.New()

	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.RetentionConfig{MaxBytes: 1 << 30, SweepInterval: longSweepInterval}

	done := make(chan struct{})
	go func() {
		runRetentionSweeps(ctx, client, root, filepath.Join(root, "missing-boot.jsonl"), func() string { return "" }, cfg, ctl)
		close(done)
	}()

	ctl.TriggerUpload()

	require.Eventually(t, func() bool { return hit.Load() }, 500*time.Millisecond, 5*time.Millisecond,
		"TriggerUpload must cause an immediate catch-up sweep without waiting for the long tick interval")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRetentionSweeps did not shut down after ctx cancellation")
	}
}
