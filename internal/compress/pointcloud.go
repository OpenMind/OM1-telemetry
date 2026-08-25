package compress

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"

	"om1-telemetry/internal/cdr"
)

const (
	pointcloudFramesName     = "pointcloud_frames.bin"
	pointcloudTimestampsName = "pointcloud_timestamps.csv"
	pointcloudDracoName      = "pointcloud_frames.drc"

	// dracoQuantizationBits sets draco_encoder's position quantization (-qp).
	dracoQuantizationBits = 11

	pointFieldFloat32 = 7 // sensor_msgs/PointField.FLOAT32
)

// pointcloudRecord mirrors one row of pointcloud_timestamps.csv.
type pointcloudRecord struct {
	unixNs, seq, byteOffset, byteLength int64
	method                              string
	monoNs                              int64
}

// dracoEncoderPath resolves draco_encoder on PATH once. Overridden in tests.
var dracoEncoderPath = sync.OnceValues(func() (string, error) {
	return exec.LookPath("draco_encoder")
})

// Pointcloud re-encodes each frame's XYZ geometry with Draco into pointcloud_frames.drc, rewriting
// pointcloud_timestamps.csv to match. No-op if draco_encoder is missing or a frame doesn't parse.
// Draco's quantization is lossy, so the original is kept locally under raw/ -- see rawstore.go.
func Pointcloud(localDir string) error {
	if _, err := dracoEncoderPath(); err != nil {
		return nil
	}

	if _, err := os.Stat(filepath.Join(localDir, pointcloudDracoName)); err == nil {
		if err := archiveOriginal(localDir, pointcloudFramesName); err != nil {
			return err
		}
		return ensureRawCopy(localDir, pointcloudTimestampsName)
	} else if !os.IsNotExist(err) {
		return err
	}

	binSrc, err := findSource(localDir, pointcloudFramesName)
	if err != nil {
		return err
	}
	if binSrc == "" {
		return nil // pointcloud disabled for this session
	}

	csvRaw, err := originalCSVBytes(localDir, pointcloudTimestampsName)
	if err != nil {
		return err
	}
	records, err := parsePointcloudCSV(csvRaw)
	if err != nil {
		return fmt.Errorf("preprocess: parse %s: %w", pointcloudTimestampsName, err)
	}
	if len(records) == 0 {
		return nil
	}

	bin, err := os.ReadFile(binSrc)
	if err != nil {
		return err
	}

	var out bytes.Buffer
	newRecords := make([]pointcloudRecord, 0, len(records))
	for _, rec := range records {
		if rec.byteOffset < 0 || rec.byteLength < 0 || rec.byteOffset+rec.byteLength > int64(len(bin)) {
			slog.Warn("preprocess: pointcloud record out of range; leaving pointcloud_frames.bin uncompressed", "seq", rec.seq)
			return nil
		}
		payload := bin[rec.byteOffset : rec.byteOffset+rec.byteLength]
		if rec.method == "zstd" {
			decoded, err := Decompress(payload)
			if err != nil {
				slog.Warn("preprocess: cannot zstd-decompress pointcloud frame; leaving pointcloud_frames.bin uncompressed", "seq", rec.seq, "err", err)
				return nil
			}
			payload = decoded
		}

		pc, err := decodePointCloud2(payload)
		if err != nil {
			slog.Warn("preprocess: cannot parse pointcloud frame; leaving pointcloud_frames.bin uncompressed", "seq", rec.seq, "err", err)
			return nil
		}
		pts, err := extractXYZ(pc)
		if err != nil {
			slog.Warn("preprocess: cannot extract xyz from pointcloud frame; leaving pointcloud_frames.bin uncompressed", "seq", rec.seq, "err", err)
			return nil
		}

		drc, err := encodeDraco(pts)
		if err != nil {
			return fmt.Errorf("preprocess: draco encode frame %d: %w", rec.seq, err)
		}

		newRecords = append(newRecords, pointcloudRecord{
			unixNs: rec.unixNs, seq: rec.seq,
			byteOffset: int64(out.Len()), byteLength: int64(len(drc)),
			method: "draco", monoNs: rec.monoNs,
		})
		out.Write(drc)
	}

	if err := ensureRawCopy(localDir, pointcloudTimestampsName); err != nil {
		return err
	}
	if err := archiveOriginal(localDir, pointcloudFramesName); err != nil {
		return err
	}
	if err := writeAtomic(localDir, pointcloudTimestampsName, formatPointcloudCSV(newRecords)); err != nil {
		return err
	}
	return writeAtomic(localDir, pointcloudDracoName, out.Bytes())
}

// pointField mirrors one entry of PointCloud2.fields.
type pointField struct {
	name     string
	offset   uint32
	datatype uint8
}

type pointCloud2 struct {
	width, height uint32
	fields        []pointField
	pointStep     uint32
	data          []byte
}

// decodePointCloud2 inverts encodePointCloud2, field for field, in the order it was written.
func decodePointCloud2(payload []byte) (*pointCloud2, error) {
	r, err := cdr.NewReader(payload)
	if err != nil {
		return nil, err
	}

	if _, err := r.I32(); err != nil { // stamp.sec
		return nil, err
	}
	if _, err := r.U32(); err != nil { // stamp.nanosec
		return nil, err
	}
	if _, err := r.Str(); err != nil { // frame_id
		return nil, err
	}

	height, err := r.U32()
	if err != nil {
		return nil, err
	}
	width, err := r.U32()
	if err != nil {
		return nil, err
	}

	fieldCount, err := r.U32()
	if err != nil {
		return nil, err
	}
	fields := make([]pointField, fieldCount)
	for i := range fields {
		name, err := r.Str()
		if err != nil {
			return nil, err
		}
		offset, err := r.U32()
		if err != nil {
			return nil, err
		}
		datatype, err := r.U8()
		if err != nil {
			return nil, err
		}
		if _, err := r.U32(); err != nil { // count
			return nil, err
		}
		fields[i] = pointField{name: name, offset: offset, datatype: datatype}
	}

	if _, err := r.Bool(); err != nil { // is_bigendian
		return nil, err
	}
	pointStep, err := r.U32()
	if err != nil {
		return nil, err
	}
	if _, err := r.U32(); err != nil { // row_step
		return nil, err
	}

	data, err := r.Seq()
	if err != nil {
		return nil, err
	}
	if _, err := r.Bool(); err != nil { // is_dense
		return nil, err
	}

	return &pointCloud2{width: width, height: height, fields: fields, pointStep: pointStep, data: data}, nil
}

// extractXYZ reads every point's x/y/z as float32; errors on any other layout.
func extractXYZ(pc *pointCloud2) ([][3]float32, error) {
	xOff, yOff, zOff := -1, -1, -1
	for _, f := range pc.fields {
		if f.datatype != pointFieldFloat32 {
			continue
		}
		switch f.name {
		case "x":
			xOff = int(f.offset)
		case "y":
			yOff = int(f.offset)
		case "z":
			zOff = int(f.offset)
		}
	}
	if xOff < 0 || yOff < 0 || zOff < 0 {
		return nil, fmt.Errorf("pointcloud: no float32 x/y/z fields")
	}

	n := int(pc.width) * int(pc.height)
	step := int(pc.pointStep)
	if step <= 0 || n*step > len(pc.data) {
		return nil, fmt.Errorf("pointcloud: data too short for %d points at step %d", n, step)
	}

	pts := make([][3]float32, n)
	for i := 0; i < n; i++ {
		base := i * step
		pts[i][0] = readFloat32LE(pc.data[base+xOff:])
		pts[i][1] = readFloat32LE(pc.data[base+yOff:])
		pts[i][2] = readFloat32LE(pc.data[base+zOff:])
	}
	return pts, nil
}

func readFloat32LE(b []byte) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}

// encodeDraco shells out to draco_encoder via a temporary PLY file and returns the resulting .drc bytes.
func encodeDraco(pts [][3]float32) ([]byte, error) {
	binPath, err := dracoEncoderPath()
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "om1-draco-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	plyPath := filepath.Join(dir, "in.ply")
	if err := writePLY(plyPath, pts); err != nil {
		return nil, err
	}
	drcPath := filepath.Join(dir, "out.drc")

	cmd := exec.Command(binPath,
		"-point_cloud",
		"-i", plyPath,
		"-o", drcPath,
		"-qp", strconv.Itoa(dracoQuantizationBits),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("draco_encoder: %w: %s", err, stderr.String())
	}
	return os.ReadFile(drcPath)
}

func writePLY(path string, pts [][3]float32) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	if _, err := fmt.Fprintf(w, "ply\nformat ascii 1.0\nelement vertex %d\nproperty float x\nproperty float y\nproperty float z\nend_header\n", len(pts)); err != nil {
		return err
	}
	for _, p := range pts {
		if _, err := fmt.Fprintf(w, "%g %g %g\n", p[0], p[1], p[2]); err != nil {
			return err
		}
	}
	return w.Flush()
}

func parsePointcloudCSV(raw []byte) ([]pointcloudRecord, error) {
	rows, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	records := make([]pointcloudRecord, 0, len(rows)-1)
	for _, row := range rows[1:] {
		rec, err := parsePointcloudRow(row)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, nil
}

func parsePointcloudRow(row []string) (pointcloudRecord, error) {
	var rec pointcloudRecord
	if len(row) != 6 {
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
	if rec.monoNs, err = strconv.ParseInt(row[5], 10, 64); err != nil {
		return rec, err
	}
	return rec, nil
}

func formatPointcloudCSV(records []pointcloudRecord) []byte {
	var b bytes.Buffer
	b.WriteString("unix_ns,seq,byte_offset,byte_length,method,mono_ns\n")
	for _, r := range records {
		fmt.Fprintf(&b, "%d,%d,%d,%d,%s,%d\n", r.unixNs, r.seq, r.byteOffset, r.byteLength, r.method, r.monoNs)
	}
	return b.Bytes()
}
