package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const testSessionDir = "/tmp/session"

func TestLoad_defaults(t *testing.T) {
	for _, key := range []string{"RECORDINGS_DIR", "TOP_CAMERA_RTSP_URL", "FRONT_CAMERA_RTSP_URL", "DOWN_CAMERA_RTSP_URL", "AUDIO_RTSP_URL", "POINTCLOUD_DDS_DOMAIN", "POINTCLOUD_DDS_TOPIC"} {
		t.Setenv(key, "")
	}
	t.Setenv("ROBOT_TYPE", string(RobotG1))

	cfg := Load(testSessionDir)

	require.Len(t, cfg.Video, 3, "G1 records front raw, rear raw, and the RealSense RGB down view")
	require.Equal(t, "front_camera_raw", cfg.Video[0].Name)
	require.Equal(t, "rtsp://localhost:8556/raw", cfg.Video[0].RTSPURL)
	require.Equal(t, "rear_camera_raw", cfg.Video[1].Name)
	require.Equal(t, "rtsp://localhost:8558/raw", cfg.Video[1].RTSPURL,
		"the rear camera is served gst-direct on 8558, not via mediamtx")
	require.Equal(t, "down_camera", cfg.Video[2].Name)
	require.Equal(t, "rtsp://localhost:8555/live", cfg.Audio.RTSPURL, "unexpected audio RTSP URL")
	require.Equal(t, uint32(0), cfg.Lidar.DDSDomainID, "unexpected lidar dds domain")
	require.Equal(t, "rt/scan", cfg.Lidar.DDSTopic, "unexpected lidar dds topic")
	require.Equal(t, uint32(0), cfg.PointCloud.DDSDomainID, "unexpected point cloud dds domain")
	require.Equal(t, "rt/utlidar/cloud_livox_mid360", cfg.PointCloud.DDSTopic, "unexpected point cloud dds topic")
}

func TestLoad_pointCloudEnvOverrides(t *testing.T) {
	t.Setenv("POINTCLOUD_DDS_DOMAIN", "5")
	t.Setenv("POINTCLOUD_DDS_TOPIC", "rt/custom/cloud")

	cfg := Load(testSessionDir)

	require.Equal(t, uint32(5), cfg.PointCloud.DDSDomainID, "unexpected point cloud dds domain")
	require.Equal(t, "rt/custom/cloud", cfg.PointCloud.DDSTopic, "unexpected point cloud dds topic")
}

func TestLoad_pointCloudOutputFilesInsideSessionDir(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "recordings")

	cfg := Load(testSessionDir)

	require.Equal(t, cfg.SessionDir, filepath.Dir(cfg.PointCloud.TimestampsFile), "point cloud timestamps file not in session dir: %s", cfg.PointCloud.TimestampsFile)
	require.Equal(t, "pointcloud_timestamps.csv", filepath.Base(cfg.PointCloud.TimestampsFile), "unexpected point cloud timestamps file name")
	require.Equal(t, cfg.SessionDir, filepath.Dir(cfg.PointCloud.DataFile), "point cloud data file not in session dir: %s", cfg.PointCloud.DataFile)
	require.Equal(t, "pointcloud_frames.bin", filepath.Base(cfg.PointCloud.DataFile), "unexpected point cloud data file name")
}

func TestPointCloudStreamConfig_mapsFields(t *testing.T) {
	cfg := PointCloudConfig{
		DDSDomainID:    3,
		DDSTopic:       "rt/utlidar/cloud_livox_mid360",
		TimestampsFile: "/tmp/pointcloud_timestamps.csv",
		DataFile:       "/tmp/pointcloud_frames.bin",
	}

	sc := cfg.PointCloudStreamConfig()

	require.Equal(t, cfg.DDSDomainID, sc.DDSDomainID, "domain not mapped")
	require.Equal(t, cfg.DDSTopic, sc.DDSTopic, "topic not mapped")
	require.Equal(t, cfg.TimestampsFile, sc.TimestampsFile, "timestamps file not mapped")
	require.Equal(t, cfg.DataFile, sc.DataFile, "data file not mapped")
}

func TestLoad_envOverrides(t *testing.T) {
	// Cameras are per-robot now, so a test naming one must pin the robot.
	t.Setenv("ROBOT_TYPE", string(RobotGo2))
	t.Setenv("RECORDINGS_DIR", "/tmp/test-recordings")
	t.Setenv("TOP_CAMERA_RTSP_URL", "rtsp://cam.local:8554/cam0")
	t.Setenv("AUDIO_RTSP_URL", "rtsp://cam.local:8554/mic0")

	cfg := Load(testSessionDir)

	require.Equal(t, "rtsp://cam.local:8554/cam0", cfg.Video[0].RTSPURL, "unexpected top camera RTSP URL")
	require.Equal(t, "rtsp://cam.local:8554/mic0", cfg.Audio.RTSPURL, "unexpected audio RTSP URL")
}

func TestLoad_outputFilesInsideSessionDir(t *testing.T) {
	// Cameras are per-robot now, so a test naming one must pin the robot.
	t.Setenv("ROBOT_TYPE", string(RobotGo2))
	t.Setenv("RECORDINGS_DIR", "recordings")

	cfg := Load(testSessionDir)

	require.Equal(t, cfg.SessionDir, filepath.Dir(cfg.Video[0].OutputFile), "video output file not in session dir: %s", cfg.Video[0].OutputFile)
	require.Equal(t, "top_camera.mp4", filepath.Base(cfg.Video[0].OutputFile), "unexpected video file name")
	require.Equal(t, "front_camera.mp4", filepath.Base(cfg.Video[1].OutputFile), "unexpected video file name")
	require.Equal(t, "down_camera.mp4", filepath.Base(cfg.Video[2].OutputFile), "unexpected video file name")
	require.Equal(t, cfg.SessionDir, filepath.Dir(cfg.Audio.OutputFile), "audio output file not in session dir: %s", cfg.Audio.OutputFile)
	require.Equal(t, "audio.ogg", filepath.Base(cfg.Audio.OutputFile), "unexpected audio file name")
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

// The Go2's cameras are unchanged; only the G1's differ.
func TestLoad_go2CameraSet(t *testing.T) {
	for _, key := range []string{"TOP_CAMERA_RTSP_URL", "FRONT_CAMERA_RTSP_URL", "DOWN_CAMERA_RTSP_URL"} {
		t.Setenv(key, "")
	}
	t.Setenv("ROBOT_TYPE", string(RobotGo2))

	cfg := Load(testSessionDir)

	require.Len(t, cfg.Video, 3)
	require.Equal(t, "top_camera", cfg.Video[0].Name)
	require.Equal(t, "rtsp://localhost:8556/raw", cfg.Video[0].RTSPURL)
	require.Equal(t, "rtsp://localhost:8554/front_camera", cfg.Video[1].RTSPURL)
	require.Equal(t, "rtsp://localhost:8554/down_camera", cfg.Video[2].RTSPURL)
}

// The profile names each camera's override variable, so historical names keep
// working even though the G1's cameras were renamed.
func TestLoad_cameraURLEnvOverride(t *testing.T) {
	t.Setenv("ROBOT_TYPE", string(RobotG1))
	t.Setenv("REAR_CAMERA_RAW_RTSP_URL", "rtsp://cam.local:8554/rear")

	cfg := Load(testSessionDir)

	require.Equal(t, "rtsp://cam.local:8554/rear", cfg.Video[1].RTSPURL)
}
