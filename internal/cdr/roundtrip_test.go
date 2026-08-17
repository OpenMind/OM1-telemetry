package cdr

import "testing"

func TestRoundTrip(t *testing.T) {
	w := NewWriter()
	w.U32(42)
	w.Str("rt/lowstate")
	w.U8(1)
	w.F64(3.5)

	r, err := NewReader(w.Bytes())
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	if v, err := r.U32(); err != nil || v != 42 {
		t.Fatalf("U32: got %d, %v", v, err)
	}
	if s, err := r.Str(); err != nil || s != "rt/lowstate" {
		t.Fatalf("Str: got %q, %v", s, err)
	}
	if v, err := r.U8(); err != nil || v != 1 {
		t.Fatalf("U8: got %d, %v", v, err)
	}
	if v, err := r.F64(); err != nil || v != 3.5 {
		t.Fatalf("F64: got %v, %v", v, err)
	}
}

func TestRoundTrip_lengthPrefixedBytes(t *testing.T) {
	w := NewWriter()
	w.Seq([]byte{9, 8, 7})

	r, err := NewReader(w.Bytes())
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	b, err := r.Seq()
	if err != nil {
		t.Fatalf("Seq: %v", err)
	}
	if len(b) != 3 || b[0] != 9 || b[1] != 8 || b[2] != 7 {
		t.Fatalf("Seq: got %v", b)
	}
}

func TestAlignment(t *testing.T) {
	w := NewWriter()
	w.U8(1) // body len 1
	w.U32(0xDEADBEEF)

	r, err := NewReader(w.Bytes())
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if v, err := r.U8(); err != nil || v != 1 {
		t.Fatalf("U8: got %d, %v", v, err)
	}
	if v, err := r.U32(); err != nil || v != 0xDEADBEEF {
		t.Fatalf("U32 after alignment: got %#x, %v", v, err)
	}
}
