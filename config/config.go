package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"om1-telemetry/internal/audio"
	"om1-telemetry/internal/depth"
	"om1-telemetry/internal/lidar"
	"om1-telemetry/internal/network"
	"om1-telemetry/internal/odom"
	"om1-telemetry/internal/video"
)

type Config struct {
	Collect        bool
	SessionDir     string
	SessionStartNs int64
	Video          []VideoConfig
	Audio          AudioConfig
	Lidar          LidarConfig
	Depth          DepthConfig
	Odom           OdomConfig
	Network        NetworkConfig
}

type VideoConfig struct {
	Name       string
	RTSPURL    string
	OutputFile string
}

type AudioConfig struct {
	RTSPURL    string
	OutputFile string
}

type LidarConfig struct {
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

	return Config{
		Collect:        envBool("ENABLE_COLLECTION", true),
		SessionDir:     sessionDir,
		SessionStartNs: now.UnixNano(),
		Video:          videoConfigs(sessionDir),
		Audio: AudioConfig{
			RTSPURL:    envStr("AUDIO_RTSP_URL", "rtsp://localhost:8554/audio"),
			OutputFile: filepath.Join(sessionDir, "audio.ogg"),
		},
		Lidar: LidarConfig{
			ZenohEndpoint:  envStr("LIDAR_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("LIDAR_ZENOH_TOPIC", "scan"),
			TimestampsFile: filepath.Join(sessionDir, "lidar_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "lidar_scans.bin"),
		},
		Depth: DepthConfig{
			ZenohEndpoint:  envStr("DEPTH_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("DEPTH_ZENOH_TOPIC", "camera/realsense2_camera_node/depth/image_rect_raw"),
			TimestampsFile: filepath.Join(sessionDir, "depth_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "depth_frames.bin"),
		},
		Odom: OdomConfig{
			ZenohEndpoint:  envStr("ODOM_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("ODOM_ZENOH_TOPIC", "odom"),
			TimestampsFile: filepath.Join(sessionDir, "odom_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "odom_frames.bin"),
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
		{"top_camera", "rtsp://localhost:8554/top_camera_raw"},
		{"front_camera", "rtsp://localhost:8554/front_camera"},
		{"down_camera", "rtsp://localhost:8554/down_camera"},
	}

	configs := make([]VideoConfig, 0, len(cameras))
	for _, cam := range cameras {
		envKey := strings.ToUpper(cam.name) + "_RTSP_URL"
		configs = append(configs, VideoConfig{
			Name:       cam.name,
			RTSPURL:    envStr(envKey, cam.defaultURL),
			OutputFile: filepath.Join(sessionDir, cam.name+".mp4"),
		})
	}
	return configs
}

func (c VideoConfig) VideoStreamConfig() video.Config {
	return video.Config{
		RTSPURL:    c.RTSPURL,
		OutputFile: c.OutputFile,
	}
}

func (c AudioConfig) AudioStreamConfig() audio.Config {
	return audio.Config{
		RTSPURL:    c.RTSPURL,
		OutputFile: c.OutputFile,
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
