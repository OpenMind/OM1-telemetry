// Package traces polls a co-located OM1 process's Prometheus trace-export
// endpoint (see OM1's internal/tracer/traceexport) and appends any records
// not seen yet to the session directory as traces.jsonl -- picked up by the
// existing session upload pipeline like every other stream's output.
//
// OM1 broadcasts trace records as om1_trace_info, an "info" metric (value
// always 1, full record in labels) on a private registry served at
// GET /traces/metrics -- deliberately not OM1's main /metrics endpoint,
// since it carries unbounded free-text label values a real Prometheus
// shouldn't store permanently. This package is that endpoint's one intended
// consumer: it scrapes it the same way Prometheus itself would (using the
// same exposition-format parser), not by any bespoke protocol.
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
	// URL is OM1's trace-export endpoint, e.g. http://localhost:9090/traces/metrics.
	URL string

	PollInterval time.Duration

	OutputFile string

	// CursorFile persists the dedup cursor (the newest record timestamp
	// written so far) across process restarts -- not per-session, so it
	// must sit outside the rotating session directories. See loadCursor's
	// doc comment for why this matters. Optional: an empty value just means
	// no restart-survival, matching the stream's original behavior.
	CursorFile string

	Monitor *heartbeat.Monitor
}

// TraceStream polls Config.URL and appends new records to the current
// output file. It is a persistent stream: Rotate swaps the output file
// without restarting the poll loop, so the in-memory dedup cursor (which
// record timestamps have already been written) survives session rotation --
// letting OM1's exporter re-serve its whole buffer on every poll without
// producing duplicate lines. The cursor is also persisted to CursorFile, so
// a full process restart resumes from it too, instead of re-ingesting
// OM1's whole buffered backlog into whatever session happens to be open at
// that moment (see loadCursor).
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

// poll fetches and parses one batch from OM1's trace-export endpoint and
// appends any records newer than the dedup cursor. Errors (network, parse)
// are logged and left for the next tick -- OM1's exporter buffers the last
// 200 records, so a poller that's behind by less than that window catches up
// without losing anything.
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

// loadCursor restores the dedup cursor from CursorFile, if set and present.
//
// Without this, a full process restart (not just a session rotation, which
// Rotate already handles) resets the in-memory cursor to zero. OM1's
// exporter keeps serving its whole buffer (up to 200 records) regardless of
// what already got written -- so a freshly-started process would treat
// everything still in that buffer as new and write it all into whichever
// session happens to be open at that moment, mixing in records that are
// chronologically well outside that session's own time window.
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
// restart. Best-effort: a failure here just means the next restart falls
// back to re-ingesting whatever OM1 still has buffered, same as before this
// existed -- not worth aborting the poll loop over.
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

	// om1_trace_info is a plain ASCII, underscore-separated Prometheus name
	// (client_golang's classic exposition format), not a UTF-8 name -- so
	// LegacyValidation, not the module-wide default of UTF8Validation.
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
