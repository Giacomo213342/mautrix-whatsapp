package callbridge

import (
	"context"
	"errors"
	"testing"
)

func TestSessionLifecycle(t *testing.T) {
	session := NewSession(context.Background(), "call", "login", DirectionWhatsAppToMatrix)
	for _, phase := range []Phase{PhaseRinging, PhaseConnecting, PhaseActive, PhaseEnding, PhaseEnded} {
		if err := session.Transition(phase); err != nil {
			t.Fatalf("transition to %d failed: %v", phase, err)
		}
	}
	select {
	case <-session.Done():
	default:
		t.Fatal("ended session did not close Done")
	}
}

func TestSessionRejectsStateRegression(t *testing.T) {
	session := NewSession(context.Background(), "call", "login", DirectionMatrixToWhatsApp)
	if err := session.Transition(PhaseRinging); err != nil {
		t.Fatal(err)
	}
	if err := session.Transition(PhaseNew); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected invalid transition, got %v", err)
	}
}

func TestManagerEnforcesLoginLimit(t *testing.T) {
	manager := NewManager(1)
	first := NewSession(context.Background(), "one", "login", DirectionWhatsAppToMatrix)
	second := NewSession(context.Background(), "two", "login", DirectionMatrixToWhatsApp)
	if err := manager.Add(first); err != nil {
		t.Fatal(err)
	}
	if err := manager.Add(second); !errors.Is(err, ErrLoginBusy) {
		t.Fatalf("expected busy, got %v", err)
	}
	manager.Remove(first.ID)
	if err := manager.Add(second); err != nil {
		t.Fatalf("failed to add after removal: %v", err)
	}
}

func TestSessionSelectsOnlyOneRemoteParty(t *testing.T) {
	session := NewSession(context.Background(), "call", "login", DirectionWhatsAppToMatrix)
	if !session.SelectRemoteParty("device-a") {
		t.Fatal("first party should be selected")
	}
	if !session.SelectRemoteParty("device-a") {
		t.Fatal("selected party should remain accepted")
	}
	if session.SelectRemoteParty("device-b") {
		t.Fatal("second party must not replace selected party")
	}
}
