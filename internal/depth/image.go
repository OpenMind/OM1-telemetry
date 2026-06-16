package depth

import (
	"encoding/binary"
	"fmt"
)

type Image struct {
	Height      uint32
	Width       uint32
	Encoding    string
	IsBigendian bool
	Step        uint32
	Data        []byte
}

type cdrReader struct {
	body   []byte
	pos    int
	little bool
}

func newCDRReader(payload []byte) (*cdrReader, error) {
	if len(payload) < 4 {
		return nil, fmt.Errorf("payload too short for CDR encapsulation header: %d bytes", len(payload))
	}
	little := payload[1]&0x01 == 0x01
	return &cdrReader{body: payload[4:], little: little}, nil
}

func (r *cdrReader) align(n int) {
	if m := r.pos % n; m != 0 {
		r.pos += n - m
	}
}

func (r *cdrReader) need(n int) error {
	if r.pos+n > len(r.body) {
		return fmt.Errorf("cdr: need %d bytes at offset %d, have %d", n, r.pos, len(r.body))
	}
	return nil
}

func (r *cdrReader) u32() (uint32, error) {
	r.align(4)
	if err := r.need(4); err != nil {
		return 0, err
	}
	var v uint32
	if r.little {
		v = binary.LittleEndian.Uint32(r.body[r.pos:])
	} else {
		v = binary.BigEndian.Uint32(r.body[r.pos:])
	}
	r.pos += 4
	return v, nil
}

func (r *cdrReader) u8() (uint8, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	v := r.body[r.pos]
	r.pos++
	return v, nil
}

func (r *cdrReader) str() (string, error) {
	n, err := r.u32() // length includes the trailing NUL
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

func (r *cdrReader) bytes(n int) ([]byte, error) {
	if err := r.need(n); err != nil {
		return nil, err
	}
	b := r.body[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

// ParseImage decodes a CDR-serialized sensor_msgs/Image payload (as delivered
// over the zenoh-ros bridge).
func ParseImage(payload []byte) (*Image, error) {
	r, err := newCDRReader(payload)
	if err != nil {
		return nil, err
	}

	if _, err := r.u32(); err != nil { // stamp.sec
		return nil, fmt.Errorf("stamp.sec: %w", err)
	}
	if _, err := r.u32(); err != nil { // stamp.nanosec
		return nil, fmt.Errorf("stamp.nanosec: %w", err)
	}
	if _, err := r.str(); err != nil { // frame_id
		return nil, fmt.Errorf("frame_id: %w", err)
	}

	height, err := r.u32()
	if err != nil {
		return nil, fmt.Errorf("height: %w", err)
	}
	width, err := r.u32()
	if err != nil {
		return nil, fmt.Errorf("width: %w", err)
	}
	encoding, err := r.str()
	if err != nil {
		return nil, fmt.Errorf("encoding: %w", err)
	}
	isBig, err := r.u8()
	if err != nil {
		return nil, fmt.Errorf("is_bigendian: %w", err)
	}
	step, err := r.u32()
	if err != nil {
		return nil, fmt.Errorf("step: %w", err)
	}
	dataLen, err := r.u32()
	if err != nil {
		return nil, fmt.Errorf("data length: %w", err)
	}
	data, err := r.bytes(int(dataLen))
	if err != nil {
		return nil, fmt.Errorf("data: %w", err)
	}

	return &Image{
		Height:      height,
		Width:       width,
		Encoding:    encoding,
		IsBigendian: isBig != 0,
		Step:        step,
		Data:        data,
	}, nil
}

func (img *Image) DepthPixels() ([]uint16, error) {
	switch img.Encoding {
	case "16UC1", "mono16":
	default:
		return nil, fmt.Errorf("unsupported depth encoding %q (want 16UC1 or mono16)", img.Encoding)
	}

	n := int(img.Width) * int(img.Height)
	if len(img.Data) < n*2 {
		return nil, fmt.Errorf("depth data too short: have %d bytes, need %d", len(img.Data), n*2)
	}

	pixels := make([]uint16, n)
	if img.IsBigendian {
		for i := 0; i < n; i++ {
			pixels[i] = binary.BigEndian.Uint16(img.Data[i*2:])
		}
	} else {
		for i := 0; i < n; i++ {
			pixels[i] = binary.LittleEndian.Uint16(img.Data[i*2:])
		}
	}
	return pixels, nil
}