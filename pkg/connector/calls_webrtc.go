package connector

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-whatsapp/pkg/callbridge"
	callaudio "go.mau.fi/mautrix-whatsapp/pkg/callbridge/audio"
)

func (m *voiceCallManager) newPeerConnection(call *liveVoiceCall) error {
	var mediaEngine webrtc.MediaEngine
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeOpus,
			ClockRate:    48000,
			Channels:     2,
			SDPFmtpLine:  "minptime=10;useinbandfec=1",
			RTCPFeedback: nil,
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("register Opus codec: %w", err)
	}
	var settings webrtc.SettingEngine
	if err := settings.SetEphemeralUDPPortRange(m.connector.Config.VoiceCalls.UDPPortMin, m.connector.Config.VoiceCalls.UDPPortMax); err != nil {
		return fmt.Errorf("configure WebRTC UDP range: %w", err)
	}
	if publicIP := m.connector.Config.VoiceCalls.PublicIP; publicIP != "" {
		settings.SetNAT1To1IPs([]string{publicIP}, webrtc.ICECandidateTypeHost)
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(&mediaEngine, interceptors); err != nil {
		return fmt.Errorf("register WebRTC interceptors: %w", err)
	}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(&mediaEngine),
		webrtc.WithSettingEngine(settings),
		webrtc.WithInterceptorRegistry(interceptors),
	)
	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: m.iceServers()})
	if err != nil {
		return fmt.Errorf("create Matrix peer connection: %w", err)
	}
	call.peerConnection = peerConnection

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeOpus,
		ClockRate:   48000,
		Channels:    2,
		SDPFmtpLine: "minptime=10;useinbandfec=1",
	}, "audio", "mautrix-whatsapp")
	if err != nil {
		_ = peerConnection.Close()
		return fmt.Errorf("create Matrix audio track: %w", err)
	}
	call.localTrack = track
	sender, err := peerConnection.AddTrack(track)
	if err != nil {
		_ = peerConnection.Close()
		return fmt.Errorf("attach Matrix audio track: %w", err)
	}
	go func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, readErr := sender.Read(buffer); readErr != nil {
				return
			}
		}
	}()

	call.matrixAudioQueue = callaudio.NewFrameQueue(8)
	call.matrixToWA, err = callaudio.NewOpusTrackSource(call.matrixAudioQueue)
	if err != nil {
		_ = peerConnection.Close()
		return fmt.Errorf("create Matrix Opus decoder: %w", err)
	}
	call.waToMatrix, err = callaudio.NewOpusTrackSink(track, m.connector.Config.VoiceCalls.OpusBitrate)
	if err != nil {
		_ = call.matrixToWA.Close()
		_ = peerConnection.Close()
		return fmt.Errorf("create Matrix Opus encoder: %w", err)
	}

	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			call.handleLocalCandidate(webrtc.ICECandidateInit{Candidate: ""})
			return
		}
		call.handleLocalCandidate(candidate.ToJSON())
	})
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		call.client.UserLogin.Log.Debug().
			Str("call_id", call.session.MatrixCallID).
			Str("webrtc_state", state.String()).
			Msg("Matrix WebRTC connection state changed")
		switch state {
		case webrtc.PeerConnectionStateConnected:
			if call.session.Phase() == callbridge.PhaseConnecting {
				_ = call.session.Transition(callbridge.PhaseActive)
			}
		case webrtc.PeerConnectionStateFailed:
			call.terminate(context.Background(), string(event.CallHangupICEFailed), true, true)
		}
	})
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		go call.consumeMatrixAudio(track)
	})
	return nil
}

func (m *voiceCallManager) iceServers() []webrtc.ICEServer {
	config := m.connector.Config.VoiceCalls
	if len(config.TURNURIs) == 0 || config.TURNSharedSecret == "" {
		return nil
	}
	expires := time.Now().Add(config.TURNTTL).Unix()
	username := strconv.FormatInt(expires, 10) + ":mautrix-whatsapp"
	mac := hmac.New(sha1.New, []byte(config.TURNSharedSecret))
	_, _ = mac.Write([]byte(username))
	credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return []webrtc.ICEServer{{
		URLs:       append([]string(nil), config.TURNURIs...),
		Username:   username,
		Credential: credential,
	}}
}

func (call *liveVoiceCall) consumeMatrixAudio(track *webrtc.TrackRemote) {
	buffer := samplebuilder.New(50, &codecs.OpusPacket{}, 48000)
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		buffer.Push(packet)
		for sample := buffer.Pop(); sample != nil; sample = buffer.Pop() {
			if err = call.matrixToWA.WritePacket(sample.Data); err != nil {
				call.client.UserLogin.Log.Warn().Err(err).
					Str("call_id", call.session.MatrixCallID).
					Msg("Failed to decode Matrix Opus packet")
				return
			}
		}
	}
}

func (call *liveVoiceCall) handleLocalCandidate(candidate webrtc.ICECandidateInit) {
	call.mu.Lock()
	if !call.matrixDescriptionSent {
		call.pendingLocalCandidates = append(call.pendingLocalCandidates, candidate)
		call.mu.Unlock()
		return
	}
	call.mu.Unlock()
	if err := call.sendCandidate(context.Background(), candidate); err != nil {
		call.client.UserLogin.Log.Warn().Err(err).
			Str("call_id", call.session.MatrixCallID).
			Msg("Failed to send Matrix ICE candidate")
	}
}

func (call *liveVoiceCall) setRemoteDescription(description webrtc.SessionDescription) error {
	if err := call.peerConnection.SetRemoteDescription(description); err != nil {
		return err
	}
	call.mu.Lock()
	call.remoteDescriptionSet = true
	candidates := call.pendingCandidates
	call.pendingCandidates = nil
	call.mu.Unlock()
	for _, candidate := range candidates {
		if err := call.peerConnection.AddICECandidate(candidate); err != nil {
			return err
		}
	}
	return nil
}

func (call *liveVoiceCall) addRemoteCandidate(partyID string, candidate webrtc.ICECandidateInit) error {
	call.mu.Lock()
	selectedParty := call.session.RemotePartyID()
	if selectedParty == "" {
		if call.pendingCandidatesByParty == nil {
			call.pendingCandidatesByParty = make(map[string][]webrtc.ICECandidateInit)
		}
		call.pendingCandidatesByParty[partyID] = append(call.pendingCandidatesByParty[partyID], candidate)
		call.mu.Unlock()
		return nil
	}
	if partyID != selectedParty {
		call.mu.Unlock()
		return nil
	}
	if !call.remoteDescriptionSet {
		call.pendingCandidates = append(call.pendingCandidates, candidate)
		call.mu.Unlock()
		return nil
	}
	call.mu.Unlock()
	return call.peerConnection.AddICECandidate(candidate)
}

func (call *liveVoiceCall) selectRemoteParty(partyID string) bool {
	if !call.session.SelectRemoteParty(partyID) {
		return false
	}
	call.mu.Lock()
	call.pendingCandidates = append(call.pendingCandidates, call.pendingCandidatesByParty[partyID]...)
	call.pendingCandidatesByParty = nil
	call.mu.Unlock()
	return true
}

func (call *liveVoiceCall) markDescriptionSent(ctx context.Context) {
	call.mu.Lock()
	call.matrixDescriptionSent = true
	candidates := call.pendingLocalCandidates
	call.pendingLocalCandidates = nil
	call.mu.Unlock()
	for _, candidate := range candidates {
		if err := call.sendCandidate(ctx, candidate); err != nil {
			call.client.UserLogin.Log.Warn().Err(err).
				Str("call_id", call.session.MatrixCallID).
				Msg("Failed to flush Matrix ICE candidate")
		}
	}
}
