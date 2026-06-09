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
	"om1-telemetry/internal/depth"
	"om1-telemetry/internal/lidar"
	"om1-telemetry/internal/odom"
	"om1-telemetry/internal/video"
)

func main() {
	cfg := config.Load()

	if !cfg.Collect {
		slog.Info("data collection disabled via ENABLE_COLLECTION=false, exiting")
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

	videoStreams := make([]*video.VideoRTSPStream, 0, len(cfg.Video))
	for _, vc := range cfg.Video {
		videoStreams = append(videoStreams, video.New(vc.VideoStreamConfig()))
	}
	audioStream := audio.New(cfg.Audio.AudioStreamConfig())
	lidarStream := lidar.New(cfg.Lidar.LidarStreamConfig())
	depthStream := depth.New(cfg.Depth.DepthStreamConfig())
	odomStream := odom.New(cfg.Odom.OdomStreamConfig())

	for _, vs := range videoStreams {
		vs.Start()
	}
	audioStream.Start()
	lidarStream.Start()
	depthStream.Start()
	odomStream.Start()

	videoURLs := make([]string, 0, len(cfg.Video))
	for _, vc := range cfg.Video {
		videoURLs = append(videoURLs, vc.Name+"="+vc.RTSPURL)
	}

	slog.Info("recording started",
		"session", cfg.SessionDir,
		"video-cameras", videoURLs,
		"audio-url", cfg.Audio.RTSPURL,
		"lidar-topic", cfg.Lidar.ZenohTopic,
		"depth-topic", cfg.Depth.ZenohTopic,
		"odom-topic", cfg.Odom.ZenohTopic,
	)
	slog.Info("press Ctrl-C to stop")

	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, syscall.SIGINT, syscall.SIGTERM)
	<-shutdownSignal

	slog.Info("shutting down…")
	for _, vs := range videoStreams {
		vs.Stop()
	}
	audioStream.Stop()
	lidarStream.Stop()
	depthStream.Stop()
	odomStream.Stop()
}
