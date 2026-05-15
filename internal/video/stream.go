package video

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
	RTSPURL    string
	OutputFile string
}

type VideoRTSPStream struct {
	cfg     Config
	running atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func New(cfg Config) *VideoRTSPStream {
	return &VideoRTSPStream{cfg: cfg}
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
	slog.Info("video stream stopped")
}

func (v *VideoRTSPStream) loop(ctx context.Context) {
	defer close(v.done)
	for ctx.Err() == nil {
		if err := v.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("video recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (v *VideoRTSPStream) record(ctx context.Context) error {
    cmd := exec.CommandContext(ctx, "ffmpeg",
        "-loglevel", "error",
        "-rtsp_transport", "tcp",
        "-i", v.cfg.RTSPURL,
        "-c", "copy",
        "-movflags", "+frag_keyframe+empty_moov+default_base_moof",
        "-y",
        v.cfg.OutputFile,
    )
    cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
    cmd.WaitDelay = 5 * time.Second
    cmd.Stderr = os.Stderr
    if err := cmd.Start(); err != nil {
        return fmt.Errorf("start video recorder: %w", err)
    }
    return cmd.Wait()
}
