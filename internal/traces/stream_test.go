package traces

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testConfig(t *testing.T, url string) Config {
	return Config{
		URL:          url,
		PollInterval: 20 * time.Millisecond,
		OutputFile:   filepath.Join(t.TempDir(), "traces.jsonl"),
	}
}

// exposition builds a minimal om1_trace_info text-exposition-format body,
// exactly as OM1's exporter (a real prometheus client_golang GaugeVec) would
// serialize it -- escaped quotes/newlines included, to exercise the parser
// the same way it would see a real scrape.
func exposition(records ...[5]string) string {
	var b strings.Builder
	b.WriteString("# HELP om1_trace_info test\n# TYPE om1_trace_info gauge\n")
	for _, r := range records {
		fmt.Fprintf(&b, `om1_trace_info{seq="%s",ts="%s",generation="%s",llm_input="%s",llm_output="%s"} 1`+"\n",
			r[0], r[1], r[2], r[3], r[4])
	}
	return b.String()
}

func serveText(t *testing.T, body func() string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(body()))
	}))
	t.Cleanup(server.Close)
	return server
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if fn() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for condition")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func fileLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestNew_returnsNonNilStream(t *testing.T) {
	require.NotNil(t, New(testConfig(t, "http://example.invalid")))
}

func TestStartStop_cleanLifecycle(t *testing.T) {
	server := serveText(t, func() string { return exposition() })
	stream := New(testConfig(t, server.URL))
	stream.Start()
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		stream.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return within 5s")
	}
}

func TestStart_idempotent(t *testing.T) {
	server := serveText(t, func() string { return exposition() })
	stream := New(testConfig(t, server.URL))
	stream.Start()
	stream.Start()
	stream.Stop()
}

func TestStop_beforeStart_isNoOp(t *testing.T) {
	require.NotPanics(t, func() { New(testConfig(t, "http://example.invalid")).Stop() })
}

func TestStop_idempotent(t *testing.T) {
	server := serveText(t, func() string { return exposition() })
	stream := New(testConfig(t, server.URL))
	stream.Start()
	stream.Stop()
	stream.Stop()
}

func TestPoll_writesNewRecordsOnce(t *testing.T) {
	rec := [5]string{"0", "2026-08-31T17:41:36.027193146Z", "1", "hello there", "[]"}
	server := serveText(t, func() string { return exposition(rec) })

	cfg := testConfig(t, server.URL)
	stream := New(cfg)
	stream.Start()
	defer stream.Stop()

	waitFor(t, 2*time.Second, func() bool { return len(fileLines(t, cfg.OutputFile)) == 1 })

	// The same record is served on every subsequent poll (OM1 keeps it
	// buffered until evicted); it must not be re-written.
	time.Sleep(150 * time.Millisecond)
	require.Len(t, fileLines(t, cfg.OutputFile), 1, "an already-seen record must not be duplicated across polls")
	require.Contains(t, fileLines(t, cfg.OutputFile)[0], `"llm_input":"hello there"`)
}

func TestPoll_appendsNewRecordsAcrossPolls(t *testing.T) {
	rec1 := [5]string{"0", "2026-08-31T17:41:36.000000000Z", "1", "first", "[]"}
	rec2 := [5]string{"1", "2026-08-31T17:41:37.000000000Z", "1", "second", "[]"}

	var showSecond atomic.Bool
	server := serveText(t, func() string {
		if showSecond.Load() {
			return exposition(rec1, rec2)
		}
		return exposition(rec1)
	})

	cfg := testConfig(t, server.URL)
	stream := New(cfg)
	stream.Start()
	defer stream.Stop()

	waitFor(t, 2*time.Second, func() bool { return len(fileLines(t, cfg.OutputFile)) == 1 })

	showSecond.Store(true)
	waitFor(t, 2*time.Second, func() bool { return len(fileLines(t, cfg.OutputFile)) == 2 })

	lines := fileLines(t, cfg.OutputFile)
	require.Contains(t, lines[0], `"llm_input":"first"`)
	require.Contains(t, lines[1], `"llm_input":"second"`)
}

func TestPoll_handlesEscapedQuotesAndNewlines(t *testing.T) {
	// Mirrors how a real client_golang exposition encoder escapes label values.
	rec := [5]string{"0", "2026-08-31T17:41:36.000000000Z", "1", `line one\nline two \"quoted\"`, "[]"}
	server := serveText(t, func() string { return exposition(rec) })

	cfg := testConfig(t, server.URL)
	stream := New(cfg)
	stream.Start()
	defer stream.Stop()

	waitFor(t, 2*time.Second, func() bool { return len(fileLines(t, cfg.OutputFile)) == 1 })

	lines := fileLines(t, cfg.OutputFile)
	require.Contains(t, lines[0], `line one\nline two \"quoted\"`)
}

func TestRotate_preservesDedupCursor(t *testing.T) {
	rec := [5]string{"0", "2026-08-31T17:41:36.000000000Z", "1", "hello", "[]"}
	server := serveText(t, func() string { return exposition(rec) })

	cfg := testConfig(t, server.URL)
	stream := New(cfg)
	stream.Start()
	defer stream.Stop()

	waitFor(t, 2*time.Second, func() bool { return len(fileLines(t, cfg.OutputFile)) == 1 })

	secondFile := filepath.Join(t.TempDir(), "traces2.jsonl")
	require.NoError(t, stream.Rotate(secondFile))

	// OM1 keeps serving the same buffered record; it was already consumed
	// before the rotation, so the new file must stay empty.
	time.Sleep(150 * time.Millisecond)
	require.Empty(t, fileLines(t, secondFile), "a record already written before Rotate must not reappear in the new file")
}

// Reproduces the real bug: a process restart (a fresh TraceStream, not just
// Rotate on the same one) used to reset the in-memory cursor to zero, so a
// still-buffered OM1 backlog would all look new again and get dumped into
// whatever session happened to be open at that moment -- even records that
// were already written, chronologically outside that session's window,
// minutes to hours earlier. CursorFile is what closes that gap.
func TestCursorFile_survivesProcessRestart(t *testing.T) {
	rec := [5]string{"0", "2026-08-31T17:41:36.000000000Z", "1", "hello", "[]"}
	server := serveText(t, func() string { return exposition(rec) })

	cursorFile := filepath.Join(t.TempDir(), "cursor")
	firstOutput := filepath.Join(t.TempDir(), "traces.jsonl")

	first := New(Config{
		URL: server.URL, PollInterval: 20 * time.Millisecond,
		OutputFile: firstOutput, CursorFile: cursorFile,
	})
	first.Start()
	waitFor(t, 2*time.Second, func() bool { return len(fileLines(t, firstOutput)) == 1 })
	first.Stop()

	// A brand-new TraceStream, as a fresh process restart would construct --
	// no in-memory state carried over, only CursorFile on disk.
	secondOutput := filepath.Join(t.TempDir(), "traces.jsonl")
	second := New(Config{
		URL: server.URL, PollInterval: 20 * time.Millisecond,
		OutputFile: secondOutput, CursorFile: cursorFile,
	})
	second.Start()
	defer second.Stop()

	// OM1 keeps serving the same record the first process already wrote;
	// the second process must recognize it as already-seen via the
	// persisted cursor and never write it into its own (new) session file.
	time.Sleep(150 * time.Millisecond)
	require.Empty(t, fileLines(t, secondOutput),
		"a record already written by a prior process must not be re-written after a restart")
}

func TestPoll_skipsUnparseableTimestamp(t *testing.T) {
	body := "# HELP om1_trace_info test\n# TYPE om1_trace_info gauge\n" +
		`om1_trace_info{seq="0",ts="not-a-timestamp",generation="1",llm_input="x",llm_output="[]"} 1` + "\n"
	server := serveText(t, func() string { return body })

	cfg := testConfig(t, server.URL)
	stream := New(cfg)
	stream.Start()
	defer stream.Stop()

	time.Sleep(100 * time.Millisecond)
	require.Empty(t, fileLines(t, cfg.OutputFile), "a record with an unparseable timestamp must be skipped, not crash the poller")
}

func TestPoll_serverUnreachable_doesNotPanic(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1") // nothing listens here
	stream := New(cfg)
	require.NotPanics(t, func() {
		stream.Start()
		time.Sleep(50 * time.Millisecond)
		stream.Stop()
	})
}
