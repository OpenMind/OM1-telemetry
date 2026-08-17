// Package ddstest is a DDS LaserScan publisher used only by lidar package tests.
package ddstest

/*
#cgo pkg-config: CycloneDDS
#cgo CFLAGS: -I${SRCDIR}/../../ddsgen
#include <dds/dds.h>
#include "sensor_msgs_laserscan.h"
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"unsafe"

	"om1-telemetry/internal/ddscore"
)

// SetupWriter creates a LaserScan topic and writer for p.
func SetupWriter(p *ddscore.Participant, topicName string) (ddscore.Entity, error) {
	topic, err := p.CreateTopic(topicName, unsafe.Pointer(&C.sensor_msgs_msg_dds__LaserScan__desc))
	if err != nil {
		return 0, fmt.Errorf("create topic: %w", err)
	}
	writer, err := p.CreateWriter(topic)
	if err != nil {
		return 0, fmt.Errorf("create writer: %w", err)
	}
	return writer, nil
}

// PublishScan writes one LaserScan sample with ranges/intensities of length n.
func PublishScan(writer ddscore.Entity, n int, seq int32) error {
	var sample C.sensor_msgs_msg_dds__LaserScan_

	frameID := C.CString("leak-test")
	defer C.free(unsafe.Pointer(frameID))
	sample.header.frame_id = frameID
	sample.header.stamp.sec = C.int32_t(seq)

	sample.angle_min = -1
	sample.angle_max = 1
	sample.angle_increment = 0.01
	sample.range_min = 0
	sample.range_max = 100

	floatSize := C.size_t(unsafe.Sizeof(C.float(0)))
	rangesBuf := C.malloc(C.size_t(n) * floatSize)
	defer C.free(rangesBuf)
	intensitiesBuf := C.malloc(C.size_t(n) * floatSize)
	defer C.free(intensitiesBuf)

	rangeSlice := unsafe.Slice((*float32)(rangesBuf), n)
	intensitySlice := unsafe.Slice((*float32)(intensitiesBuf), n)
	for i := 0; i < n; i++ {
		rangeSlice[i] = float32(i)
		intensitySlice[i] = float32(i) * 2
	}

	sample.ranges._buffer = (*C.float)(rangesBuf)
	sample.ranges._length = C.uint32_t(n)
	sample.ranges._maximum = C.uint32_t(n)

	sample.intensities._buffer = (*C.float)(intensitiesBuf)
	sample.intensities._length = C.uint32_t(n)
	sample.intensities._maximum = C.uint32_t(n)

	if ret := C.dds_write(C.dds_entity_t(writer), unsafe.Pointer(&sample)); ret < 0 {
		return fmt.Errorf("dds_write failed: %d", int(ret))
	}
	return nil
}
