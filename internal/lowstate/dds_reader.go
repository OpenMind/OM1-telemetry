package lowstate

/*
#cgo pkg-config: CycloneDDS
#cgo CFLAGS: -I${SRCDIR}/../ddsgen
#include "unitree_go_lowstate.h"
#include "unitree_hg_lowstate.h"
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

func subscribeDDS(ctx context.Context, domainID uint32, topic string, robotType string) (<-chan rawSample, func(), error) {
	participant, err := ddscore.NewParticipant(domainID)
	if err != nil {
		return nil, nil, fmt.Errorf("new participant: %w", err)
	}

	isG1 := robotType == "g1"

	var topicEntity ddscore.Entity
	if isG1 {
		topicEntity, err = participant.CreateTopic(topic, unsafe.Pointer(&C.unitree_hg_msg_dds__LowState__desc))
	} else {
		topicEntity, err = participant.CreateTopic(topic, unsafe.Pointer(&C.unitree_go_msg_dds__LowState__desc))
	}
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

	if isG1 {
		go pollHg(ctx, reader, ws, out)
	} else {
		go pollGo2(ctx, reader, ws, out)
	}

	return out, closer, nil
}

func pollGo2(ctx context.Context, reader ddscore.Entity, ws *ddscore.WaitSet, out chan<- rawSample) {
	defer close(out)
	for ctx.Err() == nil {
		avail, err := ws.Wait(200 * time.Millisecond)
		if err != nil || !avail {
			continue
		}
		var sample C.unitree_go_msg_dds__LowState_
		ok, info, err := ddscore.Take(reader, unsafe.Pointer(&sample))
		if err != nil || !ok {
			continue
		}
		select {
		case out <- rawSample{data: encodeGo2LowState(&sample), unixNs: info.SourceTimestamp}:
		case <-ctx.Done():
			return
		}
	}
}

func pollHg(ctx context.Context, reader ddscore.Entity, ws *ddscore.WaitSet, out chan<- rawSample) {
	defer close(out)
	for ctx.Err() == nil {
		avail, err := ws.Wait(200 * time.Millisecond)
		if err != nil || !avail {
			continue
		}
		var sample C.unitree_hg_msg_dds__LowState_
		ok, info, err := ddscore.Take(reader, unsafe.Pointer(&sample))
		if err != nil || !ok {
			continue
		}
		select {
		case out <- rawSample{data: encodeHgLowState(&sample), unixNs: info.SourceTimestamp}:
		case <-ctx.Done():
			return
		}
	}
}

func encodeGo2LowState(s *C.unitree_go_msg_dds__LowState_) []byte {
	w := cdr.NewWriter()

	w.U8(uint8(s.head[0]))
	w.U8(uint8(s.head[1]))
	w.U8(uint8(s.level_flag))
	w.U8(uint8(s.frame_reserve))
	w.U32(uint32(s.sn[0]))
	w.U32(uint32(s.sn[1]))
	w.U32(uint32(s.version[0]))
	w.U32(uint32(s.version[1]))
	w.U16(uint16(s.bandwidth))

	writeGo2IMUState(w, &s.imu_state)
	for i := 0; i < 20; i++ {
		writeGo2MotorState(w, &s.motor_state[i])
	}
	writeGo2BmsState(w, &s.bms_state)

	for i := 0; i < 4; i++ {
		w.I16(int16(s.foot_force[i]))
	}
	for i := 0; i < 4; i++ {
		w.I16(int16(s.foot_force_est[i]))
	}
	w.U32(uint32(s.tick))
	for i := 0; i < 40; i++ {
		w.U8(uint8(s.wireless_remote[i]))
	}
	w.U8(uint8(s.bit_flag))
	w.F32(float32(s.adc_reel))
	w.U8(uint8(int8(s.temperature_ntc1)))
	w.U8(uint8(int8(s.temperature_ntc2)))
	w.F32(float32(s.power_v))
	w.F32(float32(s.power_a))
	for i := 0; i < 4; i++ {
		w.U16(uint16(s.fan_frequency[i]))
	}
	w.U32(uint32(s.reserve))
	w.U32(uint32(s.crc))

	return w.Bytes()
}

func writeGo2IMUState(w *cdr.Writer, s *C.unitree_go_msg_dds__IMUState_) {
	for i := 0; i < 4; i++ {
		w.F32(float32(s.quaternion[i]))
	}
	for i := 0; i < 3; i++ {
		w.F32(float32(s.gyroscope[i]))
	}
	for i := 0; i < 3; i++ {
		w.F32(float32(s.accelerometer[i]))
	}
	for i := 0; i < 3; i++ {
		w.F32(float32(s.rpy[i]))
	}
	w.U8(uint8(int8(s.temperature)))
}

func writeGo2MotorState(w *cdr.Writer, s *C.unitree_go_msg_dds__MotorState_) {
	w.U8(uint8(s.mode))
	w.F32(float32(s.q))
	w.F32(float32(s.dq))
	w.F32(float32(s.ddq))
	w.F32(float32(s.tau_est))
	w.F32(float32(s.q_raw))
	w.F32(float32(s.dq_raw))
	w.F32(float32(s.ddq_raw))
	w.U8(uint8(int8(s.temperature)))
	w.U32(uint32(s.lost))
	w.U32(uint32(s.reserve[0]))
	w.U32(uint32(s.reserve[1]))
}

func writeGo2BmsState(w *cdr.Writer, s *C.unitree_go_msg_dds__BmsState_) {
	w.U8(uint8(s.version_high))
	w.U8(uint8(s.version_low))
	w.U8(uint8(s.status))
	w.U8(uint8(s.soc))
	w.I32(int32(s.current))
	w.U16(uint16(s.cycle))
	for i := 0; i < 2; i++ {
		w.U8(uint8(int8(s.bq_ntc[i])))
	}
	for i := 0; i < 2; i++ {
		w.U8(uint8(int8(s.mcu_ntc[i])))
	}
	for i := 0; i < 15; i++ {
		w.U16(uint16(s.cell_vol[i]))
	}
}

func encodeHgLowState(s *C.unitree_hg_msg_dds__LowState_) []byte {
	w := cdr.NewWriter()

	w.U32(uint32(s.version[0]))
	w.U32(uint32(s.version[1]))
	w.U8(uint8(s.mode_pr))
	w.U8(uint8(s.mode_machine))
	w.U32(uint32(s.tick))

	writeHgIMUState(w, &s.imu_state)
	for i := 0; i < 35; i++ {
		writeHgMotorState(w, &s.motor_state[i])
	}

	for i := 0; i < 40; i++ {
		w.U8(uint8(s.wireless_remote[i]))
	}
	for i := 0; i < 4; i++ {
		w.U32(uint32(s.reserve[i]))
	}
	w.U32(uint32(s.crc))

	return w.Bytes()
}

func writeHgIMUState(w *cdr.Writer, s *C.unitree_hg_msg_dds__IMUState_) {
	for i := 0; i < 4; i++ {
		w.F32(float32(s.quaternion[i]))
	}
	for i := 0; i < 3; i++ {
		w.F32(float32(s.gyroscope[i]))
	}
	for i := 0; i < 3; i++ {
		w.F32(float32(s.accelerometer[i]))
	}
	for i := 0; i < 3; i++ {
		w.F32(float32(s.rpy[i]))
	}
	w.I16(int16(s.temperature))
}

func writeHgMotorState(w *cdr.Writer, s *C.unitree_hg_msg_dds__MotorState_) {
	w.U8(uint8(s.mode))
	w.F32(float32(s.q))
	w.F32(float32(s.dq))
	w.F32(float32(s.ddq))
	w.F32(float32(s.tau_est))
	for i := 0; i < 2; i++ {
		w.I16(int16(s.temperature[i]))
	}
	w.F32(float32(s.vol))
	w.U32(uint32(s.sensor[0]))
	w.U32(uint32(s.sensor[1]))
	w.U32(uint32(s.motorstate))
	for i := 0; i < 4; i++ {
		w.U32(uint32(s.reserve[i]))
	}
}
