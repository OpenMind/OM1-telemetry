// Package upload uploads finished recorder session directories to the openmind-api data-collection endpoints.
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
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMultipartThreshold is the file size at/above which uploads use S3 multipart instead of a presigned POST.
	DefaultMultipartThreshold = 100 * 1024 * 1024
	// DefaultPartSize is the chunk size used for multipart uploads.
	DefaultPartSize = 16 * 1024 * 1024
	// DefaultConcurrency is how many of a session's files are uploaded at once.
	DefaultConcurrency = 4

	// requestTimeout bounds any single HTTP request, as a backstop when the caller's context has no deadline.
	requestTimeout = 10 * time.Minute
)

// Config configures the openmind-api uploader.
type Config struct {
	// BaseURL is the openmind-api base, e.g. "https://<host>/api/core/v1" -- no trailing slash.
	BaseURL string
	APIKey  string
	// MultipartThreshold, PartSize, and Concurrency default to the Default* constants above when zero.
	MultipartThreshold int64
	PartSize           int64
	Concurrency        int
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
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
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

// UploadSession preprocesses localDir, uploads every regular file under it under sessionDir, then marks
// the session complete. Best-effort: on error the session is marked "failed" server-side for a later retry.
func (c *Client) UploadSession(ctx context.Context, localDir, sessionDir string, startedAt time.Time, opts Options) error {
	if !c.cfg.Ready() {
		return errors.New("upload: not configured (base URL / API key unset)")
	}

	if err := preprocess(localDir, opts); err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	files, err := regularFiles(localDir)
	if err != nil {
		return fmt.Errorf("upload: list %s: %w", localDir, err)
	}
	if opts.PreserveJSONL != "" {
		files = removeName(files, opts.PreserveJSONL)
	}
	if len(files) == 0 {
		return nil
	}

	sess, err := c.createSession(ctx, sessionDir, startedAt)
	if err != nil {
		return fmt.Errorf("upload: create session: %w", err)
	}
	if sess.Status == "complete" {
		return nil
	}

	if err := c.uploadFiles(ctx, sess, localDir, files); err != nil {
		c.fail(ctx, sess.SessionID, err)
		return fmt.Errorf("upload: %w", err)
	}

	return c.complete(ctx, sess.SessionID)
}

// uploadFiles uploads every file in files, up to Concurrency at a time; the first failure cancels the rest.
func (c *Client) uploadFiles(ctx context.Context, sess *sessionResp, localDir string, files []string) error {
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	post := &postBox{sessionID: sess.SessionID, post: sess.Upload}

	sem := make(chan struct{}, c.cfg.Concurrency)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	fail := func(name string, err error) {
		once.Do(func() {
			firstErr = fmt.Errorf("%s: %w", name, err)
			cancel()
		})
	}

	for _, name := range files {
		sem <- struct{}{}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			defer func() { <-sem }()
			c.uploadOne(uploadCtx, sess, localDir, name, post, fail)
		}(name)
	}
	wg.Wait()

	return firstErr
}

// uploadOne uploads a single file, choosing multipart vs. direct-POST by size, reporting failure via fail.
func (c *Client) uploadOne(ctx context.Context, sess *sessionResp, localDir, name string, post *postBox, fail func(name string, err error)) {
	path := filepath.Join(localDir, name)
	info, err := os.Stat(path)
	if err != nil {
		fail(name, fmt.Errorf("stat %s: %w", path, err))
		return
	}

	if info.Size() >= c.cfg.MultipartThreshold {
		if err := c.uploadMultipart(ctx, sess.SessionID, path, name); err != nil {
			fail(name, err)
		}
		return
	}

	p, err := post.get(ctx, c)
	if err != nil {
		fail(name, fmt.Errorf("renew policy: %w", err))
		return
	}
	if err := c.uploadDirect(ctx, p, sess.S3Prefix+name, path); err != nil {
		// Retry once against a freshly-presigned policy in case the old one expired.
		p, rerr := post.forceRenew(ctx, c)
		if rerr != nil {
			fail(name, err)
			return
		}
		if err := c.uploadDirect(ctx, p, sess.S3Prefix+name, path); err != nil {
			fail(name, err)
		}
	}
}

// postBox holds the presigned-POST policy files share for direct uploads within one session.
type postBox struct {
	sessionID string

	mu   sync.Mutex
	post *presignedPOST
}

// get returns the current policy, renewing once if none has been fetched yet.
func (b *postBox) get(ctx context.Context, c *Client) (*presignedPOST, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.post != nil {
		return b.post, nil
	}
	renewed, err := c.renew(ctx, b.sessionID)
	if err != nil {
		return nil, err
	}
	b.post = renewed
	return b.post, nil
}

// forceRenew fetches and stores a fresh policy regardless of what's cached.
func (b *postBox) forceRenew(ctx context.Context, c *Client) (*presignedPOST, error) {
	renewed, err := c.renew(ctx, b.sessionID)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.post = renewed
	b.mu.Unlock()
	return renewed, nil
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

// fail best-effort marks a session failed server-side.
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
func (c *Client) uploadDirect(ctx context.Context, post *presignedPOST, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

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
	// "file" must be the last field: S3 stops reading the policy against fields that follow it.
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
	defer func() { _ = resp.Body.Close() }()
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

// uploadMultipart sends one file through the API's S3-multipart endpoints, reading it in PartSize chunks.
func (c *Client) uploadMultipart(ctx context.Context, sessionID, path, filename string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

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
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
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

// regularFiles lists the plain files directly under dir, sorted for deterministic upload order, skipping dotfiles.
func regularFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// removeName returns names with the first occurrence of target removed.
func removeName(names []string, target string) []string {
	for i, n := range names {
		if n == target {
			return append(names[:i:i], names[i+1:]...)
		}
	}
	return names
}
