package main

import (
	"context"
	"log/slog"
	"time"

	"om1-telemetry/internal/control"
	"om1-telemetry/internal/schedule"
)

// scheduleTickInterval is how often runSchedule checks whether recording
// or uploading should change state. Coarse enough to be cheap, fine
// enough that a scheduled transition takes effect within a minute of its
// boundary.
const scheduleTickInterval = 30 * time.Second

// setupSchedule loads path (see config.ScheduleFile) and, if it names a
// file, starts a background reconciler that drives ctl to match it.
//
// A blank path is the default, "no schedule" case: setupSchedule does
// nothing and returns a no-op cancel func, so recording and uploading
// stay on continuously exactly as they did before this feature existed.
// A file that fails to load or parse logs an error and also falls back to
// no schedule -- a bad config should fail open into recording everything,
// not crash the recorder or block it from starting.
func setupSchedule(ctx context.Context, path string, ctl *control.State) context.CancelFunc {
	if path == "" {
		return func() {}
	}

	cfg, err := schedule.Load(path)
	if err != nil {
		slog.Error("could not load schedule; recording and uploading stay on continuously",
			"path", path, "err", err)
		return func() {}
	}

	slog.Info("schedule loaded", "path", path)
	scheduleCtx, cancel := context.WithCancel(ctx)
	go runSchedule(scheduleCtx, cfg, ctl)
	return cancel
}

// runSchedule reconciles ctl's recording/uploading state against cfg every
// scheduleTickInterval, driving it through the same calls the control API
// itself uses -- SendRecordingCmd and SetUploading -- so a schedule is
// just another caller of control.State, not a special case main's event
// loop needs to know about.
//
// This is a simple periodic reconciler, not a lock on the control API: a
// manual curl between ticks is not overridden until the next scheduled
// transition.
func runSchedule(ctx context.Context, cfg *schedule.Config, ctl *control.State) {
	reconcile := func() {
		now := time.Now()

		if want := cfg.Recording.Active(now); want != ctl.Recording() {
			cmdCtx, cancel := context.WithTimeout(ctx, scheduleTickInterval)
			err := ctl.SendRecordingCmd(cmdCtx, want)
			cancel()
			if err != nil {
				slog.Error("schedule: could not change recording state", "want", want, "err", err)
			}
		}

		if want := cfg.Upload.Active(now); want != ctl.Uploading() {
			ctl.SetUploading(want)
			if want {
				ctl.TriggerUpload()
			}
		}
	}

	reconcile() // apply the schedule immediately at startup, not after the first tick
	ticker := time.NewTicker(scheduleTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}
