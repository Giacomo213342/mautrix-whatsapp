package connector

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/pion/webrtc/v4"
	"github.com/purpshell/meowcaller"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-whatsapp/pkg/callbridge"
	callaudio "go.mau.fi/mautrix-whatsapp/pkg/callbridge/audio"
)

type voiceCallManager struct {
	connector *WhatsAppConnector
	sessions  *callbridge.Manager

	mu           sync.RWMutex
	byMatrixCall map[string]*liveVoiceCall
	byWhatsApp   map[string]*liveVoiceCall
	closed       bool
}

type liveVoiceCall struct {
	manager      *voiceCallManager
	session      *callbridge.Session
	client       *WhatsAppClient
	portal       *bridgev2.Portal
	intent       bridgev2.MatrixAPI
	waCall       *meowcaller.Call
	peer         string
	localPartyID string

	setupMu                  sync.Mutex
	ending                   atomic.Bool
	mu                       sync.Mutex
	peerConnection           *webrtc.PeerConnection
	localTrack               *webrtc.TrackLocalStaticSample
	waToMatrix               *callaudio.OpusTrackSink
	matrixToWA               *callaudio.OpusTrackSource
	matrixAudioQueue         *callaudio.FrameQueue
	pendingCandidates        []webrtc.ICECandidateInit
	pendingCandidatesByParty map[string][]webrtc.ICECandidateInit
	pendingLocalCandidates   []webrtc.ICECandidateInit
	remoteDescriptionSet     bool
	matrixDescriptionSent    bool
	cleanupOnce              sync.Once
}

func (m *voiceCallManager) bindWhatsAppID(call *liveVoiceCall) {
	m.mu.Lock()
	if !m.closed && call.session.WhatsAppID != "" {
		m.byWhatsApp[call.session.WhatsAppID] = call
	}
	m.mu.Unlock()
}

func newVoiceCallManager(connector *WhatsAppConnector) *voiceCallManager {
	return &voiceCallManager{
		connector:    connector,
		sessions:     callbridge.NewManager(connector.Config.VoiceCalls.MaxConcurrentPerLogin),
		byMatrixCall: make(map[string]*liveVoiceCall),
		byWhatsApp:   make(map[string]*liveVoiceCall),
	}
}

func (m *voiceCallManager) add(call *liveVoiceCall) error {
	if err := m.sessions.Add(call.session); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		m.sessions.Remove(call.session.ID)
		return errors.New("voice call manager is closed")
	}
	if _, exists := m.byMatrixCall[call.session.MatrixCallID]; exists {
		m.sessions.Remove(call.session.ID)
		return callbridge.ErrDuplicateCall
	}
	if call.session.WhatsAppID != "" {
		if _, exists := m.byWhatsApp[call.session.WhatsAppID]; exists {
			m.sessions.Remove(call.session.ID)
			return callbridge.ErrDuplicateCall
		}
	}
	m.byMatrixCall[call.session.MatrixCallID] = call
	if call.session.WhatsAppID != "" {
		m.byWhatsApp[call.session.WhatsAppID] = call
	}
	return nil
}

func (m *voiceCallManager) getMatrix(callID string) *liveVoiceCall {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byMatrixCall[callID]
}

func (m *voiceCallManager) remove(call *liveVoiceCall) {
	m.sessions.Remove(call.session.ID)
	m.mu.Lock()
	delete(m.byMatrixCall, call.session.MatrixCallID)
	if call.session.WhatsAppID != "" {
		delete(m.byWhatsApp, call.session.WhatsAppID)
	}
	m.mu.Unlock()
}

func (m *voiceCallManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	calls := make([]*liveVoiceCall, 0, len(m.byMatrixCall))
	for _, call := range m.byMatrixCall {
		calls = append(calls, call)
	}
	m.mu.Unlock()
	for _, call := range calls {
		call.terminate(context.Background(), "bridge_shutdown", false, true)
	}
}

func (m *voiceCallManager) ActiveCount() int {
	return m.sessions.ActiveCount()
}

func newCallIdentifier() (string, error) {
	data := make([]byte, 18)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (call *liveVoiceCall) terminate(ctx context.Context, reason string, notifyMatrix, notifyWhatsApp bool) {
	call.ending.Store(true)
	var whatsAppCall *meowcaller.Call
	call.cleanupOnce.Do(func() {
		call.setupMu.Lock()
		defer call.setupMu.Unlock()
		phase := call.session.Phase()
		if phase != callbridge.PhaseEnding && phase != callbridge.PhaseEnded {
			_ = call.session.Transition(callbridge.PhaseEnding)
		}
		if notifyMatrix && call.intent != nil {
			_ = call.sendHangup(ctx, reason)
		}
		if notifyWhatsApp {
			whatsAppCall = call.waCall
		}
		if call.matrixToWA != nil {
			_ = call.matrixToWA.Close()
		} else if call.matrixAudioQueue != nil {
			_ = call.matrixAudioQueue.Close()
		}
		if call.waToMatrix != nil {
			_ = call.waToMatrix.Close()
		}
		if call.peerConnection != nil {
			_ = call.peerConnection.Close()
		}
		if call.session.Phase() != callbridge.PhaseEnded {
			_ = call.session.Transition(callbridge.PhaseEnded)
		}
		call.manager.remove(call)
	})
	if whatsAppCall != nil {
		if err := whatsAppCall.Hangup(); err != nil {
			call.client.UserLogin.Log.Debug().Err(err).Str("call_id", call.session.MatrixCallID).Msg("Failed to signal WhatsApp hangup")
		}
	}
}

func (call *liveVoiceCall) roomID() id.RoomID {
	if call.portal == nil {
		return ""
	}
	return call.portal.MXID
}
