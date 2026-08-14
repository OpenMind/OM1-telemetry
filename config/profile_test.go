package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The embedded file is what every deployment gets when no override is mounted,
// so a typo in it is a shipping bug, not a runtime warning.
func TestEmbeddedProfiles_parse(t *testing.T) {
	profiles, err := parseProfiles(embeddedProfiles)
	require.NoError(t, err)
	require.Contains(t, profiles, RobotG1)
	require.Contains(t, profiles, RobotGo2)
}

func TestResolveProfile_g1(t *testing.T) {
	t.Setenv("ROBOT_PROFILES_FILE", "")
	t.Setenv("ROBOT_TYPE", "g1")

	rt, p := ResolveProfile()

	require.Equal(t, RobotG1, rt)
	require.Len(t, p.Cameras, 2)

	pc, ok := p.Topic(TopicPointCloud)
	require.True(t, ok, "G1 records the Livox MID360")
	require.True(t, pc.Enabled)
	require.Equal(t, "rt/utlidar/cloud_livox_mid360", pc.Key)

	d, ok := p.Topic(TopicDepth)
	require.True(t, ok, "depth stays documented so it can be switched on when hardware is fitted")
	require.False(t, d.Enabled)
}

func TestResolveProfile_go2HasNoPointCloudEntry(t *testing.T) {
	t.Setenv("ROBOT_PROFILES_FILE", "")
	t.Setenv("ROBOT_TYPE", "go2")

	rt, p := ResolveProfile()

	require.Equal(t, RobotGo2, rt)
	_, ok := p.Topic(TopicPointCloud)
	require.False(t, ok, "the Go2 publishes no point cloud; the placeholder topic was removed")
}

func TestResolveProfile_isCaseInsensitiveAndTrimmed(t *testing.T) {
	t.Setenv("ROBOT_PROFILES_FILE", "")
	t.Setenv("ROBOT_TYPE", "  G1  ")

	rt, _ := ResolveProfile()

	require.Equal(t, RobotG1, rt)
}

func TestResolveProfile_unsetFallsBackToDefault(t *testing.T) {
	t.Setenv("ROBOT_PROFILES_FILE", "")
	t.Setenv("ROBOT_TYPE", "")

	rt, p := ResolveProfile()

	require.Equal(t, DefaultRobotType, rt)
	require.NotEmpty(t, p.Cameras)
}

func TestResolveProfile_unknownFallsBackToDefault(t *testing.T) {
	t.Setenv("ROBOT_PROFILES_FILE", "")
	t.Setenv("ROBOT_TYPE", "spot")

	rt, p := ResolveProfile()

	require.Equal(t, DefaultRobotType, rt)
	require.NotEmpty(t, p.Cameras)
}

// Changing what a robot records should not need a rebuild.
func TestResolveProfile_fileOverridesEmbedded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "robots.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
robots:
  g1:
    cameras:
      - name: only_camera
        url: rtsp://example/one
    audio:
      url: rtsp://example/audio
    topics:
      lidar:
        enabled: true
        key: custom/scan
        rate_hz: 5
`), 0o644))

	t.Setenv("ROBOT_PROFILES_FILE", path)
	t.Setenv("ROBOT_TYPE", "g1")

	_, p := ResolveProfile()

	require.Len(t, p.Cameras, 1)
	require.Equal(t, "only_camera", p.Cameras[0].Name)

	l, ok := p.Topic(TopicLidar)
	require.True(t, ok)
	require.Equal(t, "custom/scan", l.Key)
	require.Equal(t, 5.0, l.RateHz)
}

// A bad mount must not stop a robot from recording.
func TestResolveProfile_unreadableFileFallsBackToEmbedded(t *testing.T) {
	t.Setenv("ROBOT_PROFILES_FILE", filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	t.Setenv("ROBOT_TYPE", "g1")

	_, p := ResolveProfile()

	require.Len(t, p.Cameras, 2, "fell back to the embedded G1 profile")
}

func TestResolveProfile_malformedFileFallsBackToEmbedded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "robots.yaml")
	require.NoError(t, os.WriteFile(path, []byte("robots: [this is not a map"), 0o644))

	t.Setenv("ROBOT_PROFILES_FILE", path)
	t.Setenv("ROBOT_TYPE", "g1")

	_, p := ResolveProfile()

	require.Len(t, p.Cameras, 2)
}

func TestParseProfiles_rejectsIncompleteEntries(t *testing.T) {
	_, err := parseProfiles([]byte("robots:\n  g1:\n    cameras:\n      - url: rtsp://x\n"))
	require.Error(t, err, "a camera with no name has no file to write to")

	_, err = parseProfiles([]byte("robots:\n  g1:\n    cameras:\n      - name: c\n"))
	require.Error(t, err, "a camera with no url has nothing to record")

	_, err = parseProfiles([]byte("robots:\n  g1:\n    topics:\n      lidar:\n        enabled: true\n"))
	require.Error(t, err, "a topic with no key would subscribe to nothing")

	_, err = parseProfiles([]byte("robots: {}\n"))
	require.Error(t, err, "an empty profile set is a broken file, not a valid one")
}
