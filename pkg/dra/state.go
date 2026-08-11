package dra

import (
	"sync"
	"time"
)

// WolfResourceState tracks everything we need to unprepare a claim.
type WolfResourceState struct {
	ClaimUID          string
	LobbyID           string
	LobbyName         string
	WaylandIndex      int
	WaylandSocketName string
	// PulseSinkName     string // TODO PulseAudio
	CreatedAt time.Time
}

// State is a thread-safe in-memory registry keyed by claim UID.
// It is reconciled against Wolf's lobby list on startup and on the
// first PrepareResourceClaims call
type State struct {
	mu     sync.RWMutex
	claims map[string]*WolfResourceState
}

func NewState() *State {
	return &State{
		claims: make(map[string]*WolfResourceState),
	}
}

func (s *State) Set(claimUID string, st *WolfResourceState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims[claimUID] = st
}

func (s *State) Get(claimUID string) (*WolfResourceState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.claims[claimUID]
	return st, ok
}

func (s *State) Delete(claimUID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claims, claimUID)
}

func (s *State) List() []*WolfResourceState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*WolfResourceState, 0, len(s.claims))
	for _, v := range s.claims {
		out = append(out, v)
	}
	return out
}

// UsedWaylandIndices returns the set of wayland indices currently in use.
func (s *State) UsedWaylandIndices() map[int]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[int]struct{}, len(s.claims))
	for _, v := range s.claims {
		out[v.WaylandIndex] = struct{}{}
	}
	return out
}
