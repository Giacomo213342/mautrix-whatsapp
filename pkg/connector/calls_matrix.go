package connector

import (
	"context"
	"fmt"

	"github.com/pion/webrtc/v4"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/event"
)

func (wa *WhatsAppConnector) registerMatrixVoiceCallHandlers() {
	matrixConnector, ok := wa.Bridge.Matrix.(*matrix.Connector)
	if !ok {
		wa.Bridge.Log.Error().Msg("Voice calls require the standard bridgev2 Matrix connector")
		return
	}
	for _, eventType := range []event.Type{
		event.CallInvite,
		event.CallCandidates,
		event.CallAnswer,
		event.CallReject,
		event.CallSelectAnswer,
		event.CallHangup,
	} {
		matrixConnector.EventProcessor.On(eventType, wa.handleMatrixVoiceCallEvent)
	}
}

func (wa *WhatsAppConnector) handleMatrixVoiceCallEvent(ctx context.Context, evt *event.Event) {
	if wa.VoiceCalls == nil || wa.Bridge.IsGhostMXID(evt.Sender) {
		return
	}
	portal, err := wa.Bridge.GetPortalByMXID(ctx, evt.RoomID)
	if err != nil || portal == nil || portal.MXID == "" {
		return
	}
	logins, err := wa.Bridge.GetUserLoginsInPortal(ctx, portal.PortalKey)
	if err != nil {
		wa.Bridge.Log.Warn().Err(err).Stringer("room_id", evt.RoomID).Msg("Failed to resolve login for Matrix call event")
		return
	}
	var client *WhatsAppClient
	for _, login := range logins {
		if login.UserMXID != evt.Sender {
			continue
		}
		client, _ = login.Client.(*WhatsAppClient)
		if client != nil {
			break
		}
	}
	if client == nil || client.CallClient == nil {
		return
	}

	switch evt.Type {
	case event.CallInvite:
		client.handleMatrixCallInvite(ctx, portal, evt)
	case event.CallCandidates:
		content := evt.Content.AsCallCandidates()
		call := wa.VoiceCalls.getMatrix(content.CallID)
		if !validMatrixCallEvent(call, client, evt) {
			return
		}
		for _, candidate := range content.Candidates {
			var lineIndex *uint16
			if candidate.SDPMLineIndex >= 0 {
				index := uint16(candidate.SDPMLineIndex)
				lineIndex = &index
			}
			var mid *string
			if candidate.SDPMID != "" {
				value := candidate.SDPMID
				mid = &value
			}
			if err = call.addRemoteCandidate(content.PartyID, webrtc.ICECandidateInit{
				Candidate:     candidate.Candidate,
				SDPMid:        mid,
				SDPMLineIndex: lineIndex,
			}); err != nil {
				client.UserLogin.Log.Warn().Err(err).Str("call_id", content.CallID).Msg("Failed to add Matrix ICE candidate")
				call.terminate(ctx, string(event.CallHangupICEFailed), true, true)
				return
			}
		}
	case event.CallAnswer:
		content := evt.Content.AsCallAnswer()
		call := wa.VoiceCalls.getMatrix(content.CallID)
		if validMatrixCallEvent(call, client, evt) {
			call.handleMatrixAnswer(ctx, content)
		}
	case event.CallSelectAnswer:
		content := evt.Content.AsCallSelectAnswer()
		call := wa.VoiceCalls.getMatrix(content.CallID)
		if validMatrixCallEvent(call, client, evt) {
			call.handleMatrixSelectAnswer(ctx, content)
		}
	case event.CallReject:
		content := evt.Content.AsCallReject()
		call := wa.VoiceCalls.getMatrix(content.CallID)
		if validMatrixCallEvent(call, client, evt) {
			call.handleMatrixReject(ctx, content)
		}
	case event.CallHangup:
		content := evt.Content.AsCallHangup()
		call := wa.VoiceCalls.getMatrix(content.CallID)
		if validMatrixCallEvent(call, client, evt) && content.PartyID == call.session.RemotePartyID() {
			call.terminate(ctx, string(content.Reason), false, true)
		}
	}
}

func validMatrixCallEvent(call *liveVoiceCall, client *WhatsAppClient, evt *event.Event) bool {
	return call != nil && call.client == client && call.roomID() == evt.RoomID
}

func (call *liveVoiceCall) sendCallEvent(ctx context.Context, eventType event.Type, content map[string]any) error {
	content["call_id"] = call.session.MatrixCallID
	content["party_id"] = call.localPartyID
	content["version"] = "1"
	_, err := call.intent.SendMessage(ctx, call.roomID(), eventType, &event.Content{Raw: content}, nil)
	return err
}

func (call *liveVoiceCall) sendInvite(ctx context.Context, description webrtc.SessionDescription, lifetimeMS int) error {
	return call.sendCallEvent(ctx, event.CallInvite, map[string]any{
		"lifetime": lifetimeMS,
		"offer": map[string]any{
			"type": description.Type.String(),
			"sdp":  description.SDP,
		},
	})
}

func (call *liveVoiceCall) sendAnswer(ctx context.Context, description webrtc.SessionDescription) error {
	return call.sendCallEvent(ctx, event.CallAnswer, map[string]any{
		"answer": map[string]any{
			"type": description.Type.String(),
			"sdp":  description.SDP,
		},
	})
}

func (call *liveVoiceCall) sendCandidate(ctx context.Context, candidate webrtc.ICECandidateInit) error {
	content := map[string]any{"candidate": candidate.Candidate}
	if candidate.SDPMid != nil {
		content["sdpMid"] = *candidate.SDPMid
	}
	if candidate.SDPMLineIndex != nil {
		content["sdpMLineIndex"] = *candidate.SDPMLineIndex
	}
	return call.sendCallEvent(ctx, event.CallCandidates, map[string]any{
		"candidates": []any{content},
	})
}

func (call *liveVoiceCall) sendSelectAnswer(ctx context.Context, selectedPartyID string) error {
	return call.sendCallEvent(ctx, event.CallSelectAnswer, map[string]any{
		"selected_party_id": selectedPartyID,
	})
}

func (call *liveVoiceCall) sendReject(ctx context.Context) error {
	return call.sendCallEvent(ctx, event.CallReject, map[string]any{})
}

func (call *liveVoiceCall) sendHangup(ctx context.Context, reason string) error {
	if reason == "" {
		reason = string(event.CallHangupUserHangup)
	}
	return call.sendCallEvent(ctx, event.CallHangup, map[string]any{"reason": reason})
}

func matrixDescription(content event.CallData) (webrtc.SessionDescription, error) {
	var descriptionType webrtc.SDPType
	switch content.Type {
	case event.CallDataTypeOffer:
		descriptionType = webrtc.SDPTypeOffer
	case event.CallDataTypeAnswer:
		descriptionType = webrtc.SDPTypeAnswer
	default:
		return webrtc.SessionDescription{}, fmt.Errorf("unsupported SDP type %q", content.Type)
	}
	return webrtc.SessionDescription{Type: descriptionType, SDP: content.SDP}, nil
}
