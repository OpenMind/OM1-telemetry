package depth

/*
#cgo pkg-config: CycloneDDS
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

// NOTE ON GENERATED SYMBOL NAMES: the exact C type/descriptor names below
// (C.sensor_msgs_msg_dds__Image_, C.sensor_msgs_msg_dds__Image__desc) are
// idlc's expected flattened-module naming convention (module::module::struct
// -> module_module_struct_, descriptor suffixed _desc) but have NOT been
// verified by actually running idlc (not available in the authoring
// environment — see Makefile's idl-gen target and README's Build
// prerequisites). The first `make build` on a host with CycloneDDS installed
// will fail to compile here if idlc emitted different names; fix by matching
// whatever internal/ddsgen/sensor_msgs_image.h actually declares.

// rawSample is one decoded-then-re-encoded sensor_msgs/Image message, ready
// to be handed to encodeFrame (which calls ParseImage), plus its DDS source
// timestamp.
type rawSample struct {
	data   []byte
	unixNs int64
}

// subscribeDDS opens a CycloneDDS participant on domainID, subscribes to
// topic using the sensor_msgs/Image schema, and streams samples (re-encoded
// to CDR bytes matching sensor_msgs/Image's own wire format, byte-for-byte
// compatible with ParseImage in image.go) on the returned channel until ctx
// is cancelled or Stop() (the returned closer) is called.
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
		select {
		case out <- rawSample{data: encodeImage(&sample), unixNs: info.SourceTimestamp}:
		case <-ctx.Done():
			return
		}
	}
}

// encodeImage re-serializes a decoded sensor_msgs/Image sample to CDR bytes,
// field-for-field in IDL declaration order (see idl/sensor_msgs_image.idl),
// so that image.go's ParseImage (which decodes the same layout as delivered
// by the previous zenoh-ros bridge) can decode it unchanged.
//
// stamp.sec is written via w.U32 (not w.I32) even though the IDL field is
// int32 — ParseImage reads it as u32 too, so this preserves identical bits
// without caring about sign.
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
