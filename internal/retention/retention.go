// Package retention runs catch-up uploads and disk-cap enforcement for
// closed session directories, and tracks which ones have been uploaded.
package retention

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
	"om1-telemetry/internal/clock"
	"om1-telemetry/internal/control"
	"om1-telemetry/internal/session"
	"om1-telemetry/internal/upload"
)

// uploadMarkerName marks a session directory as already uploaded.
const uploadMarkerName = ".uploaded"

// uploadTimeout bounds one session's upload, whether kicked off by rotation, schedule, or shutdown.
const uploadTimeout = 10 * time.Minute

// minSessionAge is a second, time-based line of defense protecting the
// currently-recording session, alongside the dir == currentDir check.
const minSessionAge = 30 * time.Second

// tooYoungToSweep reports whether dir's own recorded start time is within
// minSessionAge of now.
func tooYoungToSweep(dir string) bool {
	start := ReadStartedAt(dir)
	if start.IsZero() {
		return false
	}
	return time.Since(start) < minSessionAge
}

// IsUploaded reports whether dir was already fully uploaded.
func IsUploaded(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, uploadMarkerName))
	return err == nil
}

// MarkUploaded records that dir's files have all been uploaded.
func MarkUploaded(dir string) {
	path := filepath.Join(dir, uploadMarkerName)
	if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		slog.Warn("retention: could not write upload marker", "dir", dir, "err", err)
	}
}

// DirSize sums the size of every regular file under path, recursively.
func DirSize(path string) (int64, error) {
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

// ReadStartedAt recovers a session's start time from its own meta.json.
func ReadStartedAt(dir string) time.Time {
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

// APISessionDir builds the openmind-api's session_dir grouping key from a session's real directory.
func APISessionDir(recordingsDir, realDir string) string {
	rel, err := filepath.Rel(recordingsDir, realDir)
	if err != nil {
		rel = filepath.Base(realDir)
	}
	return filepath.ToSlash(filepath.Join(filepath.Base(recordingsDir), rel))
}

// BootSessionDir reports whether dir holds the live boot-relative clock journal.
func BootSessionDir(bootTimebasePath, dir string) bool {
	want, err := os.Stat(bootTimebasePath)
	if err != nil {
		return false
	}
	got, err := os.Stat(filepath.Join(dir, clock.TimebaseName))
	if err != nil {
		return false
	}
	return os.SameFile(want, got)
}

// UploadOptions protects the live clock journal when dir is the boot session's own directory.
func UploadOptions(bootTimebasePath, dir string) upload.Options {
	if BootSessionDir(bootTimebasePath, dir) {
		return upload.Options{PreserveJSONL: clock.TimebaseName}
	}
	return upload.Options{}
}

// SnapshotTimebase copies the process-wide clock journal into a rotated-away session's own directory.
func SnapshotTimebase(bootPath, sessionDir string) {
	if BootSessionDir(bootPath, sessionDir) {
		return
	}
	raw, err := os.ReadFile(bootPath)
	if err != nil {
		slog.Warn("could not snapshot clock timebase for rotated session", "err", err)
		return
	}
	dst := filepath.Join(sessionDir, clock.TimebaseName)
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		slog.Warn("could not write clock timebase snapshot", "dst", dst, "err", err)
	}
}

// UploadSession uploads one finished session directory; on failure the files are kept locally for a later retry.
//
// dir is claimed for the duration of the upload, so a concurrent call for
// the same dir (another upload path, or retention's cap enforcement) skips
// it instead of racing.
func UploadSession(ctl *control.State, client *upload.Client, dir, sessionDir string, startedAt time.Time, deleteAfter bool, opts upload.Options) {
	if !ctl.TryClaimDir(dir) {
		slog.Info("retention: skipping upload, already in flight", "dir", dir)
		return
	}
	defer ctl.ReleaseDir(dir)

	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()

	if err := client.UploadSession(ctx, dir, sessionDir, startedAt, opts); err != nil {
		slog.Error("session upload failed; files kept locally for retry", "dir", dir, "err", err)
		return
	}
	slog.Info("session uploaded", "dir", dir, "session_dir", sessionDir)
	MarkUploaded(dir)

	if deleteAfter && opts.PreserveJSONL == "" {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("could not delete uploaded session directory", "dir", dir, "err", err)
		}
	}
}

// UploadFinishedSessionAsync kicks off finished's upload in the background if uploading is enabled.
func UploadFinishedSessionAsync(wg *sync.WaitGroup, uploader *upload.Client, ctl *control.State, recordingsDir, bootTimebasePath string, finished *session.Session, uploadDelete bool) {
	if uploader == nil || !ctl.Uploading() {
		return
	}
	opts := UploadOptions(bootTimebasePath, finished.RealDir())
	wg.Add(1)
	go func(dir, apiDir string, startedAt time.Time, opts upload.Options) {
		defer wg.Done()
		UploadSession(ctl, uploader, dir, apiDir, startedAt, uploadDelete, opts)
	}(finished.RealDir(), APISessionDir(recordingsDir, finished.RealDir()), time.Unix(0, finished.StartUnixNs()), opts)
}

// Sweep runs catch-up uploads and cap enforcement once; kept as a building block for tests.
func Sweep(ctl *control.State, uploader *upload.Client, recordingsDir, bootTimebasePath, currentDir string, maxBytes int64) {
	dirs, err := session.ListClosed(recordingsDir)
	if err != nil {
		slog.Warn("retention: cannot list session directories", "dir", recordingsDir, "err", err)
		return
	}

	protected := func(dir string) bool {
		return dir == currentDir || BootSessionDir(bootTimebasePath, dir) || tooYoungToSweep(dir)
	}

	CatchUpUploads(ctl, uploader, dirs, protected, recordingsDir, bootTimebasePath)
	EnforceRetentionCap(ctl, recordingsDir, dirs, protected, maxBytes)
}

// CatchUpUploads retries every closed, not-yet-uploaded, non-protected directory in dirs, oldest first.
func CatchUpUploads(ctl *control.State, uploader *upload.Client, dirs []string, protected func(string) bool, recordingsDir, bootTimebasePath string) {
	if uploader == nil {
		return
	}
	for _, dir := range dirs {
		if protected(dir) || IsUploaded(dir) {
			continue
		}
		opts := UploadOptions(bootTimebasePath, dir)
		UploadSession(ctl, uploader, dir, APISessionDir(recordingsDir, dir), ReadStartedAt(dir), false, opts)
	}
}

// EnforceRetentionCap deletes directories oldest-first (uploaded ones first) until recordingsDir is at or under maxBytes.
func EnforceRetentionCap(ctl *control.State, recordingsDir string, dirs []string, protected func(string) bool, maxBytes int64) {
	if maxBytes <= 0 {
		return
	}
	total, err := DirSize(recordingsDir)
	if err != nil {
		slog.Warn("retention: cannot measure recordings directory size", "dir", recordingsDir, "err", err)
		return
	}

	deleteDir := func(dir string) bool {
		if !ctl.TryClaimDir(dir) {
			slog.Info("retention: skipping delete, dir is in flight", "dir", dir)
			return false
		}
		defer ctl.ReleaseDir(dir)

		freed, err := DirSize(dir)
		if err != nil {
			slog.Warn("retention: cannot measure session directory size", "dir", dir, "err", err)
			return true
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("retention: could not delete session directory", "dir", dir, "err", err)
			return true
		}
		total -= freed
		if IsUploaded(dir) {
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
	for _, dir := range dirs {
		if total <= maxBytes {
			break
		}
		if protected(dir) || !IsUploaded(dir) {
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

// RunSweeps drives catch-up uploads and cap enforcement on separate tickers until ctx is canceled.
func RunSweeps(ctx context.Context, uploader *upload.Client, recordingsDir, bootTimebasePath string, currentDir func() string, cfg config.RetentionConfig, ctl *control.State) {
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
			return dir == cd || BootSessionDir(bootTimebasePath, dir) || tooYoungToSweep(dir)
		}, true
	}

	var wg sync.WaitGroup

	if cfg.MaxBytes > 0 {
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
						EnforceRetentionCap(ctl, recordingsDir, dirs, protected, cfg.MaxBytes)
					}
				}
			}
		}()
	}

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
					CatchUpUploads(ctl, uploader, dirs, protected, recordingsDir, bootTimebasePath)
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
