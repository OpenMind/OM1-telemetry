package recordutil

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// FrameCSVWriter appends per-frame timestamps (extracted post-hoc via
// ffprobe) into a master CSV.  One instance per stream (video/audio).
//
// CSV schema:
//
//	segment_file,frame_idx,pts,pts_time_sec,wallclock_unix_ns
//
// Where:
//   - segment_file       basename of the .mp4 the frame came from
//   - frame_idx          0-based index within the segment
//   - pts                raw PTS value (in container time_base units)
//   - pts_time_sec       PTS in seconds since the segment's first frame
//   - wallclock_unix_ns  absolute time the frame was captured:
//                          segment_start_unix_ns + pts_time_sec * 1e9
type FrameCSVWriter struct {
	path string
	mu   sync.Mutex
}

func NewFrameCSVWriter(path string) *FrameCSVWriter {
	return &FrameCSVWriter{path: path}
}

// ExtractAndAppend runs `ffprobe` on segmentFile to dump per-packet PTS,
// computes each frame's wallclock unix-ns timestamp using
// segmentStartUnixNs as the anchor, and appends a row per frame to the
// master CSV.
//
// streamSelector is what ffprobe's -select_streams flag wants:
//   - "v:0" for the first video stream
//   - "a:0" for the first audio stream
//
// If FrameCSVWriter.path is empty, this is a no-op (frame extraction
// is opt-in via Config).
//
// On any error (ffprobe fails, segment file corrupt, etc.), this
// returns the error but does NOT crash the caller.  The segment is
// still in the segments index — only its per-frame CSV is missing,
// which can be regenerated later by running ffprobe manually.
func (w *FrameCSVWriter) ExtractAndAppend(segmentFile, streamSelector string, segmentStartUnixNs int64) error {
	if w == nil || w.path == "" {
		return nil // disabled
	}

	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", streamSelector,
		"-show_entries", "packet=pts,pts_time",
		"-of", "csv=p=0",
		segmentFile,
	)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("ffprobe %s: %w", segmentFile, err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open frames csv: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat frames csv: %w", err)
	}
	if stat.Size() == 0 {
		if _, err := fmt.Fprintln(f,
			"segment_file,frame_idx,pts,pts_time_sec,wallclock_unix_ns"); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
	}

	buf := bufio.NewWriter(f)
	base := filepath.Base(segmentFile)
	frameIdx := 0
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 1<<20), 16<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Each line: "<pts>,<pts_time>"
		parts := strings.SplitN(line, ",", 2)
		if len(parts) != 2 {
			continue
		}
		ptsStr := strings.TrimSpace(parts[0])
		ptsTimeStr := strings.TrimSpace(parts[1])

		// pts_time may be "N/A" for some malformed packets; skip those.
		if ptsTimeStr == "" || ptsTimeStr == "N/A" {
			frameIdx++
			continue
		}
		ptsTimeF, err := strconv.ParseFloat(ptsTimeStr, 64)
		if err != nil {
			frameIdx++
			continue
		}

		wallclockNs := segmentStartUnixNs + int64(ptsTimeF*1e9)

		if _, err := fmt.Fprintf(buf, "%s,%d,%s,%s,%d\n",
			base, frameIdx, ptsStr, ptsTimeStr, wallclockNs); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
		frameIdx++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan ffprobe output: %w", err)
	}
	if err := buf.Flush(); err != nil {
		return fmt.Errorf("flush frames csv: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync frames csv: %w", err)
	}
	return nil
}