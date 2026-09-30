package controllers

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

func TestPortAllocatorExhaustionAndReuse(t *testing.T) {
	// Room for exactly two blocks; the trailing ports cannot hold a third.
	a := newPortAllocator(PortRange{Min: 40000, Max: 40000 + 2*sessionPortBlockSize + 3})

	p1, err := a.Allocate("alex-steam")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := a.Allocate("sam-steam")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != blockPorts(40000) || p2 != blockPorts(40000+sessionPortBlockSize) {
		t.Fatalf("blocks = %+v, %+v", p1, p2)
	}
	if _, exhausted := a.Allocate("kim-steam"); exhausted == nil {
		t.Fatal("third allocation succeeded in a two-block range")
	}

	// Allocating again for the same owner returns its block, not a new one.
	if again, againErr := a.Allocate("alex-steam"); againErr != nil || again != p1 {
		t.Fatalf("re-allocate = %+v, %v; want %+v", again, againErr, p1)
	}

	// Releasing alex frees exactly that block for the next owner.
	a.Retain(sets.New("sam-steam"))
	p3, err := a.Allocate("kim-steam")
	if err != nil {
		t.Fatal(err)
	}
	if p3 != p1 {
		t.Fatalf("after release got %+v, want reused %+v", p3, p1)
	}
}

func TestPortAllocatorBlocksDoNotOverlap(t *testing.T) {
	a := newPortAllocator(PortRange{Min: 40000, Max: 40999})
	seen := sets.New[int32]()
	for i := range int(PortRange{Min: 40000, Max: 40999}.blocks()) {
		p, err := a.Allocate(string(rune('a'+i%26)) + string(rune('0'+i/26)))
		if err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
		for _, port := range []int32{p.HTTP, p.HTTPS, p.RTSP, p.Control, p.VideoRTP, p.AudioRTP, p.WolfAgent} {
			if port < 40000 || port > 40999 || seen.Has(port) {
				t.Fatalf("allocation %d: port %d out of range or reused", i, port)
			}
			seen.Insert(port)
		}
	}
}

func TestPortAllocatorClaim(t *testing.T) {
	a := newPortAllocator(PortRange{Min: 40000, Max: 40999})

	if err := a.Claim("alex-steam", blockPorts(40000+sessionPortBlockSize)); err != nil {
		t.Fatal(err)
	}
	// Idempotent for the holder, refused for anyone else.
	if err := a.Claim("alex-steam", blockPorts(40000+sessionPortBlockSize)); err != nil {
		t.Fatal(err)
	}
	if err := a.Claim("sam-steam", blockPorts(40000+sessionPortBlockSize)); err == nil {
		t.Fatal("claimed a block held by another owner")
	}
	if err := a.Claim("alex-steam", blockPorts(40000)); err == nil {
		t.Fatal("owner claimed a second block")
	}
	// A claimed block is skipped by Allocate.
	if p, err := a.Allocate("sam-steam"); err != nil || p != blockPorts(40000) {
		t.Fatalf("allocate = %+v, %v", p, err)
	}

	for name, ports := range map[string]v1alpha1types.SessionPorts{
		"legacy fixed ports": {RTSP: 48010, Control: 47999, VideoRTP: 48100, AudioRTP: 48200},
		"misaligned":         blockPorts(40001),
		"out of range":       blockPorts(41000),
		"not a block": func() v1alpha1types.SessionPorts {
			p := blockPorts(40000 + 5*sessionPortBlockSize)
			p.RTSP = 1
			return p
		}(),
	} {
		if err := a.Claim("kim-"+name, ports); err == nil {
			t.Errorf("%s: claim of %+v succeeded", name, ports)
		}
	}
}

func TestParsePortRange(t *testing.T) {
	if r, err := ParsePortRange("40000-40999"); err != nil || r != (PortRange{Min: 40000, Max: 40999}) {
		t.Fatalf("ParsePortRange = %+v, %v", r, err)
	}
	for _, bad := range []string{"", "40000", "40999-40000", "40000-40005", "0-100", "40000-70000", "a-b"} {
		if _, err := ParsePortRange(bad); err == nil {
			t.Errorf("ParsePortRange(%q) succeeded", bad)
		}
	}
}
