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

	"om1-telemetry/internal/clock"
	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
)

const HeartbeatName = "audio"

const heartbeatInterval = 5 * time.Second

type Config struct {
	RTSPURL        string
	OutputFile     string // base path; each segment is a uniquely-named sibling
	TimestampsFile string // CSV index of all segments
	FramesFile     string // per-packet timestamps CSV (highly recommended)

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
	startMono := clock.MonoNs()
	segmentFile := recordutil.UniqueSegmentFile(a.cfg.OutputFile, start)

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", a.cfg.RTSPURL,
		// The source is the video-processor's muxed session stream: h264 in
		// stream 0, opus in stream 1. Without -vn, ffmpeg tries to copy the
		// video into the Ogg container too and exits with "Unsupported codec
		// id in stream 0", leaving a 0-byte file and a reconnect loop.
		"-vn",
		"-c", "copy",
		"-metadata", "creation_time="+start.UTC().Format(time.RFC3339Nano),
		segmentFile,
	)
	// Shut ffmpeg down the way the video recorder does. Without this,
	// CommandContext SIGKILLs it on cancel, and Ogg -- which buffers pages --
	// never flushes, so a clean shutdown left a 0-byte audio.ogg behind.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start audio recorder: %w", err)
	}
	if err := appendSegmentEntry(a.cfg.TimestampsFile, start, startMono, segmentFile); err != nil {
		slog.Error("failed to append audio segment entry",
			"file", a.cfg.TimestampsFile, "err", err)
	}
	slog.Info("audio segment started", "file", segmentFile)

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

	waitErr := cmd.Wait()
	close(hbStop)
	<-hbDone

	if a.cfg.FramesFile != "" {
		a.pending.Add(1)
		go func(segFile string, startUnixNs, startMonoNs int64) {
			defer a.markDone()
			if err := a.framesWrite.ExtractAndAppend(segFile, "a:0", startUnixNs, startMonoNs); err != nil {
				slog.Error("audio frames extraction failed",
					"segment", segFile, "err", err)
			} else {
				slog.Info("audio frames extracted", "segment", filepath.Base(segFile))
			}
		}(segmentFile, start.UnixNano(), startMono)
	}

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

func appendSegmentEntry(path string, start time.Time, startMonoNs int64, segmentFile string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if stat.Size() == 0 {
		if _, err := fmt.Fprintln(f, "recording_start_unix_ns,segment_file,mono_ns"); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
	}

	if _, err := fmt.Fprintf(f, "%d,%s,%d\n", start.UnixNano(), filepath.Base(segmentFile), startMonoNs); err != nil {
		return fmt.Errorf("write entry: %w", err)
	}
	return f.Sync()
}
