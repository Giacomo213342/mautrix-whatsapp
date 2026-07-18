package callbridge

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Direction uint8

const (
	DirectionWhatsAppToMatrix Direction = iota + 1
	DirectionMatrixToWhatsApp
)

type Phase uint8

const (
	PhaseNew Phase = iota
	PhaseRinging
	PhaseConnecting
	PhaseActive
	PhaseEnding
	PhaseEnded
)

var (
	ErrInvalidTransition = errors.New("invalid call state transition")
	ErrLoginBusy         = errors.New("another call is already active for this login")
	ErrDuplicateCall     = errors.New("call already exists")
)

type Session struct {
	ID           string
	LoginID      string
	MatrixCallID string
	WhatsAppID   string
	Direction    Direction
	CreatedAt    time.Time

	ctx    context.Context
	cancel context.CancelFunc

	mu            sync.RWMutex
	phase         Phase
	remotePartyID string
	closed        chan struct{}
	closeOnce     sync.Once
}

func NewSession(parent context.Context, id, loginID string, direction Direction) *Session {
	ctx, cancel := context.WithCancel(parent)
	return &Session{
		ID:        id,
		LoginID:   loginID,
		Direction: direction,
		CreatedAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
		closed:    make(chan struct{}),
	}
}

func (s *Session) Context() context.Context {
	return s.ctx
}

func (s *Session) Done() <-chan struct{} {
	return s.closed
}

func (s *Session) Phase() Phase {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.phase
}

func (s *Session) Transition(next Phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !canTransition(s.phase, next) {
		return ErrInvalidTransition
	}
	s.phase = next
	if next == PhaseEnded {
		s.closeOnce.Do(func() {
			s.cancel()
			close(s.closed)
		})
	}
	return nil
}

func (s *Session) SelectRemoteParty(partyID string) bool {
	if partyID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remotePartyID != "" {
		return s.remotePartyID == partyID
	}
	s.remotePartyID = partyID
	return true
}

func (s *Session) RemotePartyID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.remotePartyID
}

func canTransition(current, next Phase) bool {
	switch current {
	case PhaseNew:
		return next == PhaseRinging || next == PhaseEnding
	case PhaseRinging:
		return next == PhaseConnecting || next == PhaseEnding
	case PhaseConnecting:
		return next == PhaseActive || next == PhaseEnding
	case PhaseActive:
		return next == PhaseEnding
	case PhaseEnding:
		return next == PhaseEnded
	default:
		return false
	}
}

type Manager struct {
	mu       sync.RWMutex
	byID     map[string]*Session
	byLogin  map[string]map[string]*Session
	maxCalls int
}

func NewManager(maxCallsPerLogin int) *Manager {
	if maxCallsPerLogin < 1 {
		maxCallsPerLogin = 1
	}
	return &Manager{
		byID:     make(map[string]*Session),
		byLogin:  make(map[string]map[string]*Session),
		maxCalls: maxCallsPerLogin,
	}
}

func (m *Manager) Add(session *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byID[session.ID]; exists {
		return ErrDuplicateCall
	}
	loginCalls := m.byLogin[session.LoginID]
	if len(loginCalls) >= m.maxCalls {
		return ErrLoginBusy
	}
	if loginCalls == nil {
		loginCalls = make(map[string]*Session)
		m.byLogin[session.LoginID] = loginCalls
	}
	m.byID[session.ID] = session
	loginCalls[session.ID] = session
	return nil
}

func (m *Manager) Get(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[id]
}

func (m *Manager) Remove(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.byID[id]
	if session == nil {
		return nil
	}
	delete(m.byID, id)
	loginCalls := m.byLogin[session.LoginID]
	delete(loginCalls, id)
	if len(loginCalls) == 0 {
		delete(m.byLogin, session.LoginID)
	}
	return session
}

func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byID)
}
