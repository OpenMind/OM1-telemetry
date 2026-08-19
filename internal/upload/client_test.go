package upload

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeAPI stands in for the openmind-api's data-collection endpoints. It
// hands back presigned "POST" fields that actually point at fakeS3, and
// records what a real server would need to, so tests can assert on it.
type fakeAPI struct {
	mu sync.Mutex

	s3URL string

	sessions      map[string]*fakeSession
	nextID        int
	preComplete   string // session_dir that should short-circuit as already complete
	failRequests  map[string]int
	multipartData map[string][]byte // uploadID -> reassembled bytes
}

type fakeSession struct {
	id         string
	sessionDir string
	startedAt  string
	status     string
	failReason string
	prefix     string
	uploaded   map[string][]byte // key -> body received by fakeS3
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server, *httptest.Server) {
	api := &fakeAPI{
		sessions:      map[string]*fakeSession{},
		failRequests:  map[string]int{},
		multipartData: map[string][]byte{},
	}

	s3 := httptest.NewServer(http.HandlerFunc(api.handleS3))
	t.Cleanup(s3.Close)
	api.s3URL = s3.URL

	apiSrv := httptest.NewServer(http.HandlerFunc(api.handleAPI))
	t.Cleanup(apiSrv.Close)

	return api, apiSrv, s3
}

func (a *fakeAPI) handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-key" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/data/collection/sessions":
		a.createSession(w, r)
	case r.Method == http.MethodPost && matchSuffix(r.URL.Path, "/complete") && !hasFilesPrefix(r.URL.Path):
		a.completeSession(w, r)
	case r.Method == http.MethodPost && matchSuffix(r.URL.Path, "/renew"):
		a.renew(w, r)
	case r.Method == http.MethodPost && matchSuffix(r.URL.Path, "/files/start"):
		a.startMultipart(w, r)
	case r.Method == http.MethodPost && matchSuffix(r.URL.Path, "/files/part-url"):
		a.partURL(w, r)
	case r.Method == http.MethodPost && matchSuffix(r.URL.Path, "/files/complete"):
		a.completeMultipart(w, r)
	case r.Method == http.MethodPost && matchSuffix(r.URL.Path, "/files/abort"):
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":"Upload aborted"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func matchSuffix(path, suffix string) bool {
	return len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix
}
func hasFilesPrefix(path string) bool {
	return len(path) > len("/files/complete") && path[len(path)-len("/files/complete"):] == "/files/complete"
}

func (a *fakeAPI) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionDir string `json:"session_dir"`
		StartedAt  string `json:"started_at"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	a.mu.Lock()
	defer a.mu.Unlock()

	if body.SessionDir == a.preComplete {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": "already-done",
			"status":     "complete",
		})
		return
	}

	a.nextID++
	id := fmt.Sprintf("sess-%d", a.nextID)
	sess := &fakeSession{
		id: id, sessionDir: body.SessionDir, startedAt: body.StartedAt,
		status: "uploading", prefix: "cust/key/" + id + "/",
		uploaded: map[string][]byte{},
	}
	a.sessions[id] = sess

	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"s3_prefix":  sess.prefix,
		"status":     sess.status,
		"upload": map[string]any{
			"url":    a.s3URL + "/",
			"fields": map[string]string{"policy": "p", "x-amz-signature": "s"},
		},
		"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	})
}

func (a *fakeAPI) sessionIDFromPath(path, tail string) string {
	// /data/collection/sessions/{id}{tail}
	const prefix = "/data/collection/sessions/"
	rest := path[len(prefix):]
	return rest[:len(rest)-len(tail)]
}

func (a *fakeAPI) completeSession(w http.ResponseWriter, r *http.Request) {
	id := a.sessionIDFromPath(r.URL.Path, "/complete")
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	a.mu.Lock()
	defer a.mu.Unlock()
	sess, ok := a.sessions[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	sess.status = body.Status
	sess.failReason = body.Error
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "ok", "session_id": id})
}

func (a *fakeAPI) renew(w http.ResponseWriter, r *http.Request) {
	id := a.sessionIDFromPath(r.URL.Path, "/renew")
	a.mu.Lock()
	_, ok := a.sessions[id]
	a.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"upload": map[string]any{
			"url":    a.s3URL + "/",
			"fields": map[string]string{"policy": "p2", "x-amz-signature": "s2"},
		},
		"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	})
}

func (a *fakeAPI) startMultipart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filename string `json:"filename"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	uploadID := "up-" + body.Filename
	a.mu.Lock()
	a.multipartData[uploadID] = nil
	a.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"upload_id": uploadID, "key": "k/" + body.Filename})
}

func (a *fakeAPI) partURL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filename   string `json:"filename"`
		UploadID   string `json:"upload_id"`
		PartNumber int64  `json:"part_number"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"url": fmt.Sprintf("%s/part?upload_id=%s&part=%d", a.s3URL, body.UploadID, body.PartNumber),
	})
}

func (a *fakeAPI) completeMultipart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filename string `json:"filename"`
		UploadID string `json:"upload_id"`
		Parts    []struct {
			PartNumber int64  `json:"part_number"`
			ETag       string `json:"etag"`
		} `json:"parts"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "Upload completed", "key": body.Filename})
}

// handleS3 stands in for both a presigned-POST bucket endpoint and the
// presigned PUT part URLs.
func (a *fakeAPI) handleS3(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/":
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		key := r.FormValue("key")
		f, _, err := r.FormFile("file")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer f.Close()
		data, _ := io.ReadAll(f)

		a.mu.Lock()
		for _, sess := range a.sessions {
			if len(key) >= len(sess.prefix) && key[:len(sess.prefix)] == sess.prefix {
				sess.uploaded[key[len(sess.prefix):]] = data
			}
		}
		a.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPut && r.URL.Path == "/part":
		uploadID := r.URL.Query().Get("upload_id")
		part := r.URL.Query().Get("part")
		data, _ := io.ReadAll(r.Body)

		a.mu.Lock()
		a.multipartData[uploadID] = append(a.multipartData[uploadID], data...)
		a.mu.Unlock()

		w.Header().Set("ETag", `"etag-`+uploadID+`-`+part+`"`)
		w.WriteHeader(http.StatusOK)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
}

func TestUploadSession_smallFilesUseDirectPost(t *testing.T) {
	api, apiSrv, _ := newFakeAPI(t)

	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{"ok":true}`))
	writeFile(t, dir, "lidar_scans.bin", []byte("scandata"))

	c := New(Config{BaseURL: apiSrv.URL, APIKey: "test-key"})
	err := c.UploadSession(context.Background(), dir, "recordings/2026-08-18/2026-08-18_20-00-00", time.Now(), Options{})
	require.NoError(t, err)

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Len(t, api.sessions, 1)
	for _, sess := range api.sessions {
		require.Equal(t, "recordings/2026-08-18/2026-08-18_20-00-00", sess.sessionDir)
		require.Equal(t, "complete", sess.status)
		require.Equal(t, []byte(`{"ok":true}`), sess.uploaded["meta.json"])
		require.Equal(t, []byte("scandata"), sess.uploaded["lidar_scans.bin"])
	}
}

func TestUploadSession_alreadyCompleteShortCircuits(t *testing.T) {
	api, apiSrv, _ := newFakeAPI(t)
	api.preComplete = "recordings/done"

	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{}`))

	c := New(Config{BaseURL: apiSrv.URL, APIKey: "test-key"})
	err := c.UploadSession(context.Background(), dir, "recordings/done", time.Now(), Options{})
	require.NoError(t, err)

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Empty(t, api.sessions, "should not have created a new session for one already complete")
}

func TestUploadSession_emptyDirIsANoop(t *testing.T) {
	api, apiSrv, _ := newFakeAPI(t)
	dir := t.TempDir()

	c := New(Config{BaseURL: apiSrv.URL, APIKey: "test-key"})
	err := c.UploadSession(context.Background(), dir, "recordings/empty", time.Now(), Options{})
	require.NoError(t, err)

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Empty(t, api.sessions, "an empty directory should never create a session")
}

func TestUploadSession_largeFileUsesMultipart(t *testing.T) {
	api, apiSrv, _ := newFakeAPI(t)

	dir := t.TempDir()
	big := make([]byte, 30) // threshold set to 10 below, so this forces >1 part
	for i := range big {
		big[i] = byte(i)
	}
	writeFile(t, dir, "video.mp4", big)

	c := New(Config{BaseURL: apiSrv.URL, APIKey: "test-key", MultipartThreshold: 10, PartSize: 10})
	err := c.UploadSession(context.Background(), dir, "recordings/big", time.Now(), Options{})
	require.NoError(t, err)

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Len(t, api.sessions, 1)
	for _, sess := range api.sessions {
		require.Equal(t, "complete", sess.status)
	}
	got := api.multipartData["up-video.mp4"]
	require.Equal(t, big, got, "reassembled multipart bytes must match the source file")
}

func TestUploadSession_failureMarksSessionFailed(t *testing.T) {
	api, apiSrv, s3 := newFakeAPI(t)
	s3.Close() // every S3 request will now fail to connect

	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{}`))

	c := New(Config{BaseURL: apiSrv.URL, APIKey: "test-key"})
	err := c.UploadSession(context.Background(), dir, "recordings/fails", time.Now(), Options{})
	require.Error(t, err)

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Len(t, api.sessions, 1)
	for _, sess := range api.sessions {
		require.Equal(t, "failed", sess.status)
		require.NotEmpty(t, sess.failReason)
	}
}

func TestUploadSession_wrongAPIKeyFails(t *testing.T) {
	_, apiSrv, _ := newFakeAPI(t)
	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{}`))

	c := New(Config{BaseURL: apiSrv.URL, APIKey: "wrong"})
	err := c.UploadSession(context.Background(), dir, "recordings/x", time.Now(), Options{})
	require.Error(t, err)
}

func TestConfig_readyRequiresBothFields(t *testing.T) {
	require.False(t, Config{}.Ready())
	require.False(t, Config{BaseURL: "https://x"}.Ready())
	require.False(t, Config{APIKey: "k"}.Ready())
	require.True(t, Config{BaseURL: "https://x", APIKey: "k"}.Ready())
}
