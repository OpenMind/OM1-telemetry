package zenohutil

import (
	"github.com/eclipse-zenoh/zenoh-go/zenoh"
)

func TimestampToUnixNs(ts zenoh.TimeStamp) int64 {
	return ntpTimeToUnixNs(ts.Time())
}

func ntpTimeToUnixNs(ntpTime uint64) int64 {
	seconds := int64(ntpTime >> 32)
	fraction := uint32(ntpTime & 0xFFFFFFFF)
	nanos := (int64(fraction) * 1e9) >> 32
	return seconds*1e9 + nanos
}
