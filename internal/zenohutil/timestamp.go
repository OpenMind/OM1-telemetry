package zenohutil

import (
	"github.com/eclipse-zenoh/zenoh-go/zenoh"
)

func TimestampToUnixNs(ts zenoh.TimeStamp) int64 {
	return ntpTimeToUnixNs(ts.Time())
}

func ntpTimeToUnixNs(ntpTime uint64) int64 {
	const ntpToUnixOffset = 2208988800

	seconds := int64(ntpTime >> 32)
	fraction := uint32(ntpTime & 0xFFFFFFFF)

	unixSeconds := seconds - ntpToUnixOffset

	nanos := (int64(fraction) * 1e9) >> 32

	return unixSeconds*1e9 + nanos
}
