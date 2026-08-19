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

	require.NoError(t, convertJSONLToJSON(dir, Options{}))

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

	require.NoError(t, convertJSONLToJSON(dir, Options{}))

	raw, err := os.ReadFile(filepath.Join(dir, "video_features.json"))
	require.NoError(t, err)
	require.JSONEq(t, "[]", string(raw))
}

func TestConvertJSONLToJSON_noJSONLFilesIsANoop(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{"ok":true}`))
	writeFile(t, dir, "lidar_scans.bin", []byte("binary"))

	require.NoError(t, convertJSONLToJSON(dir, Options{}))

	require.FileExists(t, filepath.Join(dir, "meta.json"))
	require.FileExists(t, filepath.Join(dir, "lidar_scans.bin"))
}

func TestConvertJSONLToJSON_isIdempotentOnRetry(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "clock_timebase.jsonl", []byte("{\"kind\":\"start\"}\n"))

	require.NoError(t, convertJSONLToJSON(dir, Options{}))
	require.NoError(t, convertJSONLToJSON(dir, Options{}), "a second run over an already-converted dir must not error")

	require.NoFileExists(t, filepath.Join(dir, "clock_timebase.jsonl"))
	require.FileExists(t, filepath.Join(dir, "clock_timebase.json"))
}

func TestConvertJSONLToJSON_invalidLineFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "clock_timebase.jsonl", []byte("not json\n"))

	err := convertJSONLToJSON(dir, Options{})
	require.Error(t, err)
	require.FileExists(t, filepath.Join(dir, "clock_timebase.jsonl"),
		"source must survive a failed conversion so it can be retried")
}

func TestConvertJSONLToJSON_preserveKeepsSourceButStillConverts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "clock_timebase.jsonl", []byte("{\"kind\":\"start\"}\n{\"kind\":\"sync\"}\n"))
	writeFile(t, dir, "video_features.jsonl", []byte("{\"type\":\"header\"}\n"))

	require.NoError(t, convertJSONLToJSON(dir, Options{PreserveJSONL: "clock_timebase.jsonl"}))

	require.FileExists(t, filepath.Join(dir, "clock_timebase.jsonl"),
		"the preserved file's source must survive -- something else is still appending to it")
	require.FileExists(t, filepath.Join(dir, "clock_timebase.json"),
		"it must still be converted for upload")
	require.NoFileExists(t, filepath.Join(dir, "video_features.jsonl"),
		"a file not named by PreserveJSONL is removed as usual")
}

func TestConvertJSONLToJSON_preserveIsIdempotentAcrossRetries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "clock_timebase.jsonl", []byte("{\"kind\":\"start\"}\n"))

	require.NoError(t, convertJSONLToJSON(dir, Options{PreserveJSONL: "clock_timebase.jsonl"}))
	// Simulate the source growing between a failed attempt and a retry, as
	// the live clock.Watcher would keep appending to it.
	f, err := os.OpenFile(filepath.Join(dir, "clock_timebase.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("{\"kind\":\"sync\"}\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.NoError(t, convertJSONLToJSON(dir, Options{PreserveJSONL: "clock_timebase.jsonl"}))

	raw, err := os.ReadFile(filepath.Join(dir, "clock_timebase.json"))
	require.NoError(t, err)
	var records []map[string]any
	require.NoError(t, json.Unmarshal(raw, &records))
	require.Len(t, records, 2, "the retry must reflect what the still-live source has grown to")
}

func TestUploadSession_convertsJSONLBeforeUpload(t *testing.T) {
	api, apiSrv, _ := newFakeAPI(t)

	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{"ok":true}`))
	writeFile(t, dir, "clock_timebase.jsonl", []byte("{\"kind\":\"start\"}\n{\"kind\":\"sync\"}\n"))

	c := New(Config{BaseURL: apiSrv.URL, APIKey: "test-key"})
	err := c.UploadSession(t.Context(), dir, "recordings/2026-08-19/2026-08-19_00-00-00", time.Now(), Options{})
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

func TestUploadSession_preserveOptionKeepsLocalSourceAfterUpload(t *testing.T) {
	api, apiSrv, _ := newFakeAPI(t)

	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{"ok":true}`))
	writeFile(t, dir, "clock_timebase.jsonl", []byte("{\"kind\":\"start\"}\n"))

	c := New(Config{BaseURL: apiSrv.URL, APIKey: "test-key"})
	err := c.UploadSession(t.Context(), dir, "recordings/2026-08-19/2026-08-19_00-00-00", time.Now(),
		Options{PreserveJSONL: "clock_timebase.jsonl"})
	require.NoError(t, err)

	require.FileExists(t, filepath.Join(dir, "clock_timebase.jsonl"),
		"a still-live journal must survive its own segment's upload")

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Len(t, api.sessions, 1)
	for _, sess := range api.sessions {
		require.Equal(t, "complete", sess.status)
		_, gotJSONL := sess.uploaded["clock_timebase.jsonl"]
		require.False(t, gotJSONL,
			"a preserved source must never be uploaded itself -- only its .json conversion stands in for it")
		_, gotJSON := sess.uploaded["clock_timebase.json"]
		require.True(t, gotJSON)
	}
}
