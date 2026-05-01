package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ticketremote/internal/auth"
	"ticketremote/internal/config"
	"ticketremote/internal/phone"
	"ticketremote/internal/state"
)

func TestDirectStreamTracksConfigFramesAndTelemetry(t *testing.T) {
	hub := newDirectStreamHub()
	hub.addVideoClient()
	hub.setConfig([]byte(`{"type":"config","codec":"avc1.42E01E","transport":"h264-annexb","width":540,"height":1212,"rootCapture":true}`))
	hub.recordFrame(append([]byte{1, 0, 0, 0, 0, 0, 0, 0, 1}, []byte{0x65, 0x88}...))
	hub.recordFrame(append([]byte{0, 0, 0, 0, 0, 0, 0, 0, 2}, []byte{0x41, 0x9a}...))
	hub.recordClientTelemetry("decoder_error", "bad keyframe")

	snapshot := hub.snapshot(time.Now(), phone.Health{Connected: true, Desired: true, Viewers: 1, StreamState: "streaming"})

	if snapshot["path"] != "cloudflare_tunnel_websocket" {
		t.Fatalf("unexpected path %v", snapshot["path"])
	}
	if snapshot["codec"] != "avc1.42E01E" || snapshot["transport"] != "h264-annexb" {
		t.Fatalf("unexpected stream config %#v", snapshot)
	}
	if snapshot["activeVideoClients"] != 1 || snapshot["framesForwarded"] != uint64(2) || snapshot["keyframesForwarded"] != uint64(1) {
		t.Fatalf("unexpected counters %#v", snapshot)
	}
	if snapshot["browserDecodeError"] != "bad keyframe" {
		t.Fatalf("decode error = %q", snapshot["browserDecodeError"])
	}
	if snapshot["phoneConnected"] != true || snapshot["phoneStreamState"] != "streaming" {
		t.Fatalf("phone state missing %#v", snapshot)
	}
}

func TestWebRTCRoutesAreGoneAndHealthReportsDirectStream(t *testing.T) {
	server := newDirectTestServer(t)

	iceReq := httptest.NewRequest(http.MethodPost, "/api/v1/webrtc/ice", strings.NewReader("{}"))
	iceReq.Header.Set("X-Ticket-Remote-Email", "ticket@jolkins.id.lv")
	iceRec := httptest.NewRecorder()
	server.ServeHTTP(iceRec, iceReq)
	if iceRec.Code != http.StatusNotFound {
		t.Fatalf("webrtc ice status = %d body = %s", iceRec.Code, iceRec.Body.String())
	}

	healthReq := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	healthReq.Header.Set("X-Ticket-Remote-Email", "ticket@jolkins.id.lv")
	healthRec := httptest.NewRecorder()
	server.ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("health status = %d body = %s", healthRec.Code, healthRec.Body.String())
	}
	var health map[string]any
	if err := json.NewDecoder(healthRec.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if _, ok := health["webrtc"]; ok {
		t.Fatalf("health should not expose webrtc: %#v", health)
	}
	if _, ok := health["directStream"]; !ok {
		t.Fatalf("health missing directStream: %#v", health)
	}
}

func newDirectTestServer(t *testing.T) http.Handler {
	t.Helper()
	store := state.NewMemoryStore()
	if err := store.Bootstrap(context.Background(), state.BootstrapInput{
		TicketID:        "vivi-default",
		DisplayName:     "ViVi timed ticket",
		AdminEmail:      "ticket@jolkins.id.lv",
		PhoneBackendID:  "pixel",
		PhoneBaseURL:    "http://phone.test",
		PhoneAttachName: "Pixel",
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(config.Config{
		PublicBaseURL: "http://ticket.test",
		TicketID:      "vivi-default",
		CookieName:    "ticket_remote_session",
		CookieTTL:     time.Hour,
		Access: auth.AccessConfig{
			Mode:     "dev",
			DevEmail: "ticket@jolkins.id.lv",
		},
		Phone: config.PhoneConfig{BaseURL: "http://phone.test"},
	}, store, phone.NewRelay(phone.RelayConfig{BaseURL: "http://phone.test"}))
	if err != nil {
		t.Fatal(err)
	}
	return server
}
