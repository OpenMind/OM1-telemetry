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
	"runtime"
	"time"
	"unsafe"
)

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

type Participant struct {
	entity Entity
}

func NewParticipant(domainID uint32) (*Participant, error) {
	e, err := check(C.dds_create_participant(C.dds_domainid_t(domainID), nil, nil))
	if err != nil {
		return nil, fmt.Errorf("dds_create_participant(domain=%d): %w", domainID, err)
	}
	return &Participant{entity: e}, nil
}

// closeTimeout bounds how long dds_delete is given to return. An entity
// built on a subscription that never received any data can leave the call
// hanging in native code forever, which no Go-level cancellation can
// interrupt, so callers stop waiting instead of wedging shutdown.
const closeTimeout = 5 * time.Second

// closeEntity deletes entity, abandoning (and leaking) the underlying
// cgo call if it does not return within closeTimeout.
func closeEntity(entity C.dds_entity_t, what string) error {
	done := make(chan error, 1)
	go func() {
		if ret := C.dds_delete(entity); ret < 0 {
			done <- retcodeError(ret)
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("dds_delete(%s): %w", what, err)
		}
		return nil
	case <-time.After(closeTimeout):
		return fmt.Errorf("dds_delete(%s): timed out after %s, abandoning", what, closeTimeout)
	}
}

func (p *Participant) Close() error {
	return closeEntity(C.dds_entity_t(p.entity), "participant")
}

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

func (p *Participant) CreateReader(topic Entity) (Entity, error) {
	e, err := check(C.dds_create_reader(C.dds_entity_t(p.entity), C.dds_entity_t(topic), nil, nil))
	if err != nil {
		return 0, fmt.Errorf("dds_create_reader: %w", err)
	}
	return e, nil
}

func (p *Participant) CreateWriter(topic Entity) (Entity, error) {
	e, err := check(C.dds_create_writer(C.dds_entity_t(p.entity), C.dds_entity_t(topic), nil, nil))
	if err != nil {
		return 0, fmt.Errorf("dds_create_writer: %w", err)
	}
	return e, nil
}

type SampleInfo struct {
	ValidData       bool
	SourceTimestamp int64
}

func Take(reader Entity, sampleBuf unsafe.Pointer) (ok bool, info SampleInfo, err error) {
	var pinner runtime.Pinner
	pinner.Pin(sampleBuf)
	defer pinner.Unpin()

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

type WaitSet struct {
	entity Entity
	cond   Entity
}

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

// Wait blocks for up to timeout; on error it sleeps for timeout too, since
// callers retry on error with no backoff of their own.
func (w *WaitSet) Wait(timeout time.Duration) (bool, error) {
	var xs [1]C.dds_attach_t
	ret := C.dds_waitset_wait(C.dds_entity_t(w.entity), &xs[0], 1, C.dds_duration_t(timeout.Nanoseconds()))
	if ret < 0 {
		time.Sleep(timeout)
		return false, fmt.Errorf("dds_waitset_wait: %w", retcodeError(ret))
	}
	return ret > 0, nil
}

func (w *WaitSet) Close() error {
	return closeEntity(C.dds_entity_t(w.entity), "waitset")
}
