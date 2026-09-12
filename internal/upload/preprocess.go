package upload

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"om1-telemetry/internal/compress"
)

// Options carries per-call context the preprocess pipeline needs.
type Options struct {
	// PreserveJSONL, if set, names a .jsonl file that must be converted but never removed.
	PreserveJSONL string
}

// preprocessStep transforms localDir in place before upload; must be idempotent.
type preprocessStep func(localDir string, opts Options) error

// preprocessSteps is the fixed pipeline UploadSession runs once a session is closed.
var preprocessSteps = []preprocessStep{
	convertJSONLToJSON,
	func(localDir string, _ Options) error { return compress.WholeFiles(localDir) },
	func(localDir string, _ Options) error { return compress.Depth(localDir) },
	func(localDir string, _ Options) error { return compress.Pointcloud(localDir) },
}

func preprocess(localDir string, opts Options) error {
	for _, step := range preprocessSteps {
		if err := step(localDir, opts); err != nil {
			return err
		}
	}
	return nil
}

// convertJSONLToJSON rewrites every *.jsonl file under localDir into a same-named *.json array, then
// removes the .jsonl (unless named by opts.PreserveJSONL). Idempotent: already-converted files are left alone.
func convertJSONLToJSON(localDir string, opts Options) error {
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
		if e.Name() == opts.PreserveJSONL {
			continue
		}
		if err := os.Remove(src); err != nil {
			return fmt.Errorf("preprocess: remove %s: %w", e.Name(), err)
		}
	}
	return nil
}

// jsonlToJSONArray writes dst as a JSON array of src's lines, carried as raw JSON values.
func jsonlToJSONArray(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

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
	if _, err := w.WriteString("[\n"); err != nil {
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
			if _, err := w.WriteString(",\n"); err != nil {
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
	if first {
		if _, err := w.WriteString("]"); err != nil {
			return err
		}
	} else if _, err := w.WriteString("\n]"); err != nil {
		return err
	}
	return w.Flush()
}
