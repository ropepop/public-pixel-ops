package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"ticketremote/internal/auth"
	"ticketremote/internal/config"
	"ticketremote/internal/phone"
	"ticketremote/internal/state"
)

func TestControlRoutesClaimExtendRelease(t *testing.T) {
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

	claim := postControl(t, server, nil, "/api/v1/control/claim")
	if claim.State.ActiveControl == nil {
		t.Fatal("expected claimed control session")
	}
	cookies := claim.Cookies

	extend := postControl(t, server, cookies, "/api/v1/control/extend")
	if extend.State.ActiveControl == nil || !extend.State.ActiveControl.Extended {
		t.Fatalf("expected extended control session, got %#v", extend.State.ActiveControl)
	}

	release := postControl(t, server, cookies, "/api/v1/control/release")
	if release.State.ActiveControl != nil {
		t.Fatalf("expected released control session, got %#v", release.State.ActiveControl)
	}
}

func TestControlReleaseNotifiesPhoneControlExit(t *testing.T) {
	messages := make(chan string, 10)
	phoneServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/session/start":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/session":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept phone control websocket: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "test complete")
			for {
				_, data, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				messages <- string(data)
			}
		case "/api/v1/stream":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept phone video websocket: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "test complete")
			_, _, _ = conn.Read(r.Context())
			<-r.Context().Done()
		case "/api/v1/session/stop":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer phoneServer.Close()

	store := state.NewMemoryStore()
	if err := store.Bootstrap(context.Background(), state.BootstrapInput{
		TicketID:        "vivi-default",
		DisplayName:     "ViVi timed ticket",
		AdminEmail:      "ticket@jolkins.id.lv",
		PhoneBackendID:  "pixel",
		PhoneBaseURL:    phoneServer.URL,
		PhoneAttachName: "Pixel",
	}); err != nil {
		t.Fatal(err)
	}
	relay := phone.NewRelay(phone.RelayConfig{
		BackendID:         "pixel",
		AttachName:        "Pixel",
		BaseURL:           phoneServer.URL,
		ReconnectMinDelay: time.Hour,
		ReconnectMaxDelay: time.Hour,
		NoViewerStopDelay: time.Hour,
	})
	defer relay.Close()
	server, err := NewServer(config.Config{
		PublicBaseURL: "http://ticket.test",
		TicketID:      "vivi-default",
		CookieName:    "ticket_remote_session",
		CookieTTL:     time.Hour,
		Access: auth.AccessConfig{
			Mode:     "dev",
			DevEmail: "ticket@jolkins.id.lv",
		},
		Phone: config.PhoneConfig{BackendID: "pixel", AttachName: "Pixel", BaseURL: phoneServer.URL},
	}, store, relay)
	if err != nil {
		t.Fatal(err)
	}

	relay.AddViewer()
	waitForPhoneMessage(t, messages, `"type":"start"`)
	claim := postControl(t, server, nil, "/api/v1/control/claim")
	postControl(t, server, claim.Cookies, "/api/v1/control/release")
	waitForPhoneMessage(t, messages, `"type":"control_exit"`)
}

func TestControlGateEndTransitionNotifiesPhoneControlExit(t *testing.T) {
	messages := make(chan string, 10)
	phoneServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/session/start":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/session":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept phone control websocket: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "test complete")
			for {
				_, data, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				messages <- string(data)
			}
		case "/api/v1/stream":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept phone video websocket: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "test complete")
			_, _, _ = conn.Read(r.Context())
			<-r.Context().Done()
		case "/api/v1/session/stop":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer phoneServer.Close()

	store := state.NewMemoryStore()
	server, err := NewServer(config.Config{
		PublicBaseURL: "http://ticket.test",
		TicketID:      "vivi-default",
		CookieName:    "ticket_remote_session",
		CookieTTL:     time.Hour,
		Access: auth.AccessConfig{
			Mode:     "dev",
			DevEmail: "ticket@jolkins.id.lv",
		},
		Phone: config.PhoneConfig{BackendID: "pixel", AttachName: "Pixel", BaseURL: phoneServer.URL},
	}, store, phone.NewRelay(phone.RelayConfig{
		BackendID:         "pixel",
		AttachName:        "Pixel",
		BaseURL:           phoneServer.URL,
		ReconnectMinDelay: time.Hour,
		ReconnectMaxDelay: time.Hour,
		NoViewerStopDelay: time.Hour,
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer server.relay.Close()
	server.relay.AddViewer()
	waitForPhoneMessage(t, messages, `"type":"start"`)

	now := time.Now()
	server.rememberControlGate(state.Snapshot{ActiveControl: &state.ControlSession{
		SessionID: "session",
		Email:     "ticket@jolkins.id.lv",
		ExpiresAt: now.Add(time.Minute).UTC().Format(time.RFC3339),
	}}, now)
	server.rememberControlGate(state.Snapshot{}, now.Add(time.Second))
	waitForPhoneMessage(t, messages, `"type":"control_exit"`)
}

func waitForPhoneMessage(t *testing.T, messages <-chan string, snippet string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case message := <-messages:
			if strings.Contains(message, snippet) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for phone message containing %s", snippet)
		}
	}
}

type controlResponse struct {
	State   state.Snapshot
	Cookies []*http.Cookie
}

func postControl(t *testing.T, handler http.Handler, cookies []*http.Cookie, path string) controlResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{}"))
	req.Header.Set("X-Ticket-Remote-Email", "ticket@jolkins.id.lv")
	req.Header.Set("Content-Type", "application/json")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status = %d body = %s", path, rec.Code, rec.Body.String())
	}
	var body apiResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK {
		t.Fatalf("%s returned not ok: %#v", path, body)
	}
	return controlResponse{State: body.State, Cookies: rec.Result().Cookies()}
}
