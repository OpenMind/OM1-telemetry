package retention

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
	require.False(t, IsUploaded(dir))

	MarkUploaded(dir)
	require.True(t, IsUploaded(dir))
}

func TestDirSize_sumsFilesRecursively(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.bin", make([]byte, 10))
	writeFile(t, filepath.Join(dir, "sub"), "b.bin", make([]byte, 5))

	got, err := DirSize(dir)
	require.NoError(t, err)
	require.EqualValues(t, 15, got)
}

func TestEnforceRetentionCap_deletesOldestUploadedFirstUntilUnderCap(t *testing.T) {
	root := t.TempDir()
	oldest := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	middle := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")
	newest := filepath.Join(root, "2026-08-16", "2026-08-16_00-00-00")

	writeFile(t, oldest, "a.bin", make([]byte, 100))
	MarkUploaded(oldest)
	writeFile(t, middle, "b.bin", make([]byte, 100))
	MarkUploaded(middle)
	writeFile(t, newest, "c.bin", make([]byte, 100))
	MarkUploaded(newest)

	middleSize, err := DirSize(middle)
	require.NoError(t, err)
	newestSize, err := DirSize(newest)
	require.NoError(t, err)

	EnforceRetentionCap(root, []string{oldest, middle, newest}, func(string) bool { return false }, middleSize+newestSize)

	require.NoDirExists(t, oldest, "oldest uploaded dir must go first to free space")
	require.DirExists(t, middle, "stops as soon as the cap is satisfied")
	require.DirExists(t, newest)
}

func TestEnforceRetentionCap_fallsThroughToUnuploadedWhenNothingElseCanFreeEnough(t *testing.T) {
	root := t.TempDir()
	notUploaded := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")

	writeFile(t, notUploaded, "a.bin", make([]byte, 200))

	EnforceRetentionCap(root, []string{notUploaded}, func(string) bool { return false }, 10)

	require.NoDirExists(t, notUploaded, "the cap must be honored even with no uploaded data to fall back on")
}

func TestEnforceRetentionCap_neverDeletesProtected(t *testing.T) {
	root := t.TempDir()
	protectedDir := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")

	writeFile(t, protectedDir, "b.bin", make([]byte, 200))
	MarkUploaded(protectedDir)

	EnforceRetentionCap(root, []string{protectedDir}, func(dir string) bool { return true }, 10)

	require.DirExists(t, protectedDir, "must never delete a directory the caller marked protected, cap or no cap")
}

func TestEnforceRetentionCap_prefersUploadedOverOlderUnuploadedWhenSufficient(t *testing.T) {
	root := t.TempDir()
	olderUnuploaded := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	newerUploaded := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")

	writeFile(t, olderUnuploaded, "a.bin", make([]byte, 200))
	writeFile(t, newerUploaded, "b.bin", make([]byte, 200))
	MarkUploaded(newerUploaded)

	newerSize, err := DirSize(newerUploaded)
	require.NoError(t, err)

	EnforceRetentionCap(root, []string{olderUnuploaded, newerUploaded},
		func(string) bool { return false }, newerSize)

	require.NoDirExists(t, newerUploaded, "the uploaded directory should be freed first")
	require.DirExists(t, olderUnuploaded, "the older but unconfirmed directory is spared once the cap is satisfied")
}

func TestEnforceRetentionCap_prefersUploadedButDeletesOldestOverallIfNeeded(t *testing.T) {
	root := t.TempDir()
	oldestUnuploaded := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	newerUploaded := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")

	writeFile(t, oldestUnuploaded, "a.bin", make([]byte, 200))
	writeFile(t, newerUploaded, "b.bin", make([]byte, 200))
	MarkUploaded(newerUploaded)

	EnforceRetentionCap(root, []string{oldestUnuploaded, newerUploaded}, func(string) bool { return false }, 10)

	require.NoDirExists(t, oldestUnuploaded)
	require.NoDirExists(t, newerUploaded)
}

func TestEnforceRetentionCap_zeroMaxBytesDisablesEnforcement(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	writeFile(t, dir, "a.bin", make([]byte, 1000))
	MarkUploaded(dir)

	EnforceRetentionCap(root, []string{dir}, func(string) bool { return false }, 0)

	require.DirExists(t, dir)
}

func TestSweep_nilUploaderSkipsCatchUpButStillEnforcesCap(t *testing.T) {
	root := t.TempDir()
	underCap := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	writeFile(t, underCap, "a.bin", []byte("data"))

	Sweep(nil, root, filepath.Join(root, "boot_timebase.jsonl"), "", 100)

	require.False(t, IsUploaded(underCap), "without an uploader nothing can be marked uploaded")
	require.DirExists(t, underCap, "comfortably under the cap, so nothing needs to be deleted")

	overCap := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")
	writeFile(t, overCap, "b.bin", make([]byte, 200))

	Sweep(nil, root, filepath.Join(root, "boot_timebase.jsonl"), "", 100)

	require.NoDirExists(t, underCap,
		"with no uploader configured, cap enforcement must still delete the oldest directory rather than let the disk fill up")
}

// minimalFakeAPI is a minimal openmind-api stand-in for exercising catch-up/cap-enforcement end to end.
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

func TestSweep_uploadsOldestUnmarkedFirstAndSkipsProtectedAndAlreadyUploaded(t *testing.T) {
	api, apiURL := newMinimalFakeAPI(t)
	client := upload.New(upload.Config{BaseURL: apiURL, APIKey: "k"})

	root := t.TempDir()
	oldest := filepath.Join(root, "2026-08-14", "2026-08-14_00-00-00")
	alreadyDone := filepath.Join(root, "2026-08-15", "2026-08-15_00-00-00")
	live := filepath.Join(root, "2026-08-16", "2026-08-16_00-00-00")

	writeFile(t, oldest, "meta.json", []byte(`{}`))
	writeFile(t, alreadyDone, "meta.json", []byte(`{}`))
	MarkUploaded(alreadyDone)
	writeFile(t, live, "meta.json", []byte(`{}`))

	bootTimebasePath := filepath.Join(root, "does-not-exist.jsonl") // no boot session protection in play

	Sweep(client, root, bootTimebasePath, live, 0)

	require.True(t, IsUploaded(oldest), "the oldest not-yet-uploaded closed dir must get uploaded")
	require.False(t, IsUploaded(live), "the currently-open session must never be swept")

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Equal(t, 1, api.createCalls[APISessionDir(root, oldest)])
	require.Zero(t, api.createCalls[APISessionDir(root, alreadyDone)],
		"a session already marked uploaded must not be re-uploaded")
	require.Zero(t, api.createCalls[APISessionDir(root, live)])
}

// Cap enforcement must not block on a stuck catch-up upload.
func TestRunSweeps_capEnforcementNotBlockedByStuckCatchUpUpload(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.RetentionConfig{MaxBytes: 10, SweepInterval: sweepInterval}

	done := make(chan struct{})
	go func() {
		RunSweeps(ctx, client, root, filepath.Join(root, "missing-boot.jsonl"), func() string { return "" }, cfg, control.New())
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
		t.Fatal("RunSweeps did not shut down after ctx cancellation")
	}
}

// Disabling uploading must not stop RETENTION_MAX_BYTES enforcement.
func TestRunSweeps_uploadDisabled_skipsCatchUpButStillEnforcesCap(t *testing.T) {
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
		RunSweeps(ctx, client, root, filepath.Join(root, "missing-boot.jsonl"), func() string { return "" }, cfg, ctl)
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
		t.Fatal("RunSweeps did not shut down after ctx cancellation")
	}
}

// TriggerUpload must cause an immediate sweep, not wait for the next tick.
func TestRunSweeps_uploadTrigger_runsImmediateSweep(t *testing.T) {
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
		RunSweeps(ctx, client, root, filepath.Join(root, "missing-boot.jsonl"), func() string { return "" }, cfg, ctl)
		close(done)
	}()

	ctl.TriggerUpload()

	require.Eventually(t, func() bool { return hit.Load() }, 500*time.Millisecond, 5*time.Millisecond,
		"TriggerUpload must cause an immediate catch-up sweep without waiting for the long tick interval")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunSweeps did not shut down after ctx cancellation")
	}
}
