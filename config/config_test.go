package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_defaults(t *testing.T) {
	for _, key := range []string{"RECORDINGS_DIR", "TOP_CAMERA_RTSP_URL", "FRONT_CAMERA_RTSP_URL", "DOWN_CAMERA_RTSP_URL", "AUDIO_RTSP_URL", "POINTCLOUD_ZENOH_ENDPOINT", "POINTCLOUD_ZENOH_TOPIC"} {
		t.Setenv(key, "")
	}

	cfg := Load()

	require.Len(t, cfg.Video, 3, "expected 3 cameras")
	require.Equal(t, "top_camera", cfg.Video[0].Name, "unexpected first camera name")
	require.Equal(t, "rtsp://localhost:8554/top_camera_raw", cfg.Video[0].RTSPURL, "unexpected top camera RTSP URL")
	require.Equal(t, "rtsp://localhost:8554/front_camera", cfg.Video[1].RTSPURL, "unexpected front camera RTSP URL")
	require.Equal(t, "rtsp://localhost:8554/down_camera", cfg.Video[2].RTSPURL, "unexpected down camera RTSP URL")
	require.Equal(t, "rtsp://localhost:8554/audio", cfg.Audio.RTSPURL, "unexpected audio RTSP URL")
	require.Equal(t, "tcp/127.0.0.1:7447", cfg.Lidar.ZenohEndpoint, "unexpected lidar zenoh endpoint")
	require.Equal(t, "scan", cfg.Lidar.ZenohTopic, "unexpected lidar zenoh topic")
	require.Equal(t, "tcp/127.0.0.1:7447", cfg.PointCloud.ZenohEndpoint, "unexpected point cloud zenoh endpoint")
	require.Equal(t, "rt/utlidar/cloud_livox_mid360", cfg.PointCloud.ZenohTopic, "unexpected point cloud zenoh topic")
	require.True(t, strings.HasPrefix(cfg.SessionDir, "recordings"), "session dir should be under recordings/, got: %s", cfg.SessionDir)
}

func TestLoad_pointCloudEnvOverrides(t *testing.T) {
	t.Setenv("POINTCLOUD_ZENOH_ENDPOINT", "tcp/192.168.1.10:7447")
	t.Setenv("POINTCLOUD_ZENOH_TOPIC", "rt/custom/cloud")

	cfg := Load()

	require.Equal(t, "tcp/192.168.1.10:7447", cfg.PointCloud.ZenohEndpoint, "unexpected point cloud zenoh endpoint")
	require.Equal(t, "rt/custom/cloud", cfg.PointCloud.ZenohTopic, "unexpected point cloud zenoh topic")
}

func TestLoad_pointCloudOutputFilesInsideSessionDir(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "recordings")

	cfg := Load()

	require.Equal(t, cfg.SessionDir, filepath.Dir(cfg.PointCloud.TimestampsFile), "point cloud timestamps file not in session dir: %s", cfg.PointCloud.TimestampsFile)
	require.Equal(t, "pointcloud_timestamps.csv", filepath.Base(cfg.PointCloud.TimestampsFile), "unexpected point cloud timestamps file name")
	require.Equal(t, cfg.SessionDir, filepath.Dir(cfg.PointCloud.DataFile), "point cloud data file not in session dir: %s", cfg.PointCloud.DataFile)
	require.Equal(t, "pointcloud_frames.bin", filepath.Base(cfg.PointCloud.DataFile), "unexpected point cloud data file name")
}

func TestPointCloudStreamConfig_mapsFields(t *testing.T) {
	cfg := PointCloudConfig{
		ZenohEndpoint:  "tcp/10.0.0.1:7447",
		ZenohTopic:     "rt/utlidar/cloud_livox_mid360",
		TimestampsFile: "/tmp/pointcloud_timestamps.csv",
		DataFile:       "/tmp/pointcloud_frames.bin",
	}

	sc := cfg.PointCloudStreamConfig()

	require.Equal(t, cfg.ZenohEndpoint, sc.ZenohEndpoint, "endpoint not mapped")
	require.Equal(t, cfg.ZenohTopic, sc.ZenohTopic, "topic not mapped")
	require.Equal(t, cfg.TimestampsFile, sc.TimestampsFile, "timestamps file not mapped")
	require.Equal(t, cfg.DataFile, sc.DataFile, "data file not mapped")
}

func TestLoad_envOverrides(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "/tmp/test-recordings")
	t.Setenv("TOP_CAMERA_RTSP_URL", "rtsp://cam.local:8554/cam0")
	t.Setenv("AUDIO_RTSP_URL", "rtsp://cam.local:8554/mic0")

	cfg := Load()

	require.Equal(t, "rtsp://cam.local:8554/cam0", cfg.Video[0].RTSPURL, "unexpected top camera RTSP URL")
	require.Equal(t, "rtsp://cam.local:8554/mic0", cfg.Audio.RTSPURL, "unexpected audio RTSP URL")
	require.True(t, strings.HasPrefix(cfg.SessionDir, "/tmp/test-recordings"), "session dir should be under /tmp/test-recordings/, got: %s", cfg.SessionDir)
}

func TestLoad_sessionDirLayout(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "recordings")

	cfg := Load()

	parts := strings.Split(filepath.ToSlash(cfg.SessionDir), "/")
	require.Len(t, parts, 3, "expected 3 path components: %s", cfg.SessionDir)
	dateDir := parts[1]
	sessionDir := parts[2]
	require.Len(t, dateDir, 10, "date directory has wrong length: %s", dateDir)
	require.Len(t, sessionDir, 19, "session directory has wrong length: %s", sessionDir)
}

func TestLoad_outputFilesInsideSessionDir(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "recordings")

	cfg := Load()

	require.Equal(t, cfg.SessionDir, filepath.Dir(cfg.Video[0].OutputFile), "video output file not in session dir: %s", cfg.Video[0].OutputFile)
	require.Equal(t, "top_camera.mp4", filepath.Base(cfg.Video[0].OutputFile), "unexpected video file name")
	require.Equal(t, "front_camera.mp4", filepath.Base(cfg.Video[1].OutputFile), "unexpected video file name")
	require.Equal(t, "down_camera.mp4", filepath.Base(cfg.Video[2].OutputFile), "unexpected video file name")
	require.Equal(t, cfg.SessionDir, filepath.Dir(cfg.Audio.OutputFile), "audio output file not in session dir: %s", cfg.Audio.OutputFile)
	require.Equal(t, "audio.ogg", filepath.Base(cfg.Audio.OutputFile), "unexpected audio file name")
}

func TestLoad_eachCallProducesUniqueSessionDir(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "recordings")

	cfg1 := Load()
	cfg2 := Load()

	require.NotEmpty(t, cfg1.SessionDir, "session dir must not be empty")
	require.NotEmpty(t, cfg2.SessionDir, "session dir must not be empty")
}

func TestEnvStr_emptyValueFallsBackToDefault(t *testing.T) {
	err := os.Unsetenv("_TEST_KEY_ABSENT")
	require.NoError(t, err, "could not unset env var")
	result := envStr("_TEST_KEY_ABSENT", "fallback")
	if result != "fallback" {
		t.Errorf("expected fallback, got %s", result)
	}
}

func TestEnvStr_setValueOverridesDefault(t *testing.T) {
	t.Setenv("_TEST_KEY_SET", "override")
	result := envStr("_TEST_KEY_SET", "fallback")
	if result != "override" {
		t.Errorf("expected override, got %s", result)
	}
}
