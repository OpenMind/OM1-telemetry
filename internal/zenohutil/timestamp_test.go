package zenohutil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNtpTimeToUnixNs_zero(t *testing.T) {
	require.Equal(t, int64(0), ntpTimeToUnixNs(0))
}

func TestNtpTimeToUnixNs_oneSecond(t *testing.T) {
	result := ntpTimeToUnixNs(uint64(1) << 32)
	require.Equal(t, int64(1_000_000_000), result)
}

func TestNtpTimeToUnixNs_halfSecondFraction(t *testing.T) {
	result := ntpTimeToUnixNs(uint64(0x80000000))
	require.InDelta(t, float64(500_000_000), float64(result), 1.0)
}

func TestNtpTimeToUnixNs_quarterSecondFraction(t *testing.T) {
	result := ntpTimeToUnixNs(uint64(0x40000000))
	require.InDelta(t, float64(250_000_000), float64(result), 1.0)
}

func TestNtpTimeToUnixNs_recentTimestamp(t *testing.T) {
	ntpTime := uint64(1_000_000_000) << 32
	result := ntpTimeToUnixNs(ntpTime)
	require.Equal(t, int64(1_000_000_000)*int64(1_000_000_000), result)
}

func TestNtpTimeToUnixNs_doesNotSubtractNTPOffset(t *testing.T) {
	ntpTime := uint64(2208988800) << 32
	result := ntpTimeToUnixNs(ntpTime)
	require.Equal(t, int64(2208988800)*int64(1_000_000_000), result)
}
