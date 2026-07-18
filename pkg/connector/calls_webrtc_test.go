package connector

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestVoiceCallTURNCredentials(t *testing.T) {
	connector := &WhatsAppConnector{}
	connector.Config.VoiceCalls.TURNURIs = []string{"turn:turn.example.com:3478?transport=udp"}
	connector.Config.VoiceCalls.TURNSharedSecret = "test-secret"
	connector.Config.VoiceCalls.TURNTTL = time.Hour
	manager := newVoiceCallManager(connector)

	servers := manager.iceServers()
	if len(servers) != 1 || len(servers[0].URLs) != 1 {
		t.Fatalf("unexpected ICE servers: %#v", servers)
	}
	expiryText, _, ok := strings.Cut(servers[0].Username, ":")
	if !ok {
		t.Fatalf("invalid TURN REST username: %q", servers[0].Username)
	}
	expiry, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil || expiry <= time.Now().Unix() {
		t.Fatalf("invalid TURN expiry: %q", expiryText)
	}
	mac := hmac.New(sha1.New, []byte("test-secret"))
	_, _ = mac.Write([]byte(servers[0].Username))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if servers[0].Credential != want {
		t.Fatalf("unexpected TURN credential: got %q, want %q", servers[0].Credential, want)
	}
}

func TestVoiceCallTURNDisabledWithoutSecret(t *testing.T) {
	connector := &WhatsAppConnector{}
	connector.Config.VoiceCalls.TURNURIs = []string{"turn:turn.example.com"}
	manager := newVoiceCallManager(connector)
	if servers := manager.iceServers(); len(servers) != 0 {
		t.Fatalf("expected no ICE server without a shared secret, got %#v", servers)
	}
}
