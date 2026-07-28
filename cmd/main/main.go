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
	"om1-telemetry/internal/features"
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

	enableLidar := cfg.EnableLidar
	enablePointcloud := cfg.EnablePointCloud

	mon := heartbeat.NewMonitor(30 * time.Second)

	if enableLidar {
		mon.Register(lidar.HeartbeatName, 10) // ~10 Hz nominal
	}
	if enablePointcloud {
		mon.Register(pointcloud.HeartbeatName, 10) // ~10 Hz nominal
	}
	mon.Register(depth.HeartbeatName, 10)     // measured 15 Hz; floor at 10
	mon.Register(odom.HeartbeatName, 30)      // 30-50 Hz typical
	mon.Register(lowstate.HeartbeatName, 400) // Go2 ~500 / G1 ~1053; 400 = safe floor
	mon.Register(network.HeartbeatName, 0)    // 0 = liveness check only
	mon.Register(audio.HeartbeatName, 0)      // 0 = liveness check only
	if cfg.Features.Enabled() {
		mon.Register(features.HeartbeatName, 0) // 0 = liveness check only
	}

	videoHeartbeatNames := make([]string, len(cfg.Video))
	for i, vc := range cfg.Video {
		videoHeartbeatNames[i] = "video_" + vc.Name
		mon.Register(videoHeartbeatNames[i], 0)
	}

	hbCtx, hbCancel := context.WithCancel(context.Background())
	go mon.Run(hbCtx)

	videoStreams := make([]*video.VideoRTSPStream, 0, len(cfg.Video))
	for i, vc := range cfg.Video {
		vcfg := vc.VideoStreamConfig()
		vcfg.Monitor = mon
		vcfg.HeartbeatName = videoHeartbeatNames[i]
		if vcfg.FramesFile == "" {
			ext := filepath.Ext(vcfg.OutputFile)
			stem := vcfg.OutputFile[:len(vcfg.OutputFile)-len(ext)]
			vcfg.FramesFile = stem + "_frames.csv"
		}
		videoStreams = append(videoStreams, video.New(vcfg))
	}

	audioCfg := cfg.Audio.AudioStreamConfig()
	audioCfg.Monitor = mon
	if audioCfg.FramesFile == "" {
		ext := filepath.Ext(audioCfg.OutputFile)
		stem := audioCfg.OutputFile[:len(audioCfg.OutputFile)-len(ext)]
		audioCfg.FramesFile = stem + "_packets.csv"
	}
	audioStream := audio.New(audioCfg)

	var featureStream *features.FeatureStream
	if cfg.Features.Enabled() {
		featureCfg := cfg.Features.FeatureStreamConfig()
		featureCfg.Monitor = mon
		featureStream = features.New(featureCfg)
	}

	var lidarStream *lidar.LidarStream
	if enableLidar {
		lidarCfg := cfg.Lidar.LidarStreamConfig()
		lidarCfg.Monitor = mon
		lidarStream = lidar.New(lidarCfg)
	}

	var pointCloudStream *pointcloud.PointCloudStream
	if enablePointcloud {
		pointCloudCfg := cfg.PointCloud.PointCloudStreamConfig()
		pointCloudCfg.Monitor = mon
		pointCloudStream = pointcloud.New(pointCloudCfg)
	}

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

	for _, vs := range videoStreams {
		vs.Start()
	}
	audioStream.Start()
	if featureStream != nil {
		featureStream.Start()
	}
	if lidarStream != nil {
		lidarStream.Start()
	}
	if pointCloudStream != nil {
		pointCloudStream.Start()
	}
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
		"robot-type", cfg.RobotType,
		"video-cameras", videoURLs,
		"audio-url", cfg.Audio.RTSPURL,
		"features-enabled", cfg.Features.Enabled(),
		"features-source", cfg.Features.SourcePath,
		"lidar-enabled", enableLidar,
		"lidar-topic", cfg.Lidar.ZenohTopic,
		"pointcloud-enabled", enablePointcloud,
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

	hbCancel()

	for _, vs := range videoStreams {
		vs.Stop()
	}
	audioStream.Stop()
	if featureStream != nil {
		featureStream.Stop()
	}
	if lidarStream != nil {
		lidarStream.Stop()
	}
	if pointCloudStream != nil {
		pointCloudStream.Stop()
	}
	depthStream.Stop()
	odomStream.Stop()
	lowstateStream.Stop()
	networkStream.Stop()
}
