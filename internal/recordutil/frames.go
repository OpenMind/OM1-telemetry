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

type FrameCSVWriter struct {
	path string
	mu   sync.Mutex
}

func NewFrameCSVWriter(path string) *FrameCSVWriter {
	return &FrameCSVWriter{path: path}
}

func (w *FrameCSVWriter) ExtractAndAppend(segmentFile, streamSelector string, segmentStartUnixNs int64) error {
	if w == nil || w.path == "" {
		return nil
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
	defer func() { _ = f.Close() }()

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
		parts := strings.SplitN(line, ",", 2)
		if len(parts) != 2 {
			continue
		}
		ptsStr := strings.TrimSpace(parts[0])
		ptsTimeStr := strings.TrimSpace(parts[1])

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
