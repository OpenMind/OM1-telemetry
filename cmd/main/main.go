package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"om1-telemetry/config"
	"om1-telemetry/internal/audio"
	"om1-telemetry/internal/lidar"
	"om1-telemetry/internal/video"
)

func main() {
	cfg := config.Load()

	if !cfg.Collect {
		slog.Info("data collection disabled via COLLECT=false, exiting")
		return
	}

	if err := os.MkdirAll(cfg.SessionDir, 0o755); err != nil {
		slog.Error("cannot create session directory", "dir", cfg.SessionDir, "err", err)
		os.Exit(1)
	}

	metaPath := filepath.Join(cfg.SessionDir, "meta.json")
	metaData := map[string]interface{}{
		"session_start_unix_ns": cfg.SessionStartNs,
		"session_dir":           cfg.SessionDir,
	}
	metaJSON, err := json.MarshalIndent(metaData, "", "  ")
	if err != nil {
		slog.Error("cannot marshal metadata", "err", err)
		os.Exit(1)
	}
	if err := os.WriteFile(metaPath, metaJSON, 0o644); err != nil {
		slog.Error("cannot write metadata", "path", metaPath, "err", err)
		os.Exit(1)
	}

	videoStream := video.New(cfg.Video.VideoStreamConfig())
	audioStream := audio.New(cfg.Audio.AudioStreamConfig())
	lidarStream := lidar.New(cfg.Lidar.LidarStreamConfig())

	videoStream.Start()
	audioStream.Start()
	lidarStream.Start()

	slog.Info("recording started",
		"session", cfg.SessionDir,
		"video-url", cfg.Video.RTSPURL,
		"audio-url", cfg.Audio.RTSPURL,
		"lidar-topic", cfg.Lidar.ZenohTopic,
	)
	slog.Info("press Ctrl-C to stop")

	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, syscall.SIGINT, syscall.SIGTERM)
	<-shutdownSignal

	slog.Info("shutting down…")
	videoStream.Stop()
	audioStream.Stop()
	lidarStream.Stop()
}
