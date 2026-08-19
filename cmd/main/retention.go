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
//  1. If uploader is configured, walks every dated session directory,
//     oldest first, and uploads any that were never marked uploaded --
//     catching up anything a prior failed/partial upload, a crash, or a
//     manual copy left behind.
//  2. If recordingsDir is still over maxBytes afterward, deletes
//     directories oldest first -- uploaded ones first if that alone is
//     enough, but falling through to not-yet-uploaded ones if it isn't --
//     until it's back under the cap. See enforceRetentionCap.
//
// It never touches currentDir (the segment recorders are writing to right
// now) or the directory still holding the live boot-relative clock journal
// (see bootSessionDir) -- both are still changing, so neither "uploaded" nor
// "safe to delete" can be true of them yet.
func retentionSweep(uploader *upload.Client, recordingsDir, bootTimebasePath, currentDir string, maxBytes int64) {
	dirs, err := session.ListClosed(recordingsDir)
	if err != nil {
		slog.Warn("retention: cannot list session directories", "dir", recordingsDir, "err", err)
		return
	}

	protected := func(dir string) bool {
		return dir == currentDir || bootSessionDir(bootTimebasePath, dir)
	}

	if uploader != nil {
		for _, dir := range dirs {
			if protected(dir) || isUploaded(dir) {
				continue
			}
			opts := uploadOptions(bootTimebasePath, dir)
			uploadSession(uploader, dir, apiSessionDir(recordingsDir, dir), readStartedAt(dir), false, opts)
		}
	}

	enforceRetentionCap(recordingsDir, dirs, protected, maxBytes)
}

// enforceRetentionCap deletes directories from dirs, oldest first, until
// recordingsDir's total size is at or under maxBytes. protected directories
// are never deleted (the live session, or the segment still holding the
// live clock journal).
//
// It runs in two passes: first only already-uploaded directories, so a
// robot that's keeping up with its upload quota never loses data it hasn't
// already backed up. If that alone isn't enough to get under the cap --
// uploading is behind, failing, or not configured at all -- it falls
// through to a second pass over whatever's left, deleting the oldest
// remaining directories regardless of upload status. Keeping local disk
// usage bounded takes priority over keeping unuploaded data: a recorder
// that silently stops recording because its disk filled up is worse than
// one that loses its oldest, least-recoverable segment to stay running.
func enforceRetentionCap(recordingsDir string, dirs []string, protected func(string) bool, maxBytes int64) {
	if maxBytes <= 0 {
		return
	}
	total, err := dirSize(recordingsDir)
	if err != nil {
		slog.Warn("retention: cannot measure recordings directory size", "dir", recordingsDir, "err", err)
		return
	}

	// deleteDir removes dir and returns whether it was handled (deleted, or
	// failed in a way not worth retrying against in the second pass below).
	// Called only once total is already confirmed over maxBytes.
	deleteDir := func(dir string) bool {
		freed, err := dirSize(dir)
		if err != nil {
			slog.Warn("retention: cannot measure session directory size", "dir", dir, "err", err)
			return true
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("retention: could not delete session directory", "dir", dir, "err", err)
			return true
		}
		total -= freed
		if isUploaded(dir) {
			slog.Info("retention: deleted uploaded session directory to stay under the recordings cap",
				"dir", dir, "freed_bytes", freed, "recordings_bytes", total, "max_bytes", maxBytes)
		} else {
			slog.Warn("retention: deleted a session directory that was never confirmed uploaded, to stay under the recordings cap",
				"dir", dir, "freed_bytes", freed, "recordings_bytes", total, "max_bytes", maxBytes,
				"note", "data loss: RETENTION_MAX_BYTES takes priority over keeping data with no confirmed copy in S3")
		}
		return true
	}

	deleted := make(map[string]bool, len(dirs))
	for _, dir := range dirs { // pass 1: already-uploaded directories only
		if total <= maxBytes {
			break
		}
		if protected(dir) || !isUploaded(dir) {
			continue
		}
		if deleteDir(dir) {
			deleted[dir] = true
		}
	}
	for _, dir := range dirs { // pass 2: fall through to unuploaded directories
		if total <= maxBytes {
			break
		}
		if protected(dir) || deleted[dir] {
			continue
		}
		deleteDir(dir)
	}

	if total > maxBytes {
		slog.Warn("retention: recordings directory still exceeds its cap",
			"recordings_bytes", total, "max_bytes", maxBytes,
			"note", "every deletable (not currently open, not the live boot session) directory has already been removed")
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
//
// Runs regardless of whether uploader is configured: cap enforcement
// (deleting the oldest directories, uploaded or not) must hold even when
// there's nowhere to upload to -- see enforceRetentionCap.
func runRetentionSweeps(ctx context.Context, uploader *upload.Client, recordingsDir, bootTimebasePath string, currentDir func() string, cfg config.RetentionConfig) {
	if cfg.MaxBytes <= 0 {
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
