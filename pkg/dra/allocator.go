package dra

import (
	"sync"
)

// Allocator tracks which wayland indices (1..N) are in use.
// It does not allocate the socket itself — Wolf does that.
// We use it to sync driver state with the filesystem and to
// know when we've hit capacity.
type Allocator struct {
	mu   sync.Mutex
	max  int
	used map[int]struct{}
}

func NewAllocator(max int) *Allocator {
	return &Allocator{
		max:  max,
		used: make(map[int]struct{}),
	}
}

// SyncFromState rebuilds the used set from the current state registry.
func (a *Allocator) SyncFromState(state *State) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used = state.UsedWaylandIndices()
}

// Allocate atomically picks the lowest unused index in [1, max] and
// marks it used. Returns false if at capacity. Used by the reconciliation
// path where we recover orphaned lobbies after a driver restart
// so that wolf-dra crashes don't stop the lobby stream
func (a *Allocator) Allocate() (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 1; i <= a.max; i++ {
		if _, used := a.used[i]; !used {
			a.used[i] = struct{}{}
			return i, true
		}
	}
	return 0, false
}

// MarkUsed records that an index is consumed.
func (a *Allocator) MarkUsed(idx int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used[idx] = struct{}{}
}

// Release frees an index.
func (a *Allocator) Release(idx int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.used, idx)
}

// Available returns true if we haven't reached max capacity.
func (a *Allocator) Available() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.used) < a.max
}

// Max returns the configured ceiling.
func (a *Allocator) Max() int {
	return a.max
}

// Used returns a snapshot of the currently used indices.
func (a *Allocator) Used() map[int]struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[int]struct{}, len(a.used))
	for k := range a.used {
		out[k] = struct{}{}
	}
	return out
}
