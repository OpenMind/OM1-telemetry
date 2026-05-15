package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_defaults(t *testing.T) {
	for _, key := range []string{"RECORDINGS_DIR", "VIDEO_RTSP_URL", "AUDIO_RTSP_URL"} {
		t.Setenv(key, "")
	}

	cfg := Load()

	require.Equal(t, "rtsp://localhost:8554/live", cfg.Video.RTSPURL, "unexpected video RTSP URL")
	require.Equal(t, "rtsp://localhost:8554/audio", cfg.Audio.RTSPURL, "unexpected audio RTSP URL")
	require.True(t, strings.HasPrefix(cfg.SessionDir, "recordings"), "session dir should be under recordings/, got: %s", cfg.SessionDir)
}

func TestLoad_envOverrides(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "/tmp/test-recordings")
	t.Setenv("VIDEO_RTSP_URL", "rtsp://cam.local:8554/cam0")
	t.Setenv("AUDIO_RTSP_URL", "rtsp://cam.local:8554/mic0")

	cfg := Load()

	require.Equal(t, "rtsp://cam.local:8554/cam0", cfg.Video.RTSPURL, "unexpected video RTSP URL")
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

	require.Equal(t, cfg.SessionDir, filepath.Dir(cfg.Video.OutputFile), "video output file not in session dir: %s", cfg.Video.OutputFile)
	require.Equal(t, "video.mp4", filepath.Base(cfg.Video.OutputFile), "unexpected video file name")
	require.Equal(t, cfg.SessionDir, filepath.Dir(cfg.Audio.OutputFile), "audio output file not in session dir: %s", cfg.Audio.OutputFile)
	require.Equal(t, "audio.wav", filepath.Base(cfg.Audio.OutputFile), "unexpected audio file name")
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
