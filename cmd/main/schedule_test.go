package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"om1-telemetry/internal/control"
)

func writeScheduleFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schedule.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestSetupSchedule_emptyPath_isNoOp(t *testing.T) {
	ctl := control.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduleCancel := setupSchedule(ctx, "", ctl)
	defer scheduleCancel()

	select {
	case <-ctl.RecordingCmds:
		t.Fatal("an unset SCHEDULE_FILE must not start a reconciler")
	case <-time.After(50 * time.Millisecond):
	}
	require.True(t, ctl.Recording())
	require.True(t, ctl.Uploading())
}

func TestSetupSchedule_missingFile_logsAndIsNoOp(t *testing.T) {
	ctl := control.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduleCancel := setupSchedule(ctx, filepath.Join(t.TempDir(), "missing.yaml"), ctl)
	defer scheduleCancel()

	select {
	case <-ctl.RecordingCmds:
		t.Fatal("a schedule file that fails to load must fall back to no schedule")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRunSchedule_asksToStopRecordingOutsideItsWindow(t *testing.T) {
	// A window guaranteed not to contain "now", regardless of when this test runs.
	start := time.Now().Add(2 * time.Hour).Format("15:04")
	end := time.Now().Add(3 * time.Hour).Format("15:04")
	path := writeScheduleFile(t, "recording:\n  start: \""+start+"\"\n  end: \""+end+"\"\n")

	ctl := control.New() // Recording defaults true

	received := make(chan control.RecordingCmd, 1)
	go func() {
		cmd := <-ctl.RecordingCmds
		received <- cmd
		cmd.Result <- nil
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduleCancel := setupSchedule(ctx, path, ctl)
	defer scheduleCancel()

	select {
	case cmd := <-received:
		require.False(t, cmd.Start, "schedule should ask to stop recording outside its configured window")
	case <-time.After(time.Second):
		t.Fatal("expected an immediate reconcile on startup, not just on the next tick")
	}
	require.True(t, ctl.Uploading(), "upload has no schedule section here and must be left alone")
}

func TestRunSchedule_turnsUploadingOffOutsideItsWindow(t *testing.T) {
	start := time.Now().Add(2 * time.Hour).Format("15:04")
	end := time.Now().Add(3 * time.Hour).Format("15:04")
	path := writeScheduleFile(t, "upload:\n  start: \""+start+"\"\n  end: \""+end+"\"\n")

	ctl := control.New() // Uploading defaults true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduleCancel := setupSchedule(ctx, path, ctl)
	defer scheduleCancel()

	require.Eventually(t, func() bool { return !ctl.Uploading() }, time.Second, 5*time.Millisecond,
		"schedule should turn uploading off outside its configured window")

	select {
	case <-ctl.RecordingCmds:
		t.Fatal("recording has no schedule section here and must not receive a command")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRunSchedule_noOpWhenAlreadyMatchingState(t *testing.T) {
	// A zero-length window is always-on, matching ctl's default Recording()==true.
	path := writeScheduleFile(t, "recording:\n  start: \"00:00\"\n  end: \"00:00\"\n")

	ctl := control.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduleCancel := setupSchedule(ctx, path, ctl)
	defer scheduleCancel()

	select {
	case <-ctl.RecordingCmds:
		t.Fatal("schedule already matches the current state; must not send a command")
	case <-time.After(100 * time.Millisecond):
	}
}
