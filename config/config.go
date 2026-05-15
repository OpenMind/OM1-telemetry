package config

import (
	"os"
	"path/filepath"
	"time"

	"om1-telemetry/internal/audio"
	"om1-telemetry/internal/lidar"
	"om1-telemetry/internal/video"
)

type Config struct {
	SessionDir     string
	SessionStartNs int64
	Video          VideoConfig
	Audio          AudioConfig
	Lidar          LidarConfig
}

type VideoConfig struct {
	RTSPURL    string
	OutputFile string
}

type AudioConfig struct {
	RTSPURL    string
	OutputFile string
}

type LidarConfig struct {
	ZenohTopic     string
	TimestampsFile string
	DataFile       string
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
		SessionDir:     sessionDir,
		SessionStartNs: now.UnixNano(),
		Video: VideoConfig{
			RTSPURL:    envStr("VIDEO_RTSP_URL", "rtsp://localhost:8554/live"),
			OutputFile: filepath.Join(sessionDir, "video.mp4"),
		},
		Audio: AudioConfig{
			RTSPURL:    envStr("AUDIO_RTSP_URL", "rtsp://localhost:8554/audio"),
			OutputFile: filepath.Join(sessionDir, "audio.wav"),
		},
		Lidar: LidarConfig{
			ZenohTopic:     envStr("LIDAR_ZENOH_TOPIC", "/scan"),
			TimestampsFile: filepath.Join(sessionDir, "lidar_timestamps.csv"),
			DataFile:       filepath.Join(sessionDir, "lidar_scans.bin"),
		},
	}
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
		ZenohTopic:     c.ZenohTopic,
		TimestampsFile: c.TimestampsFile,
		DataFile:       c.DataFile,
	}
}

func envStr(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
