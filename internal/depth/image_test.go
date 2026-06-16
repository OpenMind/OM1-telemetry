package depth

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"om1-telemetry/internal/rvl"
)

func buildImagePayload(width, height uint32, encoding string, frameID string, data []byte) []byte {
	return buildImagePayloadWithStep(width, height, encoding, frameID, data, width*2)
}

func buildImagePayloadWithStep(width, height uint32, encoding string, frameID string, data []byte, step uint32) []byte {
	var b []byte
	b = append(b, 0x00, 0x01, 0x00, 0x00)

	align := func(n int) {
		body := len(b) - 4
		if m := body % n; m != 0 {
			b = append(b, make([]byte, n-m)...)
		}
	}
	putU32 := func(v uint32) {
		align(4)
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	putStr := func(s string) {
		putU32(uint32(len(s) + 1))
		b = append(b, []byte(s)...)
		b = append(b, 0x00)
	}

	putU32(0)           // stamp.sec
	putU32(0)           // stamp.nanosec
	putStr(frameID)     // frame_id
	putU32(height)      // height
	putU32(width)       // width
	putStr(encoding)    // encoding
	b = append(b, 0x00) // is_bigendian (uint8)
	putU32(step)        // step (explicit)
	putU32(uint32(len(data)))
	b = append(b, data...)
	return b
}

func TestParseImage_roundTripDepth(t *testing.T) {
	const w, h = 8, 4
	pixels := make([]uint16, w*h)
	for i := range pixels {
		pixels[i] = uint16(i * 100)
	}
	data := make([]byte, len(pixels)*2)
	for i, p := range pixels {
		binary.LittleEndian.PutUint16(data[i*2:], p)
	}

	payload := buildImagePayload(w, h, "16UC1", "camera_depth_frame", data)

	img, err := ParseImage(payload)
	require.NoError(t, err)
	require.Equal(t, uint32(w), img.Width)
	require.Equal(t, uint32(h), img.Height)
	require.Equal(t, "16UC1", img.Encoding)
	require.False(t, img.IsBigendian)

	got, err := img.DepthPixels()
	require.NoError(t, err)
	require.Equal(t, pixels, got)

	decoded := rvl.Decode(rvl.Encode(got), w*h)
	require.Equal(t, pixels, decoded)
}

func TestDepthPixels_rejectsNon16Bit(t *testing.T) {
	payload := buildImagePayload(4, 2, "rgb8", "cam", make([]byte, 4*2*3))
	img, err := ParseImage(payload)
	require.NoError(t, err)
	_, err = img.DepthPixels()
	require.Error(t, err, "rgb8 should not be accepted as depth")
}

func TestEncodeFrame_rvlForDepth(t *testing.T) {
	const w, h = 16, 16
	pixels := make([]uint16, w*h)
	for i := range pixels {
		pixels[i] = uint16(500 + i%4)
	}
	data := make([]byte, len(pixels)*2)
	for i, p := range pixels {
		binary.LittleEndian.PutUint16(data[i*2:], p)
	}
	payload := buildImagePayload(w, h, "16UC1", "cam", data)

	f := encodeFrame(payload)
	require.Equal(t, "rvl", f.method)
	require.Equal(t, uint32(w), f.width)
	require.Equal(t, uint32(h), f.height)

	decoded := rvl.Decode(f.data, int(f.width*f.height))
	require.Equal(t, pixels, decoded)
}

func TestEncodeFrame_rawFallbackForUnparseable(t *testing.T) {
	f := encodeFrame([]byte{0x01, 0x02})
	require.Equal(t, "raw", f.method)
	require.Equal(t, []byte{0x01, 0x02}, f.data)
}

func TestParseImage_payloadTooShort(t *testing.T) {
	_, err := ParseImage([]byte{0x00, 0x01, 0x00}) // 3 bytes — below the 4-byte CDR header minimum
	require.Error(t, err)
}

func TestDepthPixels_mono16(t *testing.T) {
	// mono16 is an alias for 16UC1; DepthPixels must accept it.
	const w, h = 4, 2
	pixels := []uint16{100, 200, 300, 400, 500, 600, 700, 800}
	data := make([]byte, len(pixels)*2)
	for i, p := range pixels {
		binary.LittleEndian.PutUint16(data[i*2:], p)
	}
	payload := buildImagePayload(w, h, "mono16", "cam", data)
	img, err := ParseImage(payload)
	require.NoError(t, err)

	got, err := img.DepthPixels()
	require.NoError(t, err)
	require.Equal(t, pixels, got)
}

func TestDepthPixels_bigEndian(t *testing.T) {
	// Build an Image directly with big-endian pixel data.
	pixels := []uint16{1000, 2000, 3000, 4000}
	data := make([]byte, len(pixels)*2)
	for i, p := range pixels {
		binary.BigEndian.PutUint16(data[i*2:], p)
	}
	img := &Image{
		Width:       2,
		Height:      2,
		Encoding:    "16UC1",
		IsBigendian: true,
		Step:        4,
		Data:        data,
	}
	got, err := img.DepthPixels()
	require.NoError(t, err)
	require.Equal(t, pixels, got)
}

func TestDepthPixels_dataTooShort(t *testing.T) {
	img := &Image{
		Width:    4,
		Height:   4,
		Encoding: "16UC1",
		Step:     8,
		Data:     make([]byte, 4), // needs 4*4*2 = 32 bytes, only 4 provided
	}
	_, err := img.DepthPixels()
	require.Error(t, err)
}

func TestEncodeFrame_rawFallbackForPaddedRows(t *testing.T) {
	// step != width*2 means the row has padding; encodeFrame must fall back to raw.
	const w, h = 4, 2
	data := make([]byte, w*h*2)
	payload := buildImagePayloadWithStep(w, h, "16UC1", "cam", data, w*2+4)

	f := encodeFrame(payload)
	require.Equal(t, "raw", f.method, "padded rows must fall back to raw")
	require.Equal(t, uint32(w), f.width)
	require.Equal(t, uint32(h), f.height)
}

func TestEncodeFrame_rawFallbackForNon16BitImage(t *testing.T) {
	// rgb8 is parseable but not a 16-bit depth format; encodeFrame falls back to raw.
	payload := buildImagePayload(4, 2, "rgb8", "cam", make([]byte, 4*2*3))

	f := encodeFrame(payload)
	require.Equal(t, "raw", f.method)
	require.Equal(t, "rgb8", f.encoding)
}
