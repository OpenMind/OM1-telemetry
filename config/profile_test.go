package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveProfile_go2(t *testing.T) {
	t.Setenv("ROBOT_TYPE", "go2")

	rt, p := ResolveProfile()

	require.Equal(t, RobotGo2, rt)
	require.True(t, p.EnableLidar, "Go2 records 2D lidar")
	require.False(t, p.EnablePointCloud, "Go2 has no 3D point cloud")
	require.Equal(t, "rt/scan", p.LidarTopic)
}

func TestResolveProfile_g1(t *testing.T) {
	t.Setenv("ROBOT_TYPE", "g1")

	rt, p := ResolveProfile()

	require.Equal(t, RobotG1, rt)
	require.True(t, p.EnableLidar, "G1 records 2D lidar")
	require.True(t, p.EnablePointCloud, "G1 records the 3D point cloud")
	require.Equal(t, "rt/utlidar/cloud_livox_mid360", p.PointCloudTopic)
}

func TestResolveProfile_isCaseInsensitiveAndTrimmed(t *testing.T) {
	t.Setenv("ROBOT_TYPE", "  G1  ")

	rt, _ := ResolveProfile()

	require.Equal(t, RobotG1, rt)
}

func TestResolveProfile_unsetFallsBackToDefault(t *testing.T) {
	t.Setenv("ROBOT_TYPE", "")

	rt, p := ResolveProfile()

	require.Equal(t, DefaultRobotType, rt)
	require.Equal(t, profiles[DefaultRobotType], p)
}

func TestResolveProfile_unknownFallsBackToDefault(t *testing.T) {
	t.Setenv("ROBOT_TYPE", "spot")

	rt, p := ResolveProfile()

	require.Equal(t, DefaultRobotType, rt)
	require.Equal(t, profiles[DefaultRobotType], p)
}

func TestLoad_robotTypeSelectsProfile(t *testing.T) {
	for _, key := range []string{"ENABLE_LIDAR", "ENABLE_POINTCLOUD", "LIDAR_DDS_TOPIC", "POINTCLOUD_DDS_TOPIC"} {
		t.Setenv(key, "")
	}

	t.Setenv("ROBOT_TYPE", "go2")
	go2 := Load(testSessionDir)
	require.Equal(t, RobotGo2, go2.RobotType)
	require.True(t, go2.EnableLidar)
	require.False(t, go2.EnablePointCloud)
	require.Equal(t, "rt/utlidar/cloud_deskewed", go2.PointCloud.DDSTopic)
	require.Equal(t, "go2", go2.Lowstate.RobotType)

	t.Setenv("ROBOT_TYPE", "g1")
	g1 := Load(testSessionDir)
	require.Equal(t, RobotG1, g1.RobotType)
	require.True(t, g1.EnableLidar)
	require.True(t, g1.EnablePointCloud)
	require.Equal(t, "rt/utlidar/cloud_livox_mid360", g1.PointCloud.DDSTopic)
	require.Equal(t, "g1", g1.Lowstate.RobotType)
}

func TestLoad_envOverridesProfileEnableFlags(t *testing.T) {
	t.Setenv("ROBOT_TYPE", "go2") // profile would disable point cloud
	t.Setenv("ENABLE_POINTCLOUD", "true")
	t.Setenv("ENABLE_LIDAR", "false")

	cfg := Load(testSessionDir)

	require.True(t, cfg.EnablePointCloud, "ENABLE_POINTCLOUD env should override profile")
	require.False(t, cfg.EnableLidar, "ENABLE_LIDAR env should override profile")
}

// Cameras belong to the profile because a G1 and a Go2 do not have the same
// ones, and recording a camera the robot lacks costs a reconnect every two
// seconds plus a 0-byte mp4 each time.
func TestResolveProfile_camerasArePerRobot(t *testing.T) {
	t.Setenv("ROBOT_TYPE", "g1")
	_, g1 := ResolveProfile()
	require.Len(t, g1.Cameras, 3)
	require.Equal(t, "front_camera_raw", g1.Cameras[0].Name)
	require.Equal(t, "rear_camera_raw", g1.Cameras[1].Name)
	require.Equal(t, "down_camera", g1.Cameras[2].Name)

	t.Setenv("ROBOT_TYPE", "go2")
	_, go2 := ResolveProfile()
	require.Len(t, go2.Cameras, 3)
	require.Equal(t, "top_camera", go2.Cameras[0].Name)

	require.NotEqual(t, g1.Cameras, go2.Cameras)
}
