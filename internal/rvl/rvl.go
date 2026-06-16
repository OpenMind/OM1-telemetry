package rvl

import "encoding/binary"

type encoder struct {
	words          []uint32
	word           uint32
	nibblesWritten int
}

func (e *encoder) encodeVLE(value uint32) {
	for {
		nibble := value & 0x7
		value >>= 3
		if value != 0 {
			nibble |= 0x8
		}
		e.word = (e.word << 4) | nibble
		e.nibblesWritten++
		if e.nibblesWritten == 8 {
			e.words = append(e.words, e.word)
			e.nibblesWritten = 0
			e.word = 0
		}
		if value == 0 {
			return
		}
	}
}

func Encode(depth []uint16) []byte {
	e := &encoder{}
	n := len(depth)
	previous := int32(0)

	i := 0
	for i < n {
		zeros := uint32(0)
		for i < n && depth[i] == 0 {
			i++
			zeros++
		}
		e.encodeVLE(zeros)

		start := i
		nonzeros := uint32(0)
		for i < n && depth[i] != 0 {
			i++
			nonzeros++
		}
		e.encodeVLE(nonzeros)

		for j := uint32(0); j < nonzeros; j++ {
			current := int32(depth[start+int(j)])
			delta := current - previous
			zigzag := uint32((delta << 1) ^ (delta >> 31))
			e.encodeVLE(zigzag)
			previous = current
		}
	}

	if e.nibblesWritten != 0 {
		e.word <<= 4 * (8 - uint(e.nibblesWritten))
		e.words = append(e.words, e.word)
	}

	out := make([]byte, len(e.words)*4)
	for k, w := range e.words {
		binary.LittleEndian.PutUint32(out[k*4:], w)
	}
	return out
}

type decoder struct {
	words          []uint32
	index          int
	word           uint32
	nibblesWritten int
}

func (d *decoder) decodeVLE() uint32 {
	value := uint32(0)
	bits := uint(29)
	for {
		if d.nibblesWritten == 0 {
			d.word = d.words[d.index]
			d.index++
			d.nibblesWritten = 8
		}
		nibble := d.word & 0xf0000000
		value |= (nibble << 1) >> bits
		d.word <<= 4
		d.nibblesWritten--
		bits -= 3
		if nibble&0x80000000 == 0 {
			return value
		}
	}
}

func Decode(data []byte, numPixels int) []uint16 {
	words := make([]uint32, len(data)/4)
	for k := range words {
		words[k] = binary.LittleEndian.Uint32(data[k*4:])
	}

	d := &decoder{words: words}
	out := make([]uint16, numPixels)
	previous := int32(0)
	pos := 0
	remaining := numPixels

	for remaining > 0 {
		zeros := int(d.decodeVLE())
		for z := 0; z < zeros; z++ {
			out[pos] = 0
			pos++
		}
		remaining -= zeros

		nonzeros := int(d.decodeVLE())
		for nz := 0; nz < nonzeros; nz++ {
			zigzag := d.decodeVLE()
			delta := int32(zigzag>>1) ^ -int32(zigzag&1)
			current := previous + delta
			out[pos] = uint16(current)
			pos++
			previous = current
		}
		remaining -= nonzeros
	}
	return out
}
