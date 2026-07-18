package connector

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"go.mau.fi/mautrix-whatsapp/pkg/callbridge"
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

func TestSDPHasVideo(t *testing.T) {
	tests := []struct {
		name string
		sdp  string
		want bool
	}{
		{name: "audio only", sdp: "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=sendrecv\r\n"},
		{name: "active video", sdp: "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\nm=video 9 UDP/TLS/RTP/SAVPF 102\r\na=rtpmap:102 H264/90000\r\na=sendrecv\r\n", want: true},
		{name: "rejected video", sdp: "v=0\r\nm=video 0 UDP/TLS/RTP/SAVPF 102\r\n"},
		{name: "inactive video", sdp: "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 102\r\na=inactive\r\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sdpHasVideo(test.sdp); got != test.want {
				t.Fatalf("sdpHasVideo() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSDPHasH264Video(t *testing.T) {
	if !sdpHasH264Video("v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 102\r\na=rtpmap:102 H264/90000\r\n") {
		t.Fatal("expected active H.264 video to be supported")
	}
	if sdpHasH264Video("v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=rtpmap:96 VP8/90000\r\n") {
		t.Fatal("expected VP8-only video to be unsupported")
	}
}

func TestPeerConnectionVideoOffer(t *testing.T) {
	connector := &WhatsAppConnector{}
	connector.Config.VoiceCalls.UDPPortMin = 40000
	connector.Config.VoiceCalls.UDPPortMax = 40100
	connector.Config.VoiceCalls.OpusBitrate = 24000
	manager := newVoiceCallManager(connector)
	call := &liveVoiceCall{
		manager: manager,
		session: callbridge.NewSession(context.Background(), "test", "test", callbridge.DirectionWhatsAppToMatrix),
		isVideo: true,
	}
	if err := manager.newPeerConnection(call); err != nil {
		t.Fatalf("newPeerConnection: %v", err)
	}
	t.Cleanup(func() {
		_ = call.matrixToWA.Close()
		_ = call.waToMatrix.Close()
		call.peerConnection.OnConnectionStateChange(func(webrtc.PeerConnectionState) {})
		_ = call.peerConnection.Close()
	})
	offer, err := call.peerConnection.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if !sdpHasVideo(offer.SDP) || !sdpHasH264Video(offer.SDP) {
		t.Fatalf("offer does not contain active H.264 video:\n%s", offer.SDP)
	}
}
