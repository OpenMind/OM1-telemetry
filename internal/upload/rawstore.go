package upload

import (
	"os"
	"path/filepath"
)

// rawDirName holds each compression step's original, uncompressed input once
// a compressed replacement has been produced for it -- kept on local disk
// (never uploaded, since regularFiles never recurses into subdirectories) so
// nothing is lost even though only the compressed form ever leaves the
// robot. See compress_zstd.go, compress_depth.go, compress_pointcloud.go.
const rawDirName = "raw"

// archiveOriginal moves name out of localDir and into localDir/raw/,
// preserving it locally. A no-op if name isn't present at the top level --
// either it was never there (that sensor is disabled) or an earlier,
// interrupted preprocessing attempt already archived it.
func archiveOriginal(localDir, name string) error {
	src := filepath.Join(localDir, name)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	rawDir := filepath.Join(localDir, rawDirName)
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return err
	}
	return os.Rename(src, filepath.Join(rawDir, name))
}

// findSource locates name for a compression step to read: at the top level
// on a first attempt, or already archived in raw/ if an earlier,
// interrupted attempt got that far before failing. Returns "" if neither
// exists (e.g. the sensor that produces name is disabled).
func findSource(localDir, name string) (string, error) {
	top := filepath.Join(localDir, name)
	if _, err := os.Stat(top); err == nil {
		return top, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	archived := filepath.Join(localDir, rawDirName, name)
	if _, err := os.Stat(archived); err == nil {
		return archived, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return "", nil
}

// originalCSVBytes returns name's pre-rewrite content: from raw/ if an
// earlier attempt already copied it there, otherwise straight from
// localDir. Safe even mid-retry: a copy into raw/ (see ensureRawCopy)
// always happens before name is ever rewritten in place, so whichever one
// exists is guaranteed to still be the original.
func originalCSVBytes(localDir, name string) ([]byte, error) {
	archived := filepath.Join(localDir, rawDirName, name)
	if b, err := os.ReadFile(archived); err == nil {
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return os.ReadFile(filepath.Join(localDir, name))
}

// ensureRawCopy copies name into raw/ if it isn't there already, leaving
// the top-level copy in place (unlike archiveOriginal, which moves it) --
// for a small sidecar file like a timestamps CSV that a compression step
// rewrites in place afterward rather than replacing with a differently-
// named file.
func ensureRawCopy(localDir, name string) error {
	dst := filepath.Join(localDir, rawDirName, name)
	if _, err := os.Stat(dst); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	src := filepath.Join(localDir, name)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Join(localDir, rawDirName), 0o755); err != nil {
		return err
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// writeAtomic writes data to name (relative to localDir) crash-safely: a
// reader never observes a partially-written file, and a crash mid-write
// leaves either the previous contents or nothing, never a truncated one.
// The temp file is dot-prefixed so a leftover from an interrupted attempt
// is never mistaken for session data by regularFiles.
func writeAtomic(localDir, name string, data []byte) error {
	tmp, err := os.CreateTemp(localDir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed below
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(localDir, name))
}
