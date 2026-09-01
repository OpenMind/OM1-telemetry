package video

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"om1-telemetry/internal/clock"
	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
)

const HeartbeatName = "video"

const heartbeatInterval = 5 * time.Second

// fallbackSegmentTime is used when RotateInterval is unset (session
// rotation disabled): ffmpeg's segment muxer still needs a positive
// segment_time, so segments are cut on this generous cadence instead.
const fallbackSegmentTime = 24 * time.Hour

type Config struct {
	RTSPURL        string
	OutputFile     string
	TimestampsFile string
	FramesFile     string
	// SessionStart is the first session's start time.
	SessionStart time.Time

	// RotateInterval sets ffmpeg's segment_time; zero uses fallbackSegmentTime.
	RotateInterval time.Duration
	// ScratchDir must be on the same filesystem as every session directory
	// this stream rotates into, so relocation is a cheap, atomic rename.
	ScratchDir string

	Monitor *heartbeat.Monitor

	HeartbeatName string
}

// maxTargets bounds how many recent segmentTargets a stream retains.
const maxTargets = 4

type VideoRTSPStream struct {
	cfg     Config
	stem    string // relocated-filename prefix, fixed at construction
	running atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}
	pending atomic.Int64
	allDone chan struct{}

	targetMu sync.Mutex
	targets  []*segmentTarget
}

// segmentTarget is one session's relocation/indexing destination.
type segmentTarget struct {
	sessionStart   time.Time
	timestampsFile string
	framesWrite    *recordutil.FrameCSVWriter
}

func New(cfg Config) *VideoRTSPStream {
	if cfg.HeartbeatName == "" {
		cfg.HeartbeatName = HeartbeatName
	}
	ext := filepath.Ext(cfg.OutputFile)
	stem := filepath.Base(cfg.OutputFile[:len(cfg.OutputFile)-len(ext)])
	return &VideoRTSPStream{
		cfg:     cfg,
		stem:    stem,
		allDone: make(chan struct{}),
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
	v.waitForPending()

	slog.Info("video stream stopped", "camera", v.cfg.HeartbeatName)
}

// Rotate adds sessionStart's directory as a new relocation target, without
// touching the underlying ffmpeg process or its RTSP connection.
func (v *VideoRTSPStream) Rotate(sessionStart time.Time, timestampsFile, framesFile string) {
	t := newSegmentTarget(sessionStart, timestampsFile, framesFile)
	v.targetMu.Lock()
	v.targets = append(v.targets, t)
	if len(v.targets) > maxTargets {
		v.targets = v.targets[len(v.targets)-maxTargets:]
	}
	v.targetMu.Unlock()
}

// ensureTarget installs the stream's initial target, unless a Rotate call
// already installed one first.
func (v *VideoRTSPStream) ensureTarget() {
	v.targetMu.Lock()
	defer v.targetMu.Unlock()
	if len(v.targets) == 0 {
		v.targets = append(v.targets, newSegmentTarget(v.cfg.SessionStart, v.cfg.TimestampsFile, v.cfg.FramesFile))
	}
}

func newSegmentTarget(sessionStart time.Time, timestampsFile, framesFile string) *segmentTarget {
	var fw *recordutil.FrameCSVWriter
	if framesFile != "" {
		fw = recordutil.NewFrameCSVWriter(framesFile)
	}
	return &segmentTarget{sessionStart: sessionStart, timestampsFile: timestampsFile, framesWrite: fw}
}

// targetFor returns the target whose session started most recently at or
// before startWallClock.
func (v *VideoRTSPStream) targetFor(startWallClock time.Time) *segmentTarget {
	v.targetMu.Lock()
	defer v.targetMu.Unlock()
	best := v.targets[0]
	for _, t := range v.targets[1:] {
		if t.sessionStart.After(startWallClock) {
			continue
		}
		if t.sessionStart.After(best.sessionStart) {
			best = t
		}
	}
	return best
}

func (v *VideoRTSPStream) loop(ctx context.Context) {
	defer close(v.done)
	v.ensureTarget()
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

// record runs one continuous ffmpeg process for the RTSP connection's
// lifetime, letting ffmpeg's own segment muxer cut output files on
// RotateInterval; watchSegments relocates and indexes each one as it closes.
func (v *VideoRTSPStream) record(ctx context.Context) error {
	if err := os.MkdirAll(v.cfg.ScratchDir, 0o755); err != nil {
		return fmt.Errorf("create scratch dir: %w", err)
	}

	segmentTime := v.cfg.RotateInterval
	if segmentTime <= 0 {
		segmentTime = fallbackSegmentTime
	}

	start := time.Now()
	pattern := filepath.Join(v.cfg.ScratchDir, "%Y%m%dT%H%M%S.mp4")

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", v.cfg.RTSPURL,
		"-c", "copy",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-metadata", "creation_time="+start.UTC().Format(time.RFC3339Nano),
		"-f", "segment",
		"-segment_time", strconv.Itoa(int(segmentTime.Seconds())),
		"-segment_format", "mp4",
		"-reset_timestamps", "1",
		"-strftime", "1",
		"-segment_list", "pipe:1",
		"-segment_list_type", "csv",
		pattern,
	)
	// Own process group. Ctrl-C in a terminal delivers SIGINT to the whole
	// foreground group, so ffmpeg would receive one from the terminal and a
	// second from Cancel below -- and ffmpeg treats a second interrupt as
	// "exit immediately", abandoning the container's trailer and truncating
	// the last half-second. Detached, it only ever gets the one we send.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start video recorder: %w", err)
	}
	slog.Info("video recorder started", "camera", v.cfg.HeartbeatName, "rtsp_url", v.cfg.RTSPURL)

	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
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

	// Must fully drain stdout before calling Wait: Wait closes the pipe once
	// it reaps the process, and reading after that races the close.
	v.watchSegments(stdout)

	waitErr := cmd.Wait()
	close(hbStop)
	<-hbDone

	return waitErr
}

// watchSegments reads ffmpeg's own segment_list, piped to stdout as CSV
// (filename,start_seconds,end_seconds), and relocates, indexes, and
// extracts frame timestamps for each segment as it completes. Returns once
// ffmpeg closes stdout, i.e. once the process exits.
func (v *VideoRTSPStream) watchSegments(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		name, startSeconds, endSeconds, ok := parseSegmentListLine(line)
		if !ok {
			slog.Warn("video: unrecognized segment_list line", "camera", v.cfg.HeartbeatName, "line", line)
			continue
		}
		// ffmpeg's segment_list reports the filename as written to its
		// pattern, without the directory -- join it back to ScratchDir.
		scratchFile := filepath.Join(v.cfg.ScratchDir, name)
		duration := time.Duration((endSeconds - startSeconds) * float64(time.Second))
		v.finishSegment(scratchFile, duration, time.Now(), clock.MonoNs())
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("video: segment_list read error", "camera", v.cfg.HeartbeatName, "err", err)
	}
}

// parseSegmentListLine parses one "-segment_list_type csv" line:
// filename,start_seconds,end_seconds.
func parseSegmentListLine(line string) (file string, startSeconds, endSeconds float64, ok bool) {
	parts := strings.Split(line, ",")
	if len(parts) < 3 {
		return "", 0, 0, false
	}
	start, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return "", 0, 0, false
	}
	end, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
	if err != nil {
		return "", 0, 0, false
	}
	return strings.TrimSpace(parts[0]), start, end, true
}

// finishSegment relocates a just-closed segment into the current target's
// session directory, indexes it, and kicks off its (async) frame extraction.
func (v *VideoRTSPStream) finishSegment(scratchFile string, duration time.Duration, observedNow time.Time, observedMonoNs int64) {
	startWallClock := observedNow.Add(-duration)
	startMonoNs := observedMonoNs - duration.Nanoseconds()
	target := v.targetFor(startWallClock)

	finalFile := filepath.Join(filepath.Dir(target.timestampsFile),
		fmt.Sprintf("%s_%s", v.stem, filepath.Base(scratchFile)))

	if err := os.Rename(scratchFile, finalFile); err != nil {
		slog.Error("video: could not relocate segment", "camera", v.cfg.HeartbeatName, "err", err)
		return
	}

	if err := appendSegmentEntry(target.timestampsFile, startWallClock, startMonoNs, finalFile); err != nil {
		slog.Error("failed to append video segment entry",
			"file", target.timestampsFile, "err", err)
	}
	slog.Info("video segment closed", "camera", v.cfg.HeartbeatName, "file", finalFile)

	if target.framesWrite == nil {
		return
	}
	v.pending.Add(1)
	go func(segFile string, startUnixNs, startMonoNs int64, fw *recordutil.FrameCSVWriter) {
		defer v.markDone()
		if err := fw.ExtractAndAppend(segFile, "v:0", startUnixNs, startMonoNs); err != nil {
			slog.Error("video frames extraction failed",
				"camera", v.cfg.HeartbeatName, "segment", segFile, "err", err)
		} else {
			slog.Info("video frames extracted",
				"camera", v.cfg.HeartbeatName, "segment", filepath.Base(segFile))
		}
	}(finalFile, startWallClock.UnixNano(), startMonoNs, target.framesWrite)
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
