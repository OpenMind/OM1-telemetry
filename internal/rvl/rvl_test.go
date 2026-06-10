package rvl

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func roundTrip(t *testing.T, depth []uint16) {
	t.Helper()
	encoded := Encode(depth)
	require.Zero(t, len(encoded)%4, "encoded length must be a multiple of 4")
	decoded := Decode(encoded, len(depth))
	require.Equal(t, depth, decoded, "round-trip mismatch")
}

func TestRoundTrip_empty(t *testing.T) {
	roundTrip(t, []uint16{})
}

func TestRoundTrip_allZeros(t *testing.T) {
	roundTrip(t, make([]uint16, 1000))
}

func TestRoundTrip_allNonZero(t *testing.T) {
	depth := make([]uint16, 1000)
	for i := range depth {
		depth[i] = uint16(1000 + i)
	}
	roundTrip(t, depth)
}

func TestRoundTrip_mixedZeroAndNonZero(t *testing.T) {
	depth := make([]uint16, 5000)
	for i := range depth {
		if i%7 < 3 {
			depth[i] = 0
		} else {
			depth[i] = uint16((i * 13) % 4096)
		}
	}
	roundTrip(t, depth)
}

func TestRoundTrip_maxValues(t *testing.T) {
	roundTrip(t, []uint16{0, 65535, 0, 65535, 1, 65534, 0, 0, 65535})
}

func TestRoundTrip_largeSmoothFrame(t *testing.T) {
	const w, h = 640, 480
	depth := make([]uint16, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Smooth gradient with a zero border, like a real depth map.
			if x < 20 || x >= w-20 || y < 20 || y >= h-20 {
				continue
			}
			depth[y*w+x] = uint16(500 + x + y)
		}
	}
	roundTrip(t, depth)
}

func TestEncode_compressesSmoothDepth(t *testing.T) {
	const w, h = 640, 480
	depth := make([]uint16, w*h)
	for i := range depth {
		depth[i] = uint16(1000 + i%5) // highly compressible
	}
	encoded := Encode(depth)
	rawBytes := len(depth) * 2
	require.Less(t, len(encoded), rawBytes, "RVL should be smaller than raw 16-bit data")
}
