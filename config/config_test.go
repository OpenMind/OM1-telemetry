package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_defaults(t *testing.T) {
	for _, key := range []string{"RECORDINGS_DIR", "VIDEO_RTSP_URL", "AUDIO_RTSP_URL"} {
		t.Setenv(key, "")
	}

	cfg := Load()

	if cfg.Video.RTSPURL != "rtsp://localhost:8554/live" {
		t.Errorf("unexpected video RTSP URL: %s", cfg.Video.RTSPURL)
	}
	if cfg.Audio.RTSPURL != "rtsp://localhost:8554/audio" {
		t.Errorf("unexpected audio RTSP URL: %s", cfg.Audio.RTSPURL)
	}
	if !strings.HasPrefix(cfg.SessionDir, "recordings") {
		t.Errorf("session dir should be under recordings/, got: %s", cfg.SessionDir)
	}
}

func TestLoad_envOverrides(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "/tmp/test-recordings")
	t.Setenv("VIDEO_RTSP_URL", "rtsp://cam.local:8554/cam0")
	t.Setenv("AUDIO_RTSP_URL", "rtsp://cam.local:8554/mic0")

	cfg := Load()

	if cfg.Video.RTSPURL != "rtsp://cam.local:8554/cam0" {
		t.Errorf("unexpected video RTSP URL: %s", cfg.Video.RTSPURL)
	}
	if cfg.Audio.RTSPURL != "rtsp://cam.local:8554/mic0" {
		t.Errorf("unexpected audio RTSP URL: %s", cfg.Audio.RTSPURL)
	}
	if !strings.HasPrefix(cfg.SessionDir, "/tmp/test-recordings") {
		t.Errorf("session dir should be under /tmp/test-recordings/, got: %s", cfg.SessionDir)
	}
}

func TestLoad_sessionDirLayout(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "recordings")

	cfg := Load()

	parts := strings.Split(filepath.ToSlash(cfg.SessionDir), "/")
	if len(parts) != 3 {
		t.Fatalf("expected 3 path components, got %d: %s", len(parts), cfg.SessionDir)
	}
	dateDir := parts[1]
	sessionDir := parts[2]
	if len(dateDir) != 10 {
		t.Errorf("date directory has wrong length: %s", dateDir)
	}
	if len(sessionDir) != 19 {
		t.Errorf("session directory has wrong length: %s", sessionDir)
	}
}

func TestLoad_outputFilesInsideSessionDir(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "recordings")

	cfg := Load()

	if filepath.Dir(cfg.Video.OutputFile) != cfg.SessionDir {
		t.Errorf("video output file not in session dir: %s", cfg.Video.OutputFile)
	}
	if filepath.Base(cfg.Video.OutputFile) != "video.mp4" {
		t.Errorf("unexpected video file name: %s", filepath.Base(cfg.Video.OutputFile))
	}
	if filepath.Dir(cfg.Audio.OutputFile) != cfg.SessionDir {
		t.Errorf("audio output file not in session dir: %s", cfg.Audio.OutputFile)
	}
	if filepath.Base(cfg.Audio.OutputFile) != "audio.wav" {
		t.Errorf("unexpected audio file name: %s", filepath.Base(cfg.Audio.OutputFile))
	}
}

func TestLoad_eachCallProducesUniqueSessionDir(t *testing.T) {
	t.Setenv("RECORDINGS_DIR", "recordings")

	cfg1 := Load()
	cfg2 := Load()

	if cfg1.SessionDir == "" || cfg2.SessionDir == "" {
		t.Error("session dir must not be empty")
	}
}

func TestEnvStr_emptyValueFallsBackToDefault(t *testing.T) {
	os.Unsetenv("_TEST_KEY_ABSENT")
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
