// Package cdr implements just enough of the CDR (Common Data Representation)
// wire encoding used by ROS 2 / DDS messages to re-serialize samples that
// have already been decoded off a DDS reader (see internal/*/dds_reader.go).
//
// This mirrors the decode-side rules already relied on by
// internal/depth.ParseImage (a little-endian XCDR1 body prefixed by a 4-byte
// encapsulation header, with fields aligned to their own size), so that
// bytes produced here decode identically to what the previous zenoh-bridge
// pipeline delivered.
package cdr

import (
	"encoding/binary"
	"math"
)

// encapsulation header: [options, format, 0, 0]. format bit0 set = little-endian.
var littleEndianHeader = [4]byte{0x00, 0x01, 0x00, 0x00}

// Writer builds a little-endian CDR-encoded byte buffer, starting with the
// standard 4-byte encapsulation header.
type Writer struct {
	buf []byte
}

// NewWriter returns a Writer preloaded with the CDR encapsulation header.
func NewWriter() *Writer {
	w := &Writer{buf: make([]byte, 4)}
	copy(w.buf, littleEndianHeader[:])
	return w
}

// Bytes returns the encoded buffer, including the encapsulation header.
func (w *Writer) Bytes() []byte {
	return w.buf
}

// bodyLen is the number of bytes written after the 4-byte header; alignment
// is computed relative to this, matching the reader's convention.
func (w *Writer) bodyLen() int {
	return len(w.buf) - 4
}

func (w *Writer) align(n int) {
	if m := w.bodyLen() % n; m != 0 {
		pad := n - m
		w.buf = append(w.buf, make([]byte, pad)...)
	}
}

func (w *Writer) U8(v uint8) {
	w.buf = append(w.buf, v)
}

func (w *Writer) Bool(v bool) {
	if v {
		w.U8(1)
	} else {
		w.U8(0)
	}
}

func (w *Writer) U16(v uint16) {
	w.align(2)
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

func (w *Writer) U32(v uint32) {
	w.align(4)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

func (w *Writer) U64(v uint64) {
	w.align(8)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

func (w *Writer) I16(v int16) { w.U16(uint16(v)) }
func (w *Writer) I32(v int32) { w.U32(uint32(v)) }
func (w *Writer) I64(v int64) { w.U64(uint64(v)) }

func (w *Writer) F32(v float32) {
	w.U32(math.Float32bits(v))
}

func (w *Writer) F64(v float64) {
	w.U64(math.Float64bits(v))
}

// Str writes a CDR string: a uint32 length (including the trailing NUL)
// followed by the bytes and a NUL terminator.
func (w *Writer) Str(s string) {
	w.U32(uint32(len(s) + 1))
	w.buf = append(w.buf, s...)
	w.buf = append(w.buf, 0)
}

// Seq writes a CDR byte sequence: a uint32 length followed by the raw bytes
// (no terminator, no per-element alignment beyond the length prefix).
// Pairs with Reader.Seq.
func (w *Writer) Seq(b []byte) {
	w.U32(uint32(len(b)))
	w.buf = append(w.buf, b...)
}

// RawBytes appends raw bytes with no length prefix — use when the length is
// (or will be) encoded separately, e.g. sensor_msgs/Image's data field where
// step*height is implied by earlier fields. Pairs with Reader.RawBytes.
func (w *Writer) RawBytes(b []byte) {
	w.buf = append(w.buf, b...)
}
