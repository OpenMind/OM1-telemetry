// Package recordutil provides helpers for safely appending to recorder
// data files across reconnects, so that a transient Zenoh / RTSP / network
// failure does NOT clobber previously recorded data.
package recordutil

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// OpenAppendResult holds the file handle and the file's size BEFORE this
// open call. Use PrevSize to decide whether to write a CSV header
// (only when PrevSize == 0), and to continue any byte-offset counter
// from where the previous session left off.
type OpenAppendResult struct {
	File     *os.File
	PrevSize int64
}

// OpenForAppend opens (or creates) a file in append+create mode for writing.
// Unlike os.Create, this does NOT truncate an existing file.
//
// If the file already exists with data, subsequent writes append to its end.
// Use Result.PrevSize to:
//   - Decide whether to (re)write the CSV header (only if PrevSize == 0)
//   - Initialize a byte-offset counter to continue from the previous session
func OpenForAppend(path string) (*OpenAppendResult, error) {
	var prevSize int64
	if stat, err := os.Stat(path); err == nil {
		prevSize = stat.Size()
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	// Make sure the directory exists. os.OpenFile won't create parent dirs.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return &OpenAppendResult{File: f, PrevSize: prevSize}, nil
}

// ReadLastSeq scans a CSV file and returns the value of the SECOND column
// (column index 1, conventionally "seq") on its last non-empty data row.
//
// Returns -1 if:
//   - The file does not exist
//   - The file is empty or has only a header row
//   - No row could be parsed (logs are silent here; callers can warn)
//
// This is used so that after a reconnect, the recorder can continue the
// sequence counter from where the previous session left off, instead of
// resetting it to 0 and creating ambiguous duplicate seq values.
func ReadLastSeq(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return -1, nil
		}
		return -1, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Allow longer lines than the default 64 KiB (some payloads have long
	// per-row metadata, e.g. depth's encoding field).
	scanner.Buffer(make([]byte, 1<<20), 16<<20)

	var lastSeq int64 = -1
	isHeader := true
	for scanner.Scan() {
		line := scanner.Text()
		if isHeader {
			isHeader = false
			continue
		}
		if line == "" {
			continue
		}
		// We only need the second column; SplitN(_, _, 3) is enough to
		// extract parts[1] without allocating for the rest of the row.
		parts := strings.SplitN(line, ",", 3)
		if len(parts) < 2 {
			continue
		}
		n, perr := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if perr == nil {
			lastSeq = n
		}
	}
	if err := scanner.Err(); err != nil {
		return -1, fmt.Errorf("scan %s: %w", path, err)
	}
	return lastSeq, nil
}

// UniqueSegmentFile produces a unique filename for a recording segment by
// inserting a UTC timestamp suffix before the extension.
//
// Example:
//
//	base = "/data/top_camera.mp4"
//	t    = 2026-06-12 16:46:29.876543210 UTC
//	→ "/data/top_camera_20260612T164629_876543210Z.mp4"
//
// This is used for ffmpeg recorders (video/audio) where each reconnect
// must write to a brand new file, because MP4 / MOV containers are not
// append-friendly.
func UniqueSegmentFile(base string, t time.Time) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	utc := t.UTC()
	suffix := fmt.Sprintf("%s_%09dZ",
		utc.Format("20060102T150405"),
		utc.Nanosecond(),
	)
	return fmt.Sprintf("%s_%s%s", stem, suffix, ext)
}