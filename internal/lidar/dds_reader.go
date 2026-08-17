package lidar

/*
#cgo pkg-config: CycloneDDS
#cgo CFLAGS: -I${SRCDIR}/../ddsgen
#include "sensor_msgs_laserscan.h"
*/
import "C"

import (
	"context"
	"fmt"
	"time"
	"unsafe"

	"om1-telemetry/internal/cdr"
	"om1-telemetry/internal/ddscore"
)

type rawSample struct {
	data   []byte
	unixNs int64
}

func subscribeDDS(ctx context.Context, domainID uint32, topic string) (<-chan rawSample, func(), error) {
	participant, err := ddscore.NewParticipant(domainID)
	if err != nil {
		return nil, nil, fmt.Errorf("new participant: %w", err)
	}

	topicEntity, err := participant.CreateTopic(topic, unsafe.Pointer(&C.sensor_msgs_msg_dds__LaserScan__desc))
	if err != nil {
		_ = participant.Close()
		return nil, nil, fmt.Errorf("create topic %q: %w", topic, err)
	}

	reader, err := participant.CreateReader(topicEntity)
	if err != nil {
		_ = participant.Close()
		return nil, nil, fmt.Errorf("create reader: %w", err)
	}

	ws, err := ddscore.NewWaitSet(participant, reader)
	if err != nil {
		_ = participant.Close()
		return nil, nil, fmt.Errorf("create waitset: %w", err)
	}

	out := make(chan rawSample, 2048)
	closer := func() {
		_ = ws.Close()
		_ = participant.Close()
	}

	go pollLaserScan(ctx, reader, ws, out)

	return out, closer, nil
}

func pollLaserScan(ctx context.Context, reader ddscore.Entity, ws *ddscore.WaitSet, out chan<- rawSample) {
	defer close(out)
	for ctx.Err() == nil {
		avail, err := ws.Wait(200 * time.Millisecond)
		if err != nil || !avail {
			continue
		}
		var sample C.sensor_msgs_msg_dds__LaserScan_
		ok, info, err := ddscore.Take(reader, unsafe.Pointer(&sample))
		if err != nil || !ok {
			continue
		}
		select {
		case out <- rawSample{data: encodeLaserScan(&sample), unixNs: info.SourceTimestamp}:
		case <-ctx.Done():
			return
		}
	}
}

func encodeLaserScan(s *C.sensor_msgs_msg_dds__LaserScan_) []byte {
	w := cdr.NewWriter()

	w.I32(int32(s.header.stamp.sec))
	w.U32(uint32(s.header.stamp.nanosec))
	w.Str(C.GoString(s.header.frame_id))

	w.F32(float32(s.angle_min))
	w.F32(float32(s.angle_max))
	w.F32(float32(s.angle_increment))
	w.F32(float32(s.time_increment))
	w.F32(float32(s.scan_time))
	w.F32(float32(s.range_min))
	w.F32(float32(s.range_max))

	writeFloatSeq(w, s.ranges._length, s.ranges._buffer)
	writeFloatSeq(w, s.intensities._length, s.intensities._buffer)

	return w.Bytes()
}

func writeFloatSeq(w *cdr.Writer, length C.uint32_t, buf *C.float) {
	n := int(length)
	w.U32(uint32(n))
	if n == 0 {
		return
	}
	elems := unsafe.Slice((*float32)(unsafe.Pointer(buf)), n)
	for _, v := range elems {
		w.F32(v)
	}
}
