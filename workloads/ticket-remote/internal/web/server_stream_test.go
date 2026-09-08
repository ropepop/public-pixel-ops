package web

import (
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

func TestBrowserVideoSocketRequiresConfiguredSameOriginBeforeWake(t *testing.T) {
	phoneServer := httptest.NewServer(http.NotFoundHandler())
	defer phoneServer.Close()
	server, ticketServer, relay := newTicketRecoveryTestServer(t, phoneServer.URL)
	defer server.Close()
	defer ticketServer.Close()

	wsBase := "ws" + strings.TrimPrefix(ticketServer.URL, "http")
	for _, test := range []struct {
		name   string
		origin string
	}{
		{name: "missing"},
		{name: "cross_origin", origin: "https://attacker.example"},
		{name: "wrong_scheme", origin: "https://ticket.test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{"X-Ticket-Remote-Email": []string{"ticket@jolkins.id.lv"}}
			if test.origin != "" {
				header.Set("Origin", test.origin)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, response, err := websocket.Dial(ctx, wsBase+"/api/v1/stream", &websocket.DialOptions{HTTPHeader: header})
			if conn != nil {
				_ = conn.Close(websocket.StatusNormalClosure, "unexpected acceptance")
				t.Fatal("socket with invalid origin was accepted")
			}
			if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
				t.Fatalf("invalid origin response = %#v err=%v, want 403", response, err)
			}
		})
	}
	if health := relay.Snapshot(); health.Viewers != 0 || health.Desired {
		t.Fatalf("rejected origins woke the phone relay: %#v", health)
	}
}

func TestRemovedMemberCachedPageCannotPrewarmPhone(t *testing.T) {
	phoneCommands := make(chan string, 8)
	phoneServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/stream" {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "test complete")
		<-r.Context().Done()
	}))
	defer phoneServer.Close()
	registerTicketStreamCommandSink(t, phoneServer.URL, phoneCommands)

	store := newTicketMemoryStore(t, phoneServer.URL)
	memberEmail := "removed@example.com"
	cached, err := store.UpsertMember(context.Background(), "vivi-default", "ticket@jolkins.id.lv", memberEmail, state.RoleMember)
	if err != nil {
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
			Mode:              "spacetime",
			AuthCookieName:    "ticket_remote_auth",
			SessionSigningKey: "test-signing-key",
		},
		Phone: config.PhoneConfig{BackendID: "pixel", AttachName: "Pixel", BaseURL: phoneServer.URL},
	}, store, relay)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.cacheSnapshot(cached)
	if _, err := store.RemoveMember(context.Background(), "vivi-default", "ticket@jolkins.id.lv", memberEmail); err != nil {
		t.Fatal(err)
	}
	token, _, err := server.auth.IssueServerSession(auth.Identity{Email: memberEmail}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "ticket_remote_auth", Value: token})
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cached page status = %d body = %s", rec.Code, rec.Body.String())
	}
	select {
	case command := <-phoneCommands:
		t.Fatalf("removed member triggered phone prewarm: %s", command)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestVideoSocketWaitsForCurrentMembershipBeforePhoneWake(t *testing.T) {
	phoneServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/stream":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept phone stream websocket: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "test complete")
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer phoneServer.Close()

	store := &blockingSnapshotStore{
		Store:           newTicketMemoryStore(t, phoneServer.URL),
		snapshotStarted: make(chan struct{}),
		releaseSnapshot: make(chan struct{}),
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
	server := newTicketWebServer(t, store, relay, phoneServer.URL)
	freshSnapshot, err := store.Store.Snapshot(context.Background(), "vivi-default", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server.cacheSnapshot(freshSnapshot)
	ticketServer := httptest.NewServer(server)
	defer ticketServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connReady := make(chan *websocket.Conn, 1)
	go func() {
		connReady <- dialStreamTestClient(t, ctx, ticketServer.URL, "cached-fast-video")
	}()

	select {
	case <-store.snapshotStarted:
	case <-time.After(time.Second):
		t.Fatal("video socket did not start current membership lookup")
	}
	select {
	case conn := <-connReady:
		_ = conn.Close(websocket.StatusNormalClosure, "unexpected early connection")
		t.Fatal("video socket was accepted from cached membership")
	case <-time.After(250 * time.Millisecond):
	}
	close(store.releaseSnapshot)

	var conn *websocket.Conn
	select {
	case conn = <-connReady:
		defer conn.Close(websocket.StatusNormalClosure, "test complete")
	case <-time.After(2 * time.Second):
		t.Fatal("video socket did not connect after current membership completed")
	}
}

func newTicketRecoveryTestServer(t *testing.T, phoneBaseURL string) (*Server, *httptest.Server, *phone.Relay) {
	t.Helper()
	store := NewMemoryStore()
	if err := store.Bootstrap(context.Background(), state.BootstrapInput{
		TicketID:        "vivi-default",
		DisplayName:     "ViVi timed ticket",
		AdminEmail:      "ticket@jolkins.id.lv",
		PhoneBackendID:  "pixel",
		PhoneBaseURL:    phoneBaseURL,
		PhoneAttachName: "Pixel",
	}); err != nil {
		t.Fatal(err)
	}
	relay := phone.NewRelay(phone.RelayConfig{
		BackendID:         "pixel",
		AttachName:        "Pixel",
		BaseURL:           phoneBaseURL,
		ReconnectMinDelay: time.Hour,
		ReconnectMaxDelay: time.Hour,
		NoViewerStopDelay: time.Hour,
	})
	storeForServer := state.Store(store)
	if sink := ticketStreamCommandSink(phoneBaseURL); sink != nil {
		storeForServer = &recordingTicketStore{Store: store, commandSink: sink}
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
		Phone: config.PhoneConfig{
			BackendID:  "pixel",
			AttachName: "Pixel",
			BaseURL:    phoneBaseURL,
			Backends:   []config.PhoneBackend{{ID: "pixel", AttachName: "Pixel", BaseURL: phoneBaseURL}},
		},
	}, storeForServer, relay)
	if err != nil {
		t.Fatal(err)
	}
	return server, httptest.NewServer(server), relay
}

type blockingSnapshotStore struct {
	state.Store
	snapshotStarted chan struct{}
	releaseSnapshot chan struct{}
}

func (s *blockingSnapshotStore) Snapshot(ctx context.Context, ticketID string, now time.Time) (state.Snapshot, error) {
	select {
	case <-s.snapshotStarted:
	default:
		close(s.snapshotStarted)
	}
	select {
	case <-s.releaseSnapshot:
	case <-ctx.Done():
		return state.Snapshot{}, ctx.Err()
	}
	return s.Store.Snapshot(ctx, ticketID, now)
}

func dialStreamTestClient(t *testing.T, ctx context.Context, serverURL string, sessionID string) *websocket.Conn {
	return dialStreamTestClientForRun(t, ctx, serverURL, sessionID, "")
}

func dialStreamTestClientForRun(t *testing.T, ctx context.Context, serverURL string, sessionID string, runOrigin string) *websocket.Conn {
	t.Helper()
	wsBase := "ws" + strings.TrimPrefix(serverURL, "http")
	header := http.Header{"X-Ticket-Remote-Email": []string{"ticket@jolkins.id.lv"}, "Origin": []string{"http://ticket.test"}}
	header.Add("Cookie", "ticket_remote_session="+sessionID)
	options := &websocket.DialOptions{HTTPHeader: header}
	if runOrigin != "" {
		options.Subprotocols = []string{"ticket.video.v1", runOrigin}
	}
	conn, _, err := websocket.Dial(ctx, wsBase+"/api/v1/stream", options)
	if err != nil {
		t.Fatalf("dial browser video websocket: %v", err)
	}
	if runOrigin != "" && conn.Subprotocol() != "ticket.video.v1" {
		t.Fatalf("negotiated video subprotocol = %q; private startup origin must not be echoed", conn.Subprotocol())
	}
	return conn
}
