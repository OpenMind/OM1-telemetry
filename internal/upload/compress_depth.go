package upload

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"om1-telemetry/internal/rvl"
)

const (
	depthFramesName     = "depth_frames.bin"
	depthTimestampsName = "depth_timestamps.csv"
)

// depthRecord mirrors one row of depth_timestamps.csv (see the header
// internal/depth/stream.go writes): unix_ns,seq,byte_offset,byte_length,
// method,width,height,encoding,mono_ns.
type depthRecord struct {
	unixNs, seq, byteOffset, byteLength int64
	method                              string
	width, height                       uint32
	encoding                            string
	monoNs                              int64
}

// compressDepth replaces depth_frames.bin -- which stores each frame RVL-
// encoded (see internal/depth, internal/rvl) -- with depth_frames.zstd: the
// *decoded* raw little-endian uint16 depth frames, concatenated frame by
// frame and zstd-compressed as one blob, losslessly.
//
// Decoding first is deliberate: RVL is a lightweight, real-time-safe codec
// (why the live recorder uses it), not a strong entropy coder -- zstd'ing
// its own output barely helps, but zstd on the underlying raw pixels does
// much better (measured ~2.5x smaller than the already-RVL-encoded file,
// losslessly, on real recordings).
//
// Only attempted when every recorded frame used RVL at a consistent
// width/height -- the overwhelmingly common case. Any frame using
// internal/depth's "raw" fallback encoding (an unparseable or oddly-shaped
// source image) makes the whole step fall back to compressWholeFile
// instead, leaving depth_frames.bin's original per-frame bytes intact
// inside one zstd wrapper: decoding would mean guessing at ad hoc bytes
// that fallback stored, which isn't safe to assume anything about.
//
// depth_timestamps.csv is rewritten in place (atomically) to describe the
// new file: once decoded, every frame is exactly width*height*2 bytes, so
// byte_offset/byte_length become a fixed stride rather than RVL's variable
// lengths, and method becomes "raw_u16le". Its original content is copied
// (not moved -- it keeps living at the same path, just rewritten) into
// raw/ first, so the pre-compression bin+csv pair stays usable together
// there.
func compressDepth(localDir string, opts Options) error {
	dstName := zstdName(depthFramesName) // depth_frames.zstd
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
		return nil // depth disabled for this session
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

// decodeDepthFrames decodes every RVL-encoded frame in bin (per records'
// byte_offset/byte_length) back to raw little-endian uint16 pixels and
// concatenates them in order. Returns ok=false, touching nothing, if any
// record uses a method other than "rvl" or a width/height inconsistent
// with the first frame -- the two things that make "every frame is exactly
// width*height*2 bytes" a safe assumption for the caller.
func decodeDepthFrames(bin []byte, records []depthRecord) (raw []byte, newRecords []depthRecord, ok bool) {
	if len(records) == 0 {
		return nil, nil, false
	}
	width, height := records[0].width, records[0].height
	if width == 0 || height == 0 {
		return nil, nil, false
	}
	frameBytes := int(width) * int(height) * 2

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
		pixels := rvl.Decode(encoded, int(width)*int(height))
		if len(pixels) != int(width)*int(height) {
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
