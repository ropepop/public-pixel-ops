package web

import (
	"bytes"
	"context"
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

func TestBrowserHeartbeatKeepsPhoneBackendActive(t *testing.T) {
	phoneStart := make(chan struct{}, 1)
	phoneActivity := make(chan string, 1)

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
				ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
				_, data, err := conn.Read(ctx)
				cancel()
				if err != nil {
					return
				}
				if bytes.Contains(data, []byte(`"type":"start"`)) {
					select {
					case phoneStart <- struct{}{}:
					default:
					}
				}
				if bytes.Contains(data, []byte(`"type":"activity"`)) {
					phoneActivity <- string(data)
					return
				}
			}
		case "/api/v1/stream":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept phone video websocket: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "test complete")
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			_, _, _ = conn.Read(ctx)
			<-ctx.Done()
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
	server, err := NewServer(config.Config{
		PublicBaseURL: "http://ticket.test",
		TicketID:      "vivi-default",
		CookieName:    "ticket_remote_session",
		CookieTTL:     time.Hour,
		Access: auth.AccessConfig{
			Mode:     "dev",
			DevEmail: "ticket@jolkins.id.lv",
		},
		Phone: config.PhoneConfig{
			BackendID:  "pixel",
			AttachName: "Pixel",
			BaseURL:    phoneServer.URL,
			Backends:   []config.PhoneBackend{{ID: "pixel", AttachName: "Pixel", BaseURL: phoneServer.URL}},
		},
	}, store, relay)
	if err != nil {
		t.Fatal(err)
	}
	ticketServer := httptest.NewServer(server)
	defer ticketServer.Close()
	defer relay.Close()

	header := http.Header{"X-Ticket-Remote-Email": []string{"ticket@jolkins.id.lv"}}
	wsBase := "ws" + strings.TrimPrefix(ticketServer.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	videoConn, _, err := websocket.Dial(ctx, wsBase+"/api/v1/stream", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial browser video websocket: %v", err)
	}
	defer videoConn.Close(websocket.StatusNormalClosure, "test complete")

	select {
	case <-phoneStart:
	case <-time.After(3 * time.Second):
		t.Fatal("phone backend did not receive relay start")
	}

	controlConn, _, err := websocket.Dial(ctx, wsBase+"/api/v1/session", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial browser control websocket: %v", err)
	}
	defer controlConn.Close(websocket.StatusNormalClosure, "test complete")
	if err := controlConn.Write(ctx, websocket.MessageText, []byte(`{"type":"heartbeat"}`)); err != nil {
		t.Fatalf("send browser heartbeat: %v", err)
	}

	select {
	case got := <-phoneActivity:
		if !strings.Contains(got, "public_heartbeat") {
			t.Fatalf("phone activity message = %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("browser heartbeat was not forwarded to phone activity")
	}
}

func TestBrowserControlSocketStartsPhoneBackendBeforeVideoJoin(t *testing.T) {
	phoneStart := make(chan struct{}, 1)

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
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if bytes.Contains(data, []byte(`"type":"start"`)) {
				phoneStart <- struct{}{}
			}
			<-ctx.Done()
		case "/api/v1/stream":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept phone video websocket: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "test complete")
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			_, _, _ = conn.Read(ctx)
			<-ctx.Done()
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
	server, err := NewServer(config.Config{
		PublicBaseURL: "http://ticket.test",
		TicketID:      "vivi-default",
		CookieName:    "ticket_remote_session",
		CookieTTL:     time.Hour,
		Access: auth.AccessConfig{
			Mode:     "dev",
			DevEmail: "ticket@jolkins.id.lv",
		},
		Phone: config.PhoneConfig{
			BackendID:  "pixel",
			AttachName: "Pixel",
			BaseURL:    phoneServer.URL,
			Backends:   []config.PhoneBackend{{ID: "pixel", AttachName: "Pixel", BaseURL: phoneServer.URL}},
		},
	}, store, relay)
	if err != nil {
		t.Fatal(err)
	}
	ticketServer := httptest.NewServer(server)
	defer ticketServer.Close()
	defer relay.Close()

	header := http.Header{"X-Ticket-Remote-Email": []string{"ticket@jolkins.id.lv"}}
	wsBase := "ws" + strings.TrimPrefix(ticketServer.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	controlConn, _, err := websocket.Dial(ctx, wsBase+"/api/v1/session", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial browser control websocket: %v", err)
	}
	defer controlConn.Close(websocket.StatusNormalClosure, "test complete")

	select {
	case <-phoneStart:
	case <-time.After(3 * time.Second):
		t.Fatal("phone backend did not receive relay start from browser control socket")
	}
}
