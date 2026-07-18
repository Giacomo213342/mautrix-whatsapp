package connector

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
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
	if call.isVideo {
		if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
				RTCPFeedback: []webrtc.RTCPFeedback{
					{Type: "goog-remb"},
					{Type: "ccm", Parameter: "fir"},
					{Type: "nack"},
					{Type: "nack", Parameter: "pli"},
				},
			},
			PayloadType: 102,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return fmt.Errorf("register H.264 codec: %w", err)
		}
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
	call.localAudioTrack = track
	sender, err := peerConnection.AddTrack(track)
	if err != nil {
		_ = peerConnection.Close()
		return fmt.Errorf("attach Matrix audio track: %w", err)
	}
	go drainRTCP(sender)

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
	if call.isVideo {
		videoTrack, videoErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		}, "video", "mautrix-whatsapp")
		if videoErr != nil {
			_ = call.matrixToWA.Close()
			_ = call.waToMatrix.Close()
			_ = peerConnection.Close()
			return fmt.Errorf("create Matrix H.264 track: %w", videoErr)
		}
		call.localVideoTrack = videoTrack
		videoSender, videoErr := peerConnection.AddTrack(videoTrack)
		if videoErr != nil {
			_ = call.matrixToWA.Close()
			_ = call.waToMatrix.Close()
			_ = peerConnection.Close()
			return fmt.Errorf("attach Matrix H.264 track: %w", videoErr)
		}
		go drainRTCP(videoSender)
		call.waVideoToMatrix = &matrixH264Sink{track: videoTrack}
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
		switch track.Kind() {
		case webrtc.RTPCodecTypeAudio:
			go call.consumeMatrixAudio(track)
		case webrtc.RTPCodecTypeVideo:
			if !call.isVideo || !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeH264) {
				return
			}
			call.mu.Lock()
			call.remoteVideoTrack = track
			call.mu.Unlock()
			call.requestMatrixVideoKeyframe()
			go call.consumeMatrixVideo(track)
		}
	})
	return nil
}

func drainRTCP(sender *webrtc.RTPSender) {
	buffer := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buffer); err != nil {
			return
		}
	}
}

type matrixH264Sink struct {
	track *webrtc.TrackLocalStaticSample
}

func (sink *matrixH264Sink) WriteVideo(accessUnit []byte) error {
	return sink.track.WriteSample(media.Sample{Data: accessUnit, Duration: time.Second / 30})
}

func (sink *matrixH264Sink) Close() error {
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

func (call *liveVoiceCall) consumeMatrixVideo(track *webrtc.TrackRemote) {
	buffer := samplebuilder.New(100, &codecs.H264Packet{}, 90000)
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		buffer.Push(packet)
		for sample := buffer.Pop(); sample != nil; sample = buffer.Pop() {
			duration := sample.Duration
			if duration <= 0 {
				duration = time.Second / 30
			}
			waCall := call.waCall
			if waCall == nil {
				continue
			}
			if err = waCall.SendVideoWithDuration(sample.Data, duration); err != nil && call.videoDropLogged.CompareAndSwap(false, true) {
				call.client.UserLogin.Log.Debug().Err(err).
					Str("call_id", call.session.MatrixCallID).
					Msg("Dropping Matrix video until WhatsApp video media becomes ready")
			}
		}
	}
}

func (call *liveVoiceCall) requestMatrixVideoKeyframe() {
	call.mu.Lock()
	track := call.remoteVideoTrack
	peerConnection := call.peerConnection
	call.mu.Unlock()
	if track == nil || peerConnection == nil {
		return
	}
	if err := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); err != nil {
		call.client.UserLogin.Log.Debug().Err(err).
			Str("call_id", call.session.MatrixCallID).
			Msg("Failed to request Matrix video keyframe")
	}
}

func sdpHasVideo(raw string) bool {
	hasVideo, _ := sdpVideoSupport(raw)
	return hasVideo
}

func sdpHasH264Video(raw string) bool {
	_, hasH264 := sdpVideoSupport(raw)
	return hasH264
}

func sdpVideoSupport(raw string) (hasVideo, hasH264 bool) {
	videoSection := false
	videoActive := false
	videoH264 := false
	finishSection := func() {
		if videoSection && videoActive {
			hasVideo = true
			hasH264 = hasH264 || videoH264
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "m=") {
			finishSection()
			fields := strings.Fields(strings.TrimPrefix(line, "m="))
			videoSection = len(fields) >= 2 && fields[0] == "video"
			videoActive = videoSection && fields[1] != "0"
			videoH264 = false
		} else if videoActive && line == "a=inactive" {
			videoActive = false
		} else if videoSection && strings.HasPrefix(strings.ToLower(line), "a=rtpmap:") && strings.Contains(strings.ToLower(line), " h264/90000") {
			videoH264 = true
		}
	}
	finishSection()
	return
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
