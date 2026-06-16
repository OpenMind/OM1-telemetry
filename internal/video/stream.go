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

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
// Note: with multiple video cameras, you'll have multiple "video"
// streams.  See the comment in main.go on how to name them
// individually (e.g. "video_top", "video_front", "video_down").
const HeartbeatName = "video"

// heartbeatInterval is how often the recorder ticks the heartbeat
// monitor WHILE ffmpeg is alive.  We can't use per-frame ticks here
// because ffmpeg owns the frame processing — we only see segment
// start/end events in our Go code.  So we tick at a steady cadence
// while ffmpeg's PID exists, which means:
//   - If RTSP source dies and ffmpeg keeps dying → ticks stop → WARN
//   - If RTSP source works and ffmpeg runs for hours → ticks continue → silent
const heartbeatInterval = 5 * time.Second

type Config struct {
	RTSPURL        string
	OutputFile     string // base path; each segment is a uniquely-named sibling
	TimestampsFile string // CSV index of all segments
	FramesFile     string // per-frame timestamps CSV (highly recommended).

	// Monitor is optional; if non-nil, ticks every heartbeatInterval
	// while ffmpeg is running so the central heartbeat monitor can
	// detect when RTSP source is broken and ffmpeg can't connect.
	Monitor *heartbeat.Monitor

	// HeartbeatName overrides the default "video" tag.  Use a unique
	// name per camera (e.g. "video_top") so the monitor can identify
	// which camera is broken when multiple are running.  Defaults to
	// HeartbeatName ("video") if empty.
	HeartbeatName string
}

type VideoRTSPStream struct {
	cfg         Config
	running     atomic.Bool
	cancel      context.CancelFunc
	done        chan struct{}
	framesWrite *recordutil.FrameCSVWriter

	// pending tracks per-frame extraction goroutines so Stop() can wait
	// for them before returning (otherwise the last segment's frames
	// might never be appended).
	pending atomic.Int64
	allDone chan struct{}
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

	// ── HEARTBEAT: tick periodically while ffmpeg is alive ──────────────
	// We launch a goroutine that ticks every heartbeatInterval and exits
	// when ffmpeg does (signalled via hbStop).  This way the monitor sees
	// steady ticks while the recorder is healthy, and silence when ffmpeg
	// can't connect / keeps dying.
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
	// ─────────────────────────────────────────────────────────────────────

	waitErr := cmd.Wait()
	close(hbStop)
	<-hbDone

	// ── P0-3: extract per-frame timestamps via ffprobe ───────────────────
	// We do this AFTER ffmpeg exits (segment is closed and complete) and
	// in a goroutine so it doesn't block the next reconnect attempt.
	//
	// Each frame's absolute time:
	//   wallclock_unix_ns = start.UnixNano() + frame_pts_time * 1e9
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
	// ──────────────────────────────────────────────────────────────────────

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

// appendSegmentEntry appends a row to the segments index CSV.
//
//	recording_start_unix_ns,segment_file
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