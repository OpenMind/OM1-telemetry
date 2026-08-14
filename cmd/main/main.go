package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"om1-telemetry/config"
	"om1-telemetry/internal/audio"
	"om1-telemetry/internal/clock"
	"om1-telemetry/internal/depth"
	"om1-telemetry/internal/features"
	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/lidar"
	"om1-telemetry/internal/lowstate"
	"om1-telemetry/internal/network"
	"om1-telemetry/internal/odom"
	"om1-telemetry/internal/pointcloud"
	"om1-telemetry/internal/session"
	"om1-telemetry/internal/video"
)

func main() {
	// The clock is sampled before anything else so the session's monotonic
	// anchor covers the whole run.
	clk := clock.New()

	recordingsDir := config.RecordingsDir()

	sess, err := session.Open(recordingsDir, clk)
	if err != nil {
		slog.Error("cannot create session directory", "dir", recordingsDir, "err", err)
		os.Exit(1)
	}

	// Sweep sessions an earlier run left undated. Done after Open so this run's
	// own pending directory is excluded.
	session.Janitor(recordingsDir, sess.RealDir())

	cfg := config.Load(sess.Dir())
	sess.SetRobotType(string(cfg.RobotType))

	if !cfg.Collect {
		slog.Info("data collection disabled via ENABLE_COLLECTION=false, exiting")
		return
	}

	mon := heartbeat.NewMonitor(30 * time.Second)

	if cfg.EnableLidar {
		mon.Register(lidar.HeartbeatName, cfg.Lidar.RateHz)
	}
	if cfg.EnablePointCloud {
		mon.Register(pointcloud.HeartbeatName, cfg.PointCloud.RateHz)
	}
	if cfg.EnableDepth {
		mon.Register(depth.HeartbeatName, cfg.Depth.RateHz)
	}
	if cfg.EnableOdom {
		mon.Register(odom.HeartbeatName, cfg.Odom.RateHz)
	}
	if cfg.EnableLowstate {
		mon.Register(lowstate.HeartbeatName, cfg.Lowstate.RateHz)
	}
	mon.Register(network.HeartbeatName, 0) // 0 = liveness check only
	mon.Register(audio.HeartbeatName, 0)   // 0 = liveness check only
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

	// The clock watcher journals the monotonic-to-UTC mapping and, the first
	// time NTP confirms the wall clock, promotes a pending session to its true
	// date. Recorders write through a symlink, so the rename does not disturb
	// them.
	clkCtx, clkCancel := context.WithCancel(context.Background())
	watcher := clock.NewWatcher(clk, sess.TimebasePath(), func(clock.Record) {
		if err := sess.Promote(); err != nil {
			slog.Error("could not promote session to its true date", "err", err)
		}
	})
	go watcher.Run(clkCtx)

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
	if cfg.EnableLidar {
		lidarCfg := cfg.Lidar.LidarStreamConfig()
		lidarCfg.Monitor = mon
		lidarStream = lidar.New(lidarCfg)
	}

	var pointCloudStream *pointcloud.PointCloudStream
	if cfg.EnablePointCloud {
		pointCloudCfg := cfg.PointCloud.PointCloudStreamConfig()
		pointCloudCfg.Monitor = mon
		pointCloudStream = pointcloud.New(pointCloudCfg)
	}

	var depthStream *depth.DepthStream
	if cfg.EnableDepth {
		depthCfg := cfg.Depth.DepthStreamConfig()
		depthCfg.Monitor = mon
		depthStream = depth.New(depthCfg)
	}

	var odomStream *odom.OdomStream
	if cfg.EnableOdom {
		odomCfg := cfg.Odom.OdomStreamConfig()
		odomCfg.Monitor = mon
		odomStream = odom.New(odomCfg)
	}

	var lowstateStream *lowstate.LowstateStream
	if cfg.EnableLowstate {
		lowstateCfg := cfg.Lowstate.LowstateStreamConfig()
		lowstateCfg.Monitor = mon
		lowstateStream = lowstate.New(lowstateCfg)
	}

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
	if depthStream != nil {
		depthStream.Start()
	}
	if odomStream != nil {
		odomStream.Start()
	}
	if lowstateStream != nil {
		lowstateStream.Start()
	}
	networkStream.Start()

	videoURLs := make([]string, 0, len(cfg.Video))
	for _, vc := range cfg.Video {
		videoURLs = append(videoURLs, vc.Name+"="+vc.RTSPURL)
	}

	slog.Info("recording started",
		"session", sess.RealDir(),
		"session-pending", sess.Pending(),
		"clock-sync", clk.StartSync().String(),
		"robot-type", cfg.RobotType,
		"video-cameras", videoURLs,
		"audio-url", cfg.Audio.RTSPURL,
		"features-enabled", cfg.Features.Enabled(),
		"features-source", cfg.Features.SourcePath,
		"lidar", topicStatus(cfg.EnableLidar, cfg.Lidar.ZenohTopic),
		"pointcloud", topicStatus(cfg.EnablePointCloud, cfg.PointCloud.ZenohTopic),
		"depth", topicStatus(cfg.EnableDepth, cfg.Depth.ZenohTopic),
		"odom", topicStatus(cfg.EnableOdom, cfg.Odom.ZenohTopic),
		"lowstate", topicStatus(cfg.EnableLowstate, cfg.Lowstate.ZenohTopic),
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
	if depthStream != nil {
		depthStream.Stop()
	}
	if odomStream != nil {
		odomStream.Stop()
	}
	if lowstateStream != nil {
		lowstateStream.Stop()
	}
	networkStream.Stop()

	// Stopped last: a clock step during shutdown is still worth journaling, and
	// a session that syncs at the last moment still gets its true date.
	clkCancel()

	if sess.Pending() {
		slog.Warn("session is still undated: the clock never synchronized while recording",
			"dir", sess.RealDir(),
			"note", "it will be dated automatically on a later start if it ever syncs")
	}
}

// topicStatus renders a topic for the startup log: its key, or why it is off.
func topicStatus(enabled bool, key string) string {
	if !enabled {
		return "disabled"
	}
	return key
}
