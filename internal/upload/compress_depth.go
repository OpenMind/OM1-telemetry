package upload

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"om1-telemetry/internal/rvl"
)

const (
	depthFramesName     = "depth_frames.bin"
	depthTimestampsName = "depth_timestamps.csv"
)

// depthRecord mirrors one row of depth_timestamps.csv.
type depthRecord struct {
	unixNs, seq, byteOffset, byteLength int64
	method                              string
	width, height                       uint32
	encoding                            string
	monoNs                              int64
}

// compressDepth decodes RVL-encoded depth frames back to raw pixels and re-compresses them as one zstd blob,
// rewriting depth_timestamps.csv to match. Falls back to compressWholeFile if any frame isn't standard RVL.
func compressDepth(localDir string, opts Options) error {
	dstName := zstdName(depthFramesName)
	if _, err := os.Stat(filepath.Join(localDir, dstName)); err == nil {
		if err := archiveOriginal(localDir, depthFramesName); err != nil {
			return err
		}
		return ensureRawCopy(localDir, depthTimestampsName)
	} else if !os.IsNotExist(err) {
		return err
	}

	binSrc, err := findSource(localDir, depthFramesName)
	if err != nil {
		return err
	}
	if binSrc == "" {
		return nil
	}

	csvRaw, err := originalCSVBytes(localDir, depthTimestampsName)
	if err != nil {
		return err
	}
	records, err := parseDepthCSV(csvRaw)
	if err != nil {
		return fmt.Errorf("preprocess: parse %s: %w", depthTimestampsName, err)
	}
	if len(records) == 0 {
		return nil
	}

	bin, err := os.ReadFile(binSrc)
	if err != nil {
		return err
	}

	raw, newRecords, ok := decodeDepthFrames(bin, records)
	if !ok {
		return compressWholeFile(localDir, depthFramesName)
	}

	compressed, err := zstdCompress(raw)
	if err != nil {
		return fmt.Errorf("preprocess: zstd compress: %w", err)
	}

	if err := ensureRawCopy(localDir, depthTimestampsName); err != nil {
		return err
	}
	if err := archiveOriginal(localDir, depthFramesName); err != nil {
		return err
	}
	if err := writeAtomic(localDir, depthTimestampsName, formatDepthCSV(newRecords)); err != nil {
		return err
	}
	return writeAtomic(localDir, dstName, compressed)
}

// decodeDepthFrames decodes every RVL frame in bin to raw pixels; ok=false if any frame isn't standard RVL.
func decodeDepthFrames(bin []byte, records []depthRecord) (raw []byte, newRecords []depthRecord, ok bool) {
	if len(records) == 0 {
		return nil, nil, false
	}
	width, height := records[0].width, records[0].height
	if width == 0 || height == 0 || width > math.MaxInt32 || height > math.MaxInt32 {
		return nil, nil, false
	}
	w, h := int(width), int(height)
	frameBytes := w * h * 2

	raw = make([]byte, 0, frameBytes*len(records))
	newRecords = make([]depthRecord, len(records))

	for i, rec := range records {
		if rec.method != "rvl" || rec.width != width || rec.height != height {
			return nil, nil, false
		}
		if rec.byteOffset < 0 || rec.byteLength < 0 || rec.byteOffset+rec.byteLength > int64(len(bin)) {
			return nil, nil, false
		}
		encoded := bin[rec.byteOffset : rec.byteOffset+rec.byteLength]
		pixels := rvl.Decode(encoded, w*h)
		if len(pixels) != w*h {
			return nil, nil, false
		}
		for _, p := range pixels {
			raw = append(raw, byte(p), byte(p>>8)) // little-endian uint16
		}

		nr := rec
		nr.byteOffset = int64(i * frameBytes)
		nr.byteLength = int64(frameBytes)
		nr.method = "raw_u16le"
		newRecords[i] = nr
	}
	return raw, newRecords, true
}

func parseDepthCSV(raw []byte) ([]depthRecord, error) {
	rows, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	records := make([]depthRecord, 0, len(rows)-1)
	for _, row := range rows[1:] { // skip header
		rec, err := parseDepthRow(row)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, nil
}

func parseDepthRow(row []string) (depthRecord, error) {
	var rec depthRecord
	if len(row) != 9 {
		return rec, fmt.Errorf("unexpected column count %d", len(row))
	}
	var err error
	if rec.unixNs, err = strconv.ParseInt(row[0], 10, 64); err != nil {
		return rec, err
	}
	if rec.seq, err = strconv.ParseInt(row[1], 10, 64); err != nil {
		return rec, err
	}
	if rec.byteOffset, err = strconv.ParseInt(row[2], 10, 64); err != nil {
		return rec, err
	}
	if rec.byteLength, err = strconv.ParseInt(row[3], 10, 64); err != nil {
		return rec, err
	}
	rec.method = row[4]
	w, err := strconv.ParseUint(row[5], 10, 32)
	if err != nil {
		return rec, err
	}
	rec.width = uint32(w)
	h, err := strconv.ParseUint(row[6], 10, 32)
	if err != nil {
		return rec, err
	}
	rec.height = uint32(h)
	rec.encoding = row[7]
	if rec.monoNs, err = strconv.ParseInt(row[8], 10, 64); err != nil {
		return rec, err
	}
	return rec, nil
}

func formatDepthCSV(records []depthRecord) []byte {
	var b bytes.Buffer
	b.WriteString("unix_ns,seq,byte_offset,byte_length,method,width,height,encoding,mono_ns\n")
	for _, r := range records {
		fmt.Fprintf(&b, "%d,%d,%d,%d,%s,%d,%d,%s,%d\n",
			r.unixNs, r.seq, r.byteOffset, r.byteLength, r.method, r.width, r.height, r.encoding, r.monoNs)
	}
	return b.Bytes()
}
