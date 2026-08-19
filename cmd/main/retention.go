package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"om1-telemetry/config"
	"om1-telemetry/internal/session"
	"om1-telemetry/internal/upload"
)

// uploadMarkerName marks a session directory whose files have already been
// uploaded successfully -- written once by uploadSession, right after
// UploadSession returns nil. It is how retentionSweep tells "not yet
// uploaded" apart from "uploaded, just not deleted yet" without asking the
// API again for every directory on every sweep. It is a dotfile so
// upload.regularFiles never mistakes it for session data.
const uploadMarkerName = ".uploaded"

// retentionSweep is the backstop for the two upload call sites in main's
// event loop (rotation and shutdown), which only ever handle the segment
// that just closed and never retry a failed one. Each tick it:
//
//  1. Walks every dated session directory, oldest first, and uploads any
//     that were never marked uploaded -- catching up anything a prior
//     failed/partial upload, a crash, or a manual copy left behind.
//  2. If recordingsDir is still over maxBytes afterward, deletes already-
//     uploaded directories, oldest first, until it isn't.
//
// It never touches currentDir (the segment recorders are writing to right
// now) or the directory still holding the live boot-relative clock journal
// (see bootSessionDir) -- both are still changing, so neither "uploaded" nor
// "safe to delete" can be true of them yet.
//
// Marking a directory uploaded and deleting it are deliberately separate
// steps gated on the marker, not on one another: a directory can be
// uploaded and kept (DELETE_AFTER_UPLOAD=false, comfortably under the cap)
// for as long as there's room, and is only ever removed once its data is
// confirmed to have a copy in S3.
func retentionSweep(uploader *upload.Client, recordingsDir, bootTimebasePath, currentDir string, maxBytes int64) {
	if uploader == nil {
		return // nothing can be safely deleted without a place to send it first
	}

	dirs, err := session.ListClosed(recordingsDir)
	if err != nil {
		slog.Warn("retention: cannot list session directories", "dir", recordingsDir, "err", err)
		return
	}

	protected := func(dir string) bool {
		return dir == currentDir || bootSessionDir(bootTimebasePath, dir)
	}

	for _, dir := range dirs {
		if protected(dir) || isUploaded(dir) {
			continue
		}
		opts := uploadOptions(bootTimebasePath, dir)
		uploadSession(uploader, dir, apiSessionDir(recordingsDir, dir), readStartedAt(dir), false, opts)
	}

	enforceRetentionCap(recordingsDir, dirs, protected, maxBytes)
}

// enforceRetentionCap deletes already-uploaded directories from dirs, oldest
// first, until recordingsDir's total size is at or under maxBytes.
// Directories that aren't marked uploaded, or that protected reports true
// for, are left alone even if that means staying over the cap -- losing
// data no copy exists of yet is worse than a full disk.
func enforceRetentionCap(recordingsDir string, dirs []string, protected func(string) bool, maxBytes int64) {
	if maxBytes <= 0 {
		return
	}
	total, err := dirSize(recordingsDir)
	if err != nil {
		slog.Warn("retention: cannot measure recordings directory size", "dir", recordingsDir, "err", err)
		return
	}

	for _, dir := range dirs {
		if total <= maxBytes {
			return
		}
		if protected(dir) || !isUploaded(dir) {
			continue
		}
		freed, err := dirSize(dir)
		if err != nil {
			slog.Warn("retention: cannot measure session directory size", "dir", dir, "err", err)
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("retention: could not delete uploaded session directory", "dir", dir, "err", err)
			continue
		}
		total -= freed
		slog.Info("retention: deleted uploaded session directory to stay under the recordings cap",
			"dir", dir, "freed_bytes", freed, "recordings_bytes", total, "max_bytes", maxBytes)
	}

	if total > maxBytes {
		slog.Warn("retention: recordings directory still exceeds its cap",
			"recordings_bytes", total, "max_bytes", maxBytes,
			"note", "every deletable (uploaded, not currently open) session directory has already been removed")
	}
}

// isUploaded reports whether dir was already fully uploaded by an earlier
// call to uploadSession.
func isUploaded(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, uploadMarkerName))
	return err == nil
}

// markUploaded records that dir's files have all been uploaded, so later
// sweeps skip it -- best-effort: a missing marker only costs a redundant
// upload attempt next sweep, which UploadSession's own session_dir resume
// check turns into a single cheap "already complete" API call.
func markUploaded(dir string) {
	path := filepath.Join(dir, uploadMarkerName)
	if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		slog.Warn("retention: could not write upload marker", "dir", dir, "err", err)
	}
}

// dirSize sums the size of every regular file under path, recursively.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// readStartedAt recovers a session's start time from its own meta.json, for
// a directory retentionSweep discovered by scanning disk rather than from a
// live session.Session -- e.g. after a crash or a process restart. A
// missing or unparsable meta.json (crashed before session.Open finished
// writing it) yields the zero Time, which UploadSession already treats as
// "omit started_at" rather than an error.
func readStartedAt(dir string) time.Time {
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return time.Time{}
	}
	var m session.Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return time.Time{}
	}
	if m.SessionStartUnixNs == 0 {
		return time.Time{}
	}
	return time.Unix(0, m.SessionStartUnixNs)
}

// runRetentionSweeps drives retentionSweep on cfg's schedule until ctx is
// canceled. currentDir returns the directory currently being recorded to at
// the moment each tick fires -- read fresh each time, since rotation moves
// it over the sweeper's lifetime.
func runRetentionSweeps(ctx context.Context, uploader *upload.Client, recordingsDir, bootTimebasePath string, currentDir func() string, cfg config.RetentionConfig) {
	if uploader == nil || cfg.MaxBytes <= 0 {
		return
	}
	interval := cfg.SweepInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			retentionSweep(uploader, recordingsDir, bootTimebasePath, currentDir(), cfg.MaxBytes)
		}
	}
}
