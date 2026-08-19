// Package upload uploads finished recorder session directories to the
// openmind-api data-collection endpoints (github.com/OpenMind/openmind-api,
// internal/handlers/data_collection_upload.go): a presigned S3 POST policy
// for ordinary files, and true S3 multipart upload for large ones. The API
// is the authority on bucket/credentials -- this client only ever talks to
// the openmind-api itself and to the presigned S3 URLs it hands back.
package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	// DefaultMultipartThreshold is the file size at and above which a file is
	// sent through the true S3 multipart-upload endpoints instead of a single
	// presigned POST. Below the openmind-api's own 5 GiB presigned-POST cap,
	// but small enough that a single-part upload doesn't need to survive
	// wobbly robot network conditions in one shot.
	DefaultMultipartThreshold = 100 * 1024 * 1024
	// DefaultPartSize is the chunk size used for multipart uploads.
	DefaultPartSize = 16 * 1024 * 1024

	requestTimeout = 2 * time.Minute
)

// Config configures the openmind-api uploader.
type Config struct {
	// BaseURL is the openmind-api base, e.g.
	// "https://<host>/api/core/v1" -- no trailing slash.
	BaseURL string
	// APIKey is sent as "Authorization: Bearer <APIKey>".
	APIKey string
	// MultipartThreshold and PartSize default to the Default* constants above
	// when zero.
	MultipartThreshold int64
	PartSize           int64
	// HTTPClient overrides the default client; used by tests.
	HTTPClient *http.Client
}

// Ready reports whether enough is configured to actually make requests.
func (c Config) Ready() bool {
	return c.BaseURL != "" && c.APIKey != ""
}

// Client uploads one finished session directory per call to UploadSession.
type Client struct {
	cfg Config
	hc  *http.Client
}

func New(cfg Config) *Client {
	if cfg.MultipartThreshold <= 0 {
		cfg.MultipartThreshold = DefaultMultipartThreshold
	}
	if cfg.PartSize <= 0 {
		cfg.PartSize = DefaultPartSize
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	return &Client{cfg: cfg, hc: hc}
}

type presignedPOST struct {
	URL    string            `json:"url"`
	Fields map[string]string `json:"fields"`
}

type sessionResp struct {
	SessionID string         `json:"session_id"`
	S3Prefix  string         `json:"s3_prefix"`
	Status    string         `json:"status"`
	Upload    *presignedPOST `json:"upload"`
}

// UploadSession runs the preprocess pipeline over localDir (see
// preprocess.go), then uploads every regular file directly under it to the
// openmind-api under sessionDir (grouping key the API records against the
// session; see CreateDataCollectionSession's session_dir), then marks the
// session complete. startedAt should be the session's own start time, not
// the upload time.
//
// Best-effort: on any error after the session is created, the session is
// marked "failed" server-side (so a retry with the same sessionDir is
// recognized as a resume, not a duplicate) and the error is returned. Local
// files are never touched here -- callers decide whether/when to delete
// them, and only after a nil error.
func (c *Client) UploadSession(ctx context.Context, localDir, sessionDir string, startedAt time.Time) error {
	if !c.cfg.Ready() {
		return errors.New("upload: not configured (base URL / API key unset)")
	}

	if err := preprocess(localDir); err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	files, err := regularFiles(localDir)
	if err != nil {
		return fmt.Errorf("upload: list %s: %w", localDir, err)
	}
	if len(files) == 0 {
		return nil
	}

	sess, err := c.createSession(ctx, sessionDir, startedAt)
	if err != nil {
		return fmt.Errorf("upload: create session: %w", err)
	}
	if sess.Status == "complete" {
		return nil // an earlier run already finished this session's upload
	}

	post := sess.Upload
	for _, name := range files {
		path := filepath.Join(localDir, name)
		info, err := os.Stat(path)
		if err != nil {
			c.fail(ctx, sess.SessionID, err)
			return fmt.Errorf("upload: stat %s: %w", path, err)
		}

		if info.Size() >= c.cfg.MultipartThreshold {
			if err := c.uploadMultipart(ctx, sess.SessionID, path, name); err != nil {
				c.fail(ctx, sess.SessionID, err)
				return fmt.Errorf("upload: %s: %w", name, err)
			}
			continue
		}

		if post == nil {
			renewed, err := c.renew(ctx, sess.SessionID)
			if err != nil {
				c.fail(ctx, sess.SessionID, err)
				return fmt.Errorf("upload: renew policy: %w", err)
			}
			post = renewed
		}
		if err := c.uploadDirect(ctx, post, sess.S3Prefix+name, path); err != nil {
			// One retry against a freshly-presigned policy covers the
			// 45-minute-window edge case on a session that took a while.
			renewed, rerr := c.renew(ctx, sess.SessionID)
			if rerr != nil {
				c.fail(ctx, sess.SessionID, err)
				return fmt.Errorf("upload: %s: %w", name, err)
			}
			post = renewed
			if err := c.uploadDirect(ctx, post, sess.S3Prefix+name, path); err != nil {
				c.fail(ctx, sess.SessionID, err)
				return fmt.Errorf("upload: %s: %w", name, err)
			}
		}
	}

	return c.complete(ctx, sess.SessionID)
}

func (c *Client) createSession(ctx context.Context, sessionDir string, startedAt time.Time) (*sessionResp, error) {
	body := map[string]string{"session_dir": sessionDir}
	if !startedAt.IsZero() {
		body["started_at"] = startedAt.UTC().Format(time.RFC3339)
	}
	var out sessionResp
	if err := c.doJSON(ctx, http.MethodPost, "/data/collection/sessions", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) renew(ctx context.Context, sessionID string) (*presignedPOST, error) {
	var out struct {
		Upload *presignedPOST `json:"upload"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/data/collection/sessions/"+sessionID+"/renew", nil, &out); err != nil {
		return nil, err
	}
	return out.Upload, nil
}

func (c *Client) complete(ctx context.Context, sessionID string) error {
	return c.doJSON(ctx, http.MethodPost, "/data/collection/sessions/"+sessionID+"/complete",
		map[string]string{"status": "complete"}, nil)
}

// fail best-effort marks a session failed server-side; it does not surface
// its own errors since the caller already has the real error to report.
func (c *Client) fail(ctx context.Context, sessionID string, cause error) {
	if sessionID == "" {
		return
	}
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_ = c.doJSON(ctx, http.MethodPost, "/data/collection/sessions/"+sessionID+"/complete",
		map[string]string{"status": "failed", "error": msg}, nil)
}

// uploadDirect POSTs one file straight to S3 using a presigned POST policy.
// The multipart body is buffered rather than streamed: S3's presigned-POST
// endpoint rejects a chunked request with 411 Length Required, so the
// request needs a Content-Length known up front. Only files under
// MultipartThreshold take this path -- the default (100 MiB) bounds how
// much this ever holds in memory at once; anything larger goes through
// uploadMultipart's part-by-part PUT instead, which streams from disk.
func (c *Client) uploadDirect(ctx context.Context, post *presignedPOST, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	keys := make([]string, 0, len(post.Fields))
	for k := range post.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := mw.WriteField(k, post.Fields[k]); err != nil {
			return err
		}
	}
	if err := mw.WriteField("key", key); err != nil {
		return err
	}
	if err := mw.WriteField("Content-Type", "application/octet-stream"); err != nil {
		return err
	}
	// "file" must be the last field: S3 stops reading the policy against
	// fields that follow it.
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, post.URL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(body.Len())

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("s3 upload %s: %s: %s", key, resp.Status, bytes.TrimSpace(raw))
	}
	return nil
}

type completedPart struct {
	PartNumber int64  `json:"part_number"`
	ETag       string `json:"etag"`
}

// uploadMultipart sends one file through the API's true-S3-multipart-upload
// endpoints (start/part-url/complete), reading it in PartSize chunks so a
// robot with limited RAM never needs the whole file in memory.
func (c *Client) uploadMultipart(ctx context.Context, sessionID, path, filename string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var start struct {
		UploadID string `json:"upload_id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/data/collection/sessions/"+sessionID+"/files/start",
		map[string]string{"filename": filename}, &start); err != nil {
		return err
	}

	abort := func() {
		_ = c.doJSON(ctx, http.MethodPost, "/data/collection/sessions/"+sessionID+"/files/abort",
			map[string]string{"filename": filename, "upload_id": start.UploadID}, nil)
	}

	var parts []completedPart
	buf := make([]byte, c.cfg.PartSize)
	for partNumber := int64(1); ; partNumber++ {
		n, readErr := io.ReadFull(f, buf)
		if n > 0 {
			etag, err := c.uploadPart(ctx, sessionID, filename, start.UploadID, partNumber, buf[:n])
			if err != nil {
				abort()
				return err
			}
			parts = append(parts, completedPart{PartNumber: partNumber, ETag: etag})
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			abort()
			return readErr
		}
	}

	return c.doJSON(ctx, http.MethodPost, "/data/collection/sessions/"+sessionID+"/files/complete",
		map[string]any{"filename": filename, "upload_id": start.UploadID, "parts": parts}, nil)
}

func (c *Client) uploadPart(ctx context.Context, sessionID, filename, uploadID string, partNumber int64, data []byte) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/data/collection/sessions/"+sessionID+"/files/part-url",
		map[string]any{"filename": filename, "upload_id": uploadID, "part_number": partNumber}, &out); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, out.URL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(data))

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("upload part %d: %s: %s", partNumber, resp.Status, bytes.TrimSpace(raw))
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("upload part %d: response carried no ETag", partNumber)
	}
	return etag, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}

// regularFiles lists the plain files directly under dir, sorted for
// deterministic upload order. Sub-directories (none expected in a session
// dir today) are skipped rather than recursed into.
func regularFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}
