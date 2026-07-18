package connector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/purpshell/meowcaller"
	"go.mau.fi/whatsmeow/types"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-whatsapp/pkg/callbridge"
	"go.mau.fi/mautrix-whatsapp/pkg/waid"
)

func (wa *WhatsAppClient) handleIncomingVoiceCall(waCall *meowcaller.Call) {
	ctx := context.Background()
	config := wa.Main.Config.VoiceCalls
	isVideo := waCall.IsVideo()
	if wa.Main.VoiceCalls == nil || !config.Incoming || (isVideo && !config.Video) {
		wa.rejectIncomingCall(waCall, "incoming calls disabled or video calls unsupported")
		return
	}
	callPeer := waCall.Peer()
	portalPeer := wa.maybeConvertJIDToLID(ctx, callPeer)
	if portalPeer != callPeer {
		wa.UserLogin.Log.Debug().
			Stringer("lid", callPeer).
			Stringer("pn", portalPeer).
			Str("call_id", waCall.ID()).
			Msg("Using phone-number portal for incoming LID call")
	}

	matrixCallID, err := newCallIdentifier()
	if err != nil {
		wa.rejectIncomingCall(waCall, "failed to generate Matrix call ID")
		return
	}
	localPartyID, err := newCallIdentifier()
	if err != nil {
		wa.rejectIncomingCall(waCall, "failed to generate Matrix party ID")
		return
	}
	session := callbridge.NewSession(ctx, matrixCallID, string(wa.UserLogin.ID), callbridge.DirectionWhatsAppToMatrix)
	session.MatrixCallID = matrixCallID
	session.WhatsAppID = waCall.ID()
	call := &liveVoiceCall{
		manager:      wa.Main.VoiceCalls,
		session:      session,
		client:       wa,
		waCall:       waCall,
		peer:         portalPeer.String(),
		localPartyID: localPartyID,
		isVideo:      isVideo,
	}
	waCall.OnEnd(func(reason string) { call.handleWhatsAppEnd(reason) })
	waCall.OnReady(func() { call.markMediaReady() })
	if err = call.manager.add(call); err != nil {
		wa.rejectIncomingCall(waCall, "another call is already active")
		return
	}
	_ = session.Transition(callbridge.PhaseRinging)

	portal, err := wa.Main.Bridge.GetPortalByKey(ctx, wa.makeWAPortalKey(portalPeer))
	if err != nil {
		call.failIncomingSetup(ctx, fmt.Errorf("resolve portal: %w", err))
		return
	}
	if portal.MXID == "" {
		if err = portal.CreateMatrixRoom(ctx, wa.UserLogin, nil); err != nil {
			call.failIncomingSetup(ctx, fmt.Errorf("create Matrix room: %w", err))
			return
		}
	}
	intent, ok := portal.GetIntentFor(ctx, wa.makeEventSender(ctx, portalPeer), wa.UserLogin, bridgev2.RemoteEventMessage)
	if !ok || intent == nil {
		call.failIncomingSetup(ctx, errors.New("resolve Matrix ghost intent"))
		return
	}
	call.portal = portal
	call.intent = intent
	call.setupMu.Lock()
	if call.ending.Load() {
		call.setupMu.Unlock()
		return
	}
	if err = call.manager.newPeerConnection(call); err != nil {
		call.setupMu.Unlock()
		call.failIncomingSetup(ctx, err)
		return
	}
	call.attachWhatsAppMedia()
	offer, err := call.peerConnection.CreateOffer(nil)
	if err != nil {
		call.setupMu.Unlock()
		call.failIncomingSetup(ctx, fmt.Errorf("create Matrix offer: %w", err))
		return
	}
	if err = call.peerConnection.SetLocalDescription(offer); err != nil {
		call.setupMu.Unlock()
		call.failIncomingSetup(ctx, fmt.Errorf("set Matrix local offer: %w", err))
		return
	}
	lifetime := int(config.RingTimeout / time.Millisecond)
	if err = call.sendInvite(ctx, *call.peerConnection.LocalDescription(), lifetime); err != nil {
		call.setupMu.Unlock()
		call.failIncomingSetup(ctx, fmt.Errorf("send Matrix invite: %w", err))
		return
	}
	call.markDescriptionSent(ctx)
	call.setupMu.Unlock()
	call.startRingTimeout(config.RingTimeout)
}

func (wa *WhatsAppClient) handleMatrixCallInvite(ctx context.Context, portal *bridgev2.Portal, evt *event.Event) {
	config := wa.Main.Config.VoiceCalls
	content := evt.Content.AsCallInvite()
	if wa.Main.VoiceCalls == nil || !config.Outgoing || content.CallID == "" || content.PartyID == "" {
		return
	}
	target, err := waid.ParsePortalID(portal.ID)
	if err != nil || (target.Server != types.DefaultUserServer && target.Server != types.HiddenUserServer) {
		return
	}
	description, err := matrixDescription(content.Offer)
	if err != nil || description.Type != webrtc.SDPTypeOffer {
		return
	}
	isVideo := sdpHasVideo(description.SDP)
	if isVideo && !config.Video {
		return
	}
	localPartyID, err := newCallIdentifier()
	if err != nil {
		return
	}
	sessionID, err := newCallIdentifier()
	if err != nil {
		return
	}
	session := callbridge.NewSession(context.Background(), sessionID, string(wa.UserLogin.ID), callbridge.DirectionMatrixToWhatsApp)
	session.MatrixCallID = content.CallID
	call := &liveVoiceCall{
		manager:      wa.Main.VoiceCalls,
		session:      session,
		client:       wa,
		portal:       portal,
		peer:         target.String(),
		localPartyID: localPartyID,
		isVideo:      isVideo,
	}
	if !call.selectRemoteParty(content.PartyID) {
		return
	}
	intent, ok := portal.GetIntentFor(ctx, wa.makeEventSender(ctx, target), wa.UserLogin, bridgev2.RemoteEventMessage)
	if !ok || intent == nil {
		return
	}
	call.intent = intent
	if err = call.manager.add(call); err != nil {
		_ = call.sendHangup(ctx, string(event.CallHangupUserHangup))
		return
	}
	if isVideo && !sdpHasH264Video(description.SDP) {
		call.failOutgoingSetup(ctx, errors.New("Matrix video offer does not include H.264"))
		return
	}
	_ = session.Transition(callbridge.PhaseRinging)
	if err = call.manager.newPeerConnection(call); err != nil {
		call.failOutgoingSetup(ctx, err)
		return
	}
	if err = call.setRemoteDescription(description); err != nil {
		call.failOutgoingSetup(ctx, fmt.Errorf("set Matrix remote offer: %w", err))
		return
	}
	answer, err := call.peerConnection.CreateAnswer(nil)
	if err != nil {
		call.failOutgoingSetup(ctx, fmt.Errorf("create Matrix answer: %w", err))
		return
	}
	if err = call.peerConnection.SetLocalDescription(answer); err != nil {
		call.failOutgoingSetup(ctx, fmt.Errorf("set Matrix local answer: %w", err))
		return
	}

	waCall, err := wa.CallClient.CallWithOptions(session.Context(), target.String(), meowcaller.CallOptions{Video: isVideo})
	if err != nil {
		call.failOutgoingSetup(ctx, fmt.Errorf("place WhatsApp call: %w", err))
		return
	}
	call.waCall = waCall
	session.WhatsAppID = waCall.ID()
	call.manager.bindWhatsAppID(call)
	call.attachWhatsAppMedia()
	waCall.OnReady(func() { call.markMediaReady() })
	waCall.OnEnd(func(reason string) { call.handleWhatsAppEnd(reason) })
	waCall.OnPeerAccept(func() { call.handleWhatsAppAccept() })
	call.startRingTimeout(config.RingTimeout)
}

func (call *liveVoiceCall) attachWhatsAppMedia() {
	call.waCall.Receive(call.waToMatrix)
	call.waCall.Play(call.matrixAudioQueue)
	if call.isVideo {
		call.waCall.ReceiveVideo(call.waVideoToMatrix)
		call.waCall.OnVideoKeyframeRequest(call.requestMatrixVideoKeyframe)
	}
}

func (call *liveVoiceCall) handleMatrixAnswer(ctx context.Context, content *event.CallAnswerEventContent) {
	if call.session.Direction != callbridge.DirectionWhatsAppToMatrix || call.session.Phase() != callbridge.PhaseRinging || !call.selectRemoteParty(content.PartyID) {
		return
	}
	description, err := matrixDescription(content.Answer)
	if err != nil || description.Type != webrtc.SDPTypeAnswer {
		call.terminate(ctx, string(event.CallHangupUserMediaFailed), true, true)
		return
	}
	if err = call.setRemoteDescription(description); err != nil {
		call.terminate(ctx, string(event.CallHangupUserMediaFailed), true, true)
		return
	}
	if err = call.sendSelectAnswer(ctx, content.PartyID); err != nil {
		call.terminate(ctx, string(event.CallHangupUnknownError), false, true)
		return
	}
	if err = call.waCall.Answer(); err != nil {
		call.terminate(ctx, string(event.CallHangupUnknownError), true, false)
		return
	}
	_ = call.session.Transition(callbridge.PhaseConnecting)
	call.startConnectTimeout(call.client.Main.Config.VoiceCalls.ConnectTimeout)
}

func (call *liveVoiceCall) handleMatrixSelectAnswer(ctx context.Context, content *event.CallSelectAnswerEventContent) {
	if call.session.Direction != callbridge.DirectionMatrixToWhatsApp || content.PartyID != call.session.RemotePartyID() {
		return
	}
	if content.SelectedPartyID != call.localPartyID {
		call.terminate(ctx, string(event.CallHangupUserHangup), false, true)
	}
}

func (call *liveVoiceCall) handleMatrixReject(ctx context.Context, content *event.CallRejectEventContent) {
	if call.session.Direction != callbridge.DirectionWhatsAppToMatrix || call.session.Phase() != callbridge.PhaseRinging || !call.selectRemoteParty(content.PartyID) {
		return
	}
	_ = call.sendSelectAnswer(ctx, content.PartyID)
	if err := call.waCall.Reject(); err != nil {
		call.client.UserLogin.Log.Debug().Err(err).Str("call_id", call.session.MatrixCallID).Msg("Failed to reject WhatsApp call")
	}
	call.terminate(ctx, string(event.CallHangupUserHangup), false, false)
}

func (call *liveVoiceCall) handleWhatsAppAccept() {
	if call.session.Phase() != callbridge.PhaseRinging || call.peerConnection == nil || call.peerConnection.LocalDescription() == nil {
		return
	}
	ctx := call.session.Context()
	if err := call.sendAnswer(ctx, *call.peerConnection.LocalDescription()); err != nil {
		call.terminate(context.Background(), string(event.CallHangupUnknownError), false, true)
		return
	}
	call.markDescriptionSent(ctx)
	_ = call.session.Transition(callbridge.PhaseConnecting)
	call.startConnectTimeout(call.client.Main.Config.VoiceCalls.ConnectTimeout)
}

func (call *liveVoiceCall) handleWhatsAppEnd(reason string) {
	if call.session.Phase() == callbridge.PhaseEnded {
		return
	}
	if call.session.Direction == callbridge.DirectionMatrixToWhatsApp && call.session.Phase() == callbridge.PhaseRinging {
		_ = call.sendReject(context.Background())
		call.terminate(context.Background(), reason, false, false)
		return
	}
	call.terminate(context.Background(), string(event.CallHangupUserHangup), true, false)
}

func (call *liveVoiceCall) markMediaReady() {
	if call.session.Phase() == callbridge.PhaseConnecting {
		_ = call.session.Transition(callbridge.PhaseActive)
	}
}

func (call *liveVoiceCall) startRingTimeout(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	go func() {
		defer timer.Stop()
		select {
		case <-call.session.Done():
			return
		case <-timer.C:
			if call.session.Phase() != callbridge.PhaseRinging {
				return
			}
			if call.session.Direction == callbridge.DirectionWhatsAppToMatrix {
				_ = call.waCall.Reject()
				call.terminate(context.Background(), string(event.CallHangupInviteTimeout), true, false)
			} else {
				call.terminate(context.Background(), string(event.CallHangupInviteTimeout), true, true)
			}
		}
	}()
}

func (call *liveVoiceCall) startConnectTimeout(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	go func() {
		defer timer.Stop()
		select {
		case <-call.session.Done():
			return
		case <-timer.C:
			if call.session.Phase() == callbridge.PhaseConnecting {
				call.terminate(context.Background(), string(event.CallHangupICEFailed), true, true)
			}
		}
	}()
}

func (call *liveVoiceCall) failIncomingSetup(ctx context.Context, err error) {
	call.client.UserLogin.Log.Error().Err(err).Str("call_id", call.session.MatrixCallID).Msg("Failed to bridge incoming WhatsApp call")
	_ = call.waCall.Reject()
	call.terminate(ctx, string(event.CallHangupUnknownError), call.intent != nil, false)
}

func (call *liveVoiceCall) failOutgoingSetup(ctx context.Context, err error) {
	call.client.UserLogin.Log.Error().Err(err).Str("call_id", call.session.MatrixCallID).Msg("Failed to bridge outgoing Matrix call")
	call.terminate(ctx, string(event.CallHangupUnknownError), true, call.waCall != nil)
}

func (wa *WhatsAppClient) rejectIncomingCall(call *meowcaller.Call, reason string) {
	wa.UserLogin.Log.Warn().Str("component", "voice_calls").Str("call_id", call.ID()).Msg(reason)
	if err := call.Reject(); err != nil {
		wa.UserLogin.Log.Warn().Err(err).Str("component", "voice_calls").Str("call_id", call.ID()).Msg("Failed to reject incoming call")
	}
}
