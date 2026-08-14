package config

import (
	"log/slog"
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

const defaultZenohEndpoint = "tcp/127.0.0.1:7447"

type Config struct {
	Collect          bool
	RobotType        RobotType
	EnableLidar      bool
	EnablePointCloud bool
	EnableDepth      bool
	EnableOdom       bool
	EnableLowstate   bool
	SessionDir       string
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
	RateHz         float64
	TimestampsFile string
	DataFile       string
}

type PointCloudConfig struct {
	ZenohEndpoint  string
	ZenohTopic     string
	RateHz         float64
	TimestampsFile string
	DataFile       string
}

type DepthConfig struct {
	ZenohEndpoint  string
	ZenohTopic     string
	RateHz         float64
	TimestampsFile string
	DataFile       string
}

type OdomConfig struct {
	ZenohEndpoint  string
	ZenohTopic     string
	RateHz         float64
	TimestampsFile string
	DataFile       string
}

type LowstateConfig struct {
	ZenohEndpoint  string
	ZenohTopic     string
	RateHz         float64
	TimestampsFile string
	DataFile       string
}

type NetworkConfig struct {
	PingHost     string
	PingTimeout  time.Duration
	PollInterval time.Duration
	DataFile     string
}

// RecordingsDir is the root under which sessions are created. The session
// package needs it before Load runs, since it decides the session directory
// name (which depends on whether the clock can be trusted yet).
func RecordingsDir() string {
	return envStr("RECORDINGS_DIR", "recordings")
}

// Load builds the recorder configuration for a session directory that the
// caller has already created.
//
// Every value resolves in the order: environment variable, then the robot
// profile (config/robots.yaml), then a built-in default. The session directory
// is passed in rather than derived from time.Now(): on a robot that booted
// without a network the clock can be days off, and a directory named from it
// would be wrong and frozen for the whole session. See internal/session.
func Load(sessionDir string) Config {
	robotType, profile := ResolveProfile()

	return Config{
		Collect:          envBool("ENABLE_COLLECTION", true),
		RobotType:        robotType,
		SessionDir:       sessionDir,
		EnableLidar:      topicEnabled(profile, TopicLidar, "ENABLE_LIDAR", "LIDAR_ZENOH_TOPIC"),
		EnablePointCloud: topicEnabled(profile, TopicPointCloud, "ENABLE_POINTCLOUD", "POINTCLOUD_ZENOH_TOPIC"),
		EnableDepth:      topicEnabled(profile, TopicDepth, "ENABLE_DEPTH", "DEPTH_ZENOH_TOPIC"),
		EnableOdom:       topicEnabled(profile, TopicOdom, "ENABLE_ODOM", "ODOM_ZENOH_TOPIC"),
		EnableLowstate:   topicEnabled(profile, TopicLowstate, "ENABLE_LOWSTATE", "LOWSTATE_ZENOH_TOPIC"),
		Video:            videoConfigs(profile, sessionDir),
		Audio: AudioConfig{
			// Audio is the audio track of the video-processor's muxed session
			// stream (single capture clock). ffmpeg selects it with -vn. Served
			// gst-direct on :8555 for the lowest-latency, capture-stamped source;
			// override to mediamtx (rtsp://localhost:8554/live) for an off-host
			// recorder.
			RTSPURL:        envStr(orDefault(profile.Audio.Env, "AUDIO_RTSP_URL"), profile.Audio.URL),
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
			ZenohEndpoint:  envStr("LIDAR_ZENOH_ENDPOINT", defaultZenohEndpoint),
			ZenohTopic:     topicKey(profile, TopicLidar, "LIDAR_ZENOH_TOPIC"),
			RateHz:         topicRate(profile, TopicLidar, 10),
			TimestampsFile: filepath.Join(sessionDir, "lidar_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "lidar_scans.bin"),
		},
		PointCloud: PointCloudConfig{
			ZenohEndpoint:  envStr("POINTCLOUD_ZENOH_ENDPOINT", defaultZenohEndpoint),
			ZenohTopic:     topicKey(profile, TopicPointCloud, "POINTCLOUD_ZENOH_TOPIC"),
			RateHz:         topicRate(profile, TopicPointCloud, 10),
			TimestampsFile: filepath.Join(sessionDir, "pointcloud_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "pointcloud_frames.bin"),
		},
		Depth: DepthConfig{
			ZenohEndpoint:  envStr("DEPTH_ZENOH_ENDPOINT", defaultZenohEndpoint),
			ZenohTopic:     topicKey(profile, TopicDepth, "DEPTH_ZENOH_TOPIC"),
			RateHz:         topicRate(profile, TopicDepth, 10),
			TimestampsFile: filepath.Join(sessionDir, "depth_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "depth_frames.bin"),
		},
		Odom: OdomConfig{
			ZenohEndpoint:  envStr("ODOM_ZENOH_ENDPOINT", defaultZenohEndpoint),
			ZenohTopic:     topicKey(profile, TopicOdom, "ODOM_ZENOH_TOPIC"),
			RateHz:         topicRate(profile, TopicOdom, 30),
			TimestampsFile: filepath.Join(sessionDir, "odom_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "odom_frames.bin"),
		},
		Lowstate: LowstateConfig{
			ZenohEndpoint:  envStr("LOWSTATE_ZENOH_ENDPOINT", defaultZenohEndpoint),
			ZenohTopic:     topicKey(profile, TopicLowstate, "LOWSTATE_ZENOH_TOPIC"),
			RateHz:         topicRate(profile, TopicLowstate, 400),
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

// topicKey resolves a topic's Zenoh key expression: environment variable first,
// then the profile.
func topicKey(p Profile, name, envKey string) string {
	t, _ := p.Topic(name)
	return envStr(envKey, t.Key)
}

// topicRate is the nominal publish rate, used only to size the stall detector.
func topicRate(p Profile, name string, fallback float64) float64 {
	if t, ok := p.Topic(name); ok && t.RateHz > 0 {
		return t.RateHz
	}
	return fallback
}

// topicEnabled decides whether a topic is recorded.
//
// A topic the profile does not declare cannot be enabled by its ENABLE_ flag
// alone -- there would be no key to subscribe to, and subscribing to "" records
// nothing while reporting success. Supplying the key by environment variable
// does enable it, which is how a robot gains a sensor its profile predates.
func topicEnabled(p Profile, name, enableEnv, keyEnv string) bool {
	t, declared := p.Topic(name)
	enabled := declared && t.Enabled

	switch os.Getenv(enableEnv) {
	case "false", "0", "no":
		return false
	case "true", "1", "yes":
		enabled = true
	}

	if enabled && topicKey(p, name, keyEnv) == "" {
		slog.Warn("topic enabled but no key configured; not recording",
			"topic", name, "hint", "set "+keyEnv+" or add it to the robot profile")
		return false
	}
	return enabled
}

// videoConfigs builds one recorder per camera in the profile.
func videoConfigs(p Profile, sessionDir string) []VideoConfig {
	configs := make([]VideoConfig, 0, len(p.Cameras))
	for _, cam := range p.Cameras {
		// The profile names the override variable so a camera can be renamed
		// on disk without breaking deployments that set the old one.
		envKey := orDefault(cam.Env, strings.ToUpper(cam.Name)+"_RTSP_URL")
		configs = append(configs, VideoConfig{
			Name:           cam.Name,
			RTSPURL:        envStr(envKey, cam.URL),
			OutputFile:     filepath.Join(sessionDir, cam.Name+".mp4"),
			TimestampsFile: filepath.Join(sessionDir, cam.Name+"_timestamps.csv"),
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

func orDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
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
