package audio

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

const HeartbeatName = "audio"

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
}

// maxTargets bounds how many recent segmentTargets a stream retains.
const maxTargets = 4

type AudioRTSPStream struct {
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

	// ready closes once this target has received (or, on relocation
	// failure, definitively won't receive) its first segment -- see
	// WaitSegment.
	ready     chan struct{}
	readyOnce sync.Once
}

// markReady unblocks any WaitSegment callers for this target. Safe to call
// more than once (a target may receive several segments, or none).
func (t *segmentTarget) markReady() {
	t.readyOnce.Do(func() { close(t.ready) })
}

func New(cfg Config) *AudioRTSPStream {
	ext := filepath.Ext(cfg.OutputFile)
	stem := filepath.Base(cfg.OutputFile[:len(cfg.OutputFile)-len(ext)])
	return &AudioRTSPStream{
		cfg:     cfg,
		stem:    stem,
		allDone: make(chan struct{}),
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

// Rotate adds sessionStart's directory as a new relocation target, without
// touching the underlying ffmpeg process or its RTSP connection.
func (a *AudioRTSPStream) Rotate(sessionStart time.Time, timestampsFile, framesFile string) {
	t := newSegmentTarget(sessionStart, timestampsFile, framesFile)
	a.targetMu.Lock()
	a.targets = append(a.targets, t)
	if len(a.targets) > maxTargets {
		a.targets = a.targets[len(a.targets)-maxTargets:]
	}
	a.targetMu.Unlock()
}

// ensureTarget installs the stream's initial target, unless a Rotate call
// already installed one first.
func (a *AudioRTSPStream) ensureTarget() {
	a.targetMu.Lock()
	defer a.targetMu.Unlock()
	if len(a.targets) == 0 {
		a.targets = append(a.targets, newSegmentTarget(a.cfg.SessionStart, a.cfg.TimestampsFile, a.cfg.FramesFile))
	}
}

func newSegmentTarget(sessionStart time.Time, timestampsFile, framesFile string) *segmentTarget {
	var fw *recordutil.FrameCSVWriter
	if framesFile != "" {
		fw = recordutil.NewFrameCSVWriter(framesFile)
	}
	return &segmentTarget{sessionStart: sessionStart, timestampsFile: timestampsFile, framesWrite: fw, ready: make(chan struct{})}
}

// targetFor returns the target whose session started most recently at or
// before startWallClock.
func (a *AudioRTSPStream) targetFor(startWallClock time.Time) *segmentTarget {
	a.targetMu.Lock()
	defer a.targetMu.Unlock()
	best := a.targets[0]
	for _, t := range a.targets[1:] {
		if t.sessionStart.After(startWallClock) {
			continue
		}
		if t.sessionStart.After(best.sessionStart) {
			best = t
		}
	}
	return best
}

// WaitSegment blocks until the target for sessionStart (installed by an
// earlier Rotate/ensureTarget call with that exact value) has relocated its
// first segment, or ctx ends. Reports whether a segment actually arrived;
// false also covers "no such target" and "ctx ended first".
//
// This exists because a video/audio segment closes (and only then becomes
// visible to a directory listing) a full segment_time after it began -- the
// same duration as a session rotation interval -- so its close notification
// lands at roughly the same instant as the *next* rotation, well after the
// session it belongs to has already closed. A caller uploading that session
// immediately on rotation would otherwise race this and upload without it.
func (a *AudioRTSPStream) WaitSegment(ctx context.Context, sessionStart time.Time) bool {
	a.targetMu.Lock()
	var target *segmentTarget
	for _, t := range a.targets {
		if t.sessionStart.Equal(sessionStart) {
			target = t
			break
		}
	}
	a.targetMu.Unlock()
	if target == nil {
		return false
	}
	select {
	case <-target.ready:
		return true
	case <-ctx.Done():
		return false
	}
}

func (a *AudioRTSPStream) loop(ctx context.Context) {
	defer close(a.done)
	a.ensureTarget()
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

// record runs one continuous ffmpeg process for the RTSP connection's
// lifetime, letting ffmpeg's own segment muxer cut output files on
// RotateInterval; watchSegments relocates and indexes each one as it closes.
func (a *AudioRTSPStream) record(ctx context.Context) error {
	if err := os.MkdirAll(a.cfg.ScratchDir, 0o755); err != nil {
		return fmt.Errorf("create scratch dir: %w", err)
	}

	segmentTime := a.cfg.RotateInterval
	if segmentTime <= 0 {
		segmentTime = fallbackSegmentTime
	}

	start := time.Now()
	pattern := filepath.Join(a.cfg.ScratchDir, "%Y%m%dT%H%M%S.ogg")

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
		"-f", "segment",
		"-segment_time", strconv.Itoa(int(segmentTime.Seconds())),
		"-segment_format", "ogg",
		"-reset_timestamps", "1",
		"-strftime", "1",
		"-segment_list", "pipe:1",
		"-segment_list_type", "csv",
		pattern,
	)
	// Own process group. Ctrl-C in a terminal delivers SIGINT to the whole
	// foreground group, so ffmpeg would get one from the terminal and a second
	// from Cancel below -- and ffmpeg treats a second interrupt as "exit
	// immediately", abandoning the container trailer. Detached, it only ever
	// receives the one we send.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Interrupt rather than the CommandContext default of SIGKILL: Ogg buffers
	// pages, so a killed ffmpeg flushes nothing and leaves a 0-byte segment.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start audio recorder: %w", err)
	}
	slog.Info("audio recorder started", "rtsp_url", a.cfg.RTSPURL)

	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
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

	// Must fully drain stdout before calling Wait: Wait closes the pipe once
	// it reaps the process, and reading after that races the close.
	a.watchSegments(stdout)

	waitErr := cmd.Wait()
	close(hbStop)
	<-hbDone

	return waitErr
}

// watchSegments reads ffmpeg's own segment_list, piped to stdout as CSV
// (filename,start_seconds,end_seconds), and relocates, indexes, and
// extracts frame timestamps for each segment as it completes. Returns once
// ffmpeg closes stdout, i.e. once the process exits.
func (a *AudioRTSPStream) watchSegments(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		name, startSeconds, endSeconds, ok := parseSegmentListLine(line)
		if !ok {
			slog.Warn("audio: unrecognized segment_list line", "line", line)
			continue
		}
		// ffmpeg's segment_list reports the filename as written to its
		// pattern, without the directory -- join it back to ScratchDir.
		scratchFile := filepath.Join(a.cfg.ScratchDir, name)
		duration := time.Duration((endSeconds - startSeconds) * float64(time.Second))
		a.finishSegment(scratchFile, duration, time.Now(), clock.MonoNs())
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("audio: segment_list read error", "err", err)
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
func (a *AudioRTSPStream) finishSegment(scratchFile string, duration time.Duration, observedNow time.Time, observedMonoNs int64) {
	startWallClock := observedNow.Add(-duration)
	startMonoNs := observedMonoNs - duration.Nanoseconds()
	target := a.targetFor(startWallClock)

	finalFile := filepath.Join(filepath.Dir(target.timestampsFile),
		fmt.Sprintf("%s_%s", a.stem, filepath.Base(scratchFile)))

	if err := os.Rename(scratchFile, finalFile); err != nil {
		slog.Error("audio: could not relocate segment", "err", err)
		target.markReady()
		return
	}

	if err := appendSegmentEntry(target.timestampsFile, startWallClock, startMonoNs, finalFile); err != nil {
		slog.Error("failed to append audio segment entry",
			"file", target.timestampsFile, "err", err)
	}
	slog.Info("audio segment closed", "file", finalFile)
	target.markReady()

	if target.framesWrite == nil {
		return
	}
	a.pending.Add(1)
	go func(segFile string, startUnixNs, startMonoNs int64, fw *recordutil.FrameCSVWriter) {
		defer a.markDone()
		if err := fw.ExtractAndAppend(segFile, "a:0", startUnixNs, startMonoNs); err != nil {
			slog.Error("audio frames extraction failed", "segment", segFile, "err", err)
		} else {
			slog.Info("audio frames extracted", "segment", filepath.Base(segFile))
		}
	}(finalFile, startWallClock.UnixNano(), startMonoNs, target.framesWrite)
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
