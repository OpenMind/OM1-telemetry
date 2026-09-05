// Package traces polls a co-located OM1 process's Prometheus trace-export
// endpoint and appends any new records to the session directory as traces.jsonl.
package traces

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
)

// HeartbeatName is the stream identifier for the heartbeat monitor.
const HeartbeatName = "traces"

// traceMetricFamily is the Prometheus metric name OM1's exporter uses.
const traceMetricFamily = "om1_trace_info"

// httpTimeout bounds a single poll request, so an unreachable/hung OM1
// process can never stall the loop past one poll interval.
const httpTimeout = 10 * time.Second

type Config struct {
	// URL is OM1's metrics endpoint, e.g. http://localhost:9090/metrics.
	URL string

	PollInterval time.Duration

	OutputFile string

	// CursorFile persists the dedup cursor across process restarts. Optional.
	CursorFile string

	Monitor *heartbeat.Monitor
}

// TraceStream polls Config.URL and appends new records to the current
// output file, deduplicating by timestamp across polls, rotations, and restarts.
type TraceStream struct {
	cfg     Config
	running atomic.Bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	hc      *http.Client

	fileMu sync.Mutex
	file   *os.File

	lastSeenMu sync.Mutex
	lastSeen   time.Time
}

func New(cfg Config) *TraceStream {
	return &TraceStream{cfg: cfg, hc: &http.Client{Timeout: httpTimeout}}
}

func (s *TraceStream) Start() {
	if s.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go s.loop(ctx)
}

func (s *TraceStream) Stop() {
	if !s.running.Swap(false) {
		return
	}
	s.cancel()
	s.wg.Wait()
	s.closeFile()
	slog.Info("traces stream stopped")
}

// Rotate switches the stream's output to a new file without touching the
// poll loop or the dedup cursor.
func (s *TraceStream) Rotate(outputFile string) error {
	result, err := recordutil.OpenForAppend(outputFile)
	if err != nil {
		return fmt.Errorf("rotate: %w", err)
	}

	s.fileMu.Lock()
	old := s.file
	s.file = result.File
	s.fileMu.Unlock()

	if old != nil {
		if err := old.Sync(); err != nil {
			slog.Warn("traces: output sync failed", "err", err)
		}
		if err := old.Close(); err != nil {
			slog.Warn("traces: output close failed", "err", err)
		}
	}
	return nil
}

func (s *TraceStream) loop(ctx context.Context) {
	defer s.wg.Done()

	if err := s.ensureFileOpen(); err != nil {
		slog.Error("traces: cannot open output file; stream disabled", "err", err)
		return
	}
	s.loadCursor()

	slog.Info("traces recorder started", "url", s.cfg.URL, "interval", s.cfg.PollInterval)

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		s.poll(ctx)
		s.cfg.Monitor.Tick(HeartbeatName)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *TraceStream) ensureFileOpen() error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if s.file != nil {
		return nil
	}
	result, err := recordutil.OpenForAppend(s.cfg.OutputFile)
	if err != nil {
		return err
	}
	s.file = result.File
	return nil
}

// poll fetches one batch and appends any records newer than the dedup
// cursor. Errors are logged and retried on the next tick.
func (s *TraceStream) poll(ctx context.Context) {
	records, err := s.fetch(ctx)
	if err != nil {
		slog.Warn("traces: poll failed", "err", err)
		return
	}
	if len(records) == 0 {
		return
	}

	sort.Slice(records, func(i, j int) bool { return records[i].parsedTS.Before(records[j].parsedTS) })

	s.lastSeenMu.Lock()
	cursor := s.lastSeen
	s.lastSeenMu.Unlock()

	var wrote int
	newest := cursor
	for _, rec := range records {
		if !rec.parsedTS.After(cursor) {
			continue
		}
		if err := s.write(rec); err != nil {
			slog.Warn("traces: write failed", "err", err)
			return
		}
		wrote++
		if rec.parsedTS.After(newest) {
			newest = rec.parsedTS
		}
	}
	if wrote == 0 {
		return
	}

	s.lastSeenMu.Lock()
	s.lastSeen = newest
	s.lastSeenMu.Unlock()
	s.saveCursor(newest)
}

// loadCursor restores the dedup cursor from CursorFile, if set and present,
// so a full process restart doesn't re-ingest OM1's whole buffered backlog.
func (s *TraceStream) loadCursor() {
	if s.cfg.CursorFile == "" {
		return
	}
	raw, err := os.ReadFile(s.cfg.CursorFile)
	if err != nil {
		return
	}
	ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		slog.Warn("traces: cannot parse persisted cursor; starting from empty", "err", err)
		return
	}
	s.lastSeenMu.Lock()
	s.lastSeen = ts
	s.lastSeenMu.Unlock()
}

// saveCursor persists the dedup cursor so loadCursor can restore it after a
// restart. Best-effort: failures are only logged.
func (s *TraceStream) saveCursor(ts time.Time) {
	if s.cfg.CursorFile == "" {
		return
	}
	if dir := filepath.Dir(s.cfg.CursorFile); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Warn("traces: cannot create cursor file directory", "err", err)
			return
		}
	}
	if err := os.WriteFile(s.cfg.CursorFile, []byte(ts.Format(time.RFC3339Nano)), 0o644); err != nil {
		slog.Warn("traces: cannot persist cursor", "err", err)
	}
}

// line is the JSON shape written to OutputFile, matching OM1's own
// traces/tracer_<date>.jsonl records field-for-field.
type line struct {
	Timestamp  string          `json:"ts"`
	Generation int             `json:"generation"`
	LLMInput   string          `json:"llm_input"`
	LLMOutput  json.RawMessage `json:"llm_output"`
}

type record struct {
	line
	parsedTS time.Time
}

func (s *TraceStream) write(rec record) error {
	buf, err := json.Marshal(rec.line)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if _, err := s.file.Write(append(buf, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return s.file.Sync()
}

// fetch requests and parses one scrape of the trace-export endpoint.
func (s *TraceStream) fetch(ctx context.Context) ([]record, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	family, ok := families[traceMetricFamily]
	if !ok {
		return nil, nil
	}

	records := make([]record, 0, len(family.GetMetric()))
	for _, m := range family.GetMetric() {
		labels := make(map[string]string, len(m.GetLabel()))
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}

		ts := labels["ts"]
		parsedTS, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			slog.Warn("traces: record with unparseable ts, skipping", "ts", ts, "err", err)
			continue
		}

		generation, err := strconv.Atoi(labels["generation"])
		if err != nil {
			generation = 0
		}

		llmOutput := labels["llm_output"]
		if !json.Valid([]byte(llmOutput)) {
			llmOutput = "[]"
		}

		records = append(records, record{
			line: line{
				Timestamp:  ts,
				Generation: generation,
				LLMInput:   labels["llm_input"],
				LLMOutput:  json.RawMessage(llmOutput),
			},
			parsedTS: parsedTS,
		})
	}
	return records, nil
}

func (s *TraceStream) closeFile() {
	s.fileMu.Lock()
	f := s.file
	s.file = nil
	s.fileMu.Unlock()
	if f == nil {
		return
	}
	if err := f.Sync(); err != nil {
		slog.Warn("traces: output sync failed", "err", err)
	}
	if err := f.Close(); err != nil {
		slog.Warn("traces: output close failed", "err", err)
	}
}
