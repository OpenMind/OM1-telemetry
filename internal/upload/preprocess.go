package upload

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// preprocessStep transforms localDir in place before its files are uploaded.
// A session can be retried after a partial upload (see UploadSession), so
// every step must be idempotent: safe to run again on a directory it already
// processed.
type preprocessStep func(localDir string) error

// preprocessSteps is the fixed pipeline UploadSession runs, once per session,
// after the session is closed but before any file is uploaded -- this is the
// one point where the whole session is known to be final and static. Add new
// steps here as more preprocessing is needed; each should be small, focused,
// and independently idempotent.
var preprocessSteps = []preprocessStep{
	convertJSONLToJSON,
}

func preprocess(localDir string) error {
	for _, step := range preprocessSteps {
		if err := step(localDir); err != nil {
			return err
		}
	}
	return nil
}

// convertJSONLToJSON rewrites every *.jsonl file directly under localDir as a
// same-named *.json file holding a single JSON array of its records, then
// removes the .jsonl.
//
// Recording itself must keep writing JSONL (see internal/clock and
// internal/features): it's an append-only log written incrementally, in one
// case while a stream is tailed live, so it needs the property that every
// completed line is independently valid even if the process dies mid-write.
// A single JSON document doesn't have that property. But once a session is
// closed, the log is done growing, and openmind-api's S3 bucket policy only
// allows a fixed extension whitelist that doesn't include .jsonl -- so this
// step converts the now-static log into one real JSON document right before
// upload, without changing how it was recorded.
//
// Idempotent: a file with no .jsonl counterpart (already converted by an
// earlier, later-failed attempt) is left untouched.
func convertJSONLToJSON(localDir string) error {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return fmt.Errorf("preprocess: list %s: %w", localDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		src := filepath.Join(localDir, e.Name())
		dst := strings.TrimSuffix(src, "l") // "foo.jsonl" -> "foo.json"
		if err := jsonlToJSONArray(src, dst); err != nil {
			return fmt.Errorf("preprocess: convert %s: %w", e.Name(), err)
		}
		if err := os.Remove(src); err != nil {
			return fmt.Errorf("preprocess: remove %s: %w", e.Name(), err)
		}
	}
	return nil
}

// jsonlToJSONArray writes dst as a JSON array of src's lines. Each line is
// carried as a raw JSON value rather than decoded into a Go struct, so this
// has no dependency on -- and never lossily reformats -- whatever schema the
// clock or features packages happen to write.
func jsonlToJSONArray(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()

	w := bufio.NewWriter(out)
	if _, err := w.WriteString("["); err != nil {
		return err
	}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	first := true
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return fmt.Errorf("%s: invalid json line: %s", src, line)
		}
		if !first {
			if _, err := w.WriteString(","); err != nil {
				return err
			}
		}
		first = false
		if _, err := w.Write(line); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	if _, err := w.WriteString("]"); err != nil {
		return err
	}
	return w.Flush()
}
