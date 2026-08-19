package upload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConvertJSONLToJSON_producesValidArrayAndRemovesSource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "clock_timebase.jsonl",
		[]byte("{\"kind\":\"start\",\"mono_ns\":1}\n{\"kind\":\"sample\",\"mono_ns\":2}\n"))

	require.NoError(t, convertJSONLToJSON(dir))

	require.NoFileExists(t, filepath.Join(dir, "clock_timebase.jsonl"))
	raw, err := os.ReadFile(filepath.Join(dir, "clock_timebase.json"))
	require.NoError(t, err)

	var records []map[string]any
	require.NoError(t, json.Unmarshal(raw, &records))
	require.Len(t, records, 2)
	require.Equal(t, "start", records[0]["kind"])
	require.Equal(t, "sample", records[1]["kind"])
}

func TestConvertJSONLToJSON_emptyFileProducesEmptyArray(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "video_features.jsonl", []byte(""))

	require.NoError(t, convertJSONLToJSON(dir))

	raw, err := os.ReadFile(filepath.Join(dir, "video_features.json"))
	require.NoError(t, err)
	require.JSONEq(t, "[]", string(raw))
}

func TestConvertJSONLToJSON_noJSONLFilesIsANoop(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{"ok":true}`))
	writeFile(t, dir, "lidar_scans.bin", []byte("binary"))

	require.NoError(t, convertJSONLToJSON(dir))

	require.FileExists(t, filepath.Join(dir, "meta.json"))
	require.FileExists(t, filepath.Join(dir, "lidar_scans.bin"))
}

func TestConvertJSONLToJSON_isIdempotentOnRetry(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "clock_timebase.jsonl", []byte("{\"kind\":\"start\"}\n"))

	require.NoError(t, convertJSONLToJSON(dir))
	require.NoError(t, convertJSONLToJSON(dir), "a second run over an already-converted dir must not error")

	require.NoFileExists(t, filepath.Join(dir, "clock_timebase.jsonl"))
	require.FileExists(t, filepath.Join(dir, "clock_timebase.json"))
}

func TestConvertJSONLToJSON_invalidLineFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "clock_timebase.jsonl", []byte("not json\n"))

	err := convertJSONLToJSON(dir)
	require.Error(t, err)
	require.FileExists(t, filepath.Join(dir, "clock_timebase.jsonl"),
		"source must survive a failed conversion so it can be retried")
}

func TestUploadSession_convertsJSONLBeforeUpload(t *testing.T) {
	api, apiSrv, _ := newFakeAPI(t)

	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{"ok":true}`))
	writeFile(t, dir, "clock_timebase.jsonl", []byte("{\"kind\":\"start\"}\n{\"kind\":\"sync\"}\n"))

	c := New(Config{BaseURL: apiSrv.URL, APIKey: "test-key"})
	err := c.UploadSession(t.Context(), dir, "recordings/2026-08-19/2026-08-19_00-00-00", time.Now())
	require.NoError(t, err)

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Len(t, api.sessions, 1)
	for _, sess := range api.sessions {
		require.Equal(t, "complete", sess.status)
		_, gotJSONL := sess.uploaded["clock_timebase.jsonl"]
		require.False(t, gotJSONL, "the raw .jsonl must never reach S3")

		got, ok := sess.uploaded["clock_timebase.json"]
		require.True(t, ok, "the converted .json must be uploaded instead")

		var records []map[string]any
		require.NoError(t, json.Unmarshal(got, &records))
		require.Len(t, records, 2)
	}
}
