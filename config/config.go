package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"om1-telemetry/internal/audio"
	"om1-telemetry/internal/depth"
	"om1-telemetry/internal/features"
	"om1-telemetry/internal/lidar"
	"om1-telemetry/internal/lowstate"
	"om1-telemetry/internal/network"
	"om1-telemetry/internal/odom"
	"om1-telemetry/internal/pointcloud"
	"om1-telemetry/internal/video"
)

type Config struct {
	Collect          bool
	RobotType        RobotType
	EnableLidar      bool
	EnablePointCloud bool
	SessionDir       string
	SessionStartNs   int64
	Video            []VideoConfig
	Audio            AudioConfig
	Features         FeaturesConfig
	Lidar            LidarConfig
	PointCloud       PointCloudConfig
	Depth            DepthConfig
	Odom             OdomConfig
	Lowstate         LowstateConfig
	Network          NetworkConfig
}

type VideoConfig struct {
	Name           string
	RTSPURL        string
	OutputFile     string
	TimestampsFile string
}

type AudioConfig struct {
	RTSPURL        string
	OutputFile     string
	TimestampsFile string
}

// FeaturesConfig configures ingestion of the video-processor's feature-event
// log (capture-time-stamped, video-derived events). Disabled when SourcePath
// is empty.
type FeaturesConfig struct {
	SourcePath     string
	OutputFile     string
	TimestampsFile string
	TimebaseFile   string
}

type LidarConfig struct {
	ZenohEndpoint  string
	ZenohTopic     string
	TimestampsFile string
	DataFile       string
}

type PointCloudConfig struct {
	ZenohEndpoint  string
	ZenohTopic     string
	TimestampsFile string
	DataFile       string
}

type DepthConfig struct {
	ZenohEndpoint  string
	ZenohTopic     string
	TimestampsFile string
	DataFile       string
}

type OdomConfig struct {
	ZenohEndpoint  string
	ZenohTopic     string
	TimestampsFile string
	DataFile       string
}

type LowstateConfig struct {
	ZenohEndpoint  string
	ZenohTopic     string
	TimestampsFile string
	DataFile       string
}

type NetworkConfig struct {
	PingHost     string
	PingTimeout  time.Duration
	PollInterval time.Duration
	DataFile     string
}

func Load() Config {
	now := time.Now()
	baseDir := envStr("RECORDINGS_DIR", "recordings")
	sessionDir := filepath.Join(
		baseDir,
		now.Format("2006-01-02"),
		now.Format("2006-01-02_15-04-05"),
	)

	robotType, profile := ResolveProfile()

	return Config{
		Collect:          envBool("ENABLE_COLLECTION", true),
		RobotType:        robotType,
		EnableLidar:      envBool("ENABLE_LIDAR", profile.EnableLidar),
		EnablePointCloud: envBool("ENABLE_POINTCLOUD", profile.EnablePointCloud),
		SessionDir:       sessionDir,
		SessionStartNs:   now.UnixNano(),
		Video:            videoConfigs(sessionDir),
		Audio: AudioConfig{
			// Audio is the audio track of the video-processor's muxed session
			// stream (single capture clock). ffmpeg selects it with -vn. Served
			// gst-direct on :8555 for the lowest-latency, capture-stamped source;
			// override to mediamtx (rtsp://localhost:8554/live) for an off-host
			// recorder.
			RTSPURL:        envStr("AUDIO_RTSP_URL", "rtsp://localhost:8555/live"),
			OutputFile:     filepath.Join(sessionDir, "audio.ogg"),
			TimestampsFile: filepath.Join(sessionDir, "audio_timestamps.csv"),
		},
		Features: FeaturesConfig{
			// Path to the video-processor's feature-log JSONL on a shared
			// volume. Empty (default) disables ingestion.
			SourcePath:     envStr("VIDEO_FEATURES_LOG", ""),
			OutputFile:     filepath.Join(sessionDir, "video_features.jsonl"),
			TimestampsFile: filepath.Join(sessionDir, "video_features_timestamps.csv"),
			TimebaseFile:   filepath.Join(sessionDir, "video_features_timebase.json"),
		},
		Lidar: LidarConfig{
			ZenohEndpoint:  envStr("LIDAR_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("LIDAR_ZENOH_TOPIC", profile.LidarTopic),
			TimestampsFile: filepath.Join(sessionDir, "lidar_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "lidar_scans.bin"),
		},
		PointCloud: PointCloudConfig{
			ZenohEndpoint:  envStr("POINTCLOUD_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("POINTCLOUD_ZENOH_TOPIC", profile.PointCloudTopic),
			TimestampsFile: filepath.Join(sessionDir, "pointcloud_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "pointcloud_frames.bin"),
		},
		Depth: DepthConfig{
			ZenohEndpoint:  envStr("DEPTH_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("DEPTH_ZENOH_TOPIC", profile.DepthTopic),
			TimestampsFile: filepath.Join(sessionDir, "depth_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "depth_frames.bin"),
		},
		Odom: OdomConfig{
			ZenohEndpoint:  envStr("ODOM_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("ODOM_ZENOH_TOPIC", profile.OdomTopic),
			TimestampsFile: filepath.Join(sessionDir, "odom_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "odom_frames.bin"),
		},
		Lowstate: LowstateConfig{
			ZenohEndpoint:  envStr("LOWSTATE_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("LOWSTATE_ZENOH_TOPIC", profile.LowstateTopic),
			TimestampsFile: filepath.Join(sessionDir, "lowstate_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "lowstate_frames.bin"),
		},
		Network: NetworkConfig{
			PingHost:     envStr("NET_PING_HOST", "8.8.8.8"),
			PingTimeout:  envDuration("NET_PING_TIMEOUT", 2*time.Second),
			PollInterval: envDuration("NET_POLL_INTERVAL", 5*time.Second),
			DataFile:     filepath.Join(sessionDir, "network_status.csv"),
		},
	}
}

func videoConfigs(sessionDir string) []VideoConfig {
	cameras := []struct {
		name       string
		defaultURL string
	}{
		// top_camera is served by the OM1 video-processor's GStreamer pipeline.
		// /raw is the clean (no overlay/blur) camera view — the ground-truth
		// image a recorder wants — on gst-direct :8556. Use :8555/live instead
		// to record the processed view, or mediamtx :8554/raw for an off-host
		// recorder.
		{"top_camera", "rtsp://localhost:8556/raw"},
		// front/down cameras are served by other sources on the robot; left on
		// their existing mountpoints.
		{"front_camera", "rtsp://localhost:8554/front_camera"},
		{"down_camera", "rtsp://localhost:8554/down_camera"},
	}

	configs := make([]VideoConfig, 0, len(cameras))
	for _, cam := range cameras {
		envKey := strings.ToUpper(cam.name) + "_RTSP_URL"
		configs = append(configs, VideoConfig{
			Name:           cam.name,
			RTSPURL:        envStr(envKey, cam.defaultURL),
			OutputFile:     filepath.Join(sessionDir, cam.name+".mp4"),
			TimestampsFile: filepath.Join(sessionDir, cam.name+"_timestamps.csv"),
		})
	}
	return configs
}

func (c VideoConfig) VideoStreamConfig() video.Config {
	return video.Config{
		RTSPURL:        c.RTSPURL,
		OutputFile:     c.OutputFile,
		TimestampsFile: c.TimestampsFile,
	}
}

func (c AudioConfig) AudioStreamConfig() audio.Config {
	return audio.Config{
		RTSPURL:        c.RTSPURL,
		OutputFile:     c.OutputFile,
		TimestampsFile: c.TimestampsFile,
	}
}

// Enabled reports whether feature-log ingestion is configured.
func (c FeaturesConfig) Enabled() bool {
	return c.SourcePath != ""
}

func (c FeaturesConfig) FeatureStreamConfig() features.Config {
	return features.Config{
		SourcePath:     c.SourcePath,
		OutputFile:     c.OutputFile,
		TimestampsFile: c.TimestampsFile,
		TimebaseFile:   c.TimebaseFile,
	}
}

func (c LidarConfig) LidarStreamConfig() lidar.Config {
	return lidar.Config{
		ZenohEndpoint:  c.ZenohEndpoint,
		ZenohTopic:     c.ZenohTopic,
		TimestampsFile: c.TimestampsFile,
		DataFile:       c.DataFile,
	}
}

func (c PointCloudConfig) PointCloudStreamConfig() pointcloud.Config {
	return pointcloud.Config{
		ZenohEndpoint:  c.ZenohEndpoint,
		ZenohTopic:     c.ZenohTopic,
		TimestampsFile: c.TimestampsFile,
		DataFile:       c.DataFile,
	}
}

func (c DepthConfig) DepthStreamConfig() depth.Config {
	return depth.Config{
		ZenohEndpoint:  c.ZenohEndpoint,
		ZenohTopic:     c.ZenohTopic,
		TimestampsFile: c.TimestampsFile,
		DataFile:       c.DataFile,
	}
}

func (c OdomConfig) OdomStreamConfig() odom.Config {
	return odom.Config{
		ZenohEndpoint:  c.ZenohEndpoint,
		ZenohTopic:     c.ZenohTopic,
		TimestampsFile: c.TimestampsFile,
		DataFile:       c.DataFile,
	}
}

func (c LowstateConfig) LowstateStreamConfig() lowstate.Config {
	return lowstate.Config{
		ZenohEndpoint:  c.ZenohEndpoint,
		ZenohTopic:     c.ZenohTopic,
		TimestampsFile: c.TimestampsFile,
		DataFile:       c.DataFile,
	}
}

func (c NetworkConfig) NetworkStreamConfig() network.Config {
	return network.Config{
		PingHost:     c.PingHost,
		PingTimeout:  c.PingTimeout,
		PollInterval: c.PollInterval,
		DataFile:     c.DataFile,
	}
}

func envStr(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func envDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return defaultValue
}

func envBool(key string, defaultValue bool) bool {
	switch os.Getenv(key) {
	case "false", "0", "no":
		return false
	case "true", "1", "yes":
		return true
	}
	return defaultValue
}
