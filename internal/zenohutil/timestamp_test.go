package zenohutil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNtpTimeToUnixNs_zero(t *testing.T) {
	require.Equal(t, int64(0), ntpTimeToUnixNs(0))
}

func TestNtpTimeToUnixNs_oneSecond(t *testing.T) {
	// upper 32 bits = 1 Unix-epoch second, no fractional part
	result := ntpTimeToUnixNs(uint64(1) << 32)
	require.Equal(t, int64(1_000_000_000), result)
}

func TestNtpTimeToUnixNs_halfSecondFraction(t *testing.T) {
	// upper = 0 seconds, lower = 0x80000000 ≈ 0.5 s
	result := ntpTimeToUnixNs(uint64(0x80000000))
	require.InDelta(t, float64(500_000_000), float64(result), 1.0)
}

func TestNtpTimeToUnixNs_quarterSecondFraction(t *testing.T) {
	// lower = 0x40000000 ≈ 0.25 s
	result := ntpTimeToUnixNs(uint64(0x40000000))
	require.InDelta(t, float64(250_000_000), float64(result), 1.0)
}

func TestNtpTimeToUnixNs_recentTimestamp(t *testing.T) {
	// 1_000_000_000 Unix seconds (Sep 2001) with no fraction
	ntpTime := uint64(1_000_000_000) << 32
	result := ntpTimeToUnixNs(ntpTime)
	require.Equal(t, int64(1_000_000_000)*int64(1_000_000_000), result)
}

func TestNtpTimeToUnixNs_doesNotSubtractNTPOffset(t *testing.T) {
	// The old incorrect code subtracted 2,208,988,800 s (NTP→Unix offset).
	// With the corrected code, 2,208,988,800 << 32 should return
	// 2,208,988,800 * 1e9, not 0.
	ntpTime := uint64(2208988800) << 32
	result := ntpTimeToUnixNs(ntpTime)
	require.Equal(t, int64(2208988800)*int64(1_000_000_000), result)
}
