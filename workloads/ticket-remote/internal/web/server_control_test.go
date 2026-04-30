package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
