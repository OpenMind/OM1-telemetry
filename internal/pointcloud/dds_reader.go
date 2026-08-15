package pointcloud

/*
#cgo pkg-config: CycloneDDS
#cgo CFLAGS: -I${SRCDIR}/../ddsgen
#include "sensor_msgs_pointcloud2.h"
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
// (C.sensor_msgs_msg_dds__PointCloud2_, ..._desc, etc.) are idlc's expected
// flattened-module naming convention (module::module::struct ->
// module_module_struct_, descriptor suffixed _desc) but have NOT been
// verified by actually running idlc (not available in the authoring
// environment — see Makefile's idl-gen target and README's "Build
// prerequisites" section). The first `make build` on a host with
// CycloneDDS installed will fail to compile here if idlc emitted
// different names; fix by matching whatever
// internal/ddsgen/sensor_msgs_pointcloud2.h actually declares.

// rawSample is one decoded-then-re-encoded PointCloud2 message, ready to
// append (after compression) to the data file, plus its DDS source
// timestamp.
type rawSample struct {
	data   []byte
	unixNs int64
}

// subscribeDDS opens a CycloneDDS participant on domainID, subscribes to
// topic using the sensor_msgs/PointCloud2 schema, and streams samples
// (re-encoded to CDR bytes matching sensor_msgs/PointCloud2's own wire
// format) on the returned channel until ctx is cancelled or Stop() (the
// returned closer) is called.
func subscribeDDS(ctx context.Context, domainID uint32, topic string) (<-chan rawSample, func(), error) {
	participant, err := ddscore.NewParticipant(domainID)
	if err != nil {
		return nil, nil, fmt.Errorf("new participant: %w", err)
	}

	topicEntity, err := participant.CreateTopic(topic, unsafe.Pointer(&C.sensor_msgs_msg_dds__PointCloud2__desc))
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

	out := make(chan rawSample, 32)
	closer := func() {
		_ = ws.Close()
		_ = participant.Close()
	}

	go poll(ctx, reader, ws, out)

	return out, closer, nil
}

func poll(ctx context.Context, reader ddscore.Entity, ws *ddscore.WaitSet, out chan<- rawSample) {
	defer close(out)
	for ctx.Err() == nil {
		avail, err := ws.Wait(200 * time.Millisecond)
		if err != nil || !avail {
			continue
		}
		var sample C.sensor_msgs_msg_dds__PointCloud2_
		ok, info, err := ddscore.Take(reader, unsafe.Pointer(&sample))
		if err != nil || !ok {
			continue
		}
		select {
		case out <- rawSample{data: encodePointCloud2(&sample), unixNs: info.SourceTimestamp}:
		case <-ctx.Done():
			return
		}
	}
}

// encodePointCloud2 re-serializes a decoded sensor_msgs/PointCloud2 sample
// to CDR bytes, field-for-field in IDL declaration order (see
// idl/sensor_msgs_pointcloud2.idl), so downstream tools that expect the
// original wire format can decode it unchanged.
func encodePointCloud2(s *C.sensor_msgs_msg_dds__PointCloud2_) []byte {
	w := cdr.NewWriter()

	// header: std_msgs/Header { builtin_interfaces/Time stamp; string frame_id; }
	w.I32(int32(s.header.stamp.sec))
	w.U32(uint32(s.header.stamp.nanosec))
	w.Str(C.GoString(s.header.frame_id))

	w.U32(uint32(s.height))
	w.U32(uint32(s.width))

	// fields: sequence<sensor_msgs/PointField>
	fieldCount := int(s.fields._length)
	w.U32(uint32(fieldCount))
	if fieldCount > 0 {
		cFields := unsafe.Slice(s.fields._buffer, fieldCount)
		for i := 0; i < fieldCount; i++ {
			f := &cFields[i]
			w.Str(C.GoString(f.name))
			w.U32(uint32(f.offset))
			w.U8(uint8(f.datatype))
			w.U32(uint32(f.count))
		}
	}

	w.Bool(bool(s.is_bigendian))
	w.U32(uint32(s.point_step))
	w.U32(uint32(s.row_step))

	// data: sequence<uint8>
	dataLen := int(s.data._length)
	var dataBytes []byte
	if dataLen > 0 {
		dataBytes = unsafe.Slice((*byte)(unsafe.Pointer(s.data._buffer)), dataLen)
	}
	w.Seq(dataBytes)

	w.Bool(bool(s.is_dense))

	return w.Bytes()
}
