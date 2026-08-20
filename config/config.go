package config

import (
	"os"
	"path/filepath"
	"strconv"
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
	"om1-telemetry/internal/upload"
	"om1-telemetry/internal/video"
)

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
	// SessionRotateInterval, when positive, closes the current session and
	// opens a fresh one on this cadence, so each segment can be uploaded
	// (see Upload) without waiting for the whole run to end. Zero disables
	// rotation -- one session for the life of the process, as before.
	SessionRotateInterval time.Duration
	Upload                UploadConfig
	Retention             RetentionConfig
}

// RetentionConfig bounds how much local disk RecordingsDir may occupy, and
// how often the catch-up-upload / disk-cap sweep runs (see cmd/main's
// retentionSweep). It exists alongside per-segment upload because that path
// only ever handles the segment that just rotated or the process's final
// segment -- it does not retry a segment whose upload failed, nor reclaim
// space once a robot has been recording for a long time.
type RetentionConfig struct {
	// MaxBytes is the ceiling for RecordingsDir's total size once already-
	// uploaded sessions become eligible for deletion. Zero disables cap
	// enforcement (the catch-up upload sweep still runs).
	MaxBytes int64
	// SweepInterval is how often the sweep looks for finished-but-not-yet-
	// uploaded sessions (uploading the oldest first) and, if MaxBytes is
	// exceeded, deletes already-uploaded sessions, oldest first.
	SweepInterval time.Duration
}

// UploadConfig configures uploading finished session directories to the
// openmind-api (see internal/upload).
type UploadConfig struct {
	// Enabled is the operator's intent; Ready additionally requires BaseURL
	// and APIKey to actually be set, so a deployment that forgets to
	// configure the API falls back to recording-only instead of crashing.
	Enabled bool
	BaseURL string
	APIKey  string
	// DeleteAfterUpload removes a session's local files once it has uploaded
	// successfully. Off by default: a robot's disk is cheap compared to
	// losing data to a bug in a brand-new upload path.
	DeleteAfterUpload  bool
	MultipartThreshold int64
	PartSize           int64
	// Concurrency caps how many of a session's files upload at once.
	Concurrency int
}

// Ready reports whether upload is both requested and fully configured.
func (c UploadConfig) Ready() bool {
	return c.Enabled && c.BaseURL != "" && c.APIKey != ""
}

func (c UploadConfig) ClientConfig() upload.Config {
	return upload.Config{
		BaseURL:            c.BaseURL,
		APIKey:             c.APIKey,
		MultipartThreshold: c.MultipartThreshold,
		PartSize:           c.PartSize,
		Concurrency:        c.Concurrency,
	}
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
	DDSDomainID    uint32
	DDSTopic       string
	TimestampsFile string
	DataFile       string
}

type PointCloudConfig struct {
	DDSDomainID    uint32
	DDSTopic       string
	TimestampsFile string
	DataFile       string
}

type DepthConfig struct {
	DDSDomainID    uint32
	DDSTopic       string
	TimestampsFile string
	DataFile       string
}

type OdomConfig struct {
	DDSDomainID    uint32
	DDSTopic       string
	TimestampsFile string
	DataFile       string
}

type LowstateConfig struct {
	// RobotType selects the LowState DDS schema: "go2" (unitree_go/msg/
	// LowState) or "g1" (unitree_hg/msg/LowState).
	RobotType      string
	DDSDomainID    uint32
	DDSTopic       string
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
// package needs it before Load runs, because it decides the session directory
// name -- which depends on whether the clock can be trusted yet.
func RecordingsDir() string {
	return envStr("RECORDINGS_DIR", "recordings")
}

// Load builds the recorder configuration for a session directory the caller
// has already created.
//
// The directory is passed in rather than derived from time.Now(): a robot that
// booted without a network can have a wall clock days off, and a directory
// named from it would be wrong and frozen for the whole session. See
// internal/session.
func Load(sessionDir string) Config {
	robotType, profile := ResolveProfile()

	return Config{
		Collect:          envBool("ENABLE_COLLECTION", true),
		RobotType:        robotType,
		EnableLidar:      envBool("ENABLE_LIDAR", profile.EnableLidar),
		EnablePointCloud: envBool("ENABLE_POINTCLOUD", profile.EnablePointCloud),
		EnableDepth:      envBool("ENABLE_DEPTH", profile.EnableDepth),
		EnableOdom:       envBool("ENABLE_ODOM", profile.EnableOdom),
		EnableLowstate:   envBool("ENABLE_LOWSTATE", profile.EnableLowstate),
		SessionDir:       sessionDir,
		Video:            videoConfigs(profile, sessionDir),
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
			DDSDomainID:    envUint32("LIDAR_DDS_DOMAIN", 0),
			DDSTopic:       envStr("LIDAR_DDS_TOPIC", profile.LidarTopic),
			TimestampsFile: filepath.Join(sessionDir, "lidar_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "lidar_scans.bin"),
		},
		PointCloud: PointCloudConfig{
			DDSDomainID:    envUint32("POINTCLOUD_DDS_DOMAIN", 0),
			DDSTopic:       envStr("POINTCLOUD_DDS_TOPIC", profile.PointCloudTopic),
			TimestampsFile: filepath.Join(sessionDir, "pointcloud_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "pointcloud_frames.bin"),
		},
		Depth: DepthConfig{
			DDSDomainID:    envUint32("DEPTH_DDS_DOMAIN", 0),
			DDSTopic:       envStr("DEPTH_DDS_TOPIC", profile.DepthTopic),
			TimestampsFile: filepath.Join(sessionDir, "depth_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "depth_frames.bin"),
		},
		Odom: OdomConfig{
			DDSDomainID:    envUint32("ODOM_DDS_DOMAIN", 0),
			DDSTopic:       envStr("ODOM_DDS_TOPIC", profile.OdomTopic),
			TimestampsFile: filepath.Join(sessionDir, "odom_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "odom_frames.bin"),
		},
		Lowstate: LowstateConfig{
			RobotType:      string(robotType),
			DDSDomainID:    envUint32("LOWSTATE_DDS_DOMAIN", 0),
			DDSTopic:       envStr("LOWSTATE_DDS_TOPIC", profile.LowstateTopic),
			TimestampsFile: filepath.Join(sessionDir, "lowstate_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "lowstate_frames.bin"),
		},
		Network: NetworkConfig{
			PingHost:     envStr("NET_PING_HOST", "8.8.8.8"),
			PingTimeout:  envDuration("NET_PING_TIMEOUT", 2*time.Second),
			PollInterval: envDuration("NET_POLL_INTERVAL", 5*time.Second),
			DataFile:     filepath.Join(sessionDir, "network_status.csv"),
		},
		SessionRotateInterval: envDuration("SESSION_ROTATE_INTERVAL", 5*time.Minute),
		Upload: UploadConfig{
			Enabled:            envBool("UPLOAD_ENABLED", true),
			BaseURL:            strings.TrimSuffix(envStr("OPENMIND_API_URL", ""), "/"),
			APIKey:             envStr("OPENMIND_API_KEY", ""),
			DeleteAfterUpload:  envBool("DELETE_AFTER_UPLOAD", false),
			MultipartThreshold: envInt64("UPLOAD_MULTIPART_THRESHOLD_BYTES", upload.DefaultMultipartThreshold),
			PartSize:           envInt64("UPLOAD_PART_SIZE_BYTES", upload.DefaultPartSize),
			Concurrency:        int(envInt64("UPLOAD_CONCURRENCY", upload.DefaultConcurrency)),
		},
		Retention: RetentionConfig{
			MaxBytes:      envInt64("RETENTION_MAX_BYTES", 100*1024*1024*1024),
			SweepInterval: envDuration("RETENTION_SWEEP_INTERVAL", 5*time.Minute),
		},
	}
}

func videoConfigs(p Profile, sessionDir string) []VideoConfig {
	configs := make([]VideoConfig, 0, len(p.Cameras))
	for _, cam := range p.Cameras {
		envKey := cam.Env
		if envKey == "" {
			envKey = strings.ToUpper(cam.Name) + "_RTSP_URL"
		}
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
		DDSDomainID:    c.DDSDomainID,
		DDSTopic:       c.DDSTopic,
		TimestampsFile: c.TimestampsFile,
		DataFile:       c.DataFile,
	}
}

func (c PointCloudConfig) PointCloudStreamConfig() pointcloud.Config {
	return pointcloud.Config{
		DDSDomainID:    c.DDSDomainID,
		DDSTopic:       c.DDSTopic,
		TimestampsFile: c.TimestampsFile,
		DataFile:       c.DataFile,
	}
}

func (c DepthConfig) DepthStreamConfig() depth.Config {
	return depth.Config{
		DDSDomainID:    c.DDSDomainID,
		DDSTopic:       c.DDSTopic,
		TimestampsFile: c.TimestampsFile,
		DataFile:       c.DataFile,
	}
}

func (c OdomConfig) OdomStreamConfig() odom.Config {
	return odom.Config{
		DDSDomainID:    c.DDSDomainID,
		DDSTopic:       c.DDSTopic,
		TimestampsFile: c.TimestampsFile,
		DataFile:       c.DataFile,
	}
}

func (c LowstateConfig) LowstateStreamConfig() lowstate.Config {
	return lowstate.Config{
		RobotType:      c.RobotType,
		DDSDomainID:    c.DDSDomainID,
		DDSTopic:       c.DDSTopic,
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

func envUint32(key string, defaultValue uint32) uint32 {
	if value := os.Getenv(key); value != "" {
		if v, err := strconv.ParseUint(value, 10, 32); err == nil {
			return uint32(v)
		}
	}
	return defaultValue
}

func envInt64(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			return v
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
