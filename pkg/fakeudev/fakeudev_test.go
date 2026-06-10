package fakeudev

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Reference values computed with wolf's bundled MurmurHash2.cpp
// (the same implementation libudev/systemd uses for filter hashes).
func TestMurmurHash2(t *testing.T) {
	cases := map[string]uint32{
		"input":  3248653424,
		"hidraw": 3268080535,
	}
	for in, want := range cases {
		if got := murmurHash2([]byte(in), 0); got != want {
			t.Errorf("murmurHash2(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestMakeHeader(t *testing.T) {
	h := makeHeader(100, "input", "")
	if len(h) != 40 {
		t.Fatalf("header length = %d, want 40", len(h))
	}
	if !bytes.Equal(h[:8], append([]byte("libudev"), 0)) {
		t.Errorf("prefix = %q", h[:8])
	}
	if magic := binary.BigEndian.Uint32(h[8:12]); magic != udevMonitorMagic {
		t.Errorf("magic = %#x", magic)
	}
	if off := binary.LittleEndian.Uint32(h[16:20]); off != 40 {
		t.Errorf("properties_off = %d", off)
	}
	if l := binary.LittleEndian.Uint32(h[20:24]); l != 100 {
		t.Errorf("properties_len = %d", l)
	}
	if sh := binary.BigEndian.Uint32(h[24:28]); sh != 3248653424 {
		t.Errorf("subsystem hash = %d, want %d", sh, 3248653424)
	}
}

func TestEncodeProperties(t *testing.T) {
	got := encodeProperties(map[string]string{
		"ACTION":  "add",
		"DEVNAME": "/dev/input/event3",
	})
	want := []byte("ACTION=add\x00DEVNAME=/dev/input/event3\x00")
	if !bytes.Equal(got, want) {
		t.Errorf("encodeProperties = %q, want %q", got, want)
	}
}
