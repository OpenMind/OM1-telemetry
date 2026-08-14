package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const testSessionDir = "/tmp/session"

// clearEnv unsets every variable that can override a profile, so a test sees
// the profile's own values.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"RECORDINGS_DIR", "ROBOT_PROFILES_FILE",
		"TOP_CAMERA_RTSP_URL", "FRONT_CAMERA_RTSP_URL", "DOWN_CAMERA_RTSP_URL",
		"FRONT_CAMERA_RAW_RTSP_URL", "REAR_CAMERA_RAW_RTSP_URL", "AUDIO_RTSP_URL",
		"ENABLE_LIDAR", "ENABLE_POINTCLOUD", "ENABLE_DEPTH", "ENABLE_ODOM", "ENABLE_LOWSTATE",
		"LIDAR_ZENOH_TOPIC", "POINTCLOUD_ZENOH_TOPIC", "DEPTH_ZENOH_TOPIC",
		"ODOM_ZENOH_TOPIC", "LOWSTATE_ZENOH_TOPIC",
		"LIDAR_ZENOH_ENDPOINT", "POINTCLOUD_ZENOH_ENDPOINT", "DEPTH_ZENOH_ENDPOINT",
		"ODOM_ZENOH_ENDPOINT", "LOWSTATE_ZENOH_ENDPOINT",
	} {
		t.Setenv(key, "")
	}
}

func TestLoad_g1RecordsBothRawCameras(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotG1))

	cfg := Load(testSessionDir)

	require.Len(t, cfg.Video, 2, "G1 records the front and rear raw streams")
	require.Equal(t, "front_camera_raw", cfg.Video[0].Name)
	require.Equal(t, "rtsp://localhost:8556/raw", cfg.Video[0].RTSPURL)
	require.Equal(t, "rear_camera_raw", cfg.Video[1].Name)
	require.Equal(t, "rtsp://localhost:8558/raw", cfg.Video[1].RTSPURL,
		"the rear camera is served gst-direct on 8558, not via mediamtx")
	require.Equal(t, "rtsp://localhost:8555/live", cfg.Audio.RTSPURL)
}

func TestLoad_g1TopicsMatchHardware(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotG1))

	cfg := Load(testSessionDir)

	require.True(t, cfg.EnableLidar)
	require.True(t, cfg.EnablePointCloud)
	require.Equal(t, "rt/utlidar/cloud_livox_mid360", cfg.PointCloud.ZenohTopic)
	require.True(t, cfg.EnableOdom)
	require.True(t, cfg.EnableLowstate)
	require.False(t, cfg.EnableDepth,
		"the G1 has no RealSense; recording depth produced a 0-byte file every session")
}

func TestLoad_go2KeepsItsThreeCameras(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotGo2))

	cfg := Load(testSessionDir)

	require.Len(t, cfg.Video, 3)
	require.Equal(t, "top_camera", cfg.Video[0].Name)
	require.Equal(t, "rtsp://localhost:8556/raw", cfg.Video[0].RTSPURL)
	require.Equal(t, "rtsp://localhost:8554/front_camera", cfg.Video[1].RTSPURL)
	require.Equal(t, "rtsp://localhost:8554/down_camera", cfg.Video[2].RTSPURL)
}

// The Go2 profile declares no point cloud at all, because the robot does not
// publish one. Flipping the enable flag must not resurrect a subscription to a
// topic that was only ever a placeholder.
func TestLoad_go2PointCloudCannotBeEnabledWithoutATopic(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotGo2))

	cfg := Load(testSessionDir)
	require.False(t, cfg.EnablePointCloud)
	require.Empty(t, cfg.PointCloud.ZenohTopic)

	t.Setenv("ENABLE_POINTCLOUD", "true")
	cfg = Load(testSessionDir)
	require.False(t, cfg.EnablePointCloud,
		"enabling a topic with no key would subscribe to \"\" and record nothing while reporting success")
}

// Supplying the key alongside the flag is the supported way to record a sensor
// the profile predates.
func TestLoad_topicEnabledWhenKeySuppliedByEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotGo2))
	t.Setenv("ENABLE_POINTCLOUD", "true")
	t.Setenv("POINTCLOUD_ZENOH_TOPIC", "rt/custom/cloud")

	cfg := Load(testSessionDir)

	require.True(t, cfg.EnablePointCloud)
	require.Equal(t, "rt/custom/cloud", cfg.PointCloud.ZenohTopic)
}

func TestLoad_envDisablesAProfileTopic(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotG1))
	t.Setenv("ENABLE_LIDAR", "false")

	cfg := Load(testSessionDir)

	require.False(t, cfg.EnableLidar)
}

func TestLoad_envEnablesDepthOnAG1ThatHasOne(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotG1))
	t.Setenv("ENABLE_DEPTH", "true")

	cfg := Load(testSessionDir)

	require.True(t, cfg.EnableDepth, "a G1 fitted with a RealSense enables it without a rebuild")
	require.Equal(t, "camera/realsense2_camera_node/depth/image_rect_raw", cfg.Depth.ZenohTopic)
}

// The profile names each camera's override variable, so the historical names
// keep working even though the G1's cameras were renamed.
func TestLoad_cameraURLEnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotG1))
	t.Setenv("REAR_CAMERA_RAW_RTSP_URL", "rtsp://cam.local:8554/rear")

	cfg := Load(testSessionDir)

	require.Equal(t, "rtsp://cam.local:8554/rear", cfg.Video[1].RTSPURL)
}

func TestLoad_go2CameraEnvNamesUnchanged(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotGo2))
	t.Setenv("TOP_CAMERA_RTSP_URL", "rtsp://cam.local:8554/cam0")

	cfg := Load(testSessionDir)

	require.Equal(t, "rtsp://cam.local:8554/cam0", cfg.Video[0].RTSPURL)
}

func TestLoad_outputFilesInsideSessionDir(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotG1))

	cfg := Load(testSessionDir)

	require.Equal(t, testSessionDir, filepath.Dir(cfg.Video[0].OutputFile))
	require.Equal(t, "front_camera_raw.mp4", filepath.Base(cfg.Video[0].OutputFile))
	require.Equal(t, "rear_camera_raw.mp4", filepath.Base(cfg.Video[1].OutputFile))
	require.Equal(t, "audio.ogg", filepath.Base(cfg.Audio.OutputFile))
	require.Equal(t, testSessionDir, filepath.Dir(cfg.PointCloud.DataFile))
	require.Equal(t, "pointcloud_frames.bin", filepath.Base(cfg.PointCloud.DataFile))
}

func TestLoad_zenohEndpointDefaultsAndOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotG1))

	cfg := Load(testSessionDir)
	require.Equal(t, "tcp/127.0.0.1:7447", cfg.Lidar.ZenohEndpoint)

	t.Setenv("POINTCLOUD_ZENOH_ENDPOINT", "tcp/192.168.1.10:7447")
	cfg = Load(testSessionDir)
	require.Equal(t, "tcp/192.168.1.10:7447", cfg.PointCloud.ZenohEndpoint)
}

func TestLoad_heartbeatRatesComeFromProfile(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBOT_TYPE", string(RobotG1))

	cfg := Load(testSessionDir)

	require.Equal(t, 400.0, cfg.Lowstate.RateHz)
	require.Equal(t, 10.0, cfg.Lidar.RateHz)
	require.Equal(t, 30.0, cfg.Odom.RateHz)
}

func TestRecordingsDir_defaultAndOverride(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "")
	require.Equal(t, "recordings", RecordingsDir())

	t.Setenv("RECORDINGS_DIR", "/tmp/test-recordings")
	require.Equal(t, "/tmp/test-recordings", RecordingsDir())
}

func TestEnvStr_emptyValueFallsBackToDefault(t *testing.T) {
	err := os.Unsetenv("_TEST_KEY_ABSENT")
	require.NoError(t, err)
	require.Equal(t, "fallback", envStr("_TEST_KEY_ABSENT", "fallback"))
}

func TestEnvStr_setValueOverridesDefault(t *testing.T) {
	t.Setenv("_TEST_KEY_SET", "override")
	require.Equal(t, "override", envStr("_TEST_KEY_SET", "fallback"))
}
