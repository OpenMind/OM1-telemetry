package depth

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"om1-telemetry/internal/rvl"
)

func buildImagePayload(width, height uint32, encoding string, frameID string, data []byte) []byte {
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
	putU32(width * 2)   // step
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
