package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"om1-telemetry/config"
	"om1-telemetry/internal/control"
	"om1-telemetry/internal/session"
	"om1-telemetry/internal/upload"
)

// uploadMarkerName marks a session directory as already uploaded.
const uploadMarkerName = ".uploaded"

// retentionSweep runs catch-up uploads and cap enforcement once; kept as a building block for tests.
func retentionSweep(uploader *upload.Client, recordingsDir, bootTimebasePath, currentDir string, maxBytes int64) {
	dirs, err := session.ListClosed(recordingsDir)
	if err != nil {
		slog.Warn("retention: cannot list session directories", "dir", recordingsDir, "err", err)
		return
	}

	protected := func(dir string) bool {
		return dir == currentDir || bootSessionDir(bootTimebasePath, dir)
	}

	catchUpUploads(uploader, dirs, protected, recordingsDir, bootTimebasePath)
	enforceRetentionCap(recordingsDir, dirs, protected, maxBytes)
}

// catchUpUploads retries every closed, not-yet-uploaded, non-protected directory in dirs, oldest first.
func catchUpUploads(uploader *upload.Client, dirs []string, protected func(string) bool, recordingsDir, bootTimebasePath string) {
	if uploader == nil {
		return
	}
	for _, dir := range dirs {
		if protected(dir) || isUploaded(dir) {
			continue
		}
		opts := uploadOptions(bootTimebasePath, dir)
		uploadSession(uploader, dir, apiSessionDir(recordingsDir, dir), readStartedAt(dir), false, opts)
	}
}

// enforceRetentionCap deletes directories oldest-first (uploaded ones first) until recordingsDir is at or under maxBytes.
func enforceRetentionCap(recordingsDir string, dirs []string, protected func(string) bool, maxBytes int64) {
	if maxBytes <= 0 {
		return
	}
	total, err := dirSize(recordingsDir)
	if err != nil {
		slog.Warn("retention: cannot measure recordings directory size", "dir", recordingsDir, "err", err)
		return
	}

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

// isUploaded reports whether dir was already fully uploaded.
func isUploaded(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, uploadMarkerName))
	return err == nil
}

// pendingUploadCount reports how many closed sessions are not yet confirmed uploaded.
func pendingUploadCount(recordingsDir string) int {
	dirs, err := session.ListClosed(recordingsDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, dir := range dirs {
		if !isUploaded(dir) {
			n++
		}
	}
	return n
}

// markUploaded records that dir's files have all been uploaded.
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

// readStartedAt recovers a session's start time from its own meta.json.
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

// runRetentionSweeps drives catch-up uploads and cap enforcement on separate tickers until ctx is canceled.
func runRetentionSweeps(ctx context.Context, uploader *upload.Client, recordingsDir, bootTimebasePath string, currentDir func() string, cfg config.RetentionConfig, ctl *control.State) {
	if cfg.MaxBytes <= 0 {
		return
	}
	interval := cfg.SweepInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	listClosed := func() (dirs []string, protected func(string) bool, ok bool) {
		dirs, err := session.ListClosed(recordingsDir)
		if err != nil {
			slog.Warn("retention: cannot list session directories", "dir", recordingsDir, "err", err)
			return nil, nil, false
		}
		cd := currentDir()
		return dirs, func(dir string) bool {
			return dir == cd || bootSessionDir(bootTimebasePath, dir)
		}, true
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if dirs, protected, ok := listClosed(); ok {
					enforceRetentionCap(recordingsDir, dirs, protected, cfg.MaxBytes)
				}
			}
		}
	}()

	if uploader != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			sweep := func() {
				if !ctl.Uploading() {
					return
				}
				if dirs, protected, ok := listClosed(); ok {
					catchUpUploads(uploader, dirs, protected, recordingsDir, bootTimebasePath)
				}
			}
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					sweep()
				case <-ctl.UploadTrigger:
					sweep()
				}
			}
		}()
	}

	wg.Wait()
}
