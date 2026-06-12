package audio

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync/atomic"
	"time"
)

type Config struct {
	RTSPURL        string
	OutputFile     string
	TimestampsFile string
}

type AudioRTSPStream struct {
	cfg     Config
	running atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func New(cfg Config) *AudioRTSPStream {
	return &AudioRTSPStream{cfg: cfg}
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
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", a.cfg.RTSPURL,
		"-c", "copy",
		"-metadata", "creation_time="+start.UTC().Format(time.RFC3339Nano),
		"-y",
		a.cfg.OutputFile,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start audio recorder: %w", err)
	}
	if err := writeStartTimestamp(a.cfg.TimestampsFile, start); err != nil {
		slog.Error("failed to write audio start timestamp", "file", a.cfg.TimestampsFile, "err", err)
	}
	return cmd.Wait()
}

func writeStartTimestamp(path string, start time.Time) error {
	if path == "" {
		return nil
	}
	return os.WriteFile(
		path,
		[]byte(fmt.Sprintf("recording_start_unix_ns\n%d\n", start.UnixNano())),
		0o644,
	)
}
