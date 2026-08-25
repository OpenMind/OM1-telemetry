package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHandleStatus_defaultsBothOn(t *testing.T) {
	s := New()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var st Status
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&st))
	require.True(t, st.Recording)
	require.True(t, st.Uploading)
}

func TestHandleStatus_reportsExtra(t *testing.T) {
	s := New()
	s.Extra = func() Extra {
		return Extra{CurrentSession: "recordings/2026-08-24/x", RecordingsBytes: 42, MaxBytes: 100, PendingUploads: 3}
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var st Status
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&st))
	require.Equal(t, "recordings/2026-08-24/x", st.CurrentSession)
	require.Equal(t, int64(42), st.RecordingsBytes)
	require.Equal(t, int64(100), st.MaxBytes)
	require.Equal(t, 3, st.PendingUploads)
}

func TestHandleRecordingStop_sendsCmdAndReflectsResult(t *testing.T) {
	s := New()
	go func() {
		cmd := <-s.RecordingCmds
		require.False(t, cmd.Start)
		s.SetRecording(false) // what main's event loop would do before replying
		cmd.Result <- nil
	}()

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/recording/stop", "application/json", nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var st Status
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&st))
	require.False(t, st.Recording)
}

func TestHandleRecordingStart_timesOutAsGatewayTimeout(t *testing.T) {
	s := New()
	go func() {
		cmd := <-s.RecordingCmds
		cmd.Result <- nil
	}()

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/recording/start", "application/json", nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleUploadStart_enablesAndTriggers(t *testing.T) {
	s := New()
	s.SetUploading(false)

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/upload/start", "application/json", nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, s.Uploading())

	select {
	case <-s.UploadTrigger:
	default:
		t.Fatal("expected /upload/start to trigger an immediate sweep")
	}
}

func TestHandleUploadStop_disables(t *testing.T) {
	s := New()

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/upload/stop", "application/json", nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.False(t, s.Uploading())
}
