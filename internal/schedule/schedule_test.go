package schedule

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func at(hh, mm int) time.Time {
	return time.Date(2026, 8, 24, hh, mm, 0, 0, time.UTC)
}

func TestWindow_nilIsAlwaysActive(t *testing.T) {
	var w *Window
	require.True(t, w.Active(at(3, 0)))
	require.True(t, w.Active(at(23, 59)))
}

func TestWindow_sameDayRange(t *testing.T) {
	w := &Window{Start: 9 * 60, End: 17 * 60} // 09:00-17:00
	require.False(t, w.Active(at(8, 59)))
	require.True(t, w.Active(at(9, 0)))
	require.True(t, w.Active(at(12, 30)))
	require.True(t, w.Active(at(16, 59)))
	require.False(t, w.Active(at(17, 0)))
	require.False(t, w.Active(at(23, 0)))
}

func TestWindow_overnightRange(t *testing.T) {
	w := &Window{Start: 17 * 60, End: 9 * 60} // 17:00-09:00, spans midnight
	require.True(t, w.Active(at(17, 0)))
	require.True(t, w.Active(at(23, 59)))
	require.True(t, w.Active(at(0, 0)))
	require.True(t, w.Active(at(8, 59)))
	require.False(t, w.Active(at(9, 0)))
	require.False(t, w.Active(at(12, 0)))
}

func TestWindow_zeroLengthIsAlwaysActive(t *testing.T) {
	w := &Window{} // Start == End == 00:00, e.g. an empty `recording: {}`
	require.True(t, w.Active(at(3, 0)))
	require.True(t, w.Active(at(15, 0)))
}

func TestLoad_emptyPathReturnsNilConfigNoError(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.Nil(t, cfg)
}

func TestLoad_missingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.Error(t, err)
}

func TestLoad_parsesBothWindows(t *testing.T) {
	path := writeYAML(t, `
recording:
  start: "09:00"
  end: "17:00"
upload:
  start: "17:00"
  end: "09:00"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Recording)
	require.NotNil(t, cfg.Upload)
	require.True(t, cfg.Recording.Active(at(12, 0)))
	require.False(t, cfg.Recording.Active(at(20, 0)))
	require.True(t, cfg.Upload.Active(at(20, 0)))
	require.False(t, cfg.Upload.Active(at(12, 0)))
}

func TestLoad_omittedAxisIsAlwaysOn(t *testing.T) {
	path := writeYAML(t, `
upload:
  start: "17:00"
  end: "09:00"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Nil(t, cfg.Recording, "omitted section must leave that axis unscheduled (always on)")
	require.True(t, cfg.Recording.Active(at(3, 0)))
}

func TestLoad_invalidTimeFormat(t *testing.T) {
	path := writeYAML(t, `
recording:
  start: "9am"
  end: "17:00"
`)

	_, err := Load(path)
	require.Error(t, err)
}

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schedule.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}
