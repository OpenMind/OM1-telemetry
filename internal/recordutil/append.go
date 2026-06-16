package recordutil

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type OpenAppendResult struct {
	File     *os.File
	PrevSize int64
}

func OpenForAppend(path string) (*OpenAppendResult, error) {
	var prevSize int64
	if stat, err := os.Stat(path); err == nil {
		prevSize = stat.Size()
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return &OpenAppendResult{File: f, PrevSize: prevSize}, nil
}

func ReadLastSeq(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return -1, nil
		}
		return -1, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)

	var lastSeq int64 = -1
	isHeader := true
	for scanner.Scan() {
		line := scanner.Text()
		if isHeader {
			isHeader = false
			continue
		}

		if line == "" {
			continue
		}

		parts := strings.SplitN(line, ",", 3)
		if len(parts) < 2 {
			continue
		}

		n, perr := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if perr == nil {
			lastSeq = n
		}
	}
	if err := scanner.Err(); err != nil {
		return -1, fmt.Errorf("scan %s: %w", path, err)
	}
	return lastSeq, nil
}

func UniqueSegmentFile(base string, t time.Time) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	utc := t.UTC()
	suffix := fmt.Sprintf("%s_%09dZ",
		utc.Format("20060102T150405"),
		utc.Nanosecond(),
	)
	return fmt.Sprintf("%s_%s%s", stem, suffix, ext)
}
