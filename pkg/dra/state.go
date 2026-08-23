package dra

import (
	"maps"
	"slices"
	"sync"
	"time"
)

// WolfResourceState tracks everything we need to unprepare a claim.
type WolfResourceState struct {
	ClaimUID          string
	ClaimName         string
	ClaimNamespace    string
	LobbyID           string
	LobbyName         string
	WaylandIndex      int
	WaylandSocketName string
	CreatedAt         time.Time
}

// SessionState tracks an active or pending Session CRD.
type SessionState struct {
	ClaimUID      string
	SessionUID    string
	SessionName   string
	LobbyName     string
	WolfSessionID string // MoonlightClientID is the same as session ID
	WaylandIndex  int
	WaylandSocket string
	CreatedAt     time.Time
	JoinedLobby   bool
}

// State is a thread-safe in-memory registry keyed by claim UID.
// It is reconciled against Wolf's lobby list on startup and on the
// first PrepareResourceClaims call
type State struct {
	mu               sync.RWMutex
	claims           map[string]*WolfResourceState
	lobbyNameToClaim map[string]string        // maps lobby name to claim UID
	sessions         map[string]*SessionState // keyed by session UID
	pending          map[string]struct{}      // set of session UIDs waiting for a lobby
}

func NewState() *State {
	return &State{
		claims:           make(map[string]*WolfResourceState),
		lobbyNameToClaim: make(map[string]string),
		sessions:         make(map[string]*SessionState),
		pending:          make(map[string]struct{}),
	}
}

func (s *State) Set(claimUID string, st *WolfResourceState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Clean up old lobby mapping if the claim is being re-assigned.
	if old, ok := s.claims[claimUID]; ok && old.LobbyName != "" && old.LobbyName != st.LobbyName {
		delete(s.lobbyNameToClaim, old.LobbyName)
	}

	s.claims[claimUID] = st
	if st.LobbyName != "" {
		s.lobbyNameToClaim[st.LobbyName] = claimUID
	}
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
	st, ok := s.claims[claimUID]
	if ok && st.LobbyName != "" {
		delete(s.lobbyNameToClaim, st.LobbyName)
	}
	delete(s.claims, claimUID)
}

func (s *State) List() []*WolfResourceState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Collect(maps.Values(s.claims))
}

// UsedWaylandIndices returns the set of wayland indices currently in use
// by both claims and active sessions.
func (s *State) UsedWaylandIndices() map[int]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[int]struct{}, len(s.claims)+len(s.sessions))
	for _, v := range s.claims {
		out[v.WaylandIndex] = struct{}{}
	}
	for _, v := range s.sessions {
		out[v.WaylandIndex] = struct{}{}
	}
	return out
}

// GetClaimByLobbyName returns the claim UID for a given lobby name, if managed.
func (s *State) GetClaimByLobbyName(lobbyName string) (claimUID string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	claimUID, ok = s.lobbyNameToClaim[lobbyName]
	return
}

// AddSession registers an active session.
func (s *State) AddSession(ss *SessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[ss.SessionUID] = ss
}

// GetSession returns an active session by UID.
func (s *State) GetSession(sessionUID string) (*SessionState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ss, ok := s.sessions[sessionUID]
	return ss, ok
}

// DeleteSession removes an active session.
func (s *State) DeleteSession(sessionUID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionUID)
}

// GetSessionsForClaim returns all active sessions belonging to a claim.
func (s *State) GetSessionsForClaim(claimUID string) []*SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st, ok := s.claims[claimUID]
	if !ok || st.LobbyName == "" {
		return nil
	}

	out := make([]*SessionState, 0)
	for _, ss := range s.sessions {
		if ss.LobbyName == st.LobbyName {
			out = append(out, ss)
		}
	}
	return out
}

// GetSessionByWolfID looks up an active session by its Wolf session ID.
func (s *State) GetSessionByWolfID(wolfSessionID string) (*SessionState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ss := range s.sessions {
		if ss.WolfSessionID == wolfSessionID {
			return ss, true
		}
	}
	return nil, false
}

// AddPending adds a session UID to the pending set.
func (s *State) AddPending(sessionUID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[sessionUID] = struct{}{}
}

// RemovePending removes a session UID from the pending set.
func (s *State) RemovePending(sessionUID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, sessionUID)
}

// GetPendingList returns a snapshot of all pending session UIDs.
func (s *State) GetPendingList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.pending))
	for uid := range s.pending {
		out = append(out, uid)
	}
	return out
}
