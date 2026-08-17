package odom

/*
#cgo pkg-config: CycloneDDS
#cgo CFLAGS: -I${SRCDIR}/../ddsgen
#include "builtin_interfaces_time.h"
#include "std_msgs_header.h"
#include "geometry_msgs_common.h"
#include "nav_msgs_odometry.h"
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

	topicEntity, err := participant.CreateTopic(topic, unsafe.Pointer(&C.nav_msgs_msg_dds__Odometry__desc))
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

	go pollOdometry(ctx, reader, ws, out)

	return out, closer, nil
}

func pollOdometry(ctx context.Context, reader ddscore.Entity, ws *ddscore.WaitSet, out chan<- rawSample) {
	defer close(out)
	for ctx.Err() == nil {
		avail, err := ws.Wait(200 * time.Millisecond)
		if err != nil || !avail {
			continue
		}
		var sample C.nav_msgs_msg_dds__Odometry_
		ok, info, err := ddscore.Take(reader, unsafe.Pointer(&sample))
		if err != nil || !ok {
			continue
		}
		data := encodeOdometry(&sample)
		C.dds_sample_free(unsafe.Pointer(&sample), &C.nav_msgs_msg_dds__Odometry__desc, C.DDS_FREE_CONTENTS)
		select {
		case out <- rawSample{data: data, unixNs: info.SourceTimestamp}:
		case <-ctx.Done():
			return
		}
	}
}

func encodeOdometry(s *C.nav_msgs_msg_dds__Odometry_) []byte {
	w := cdr.NewWriter()

	w.I32(int32(s.header.stamp.sec))
	w.U32(uint32(s.header.stamp.nanosec))
	w.Str(C.GoString(s.header.frame_id))

	w.Str(C.GoString(s.child_frame_id))

	writePoseWithCovariance(w, &s.pose)
	writeTwistWithCovariance(w, &s.twist)

	return w.Bytes()
}

func writePoseWithCovariance(w *cdr.Writer, s *C.geometry_msgs_msg_dds__PoseWithCovariance_) {
	writePose(w, &s.pose)
	for i := 0; i < 36; i++ {
		w.F64(float64(s.covariance[i]))
	}
}

func writeTwistWithCovariance(w *cdr.Writer, s *C.geometry_msgs_msg_dds__TwistWithCovariance_) {
	writeTwist(w, &s.twist)
	for i := 0; i < 36; i++ {
		w.F64(float64(s.covariance[i]))
	}
}

func writePose(w *cdr.Writer, s *C.geometry_msgs_msg_dds__Pose_) {
	writePoint(w, &s.position)
	writeQuaternion(w, &s.orientation)
}

func writeTwist(w *cdr.Writer, s *C.geometry_msgs_msg_dds__Twist_) {
	writeVector3(w, &s.linear)
	writeVector3(w, &s.angular)
}

func writePoint(w *cdr.Writer, s *C.geometry_msgs_msg_dds__Point_) {
	w.F64(float64(s.x))
	w.F64(float64(s.y))
	w.F64(float64(s.z))
}

func writeQuaternion(w *cdr.Writer, s *C.geometry_msgs_msg_dds__Quaternion_) {
	w.F64(float64(s.x))
	w.F64(float64(s.y))
	w.F64(float64(s.z))
	w.F64(float64(s.w))
}

func writeVector3(w *cdr.Writer, s *C.geometry_msgs_msg_dds__Vector3_) {
	w.F64(float64(s.x))
	w.F64(float64(s.y))
	w.F64(float64(s.z))
}
