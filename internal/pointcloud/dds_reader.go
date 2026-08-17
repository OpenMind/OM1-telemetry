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

type rawSample struct {
	data   []byte
	unixNs int64
}

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
		data := encodePointCloud2(&sample)
		C.dds_sample_free(unsafe.Pointer(&sample), &C.sensor_msgs_msg_dds__PointCloud2__desc, C.DDS_FREE_CONTENTS)
		select {
		case out <- rawSample{data: data, unixNs: info.SourceTimestamp}:
		case <-ctx.Done():
			return
		}
	}
}

func encodePointCloud2(s *C.sensor_msgs_msg_dds__PointCloud2_) []byte {
	w := cdr.NewWriter()

	w.I32(int32(s.header.stamp.sec))
	w.U32(uint32(s.header.stamp.nanosec))
	w.Str(C.GoString(s.header.frame_id))

	w.U32(uint32(s.height))
	w.U32(uint32(s.width))

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
