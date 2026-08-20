package upload

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"om1-telemetry/internal/cdr"
)

// encodeTestPointCloud2 mirrors internal/pointcloud/dds_reader.go's
// encodePointCloud2 closely enough for decodePointCloud2/extractXYZ to be
// tested without a real DDS sample: a single FLOAT32 x/y/z field layout,
// point_step 12 (tightly packed, no padding).
func encodeTestPointCloud2(t *testing.T, pts [][3]float32) []byte {
	t.Helper()
	w := cdr.NewWriter()
	w.I32(0)                // stamp.sec
	w.U32(0)                // stamp.nanosec
	w.Str("lidar")          // frame_id
	w.U32(1)                // height
	w.U32(uint32(len(pts))) // width

	fields := []struct {
		name   string
		offset uint32
	}{{"x", 0}, {"y", 4}, {"z", 8}}
	w.U32(uint32(len(fields)))
	for _, f := range fields {
		w.Str(f.name)
		w.U32(f.offset)
		w.U8(pointFieldFloat32)
		w.U32(1) // count
	}

	w.Bool(false) // is_bigendian
	w.U32(12)     // point_step
	w.U32(12 * uint32(len(pts)))

	data := make([]byte, 0, 12*len(pts))
	for _, p := range pts {
		dw := cdr.NewWriter()
		dw.F32(p[0])
		dw.F32(p[1])
		dw.F32(p[2])
		data = append(data, dw.Bytes()[4:]...) // strip the per-writer CDR header; we only want the raw point bytes
	}
	w.Seq(data)
	w.Bool(true) // is_dense

	return w.Bytes()
}

func TestDecodePointCloud2_extractXYZ_roundTrips(t *testing.T) {
	want := [][3]float32{{1, 2, 3}, {-4.5, 0, 100.25}, {0, 0, 0}}
	payload := encodeTestPointCloud2(t, want)

	pc, err := decodePointCloud2(payload)
	require.NoError(t, err)
	require.EqualValues(t, len(want), pc.width)
	require.EqualValues(t, 1, pc.height)
	require.EqualValues(t, 12, pc.pointStep)

	got, err := extractXYZ(pc)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestExtractXYZ_errorsWithoutFloat32XYZFields(t *testing.T) {
	w := cdr.NewWriter()
	w.I32(0)
	w.U32(0)
	w.Str("lidar")
	w.U32(1)
	w.U32(1)
	w.U32(1) // one field
	w.Str("intensity")
	w.U32(0)
	w.U8(pointFieldFloat32)
	w.U32(1)
	w.Bool(false)
	w.U32(4)
	w.U32(4)
	w.Seq([]byte{0, 0, 0, 0})
	w.Bool(true)

	pc, err := decodePointCloud2(w.Bytes())
	require.NoError(t, err)

	_, err = extractXYZ(pc)
	require.Error(t, err)
}

func TestCompressPointcloud_noopWhenDracoEncoderUnavailable(t *testing.T) {
	restore := stubDracoEncoderPath(t, "", errors.New("not found"))
	defer restore()

	dir := t.TempDir()
	writeFile(t, dir, pointcloudFramesName, []byte("whatever"))
	writeFile(t, dir, pointcloudTimestampsName, []byte("unix_ns,seq,byte_offset,byte_length,method,mono_ns\n1,0,0,8,raw,1\n"))

	require.NoError(t, compressPointcloud(dir, Options{}))

	require.FileExists(t, filepath.Join(dir, pointcloudFramesName),
		"without draco_encoder on PATH, pointcloud_frames.bin must be left exactly as recorded")
	require.NoFileExists(t, filepath.Join(dir, pointcloudDracoName))
}

func TestCompressPointcloud_missingFileIsANoopEvenWithDracoAvailable(t *testing.T) {
	restore := stubDracoEncoderPath(t, "/usr/bin/true", nil)
	defer restore()

	dir := t.TempDir()
	writeFile(t, dir, "meta.json", []byte(`{}`))

	require.NoError(t, compressPointcloud(dir, Options{}))

	require.NoFileExists(t, filepath.Join(dir, pointcloudDracoName))
}

func TestCompressPointcloud_unparseableFrameLeavesFileUntouched(t *testing.T) {
	restore := stubDracoEncoderPath(t, "/usr/bin/true", nil)
	defer restore()

	dir := t.TempDir()
	bin := []byte("not a valid CDR pointcloud payload at all")
	writeFile(t, dir, pointcloudFramesName, bin)
	csv := fmt.Sprintf("unix_ns,seq,byte_offset,byte_length,method,mono_ns\n1,0,0,%d,raw,1\n", len(bin))
	writeFile(t, dir, pointcloudTimestampsName, []byte(csv))

	require.NoError(t, compressPointcloud(dir, Options{}))

	got, err := os.ReadFile(filepath.Join(dir, pointcloudFramesName))
	require.NoError(t, err)
	require.Equal(t, bin, got, "an unparseable frame must leave pointcloud_frames.bin byte-for-byte untouched")
	require.NoFileExists(t, filepath.Join(dir, pointcloudDracoName))
}

// stubDracoEncoderPath overrides dracoEncoderPath for the duration of one
// test and returns a func to restore it -- dracoEncoderPath is normally a
// sync.OnceValues, memoized process-wide, so tests must swap the whole var
// rather than trying to reset the underlying LookPath call.
func stubDracoEncoderPath(t *testing.T, path string, err error) func() {
	t.Helper()
	orig := dracoEncoderPath
	dracoEncoderPath = sync.OnceValues(func() (string, error) { return path, err })
	return func() { dracoEncoderPath = orig }
}
