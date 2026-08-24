package upload

import (
	"os"
	"path/filepath"
)

// rawDirName holds each compression step's original, uncompressed input; never uploaded.
const rawDirName = "raw"

// archiveOriginal moves name out of localDir and into localDir/raw/, preserving it locally.
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

// findSource locates name at the top level or already archived in raw/; returns "" if neither exists.
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

// originalCSVBytes returns name's pre-rewrite content, from raw/ if already archived there.
func originalCSVBytes(localDir, name string) ([]byte, error) {
	archived := filepath.Join(localDir, rawDirName, name)
	if b, err := os.ReadFile(archived); err == nil {
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return os.ReadFile(filepath.Join(localDir, name))
}

// ensureRawCopy copies name into raw/ if not already there, leaving the top-level copy in place.
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

// writeAtomic writes data to name (relative to localDir) crash-safely via a temp file + rename.
func writeAtomic(localDir, name string, data []byte) error {
	tmp, err := os.CreateTemp(localDir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(localDir, name))
}
