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

	// RotateInterval sets ffmpeg's segment_time; zero uses fallbackSegmentTime.
	RotateInterval time.Duration
	// ScratchDir must be on the same filesystem as every session directory
	// this stream rotates into, so relocation is a cheap, atomic rename.
	ScratchDir string

	Monitor *heartbeat.Monitor
}

type AudioRTSPStream struct {
	cfg     Config
	stem    string // relocated-filename prefix, fixed at construction
	running atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}
	pending atomic.Int64
	allDone chan struct{}

	targetMu sync.Mutex
	target   *segmentTarget
}

// segmentTarget is where finished segments currently get relocated, logged,
// and have their frames extracted -- swapped by Rotate.
type segmentTarget struct {
	timestampsFile string
	framesWrite    *recordutil.FrameCSVWriter
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

// Rotate switches where finished segments land, without touching the
// underlying ffmpeg process or its RTSP connection -- so a session rotation
// never drops audio the way a Stop+Start cycle would.
func (a *AudioRTSPStream) Rotate(timestampsFile, framesFile string) {
	a.targetMu.Lock()
	a.target = newSegmentTarget(timestampsFile, framesFile)
	a.targetMu.Unlock()
}

// ensureTarget installs the stream's initial target, unless a Rotate call
// already installed one first.
func (a *AudioRTSPStream) ensureTarget() {
	a.targetMu.Lock()
	defer a.targetMu.Unlock()
	if a.target == nil {
		a.target = newSegmentTarget(a.cfg.TimestampsFile, a.cfg.FramesFile)
	}
}

func newSegmentTarget(timestampsFile, framesFile string) *segmentTarget {
	var fw *recordutil.FrameCSVWriter
	if framesFile != "" {
		fw = recordutil.NewFrameCSVWriter(framesFile)
	}
	return &segmentTarget{timestampsFile: timestampsFile, framesWrite: fw}
}

func (a *AudioRTSPStream) currentTarget() *segmentTarget {
	a.targetMu.Lock()
	defer a.targetMu.Unlock()
	return a.target
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
	startMono := clock.MonoNs()
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
	a.watchSegments(stdout, start, startMono)

	waitErr := cmd.Wait()
	close(hbStop)
	<-hbDone

	return waitErr
}

// watchSegments reads ffmpeg's own segment_list, piped to stdout as CSV
// (filename,start_seconds,end_seconds), and relocates, indexes, and
// extracts frame timestamps for each segment as it completes. Returns once
// ffmpeg closes stdout, i.e. once the process exits.
func (a *AudioRTSPStream) watchSegments(stdout io.Reader, processStart time.Time, processStartMonoNs int64) {
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		name, startSeconds, ok := parseSegmentListLine(line)
		if !ok {
			slog.Warn("audio: unrecognized segment_list line", "line", line)
			continue
		}
		// ffmpeg's segment_list reports the filename as written to its
		// pattern, without the directory -- join it back to ScratchDir.
		scratchFile := filepath.Join(a.cfg.ScratchDir, name)
		a.finishSegment(scratchFile, startSeconds, processStart, processStartMonoNs)
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("audio: segment_list read error", "err", err)
	}
}

// parseSegmentListLine parses one "-segment_list_type csv" line:
// filename,start_seconds,end_seconds.
func parseSegmentListLine(line string) (file string, startSeconds float64, ok bool) {
	parts := strings.Split(line, ",")
	if len(parts) < 2 {
		return "", 0, false
	}
	start, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return "", 0, false
	}
	return strings.TrimSpace(parts[0]), start, true
}

// finishSegment relocates a just-closed segment from the scratch directory
// into the current target's session directory, indexes it, and kicks off
// its (async) frame extraction.
func (a *AudioRTSPStream) finishSegment(scratchFile string, startSeconds float64, processStart time.Time, processStartMonoNs int64) {
	target := a.currentTarget()
	startWallClock := processStart.Add(time.Duration(startSeconds * float64(time.Second)))
	startMonoNs := processStartMonoNs + int64(startSeconds*float64(time.Second))

	finalFile := filepath.Join(filepath.Dir(target.timestampsFile),
		fmt.Sprintf("%s_%s", a.stem, filepath.Base(scratchFile)))

	if err := os.Rename(scratchFile, finalFile); err != nil {
		slog.Error("audio: could not relocate segment", "err", err)
		return
	}

	if err := appendSegmentEntry(target.timestampsFile, startWallClock, startMonoNs, finalFile); err != nil {
		slog.Error("failed to append audio segment entry",
			"file", target.timestampsFile, "err", err)
	}
	slog.Info("audio segment closed", "file", finalFile)

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
