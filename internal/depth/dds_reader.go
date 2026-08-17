package depth

/*
#cgo pkg-config: CycloneDDS
#cgo CFLAGS: -I${SRCDIR}/../ddsgen
#include "sensor_msgs_image.h"
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

	topicEntity, err := participant.CreateTopic(topic, unsafe.Pointer(&C.sensor_msgs_msg_dds__Image__desc))
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

	go pollImage(ctx, reader, ws, out)

	return out, closer, nil
}

func pollImage(ctx context.Context, reader ddscore.Entity, ws *ddscore.WaitSet, out chan<- rawSample) {
	defer close(out)
	for ctx.Err() == nil {
		avail, err := ws.Wait(200 * time.Millisecond)
		if err != nil || !avail {
			continue
		}
		var sample C.sensor_msgs_msg_dds__Image_
		ok, info, err := ddscore.Take(reader, unsafe.Pointer(&sample))
		if err != nil || !ok {
			continue
		}
		data := encodeImage(&sample)
		C.dds_sample_free(unsafe.Pointer(&sample), &C.sensor_msgs_msg_dds__Image__desc, C.DDS_FREE_CONTENTS)
		select {
		case out <- rawSample{data: data, unixNs: info.SourceTimestamp}:
		case <-ctx.Done():
			return
		}
	}
}

func encodeImage(s *C.sensor_msgs_msg_dds__Image_) []byte {
	w := cdr.NewWriter()

	w.U32(uint32(s.header.stamp.sec))
	w.U32(uint32(s.header.stamp.nanosec))
	w.Str(C.GoString(s.header.frame_id))

	w.U32(uint32(s.height))
	w.U32(uint32(s.width))
	w.Str(C.GoString(s.encoding))
	w.U8(uint8(s.is_bigendian))
	w.U32(uint32(s.step))

	dataLen := uint32(s.data._length)
	data := unsafe.Slice((*byte)(unsafe.Pointer(s.data._buffer)), int(dataLen))
	w.U32(dataLen)
	w.RawBytes(data)

	return w.Bytes()
}
