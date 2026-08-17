package cdr

import (
	"encoding/binary"
	"fmt"
	"math"
)

type Reader struct {
	body   []byte
	pos    int
	little bool
}

func NewReader(payload []byte) (*Reader, error) {
	if len(payload) < 4 {
		return nil, fmt.Errorf("payload too short for CDR encapsulation header: %d bytes", len(payload))
	}
	little := payload[1]&0x01 == 0x01
	return &Reader{body: payload[4:], little: little}, nil
}

func (r *Reader) align(n int) {
	if m := r.pos % n; m != 0 {
		r.pos += n - m
	}
}

func (r *Reader) need(n int) error {
	if r.pos+n > len(r.body) {
		return fmt.Errorf("cdr: need %d bytes at offset %d, have %d", n, r.pos, len(r.body))
	}
	return nil
}

func (r *Reader) U8() (uint8, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	v := r.body[r.pos]
	r.pos++
	return v, nil
}

func (r *Reader) Bool() (bool, error) {
	v, err := r.U8()
	return v != 0, err
}

func (r *Reader) U16() (uint16, error) {
	r.align(2)
	if err := r.need(2); err != nil {
		return 0, err
	}
	v := r.order().Uint16(r.body[r.pos:])
	r.pos += 2
	return v, nil
}

func (r *Reader) U32() (uint32, error) {
	r.align(4)
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := r.order().Uint32(r.body[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *Reader) U64() (uint64, error) {
	r.align(8)
	if err := r.need(8); err != nil {
		return 0, err
	}
	v := r.order().Uint64(r.body[r.pos:])
	r.pos += 8
	return v, nil
}

func (r *Reader) I16() (int16, error) { v, err := r.U16(); return int16(v), err }
func (r *Reader) I32() (int32, error) { v, err := r.U32(); return int32(v), err }
func (r *Reader) I64() (int64, error) { v, err := r.U64(); return int64(v), err }

func (r *Reader) F32() (float32, error) {
	v, err := r.U32()
	return math.Float32frombits(v), err
}

func (r *Reader) F64() (float64, error) {
	v, err := r.U64()
	return math.Float64frombits(v), err
}

func (r *Reader) Str() (string, error) {
	n, err := r.U32() // length includes the trailing NUL
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	if err := r.need(int(n)); err != nil {
		return "", err
	}
	s := string(r.body[r.pos : r.pos+int(n)-1]) // drop NUL terminator
	r.pos += int(n)
	return s, nil
}

func (r *Reader) RawBytes(n int) ([]byte, error) {
	if err := r.need(n); err != nil {
		return nil, err
	}
	b := r.body[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

func (r *Reader) Seq() ([]byte, error) {
	n, err := r.U32()
	if err != nil {
		return nil, err
	}
	return r.RawBytes(int(n))
}

func (r *Reader) order() binary.ByteOrder {
	if r.little {
		return binary.LittleEndian
	}
	return binary.BigEndian
}
