package controllers

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// Session pods run with hostNetwork on a shared node IP, so every port a
// session pod listens on must be unique on that node. Each session Deployment
// gets one contiguous block, laid out as the offsets below.
//
// HTTP/HTTPS are in the block although Moonlight never reaches them (it talks
// to moonlight-proxy): Wolf always binds them, and on the host network two
// Wolfs, or Wolf and a host-networked moonlight-proxy, would collide on the
// defaults 47989/47984.
const (
	portOffsetHTTP = iota
	portOffsetHTTPS
	portOffsetRTSP
	portOffsetControl
	portOffsetVideoRTP
	portOffsetAudioRTP
	portOffsetWolfAgent

	sessionPortBlockSize
)

// portBlockAnnotation on a session Deployment records the first port of the
// block its pod template was built for.
const portBlockAnnotation = "direwolf.games-on-whales.github.io/port-block"

// PortRange is an inclusive range of host ports session blocks are cut from.
type PortRange struct {
	Min, Max int32
}

// ParsePortRange parses "MIN-MAX" and checks it holds at least one block.
func ParsePortRange(s string) (PortRange, error) {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return PortRange{}, fmt.Errorf("port range %q: want MIN-MAX", s)
	}
	minPort, err := strconv.ParseUint(strings.TrimSpace(lo), 10, 16)
	if err != nil {
		return PortRange{}, fmt.Errorf("port range %q: %w", s, err)
	}
	maxPort, err := strconv.ParseUint(strings.TrimSpace(hi), 10, 16)
	if err != nil {
		return PortRange{}, fmt.Errorf("port range %q: %w", s, err)
	}
	r := PortRange{Min: int32(minPort), Max: int32(maxPort)}
	if r.Min < 1 || r.blocks() < 1 {
		return PortRange{}, fmt.Errorf("port range %q must hold at least %d ports starting at 1 or above", s, sessionPortBlockSize)
	}
	return r, nil
}

func (r PortRange) blocks() int32 {
	if r.Max < r.Min {
		return 0
	}
	return (r.Max - r.Min + 1) / sessionPortBlockSize
}

// blockPorts returns the ports of the block starting at base.
func blockPorts(base int32) v1alpha1types.SessionPorts {
	return v1alpha1types.SessionPorts{
		HTTP:      base + portOffsetHTTP,
		HTTPS:     base + portOffsetHTTPS,
		RTSP:      base + portOffsetRTSP,
		Control:   base + portOffsetControl,
		VideoRTP:  base + portOffsetVideoRTP,
		AudioRTP:  base + portOffsetAudioRTP,
		WolfAgent: base + portOffsetWolfAgent,
	}
}

// portAllocator hands out non-overlapping port blocks, one per owner. The
// owner is the session Deployment: sessions sharing a Deployment share its pod
// and therefore its ports. It is safe for concurrent use.
//
// The allocator is in-memory; the source of truth is Session.status.ports,
// which the controller replays through Claim on startup.
type portAllocator struct {
	mu     sync.Mutex
	r      PortRange
	blocks map[string]int32 // owner -> block base
}

func newPortAllocator(r PortRange) *portAllocator {
	return &portAllocator{r: r, blocks: map[string]int32{}}
}

// Allocate returns owner's block, assigning the lowest free one if it has none.
func (a *portAllocator) Allocate(owner string) (v1alpha1types.SessionPorts, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if base, ok := a.blocks[owner]; ok {
		return blockPorts(base), nil
	}
	used := sets.New[int32]()
	for _, base := range a.blocks {
		used.Insert(base)
	}
	for i := range a.r.blocks() {
		base := a.r.Min + i*sessionPortBlockSize
		if !used.Has(base) {
			a.blocks[owner] = base
			return blockPorts(base), nil
		}
	}
	return v1alpha1types.SessionPorts{}, fmt.Errorf("no free session port block in %d-%d (%d in use)", a.r.Min, a.r.Max, len(a.blocks))
}

// Claim records ports previously allocated to owner, e.g. read back from a
// Session's status after an operator restart. It fails if ports are not a
// block of this range or the block belongs to another owner.
func (a *portAllocator) Claim(owner string, ports v1alpha1types.SessionPorts) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	base := ports.HTTP
	if base < a.r.Min || (base-a.r.Min)%sessionPortBlockSize != 0 ||
		(base-a.r.Min)/sessionPortBlockSize >= a.r.blocks() || ports != blockPorts(base) {
		return fmt.Errorf("ports %+v are not a block of range %d-%d", ports, a.r.Min, a.r.Max)
	}
	if held, ok := a.blocks[owner]; ok {
		if held != base {
			return fmt.Errorf("%s already holds the block at %d", owner, held)
		}
		return nil
	}
	for other, b := range a.blocks {
		if b == base {
			return fmt.Errorf("block at %d is held by %s", base, other)
		}
	}
	a.blocks[owner] = base
	return nil
}

// Retain releases the block of every owner not in live.
func (a *portAllocator) Retain(live sets.Set[string]) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for owner := range a.blocks {
		if !live.Has(owner) {
			delete(a.blocks, owner)
		}
	}
}
