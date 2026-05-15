package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"om1-telemetry/config"
	"om1-telemetry/internal/audio"
	"om1-telemetry/internal/video"
)

func main() {
	cfg := config.Load()

	if err := os.MkdirAll(cfg.SessionDir, 0o755); err != nil {
		slog.Error("cannot create session directory", "dir", cfg.SessionDir, "err", err)
		os.Exit(1)
	}

	videoStream := video.New(cfg.Video.VideoStreamConfig())
	audioStream := audio.New(cfg.Audio.AudioStreamConfig())

	videoStream.Start()
	audioStream.Start()

	slog.Info("recording started",
		"session", cfg.SessionDir,
		"video-url", cfg.Video.RTSPURL,
		"audio-url", cfg.Audio.RTSPURL,
	)
	slog.Info("press Ctrl-C to stop")

	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, syscall.SIGINT, syscall.SIGTERM)
	<-shutdownSignal

	slog.Info("shutting down…")
	videoStream.Stop()
	audioStream.Stop()
}
