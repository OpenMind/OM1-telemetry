package audio

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
)

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
const HeartbeatName = "audio"

// heartbeatInterval is how often the recorder ticks the heartbeat
// monitor WHILE ffmpeg is alive.  Same rationale as video — we can't
// tick per-frame because ffmpeg owns the audio processing.
const heartbeatInterval = 5 * time.Second

type Config struct {
	RTSPURL        string
	OutputFile     string // base path; each segment is a uniquely-named sibling
	TimestampsFile string // CSV index of all segments
	FramesFile     string // per-packet timestamps CSV (highly recommended).
	//                       Audio "frames" are really packets (~1024 samples / packet for AAC/Opus).

	// Monitor is optional; if non-nil, ticks every heartbeatInterval
	// while ffmpeg is running so the central heartbeat monitor can
	// detect when RTSP source is broken and ffmpeg can't connect.
	Monitor *heartbeat.Monitor
}

type AudioRTSPStream struct {
	cfg         Config
	running     atomic.Bool
	cancel      context.CancelFunc
	done        chan struct{}
	framesWrite *recordutil.FrameCSVWriter

	pending atomic.Int64
	allDone chan struct{}
}

func New(cfg Config) *AudioRTSPStream {
	return &AudioRTSPStream{
		cfg:         cfg,
		framesWrite: recordutil.NewFrameCSVWriter(cfg.FramesFile),
		allDone:     make(chan struct{}),
	}
}

func (a *AudioRTSPStream) Start() {
	if a.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.done = make(chan struct{})
	go a.loop(ctx)
}

func (a *AudioRTSPStream) Stop() {
	if !a.running.Swap(false) {
		return
	}
	a.cancel()
	<-a.done
	a.waitForPending()
	slog.Info("audio stream stopped")
}

func (a *AudioRTSPStream) loop(ctx context.Context) {
	defer close(a.done)
	for ctx.Err() == nil {
		if err := a.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("audio recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (a *AudioRTSPStream) record(ctx context.Context) error {
	start := time.Now()
	segmentFile := recordutil.UniqueSegmentFile(a.cfg.OutputFile, start)

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", a.cfg.RTSPURL,
		"-c", "copy",
		"-metadata", "creation_time="+start.UTC().Format(time.RFC3339Nano),
		segmentFile,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start audio recorder: %w", err)
	}
	if err := appendSegmentEntry(a.cfg.TimestampsFile, start, segmentFile); err != nil {
		slog.Error("failed to append audio segment entry",
			"file", a.cfg.TimestampsFile, "err", err)
	}
	slog.Info("audio segment started", "file", segmentFile)

	// ── HEARTBEAT: tick periodically while ffmpeg is alive ──────────────
	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		// Tick immediately so heartbeat sees life right away.
		a.cfg.Monitor.Tick(HeartbeatName)
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-t.C:
				a.cfg.Monitor.Tick(HeartbeatName)
			}
		}
	}()
	// ─────────────────────────────────────────────────────────────────────

	waitErr := cmd.Wait()
	close(hbStop)
	<-hbDone

	// ── P0-3: extract per-packet timestamps via ffprobe ──────────────────
	if a.cfg.FramesFile != "" {
		a.pending.Add(1)
		go func(segFile string, startUnixNs int64) {
			defer a.markDone()
			if err := a.framesWrite.ExtractAndAppend(segFile, "a:0", startUnixNs); err != nil {
				slog.Error("audio frames extraction failed",
					"segment", segFile, "err", err)
			} else {
				slog.Info("audio frames extracted", "segment", filepath.Base(segFile))
			}
		}(segmentFile, start.UnixNano())
	}
	// ──────────────────────────────────────────────────────────────────────

	return waitErr
}

func (a *AudioRTSPStream) markDone() {
	if a.pending.Add(-1) == 0 {
		select {
		case a.allDone <- struct{}{}:
		default:
		}
	}
}

func (a *AudioRTSPStream) waitForPending() {
	for a.pending.Load() > 0 {
		select {
		case <-a.allDone:
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func appendSegmentEntry(path string, start time.Time, segmentFile string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if stat.Size() == 0 {
		if _, err := fmt.Fprintln(f, "recording_start_unix_ns,segment_file"); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
	}

	if _, err := fmt.Fprintf(f, "%d,%s\n", start.UnixNano(), filepath.Base(segmentFile)); err != nil {
		return fmt.Errorf("write entry: %w", err)
	}
	return f.Sync()
}