// Package features ingests the OM1 video-processor's feature-event log — a
// newline-delimited JSON stream of per-frame, capture-time-stamped results
// (VVAD/speaking today; pose, recognition, etc. later).
//
// Why this exists (precision time): every other modality this recorder captures
// carries a *source* timestamp — Zenoh sensors stamp at the source, and the
// feature events are stamped at frame-capture on the video-processor's shared
// CLOCK_MONOTONIC (with a monotonic->UTC mapping recorded in a header record).
// The recorded video/audio, by contrast, is only anchored to ffmpeg
// record-start wall-clock + PTS, which is offset from true capture by the
// RTSP/pipeline latency. Ingesting the feature log gives a *capture-accurate*
// video-derived timeline that aligns cleanly, on the common UTC-ns clock, with
// the Zenoh sensors — without depending on fragile A/V stream sync.
//
// The video-processor writes the log to the path given by its --feature-log /
// OM_FEATURE_LOG option. For this recorder to read it, that path must be on a
// volume shared with this container (see README). Point VIDEO_FEATURES_LOG at
// the same file.
package features

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"sync/atomic"
	"time"

	"om1-telemetry/internal/heartbeat"
)

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
const HeartbeatName = "features"

// pollInterval is how often the tailer checks the source file for new data.
const pollInterval = 250 * time.Millisecond

// nsPerSec is nanoseconds per second, named for readability at call sites.
const nsPerSec = 1e9

// Config configures a feature-log ingestor.
type Config struct {
	// SourcePath is the video-processor feature-log JSONL to tail. When empty
	// the recorder is disabled (see config.FeaturesConfig.Enabled).
	SourcePath string
	// OutputFile is the verbatim copy of ingested records (JSONL).
	OutputFile string
	// TimestampsFile is the CSV index keyed to the common UTC-ns clock.
	TimestampsFile string
	// TimebaseFile receives the monotonic<->UTC mapping from the log header.
	TimebaseFile string

	// Monitor is optional; ticks once per record so the central heartbeat
	// monitor can detect a stalled ingestor.
	Monitor *heartbeat.Monitor

	HeartbeatName string
}

// record is the subset of a feature-log line this ingestor needs. Unknown
// fields are ignored on parse and preserved by the verbatim copy.
type record struct {
	Type        string          `json:"type"`
	TCaptureNs  int64           `json:"t_capture_ns"`
	TCaptureUTC float64         `json:"t_capture_utc"`
	Timebase    json.RawMessage `json:"timebase"`
}

type timebase struct {
	MonotonicNs int64 `json:"monotonic_ns"`
	UTCNs       int64 `json:"utc_ns"`
}

// FeatureStream tails a feature-log JSONL file and records it into the session.
type FeatureStream struct {
	cfg     Config
	running atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}

	out *os.File
	ts  *os.File

	// offset is the byte position up to which SourcePath has been consumed.
	offset int64
	// utcOffsetNs maps a monotonic ns reading to UTC ns (utc = mono + offset),
	// recovered from the log header. Zero until a header is seen.
	utcOffsetNs int64
	haveOffset  bool
	// seq numbers emitted timestamp rows.
	seq int64
	// skippedBacklog is set once we've fast-forwarded past pre-session records
	// on the first successful open.
	skippedBacklog bool
}

// New constructs a FeatureStream.
func New(cfg Config) *FeatureStream {
	if cfg.HeartbeatName == "" {
		cfg.HeartbeatName = HeartbeatName
	}
	return &FeatureStream{cfg: cfg}
}

// Start begins tailing in a background goroutine.
func (s *FeatureStream) Start() {
	if s.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.loop(ctx)
}

// Stop halts tailing and flushes output.
func (s *FeatureStream) Stop() {
	if !s.running.Swap(false) {
		return
	}
	s.cancel()
	<-s.done
	slog.Info("features stream stopped")
}

func (s *FeatureStream) loop(ctx context.Context) {
	defer close(s.done)

	if err := s.openOutputs(); err != nil {
		slog.Error("features: cannot open outputs; ingestor disabled", "err", err)
		return
	}
	defer s.closeOutputs()

	loggedMissing := false
	t := time.NewTicker(pollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			// Drain any final complete lines before exiting.
			s.poll(&loggedMissing)
			return
		case <-t.C:
			s.poll(&loggedMissing)
		}
	}
}

// openOutputs opens the copy + timestamps files and writes the CSV header.
func (s *FeatureStream) openOutputs() error {
	out, err := os.OpenFile(s.cfg.OutputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open output: %w", err)
	}
	ts, err := os.OpenFile(s.cfg.TimestampsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		out.Close()
		return fmt.Errorf("open timestamps: %w", err)
	}
	if st, err := ts.Stat(); err == nil && st.Size() == 0 {
		if _, err := fmt.Fprintln(ts, "unix_ns,t_capture_ns,type,seq"); err != nil {
			out.Close()
			ts.Close()
			return fmt.Errorf("write csv header: %w", err)
		}
	}
	s.out = out
	s.ts = ts
	return nil
}

func (s *FeatureStream) closeOutputs() {
	if s.out != nil {
		s.out.Sync()
		s.out.Close()
	}
	if s.ts != nil {
		s.ts.Sync()
		s.ts.Close()
	}
}

// poll reads any new complete lines from the source, handling first-open
// backlog skipping and rotation/truncation.
func (s *FeatureStream) poll(loggedMissing *bool) {
	f, err := os.Open(s.cfg.SourcePath)
	if err != nil {
		if !*loggedMissing {
			slog.Warn("features: source not available yet; waiting",
				"path", s.cfg.SourcePath, "err", err)
			*loggedMissing = true
		}
		return
	}
	defer f.Close()
	if *loggedMissing {
		slog.Info("features: source available", "path", s.cfg.SourcePath)
		*loggedMissing = false
	}

	st, err := f.Stat()
	if err != nil {
		slog.Warn("features: stat failed", "err", err)
		return
	}
	size := st.Size()

	// On the first successful open, capture the timebase from any existing
	// header and fast-forward past the pre-session backlog so we only record
	// events from when this session started. (Rotation later is handled below.)
	if !s.skippedBacklog {
		s.scanForHeader(f)
		s.offset = size
		s.skippedBacklog = true
		return
	}

	// Rotation/truncation: the writer rotates via os.replace, so the path now
	// points at a fresh, smaller file. Restart from the top to pick up its
	// (new) header and content.
	if size < s.offset {
		slog.Info("features: source rotated; re-reading from start")
		s.offset = 0
	}
	if size == s.offset {
		return
	}

	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		slog.Warn("features: seek failed", "err", err)
		return
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if err == io.EOF {
			// Trailing partial line (no newline yet): leave it for next poll.
			break
		}
		if err != nil {
			slog.Warn("features: read failed", "err", err)
			break
		}
		s.offset += int64(len(line))
		s.process(line[:len(line)-1]) // drop trailing '\n'
	}
}

// scanForHeader reads the whole current file looking for the latest header
// record and writes its timebase mapping to the sidecar. It does not consume
// the tail offset.
func (s *FeatureStream) scanForHeader(f *os.File) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var rec record
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec.Type == "header" && len(rec.Timebase) > 0 {
			s.applyTimebase(rec.Timebase)
		}
	}
}

// process handles one feature-log line: copy it verbatim, then either record
// the timebase (header) or emit a timestamp row (data).
func (s *FeatureStream) process(line string) {
	if line == "" {
		return
	}
	if _, err := fmt.Fprintln(s.out, line); err != nil {
		slog.Warn("features: copy write failed", "err", err)
	}

	var rec record
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		slog.Warn("features: unparseable record; copied only", "err", err)
		return
	}
	if rec.Type == "header" {
		if len(rec.Timebase) > 0 {
			s.applyTimebase(rec.Timebase)
		}
		return
	}

	unixNs, ok := s.unixNs(rec)
	if !ok {
		return // no usable capture time; verbatim copy still kept
	}
	if _, err := fmt.Fprintf(s.ts, "%d,%d,%s,%d\n", unixNs, rec.TCaptureNs, rec.Type, s.seq); err != nil {
		slog.Warn("features: timestamp write failed", "err", err)
		return
	}
	s.seq++
	if s.cfg.Monitor != nil {
		s.cfg.Monitor.Tick(s.cfg.HeartbeatName)
	}
}

// unixNs resolves a record's capture time on the common UTC-ns clock. It
// prefers the record's own t_capture_utc (the writer fills this in); failing
// that it derives UTC from t_capture_ns via the header's monotonic->UTC offset.
func (s *FeatureStream) unixNs(rec record) (int64, bool) {
	if rec.TCaptureUTC > 0 {
		return int64(math.Round(rec.TCaptureUTC * nsPerSec)), true
	}
	if rec.TCaptureNs != 0 && s.haveOffset {
		return rec.TCaptureNs + s.utcOffsetNs, true
	}
	return 0, false
}

// applyTimebase records the monotonic->UTC offset and persists the mapping.
func (s *FeatureStream) applyTimebase(raw json.RawMessage) {
	var tb timebase
	if err := json.Unmarshal(raw, &tb); err != nil {
		slog.Warn("features: bad timebase header", "err", err)
		return
	}
	if tb.MonotonicNs != 0 || tb.UTCNs != 0 {
		s.utcOffsetNs = tb.UTCNs - tb.MonotonicNs
		s.haveOffset = true
	}
	if s.cfg.TimebaseFile != "" {
		if err := os.WriteFile(s.cfg.TimebaseFile, raw, 0o644); err != nil {
			slog.Warn("features: cannot write timebase sidecar", "err", err)
		}
	}
}
