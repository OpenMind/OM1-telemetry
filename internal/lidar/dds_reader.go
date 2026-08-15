package lidar

/*
#cgo pkg-config: CycloneDDS
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

// NOTE ON GENERATED SYMBOL NAMES: the exact C type/descriptor names below
// (C.sensor_msgs_msg_dds__LaserScan_, ..._desc) are idlc's expected
// flattened-module naming convention (module::module::struct -> module_
// module_struct_, descriptor suffixed _desc) but have NOT been verified by
// actually running idlc (not available in the authoring environment — see
// Makefile's idl-gen target and README's Build prerequisites). The first
// `make build` on a host with CycloneDDS installed will fail to compile
// here if idlc emitted different names; fix by matching whatever
// internal/ddsgen/sensor_msgs_laserscan.h actually declares.

// rawSample is one decoded-then-re-encoded LaserScan message, ready to
// append to the data file, plus its DDS source timestamp.
type rawSample struct {
	data   []byte
	unixNs int64
}

// subscribeDDS opens a CycloneDDS participant on domainID, subscribes to
// topic using the sensor_msgs/LaserScan schema, and streams samples
// (re-encoded to CDR bytes matching the message's own wire format) on the
// returned channel until ctx is cancelled or Stop() (the returned closer)
// is called.
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

// encodeLaserScan re-serializes a decoded sensor_msgs/LaserScan sample to
// CDR bytes, field-for-field in IDL declaration order (see
// idl/sensor_msgs_laserscan.idl), so downstream tools that expect the
// original wire format can decode it unchanged.
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

// writeFloatSeq writes a CDR sequence<float>: a uint32 length followed by
// each element individually (per-element alignment, not a raw byte blob —
// do NOT use cdr.Writer.Seq here, that's only for sequence<octet>-like raw
// byte sequences).
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
