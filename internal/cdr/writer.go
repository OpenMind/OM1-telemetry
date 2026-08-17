package cdr

import (
	"encoding/binary"
	"math"
)

var littleEndianHeader = [4]byte{0x00, 0x01, 0x00, 0x00}

type Writer struct {
	buf []byte
}

func NewWriter() *Writer {
	w := &Writer{buf: make([]byte, 4)}
	copy(w.buf, littleEndianHeader[:])
	return w
}

func (w *Writer) Bytes() []byte {
	return w.buf
}

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

func (w *Writer) Str(s string) {
	w.U32(uint32(len(s) + 1))
	w.buf = append(w.buf, s...)
	w.buf = append(w.buf, 0)
}

func (w *Writer) Seq(b []byte) {
	w.U32(uint32(len(b)))
	w.buf = append(w.buf, b...)
}

func (w *Writer) RawBytes(b []byte) {
	w.buf = append(w.buf, b...)
}
