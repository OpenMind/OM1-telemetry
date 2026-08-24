package main

import (
	"context"
	"log/slog"
	"time"

	"om1-telemetry/internal/control"
	"om1-telemetry/internal/schedule"
)

// scheduleTickInterval is how often runSchedule checks for a state change.
const scheduleTickInterval = 30 * time.Second

// setupSchedule loads path and, if set, starts a background reconciler
// driving ctl to match it. A blank or unloadable path is a no-op, leaving
// recording and uploading on continuously.
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

// runSchedule reconciles ctl against cfg every scheduleTickInterval via the same calls the control API uses.
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

	reconcile()
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
