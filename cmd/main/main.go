package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"om1-telemetry/config"
	"om1-telemetry/internal/audio"
	"om1-telemetry/internal/depth"
	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/lidar"
	"om1-telemetry/internal/lowstate"
	"om1-telemetry/internal/network"
	"om1-telemetry/internal/odom"
	"om1-telemetry/internal/pointcloud"
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

	// ── HEARTBEAT MONITOR ────────────────────────────────────────────────
	// Centralized health monitoring.  SILENT when everything works;
	// warns once when a stream stops or its rate drops below half of
	// expected.  See internal/heartbeat/heartbeat.go for details.
	//
	// Lowstate: 400 Hz floor works for BOTH Go2 (~500 Hz) and G1 (~1053 Hz)
	// — no separate config per robot type needed, since the message content
	// (IMU + joints + battery + foot forces + remote) is functionally
	// equivalent across both, even though the message schemas differ.
	mon := heartbeat.NewMonitor(30 * time.Second)
	mon.Register(lidar.HeartbeatName, 10)      // ~10 Hz nominal
	mon.Register(pointcloud.HeartbeatName, 10) // ~10 Hz nominal
	mon.Register(depth.HeartbeatName, 10)      // measured 15 Hz; floor at 10
	mon.Register(odom.HeartbeatName, 30)       // 30-50 Hz typical
	mon.Register(lowstate.HeartbeatName, 400)  // Go2 ~500 / G1 ~1053; 400 = safe floor
	mon.Register(network.HeartbeatName, 0)     // 0 = liveness check only
	mon.Register(audio.HeartbeatName, 0)       // 0 = liveness check only (ticks while ffmpeg alive)

	// Video has multiple cameras; give each a unique heartbeat name so
	// you can identify WHICH camera is broken from the log.
	videoHeartbeatNames := make([]string, len(cfg.Video))
	for i, vc := range cfg.Video {
		videoHeartbeatNames[i] = "video_" + vc.Name
		mon.Register(videoHeartbeatNames[i], 0) // liveness check only
	}

	hbCtx, hbCancel := context.WithCancel(context.Background())
	go mon.Run(hbCtx)
	// ─────────────────────────────────────────────────────────────────────

	// Build each video recorder with its unique heartbeat name.
	videoStreams := make([]*video.VideoRTSPStream, 0, len(cfg.Video))
	for i, vc := range cfg.Video {
		vcfg := vc.VideoStreamConfig()
		vcfg.Monitor = mon
		vcfg.HeartbeatName = videoHeartbeatNames[i]
		// Derive per-camera frames CSV if not set upstream.
		if vcfg.FramesFile == "" {
			ext := filepath.Ext(vcfg.OutputFile)
			stem := vcfg.OutputFile[:len(vcfg.OutputFile)-len(ext)]
			vcfg.FramesFile = stem + "_frames.csv"
		}
		videoStreams = append(videoStreams, video.New(vcfg))
	}

	// Audio: attach monitor and ensure frames CSV path is set.
	audioCfg := cfg.Audio.AudioStreamConfig()
	audioCfg.Monitor = mon
	if audioCfg.FramesFile == "" {
		ext := filepath.Ext(audioCfg.OutputFile)
		stem := audioCfg.OutputFile[:len(audioCfg.OutputFile)-len(ext)]
		audioCfg.FramesFile = stem + "_packets.csv"
	}
	audioStream := audio.New(audioCfg)

	// Zenoh-based recorders: attach monitor to each Config.
	lidarCfg := cfg.Lidar.LidarStreamConfig()
	lidarCfg.Monitor = mon
	lidarStream := lidar.New(lidarCfg)

	pointCloudCfg := cfg.PointCloud.PointCloudStreamConfig()
	pointCloudCfg.Monitor = mon
	pointCloudStream := pointcloud.New(pointCloudCfg)

	depthCfg := cfg.Depth.DepthStreamConfig()
	depthCfg.Monitor = mon
	depthStream := depth.New(depthCfg)

	odomCfg := cfg.Odom.OdomStreamConfig()
	odomCfg.Monitor = mon
	odomStream := odom.New(odomCfg)

	lowstateCfg := cfg.Lowstate.LowstateStreamConfig()
	lowstateCfg.Monitor = mon
	lowstateStream := lowstate.New(lowstateCfg)

	networkCfg := cfg.Network.NetworkStreamConfig()
	networkCfg.Monitor = mon
	networkStream := network.New(networkCfg)

	// Start every recorder.
	for _, vs := range videoStreams {
		vs.Start()
	}
	audioStream.Start()
	lidarStream.Start()
	pointCloudStream.Start()
	depthStream.Start()
	odomStream.Start()
	lowstateStream.Start()
	networkStream.Start()

	videoURLs := make([]string, 0, len(cfg.Video))
	for _, vc := range cfg.Video {
		videoURLs = append(videoURLs, vc.Name+"="+vc.RTSPURL)
	}

	slog.Info("recording started",
		"session", cfg.SessionDir,
		"video-cameras", videoURLs,
		"audio-url", cfg.Audio.RTSPURL,
		"lidar-topic", cfg.Lidar.ZenohTopic,
		"pointcloud-topic", cfg.PointCloud.ZenohTopic,
		"depth-topic", cfg.Depth.ZenohTopic,
		"odom-topic", cfg.Odom.ZenohTopic,
		"lowstate-topic", cfg.Lowstate.ZenohTopic,
		"net-ping-host", cfg.Network.PingHost,
		"heartbeat", "30s interval; logs only when broken/recovered",
	)
	slog.Info("press Ctrl-C to stop")

	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, syscall.SIGINT, syscall.SIGTERM)
	<-shutdownSignal

	slog.Info("shutting down…")

	// Stop the heartbeat monitor BEFORE tearing down recorders, otherwise
	// it would warn about "stopped receiving" during shutdown.
	hbCancel()

	for _, vs := range videoStreams {
		vs.Stop()
	}
	audioStream.Stop()
	lidarStream.Stop()
	pointCloudStream.Stop()
	depthStream.Stop()
	odomStream.Stop()
	lowstateStream.Stop()
	networkStream.Stop()
}