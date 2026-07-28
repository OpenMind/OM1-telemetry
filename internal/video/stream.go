package video

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

const HeartbeatName = "video"

const heartbeatInterval = 5 * time.Second

type Config struct {
	RTSPURL        string
	OutputFile     string // base path; each segment is a uniquely-named sibling
	TimestampsFile string // CSV index of all segments
	FramesFile     string // per-frame timestamps CSV (highly recommended).

	Monitor *heartbeat.Monitor

	HeartbeatName string
}

type VideoRTSPStream struct {
	cfg         Config
	running     atomic.Bool
	cancel      context.CancelFunc
	done        chan struct{}
	framesWrite *recordutil.FrameCSVWriter
	pending     atomic.Int64
	allDone     chan struct{}
}

func New(cfg Config) *VideoRTSPStream {
	if cfg.HeartbeatName == "" {
		cfg.HeartbeatName = HeartbeatName
	}
	return &VideoRTSPStream{
		cfg:         cfg,
		framesWrite: recordutil.NewFrameCSVWriter(cfg.FramesFile),
		allDone:     make(chan struct{}),
	}
}

func (v *VideoRTSPStream) Start() {
	if v.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	v.cancel = cancel
	v.done = make(chan struct{})
	go v.loop(ctx)
}

func (v *VideoRTSPStream) Stop() {
	if !v.running.Swap(false) {
		return
	}
	v.cancel()
	<-v.done

	// Wait for any in-flight ffprobe goroutines to finish so the
	// frames CSV is complete on disk before we return.
	v.waitForPending()

	slog.Info("video stream stopped")
}

func (v *VideoRTSPStream) loop(ctx context.Context) {
	defer close(v.done)
	for ctx.Err() == nil {
		if err := v.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("video recorder error; reconnecting in 2 s",
				"camera", v.cfg.HeartbeatName, "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (v *VideoRTSPStream) record(ctx context.Context) error {
	start := time.Now()
	segmentFile := recordutil.UniqueSegmentFile(v.cfg.OutputFile, start)

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", v.cfg.RTSPURL,
		"-c", "copy",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-metadata", "creation_time="+start.UTC().Format(time.RFC3339Nano),
		segmentFile,
	)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start video recorder: %w", err)
	}
	if err := appendSegmentEntry(v.cfg.TimestampsFile, start, segmentFile); err != nil {
		slog.Error("failed to append video segment entry",
			"file", v.cfg.TimestampsFile, "err", err)
	}
	slog.Info("video segment started", "camera", v.cfg.HeartbeatName, "file", segmentFile)

	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		// Tick once immediately so heartbeat sees life right away.
		v.cfg.Monitor.Tick(v.cfg.HeartbeatName)
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-t.C:
				v.cfg.Monitor.Tick(v.cfg.HeartbeatName)
			}
		}
	}()

	waitErr := cmd.Wait()
	close(hbStop)
	<-hbDone

	if v.cfg.FramesFile != "" {
		v.pending.Add(1)
		go func(segFile string, startUnixNs int64) {
			defer v.markDone()
			if err := v.framesWrite.ExtractAndAppend(segFile, "v:0", startUnixNs); err != nil {
				slog.Error("video frames extraction failed",
					"camera", v.cfg.HeartbeatName, "segment", segFile, "err", err)
			} else {
				slog.Info("video frames extracted",
					"camera", v.cfg.HeartbeatName, "segment", filepath.Base(segFile))
			}
		}(segmentFile, start.UnixNano())
	}

	return waitErr
}

func (v *VideoRTSPStream) markDone() {
	if v.pending.Add(-1) == 0 {
		select {
		case v.allDone <- struct{}{}:
		default:
		}
	}
}

func (v *VideoRTSPStream) waitForPending() {
	for v.pending.Load() > 0 {
		select {
		case <-v.allDone:
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
	defer func() { _ = f.Close() }()

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
