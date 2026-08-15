// Package ddscore is a small, first-party cgo wrapper around the Eclipse
// CycloneDDS C API (libddsc). It replaces the previous zenoh-go transport:
// Go2/G1 publish sensor and state topics directly on a CycloneDDS domain,
// and this recorder now subscribes to that domain directly instead of going
// through a zenoh-bridge-dds hop.
//
// This package only wraps the generic, message-type-agnostic entity
// lifecycle (participant/topic/reader/waitset). Each stream package
// (internal/lidar, internal/lowstate, ...) brings its own cgo file that
// #includes the idlc-generated type descriptor for its specific message and
// calls back into here with an unsafe.Pointer to a correctly-typed sample
// buffer — see internal/lowstate/dds_reader.go for the reference
// implementation.
//
// Building requires CycloneDDS's C headers/library (libddsc) on the build
// host; this cannot be compiled or tested in an environment without it (see
// Makefile's `idl-gen` target and README's "Build prerequisites" section).
package ddscore

/*
#cgo pkg-config: CycloneDDS
#include <dds/dds.h>
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"time"
	"unsafe"
)

// Entity is a raw CycloneDDS entity handle (dds_entity_t is a signed int32;
// negative values are error codes).
type Entity int32

func check(ret C.dds_entity_t) (Entity, error) {
	if ret < 0 {
		return 0, retcodeError(C.dds_return_t(ret))
	}
	return Entity(ret), nil
}

func retcodeError(ret C.dds_return_t) error {
	return errors.New(C.GoString(C.dds_strretcode(-ret)))
}

// Participant is a CycloneDDS domain participant. One is created per stream,
// mirroring the previous per-stream zenoh session lifecycle so reconnect
// semantics (Stop/Start, error-driven retry) stay unchanged.
type Participant struct {
	entity Entity
}

// NewParticipant joins the given CycloneDDS domain. domainID 0 is the
// default domain Unitree's onboard DDS nodes publish on; override via
// CYCLONEDDS_URI if the robot's network config requires a non-default
// domain or a specific NIC.
func NewParticipant(domainID uint32) (*Participant, error) {
	e, err := check(C.dds_create_participant(C.dds_domainid_t(domainID), nil, nil))
	if err != nil {
		return nil, fmt.Errorf("dds_create_participant(domain=%d): %w", domainID, err)
	}
	return &Participant{entity: e}, nil
}

// Close deletes the participant and, transitively, every topic/reader/
// waitset created from it.
func (p *Participant) Close() error {
	if ret := C.dds_delete(C.dds_entity_t(p.entity)); ret < 0 {
		return fmt.Errorf("dds_delete(participant): %w", retcodeError(ret))
	}
	return nil
}

// CreateTopic creates a topic on this participant. descriptor must point to
// the idlc-generated dds_topic_descriptor_t for the message type being
// subscribed (e.g. &C.LowState_desc for unitree_go/msg/LowState).
func (p *Participant) CreateTopic(name string, descriptor unsafe.Pointer) (Entity, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	e, err := check(C.dds_create_topic(
		C.dds_entity_t(p.entity),
		(*C.dds_topic_descriptor_t)(descriptor),
		cname, nil, nil,
	))
	if err != nil {
		return 0, fmt.Errorf("dds_create_topic(%q): %w", name, err)
	}
	return e, nil
}

// CreateReader creates a best-effort-or-configured-QoS reader for topic.
// QoS is left at CycloneDDS defaults (reliable, volatile, keep-last-1),
// matching what Unitree's own tooling (ros2 topic echo/hz) uses to observe
// the same topics.
func (p *Participant) CreateReader(topic Entity) (Entity, error) {
	e, err := check(C.dds_create_reader(C.dds_entity_t(p.entity), C.dds_entity_t(topic), nil, nil))
	if err != nil {
		return 0, fmt.Errorf("dds_create_reader: %w", err)
	}
	return e, nil
}

// SampleInfo mirrors the fields of dds_sample_info_t this package needs.
type SampleInfo struct {
	ValidData       bool
	SourceTimestamp int64 // nanoseconds since the Unix epoch (dds_time_t)
}

// Take reads (and removes) up to one sample from reader into sampleBuf, a
// pointer to a single instance of the message's idlc-generated struct type
// (e.g. &C.LowState{}). It returns whether a sample was actually delivered;
// take may return ok=false with no error when woken by WaitForData without
// new data (spurious wakeup).
func Take(reader Entity, sampleBuf unsafe.Pointer) (ok bool, info SampleInfo, err error) {
	var cInfo C.dds_sample_info_t
	ret := C.dds_take(C.dds_entity_t(reader), &sampleBuf, &cInfo, 1, 1)
	if ret < 0 {
		return false, SampleInfo{}, fmt.Errorf("dds_take: %w", retcodeError(ret))
	}
	if ret == 0 || !bool(cInfo.valid_data) {
		return false, SampleInfo{}, nil
	}
	return true, SampleInfo{
		ValidData:       bool(cInfo.valid_data),
		SourceTimestamp: int64(cInfo.source_timestamp),
	}, nil
}

// WaitSet wraps a single-reader waitset used to block until a sample is
// available (or the timeout elapses), so the read loop can still observe
// context cancellation instead of blocking forever in dds_take.
type WaitSet struct {
	entity Entity
	cond   Entity
}

// NewWaitSet creates a waitset owned by participant, attached to a read
// condition on reader that triggers for any sample/view/instance state.
func NewWaitSet(participant *Participant, reader Entity) (*WaitSet, error) {
	ws, err := check(C.dds_create_waitset(C.dds_entity_t(participant.entity)))
	if err != nil {
		return nil, fmt.Errorf("dds_create_waitset: %w", err)
	}

	cond, err := check(C.dds_create_readcondition(C.dds_entity_t(reader), C.DDS_ANY_STATE))
	if err != nil {
		_ = C.dds_delete(C.dds_entity_t(ws))
		return nil, fmt.Errorf("dds_create_readcondition: %w", err)
	}

	if ret := C.dds_waitset_attach(C.dds_entity_t(ws), C.dds_entity_t(cond), C.dds_attach_t(cond)); ret < 0 {
		_ = C.dds_delete(C.dds_entity_t(ws))
		return nil, fmt.Errorf("dds_waitset_attach: %w", retcodeError(ret))
	}

	return &WaitSet{entity: ws, cond: cond}, nil
}

// Wait blocks until a sample is available on the attached reader or timeout
// elapses, whichever comes first. Returns true if data may be available.
func (w *WaitSet) Wait(timeout time.Duration) (bool, error) {
	var xs [1]C.dds_attach_t
	ret := C.dds_waitset_wait(C.dds_entity_t(w.entity), &xs[0], 1, C.dds_duration_t(timeout.Nanoseconds()))
	if ret < 0 {
		return false, fmt.Errorf("dds_waitset_wait: %w", retcodeError(ret))
	}
	return ret > 0, nil
}

// Close deletes the waitset. The read condition is owned by the reader and
// is deleted when the reader (or its participant) is deleted.
func (w *WaitSet) Close() error {
	if ret := C.dds_delete(C.dds_entity_t(w.entity)); ret < 0 {
		return fmt.Errorf("dds_delete(waitset): %w", retcodeError(ret))
	}
	return nil
}
