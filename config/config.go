package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"om1-telemetry/internal/audio"
	"om1-telemetry/internal/depth"
	"om1-telemetry/internal/lidar"
	"om1-telemetry/internal/lowstate"
	"om1-telemetry/internal/network"
	"om1-telemetry/internal/odom"
	"om1-telemetry/internal/pointcloud"
	"om1-telemetry/internal/video"
)

type Config struct {
	Collect        bool
	SessionDir     string
	SessionStartNs int64
	Video          []VideoConfig
	Audio          AudioConfig
	Lidar          LidarConfig
	PointCloud     PointCloudConfig
	Depth          DepthConfig
	Odom           OdomConfig
	Lowstate       LowstateConfig
	Network        NetworkConfig
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

// LowstateConfig: catch-all robot state (IMU + joints + battery + foot
// forces + remote).  Same config & recorder for Go2 and G1 — the message
// schema differs (unitree_go/LowState vs unitree_hg/LowState) but the
// recorder stores raw bytes, so it doesn't need to know.  Decode the
// recorded data offline with the appropriate ROS package.
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

	return Config{
		Collect:        envBool("ENABLE_COLLECTION", true),
		SessionDir:     sessionDir,
		SessionStartNs: now.UnixNano(),
		Video:          videoConfigs(sessionDir),
		Audio: AudioConfig{
			RTSPURL:        envStr("AUDIO_RTSP_URL", "rtsp://localhost:8554/audio"),
			OutputFile:     filepath.Join(sessionDir, "audio.ogg"),
			TimestampsFile: filepath.Join(sessionDir, "audio_timestamps.csv"),
		},
		// Lidar (2D scan):  /scan is the 2D laser scan converted from 3D
		// point cloud by pointcloud_to_laserscan_node.  Smaller than the
		// 3D cloud (~10 KB vs ~440 KB per scan) but loses height info.
		// On G1 the converter outputs /scan_raw (cloud_in = utlidar/cloud_livox_mid360).
		// Override with LIDAR_ZENOH_TOPIC env var (e.g. "rt/scan_raw" on G1)
		// or set ENABLE_COLLECTION=false on this recorder if you only care
		// about the 3D point cloud (recorded by the pointcloud recorder).
		Lidar: LidarConfig{
			ZenohEndpoint:  envStr("LIDAR_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("LIDAR_ZENOH_TOPIC", "scan"),
			TimestampsFile: filepath.Join(sessionDir, "lidar_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "lidar_scans.bin"),
		},
		// PointCloud (3D):  the main 3D LiDAR data source.  Default is
		// the G1 Livox Mid-360 topic; on Go2 override to "rt/utlidar/cloud_deskewed".
		PointCloud: PointCloudConfig{
			ZenohEndpoint:  envStr("POINTCLOUD_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("POINTCLOUD_ZENOH_TOPIC", "rt/utlidar/cloud_livox_mid360"),
			TimestampsFile: filepath.Join(sessionDir, "pointcloud_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "pointcloud_frames.bin"),
		},
		Depth: DepthConfig{
			ZenohEndpoint:  envStr("DEPTH_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("DEPTH_ZENOH_TOPIC", "camera/realsense2_camera_node/depth/image_rect_raw"),
			TimestampsFile: filepath.Join(sessionDir, "depth_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "depth_frames.bin"),
		},
		// Odom:  basic foot-odometry by default ("odom").  Could be
		// overridden to a higher-quality LIO topic if available:
		//   Go2: "/lio_sam_ros2/mapping/odometry"
		//   G1:  "/unitree/slam_mapping/odom"
		// The /lowstate recording already contains raw IMU, so post-hoc
		// SLAM-quality odom can be computed offline if needed.
		Odom: OdomConfig{
			ZenohEndpoint:  envStr("ODOM_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("ODOM_ZENOH_TOPIC", "odom"),
			TimestampsFile: filepath.Join(sessionDir, "odom_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "odom_frames.bin"),
		},
		// Lowstate:  Same default topic ("rt/lowstate") for Go2 and G1.
		// The message schema differs internally but the recorder is robot-agnostic
		// (stores raw bytes).  Measured rates: Go2 ~500 Hz, G1 ~1053 Hz.
		Lowstate: LowstateConfig{
			ZenohEndpoint:  envStr("LOWSTATE_ZENOH_ENDPOINT", "tcp/127.0.0.1:7447"),
			ZenohTopic:     envStr("LOWSTATE_ZENOH_TOPIC", "rt/lowstate"),
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

// videoConfigs returns the configured video RTSP cameras.  Add another entry
// (or set its env var) to record additional streams such as Insta360 (set
// INSTA360_RTSP_URL after confirming the URL via:
//   ffprobe rtsp://localhost:8554/insta360 ).
func videoConfigs(sessionDir string) []VideoConfig {
	cameras := []struct {
		name       string
		defaultURL string
	}{
		{"top_camera", "rtsp://localhost:8554/top_camera_raw"},
		{"front_camera", "rtsp://localhost:8554/front_camera"},
		{"down_camera", "rtsp://localhost:8554/down_camera"},
		// {"insta360", "rtsp://localhost:8554/insta360"},   // uncomment when URL confirmed
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