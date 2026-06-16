package zenohutil

import (
	"github.com/eclipse-zenoh/zenoh-go/zenoh"
)

// TimestampToUnixNs converts a Zenoh HLC (NTP64) timestamp to Unix nanoseconds.
// Zenoh stores timestamps in NTP64 format (epoch 1900-01-01); we subtract the
// 2,208,988,800-second offset to align with Unix epoch (1970-01-01).
func TimestampToUnixNs(ts zenoh.TimeStamp) int64 {
	return ntpTimeToUnixNs(ts.Time())
}

func ntpTimeToUnixNs(ntpTime uint64) int64 {
	const ntpToUnixOffset = 2208988800

	seconds := int64(ntpTime >> 32)
	fraction := uint32(ntpTime & 0xFFFFFFFF)

	unixSeconds := seconds - ntpToUnixOffset

	// fraction is a 32-bit fixed-point fraction-of-a-second.  Multiply by
	// 1e9 then right-shift 32 to get nanoseconds.  No overflow risk:
	// max fraction (2^32-1) * 1e9 ≈ 4.3e18, well below int64 max (9.2e18).
	nanos := (int64(fraction) * 1e9) >> 32

	return unixSeconds*1e9 + nanos
}