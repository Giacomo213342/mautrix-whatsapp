package connector

import (
	"context"
	"errors"
	"testing"

	"go.mau.fi/mautrix-whatsapp/pkg/callbridge"
)

func TestVoiceCallManagerRejectsDuplicateMatrixCallID(t *testing.T) {
	manager := newVoiceCallManager(&WhatsAppConnector{})
	firstSession := callbridge.NewSession(context.Background(), "session-1", "login-1", callbridge.DirectionMatrixToWhatsApp)
	firstSession.MatrixCallID = "matrix-call"
	first := &liveVoiceCall{manager: manager, session: firstSession}
	if err := manager.add(first); err != nil {
		t.Fatalf("failed to add first call: %v", err)
	}

	secondSession := callbridge.NewSession(context.Background(), "session-2", "login-2", callbridge.DirectionMatrixToWhatsApp)
	secondSession.MatrixCallID = "matrix-call"
	second := &liveVoiceCall{manager: manager, session: secondSession}
	if err := manager.add(second); !errors.Is(err, callbridge.ErrDuplicateCall) {
		t.Fatalf("expected duplicate call error, got %v", err)
	}
	if manager.ActiveCount() != 1 {
		t.Fatalf("duplicate call leaked a session: active=%d", manager.ActiveCount())
	}
	manager.remove(first)
}
