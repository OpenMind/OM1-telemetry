package zenohutil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNtpTimeToUnixNs(t *testing.T) {
	tests := []struct {
		name     string
		ntpTime  uint64
		expected int64
	}{
		{
			name:     "epoch_time",
			ntpTime:  2208988800 << 32,
			expected: 0,
		},
		{
			name:     "with_fraction",
			ntpTime:  (2208988801 << 32) | 0x80000000,
			expected: 1_500_000_000,
		},
		{
			name:     "recent_time",
			ntpTime:  (2208988800 + 1000000) << 32,
			expected: 1000000 * 1e9,
		},
		{
			name:     "time_with_precise_fraction",
			ntpTime:  (2208988801 << 32) | 0x40000000,
			expected: 1_250_000_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ntpTimeToUnixNs(tt.ntpTime)
			require.Equal(t, tt.expected, result, "timestamp conversion mismatch")
		})
	}
}
