package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"ticketremote/internal/auth"
	"ticketremote/internal/config"
	"ticketremote/internal/phone"
	"ticketremote/internal/state"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	cfg       config.Config
	store     state.Store
	relay     *phone.Relay
	auth      *auth.Validator
	static    fs.FS
	indexTmpl *template.Template
	adminTmpl *template.Template

	mu      sync.Mutex
	clients map[*client]struct{}

	gateMu sync.RWMutex
	gate   *controlGate

	stateMu     sync.RWMutex
	cachedState state.Snapshot

	phoneStartMu          sync.Mutex
	lastPhoneStartAttempt time.Time
}

type client struct {
	conn      *websocket.Conn
	sessionID string
	email     string
	page      string
	sendMu    sync.Mutex
}

type controlGate struct {
	sessionID string
	email     string
	expiresAt time.Time
}

type apiResponse struct {
	OK      bool           `json:"ok"`
	Error   string         `json:"error,omitempty"`
	Message string         `json:"message,omitempty"`
	State   state.Snapshot `json:"state,omitempty"`
	Phone   phone.Health   `json:"phone,omitempty"`
}

func NewServer(cfg config.Config, store state.Store, relay *phone.Relay) (*Server, error) {
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:       cfg,
		store:     store,
		relay:     relay,
		auth:      auth.NewValidator(cfg.Access),
		static:    staticSub,
		indexTmpl: template.Must(template.New("index").Parse(indexHTML)),
		adminTmpl: template.Must(template.New("admin").Parse(adminHTML)),
		clients:   map[*client]struct{}{},
	}
	relay.SetHandlers(s.handlePhoneMessage, s.handlePhoneDisconnect)
	go s.stateTicker()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	if !s.requestOriginAllowed(r) {
		writeJSON(w, http.StatusForbidden, apiResponse{OK: false, Error: "bad_origin", Message: "Request origin is not allowed."})
		return
	}
	switch {
	case path == "/api/v1/health":
		s.handleHealth(w, r)
	case path == "/static/app.css" || path == "/static/app.js":
		w.Header().Set("Cache-Control", "no-store")
		http.StripPrefix("/static/", http.FileServer(http.FS(s.static))).ServeHTTP(w, r)
	case path == "/api/v1/stream":
		s.handleStream(w, r)
	case path == "/api/v1/me":
		s.withMember(w, r, s.handleMe)
	case path == "/api/v1/state":
		s.withMember(w, r, s.handleState)
	case path == "/api/v1/control/claim":
		s.withMember(w, r, s.handleClaimControl)
	case path == "/api/v1/control/extend":
		s.withMember(w, r, s.handleExtendControl)
	case path == "/api/v1/control/release":
		s.withMember(w, r, s.handleReleaseControl)
	case path == "/api/v1/client-log":
		s.withMember(w, r, s.handleClientLog)
	case path == "/api/v1/admin/state":
		s.withAdmin(w, r, s.handleAdminState)
	case path == "/api/v1/admin/members":
		s.withAdmin(w, r, s.handleAdminMembers)
	case path == "/api/v1/admin/control/revoke":
		s.withAdmin(w, r, s.handleAdminRevokeControl)
	case path == "/admin":
		s.withAdmin(w, r, s.handleAdminPage)
	case path == "/":
		s.withMember(w, r, s.handleIndex)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.store.Snapshot(r.Context(), s.cfg.TicketID, time.Now())
	phoneHealth := s.relay.Snapshot()
	ok := err == nil
	reasons := []string{}
	if err != nil {
		reasons = append(reasons, err.Error())
	}
	if snapshot.StateBackend == "memory" {
		reasons = append(reasons, "state backend is memory; configure SpacetimeDB for production")
	}
	if err == nil {
		s.cacheSnapshot(snapshot)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      ok,
		"reasons": reasons,
		"state":   snapshot,
		"phone":   phoneHealth,
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, snapshot state.Snapshot) {
	w.Header().Set("Cache-Control", "no-store")
	_ = s.indexTmpl.Execute(w, map[string]any{
		"ConfigJSON": template.JS(mustJSON(map[string]any{
			"publicBaseUrl": s.cfg.PublicBaseURL,
			"email":         id.Email,
			"sessionId":     sessionID,
			"stateBackend":  snapshot.StateBackend,
		})),
	})
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, snapshot state.Snapshot) {
	w.Header().Set("Cache-Control", "no-store")
	_ = s.adminTmpl.Execute(w, map[string]any{
		"Email": id.Email,
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, snapshot state.Snapshot) {
	writeJSON(w, http.StatusOK, apiResponse{OK: true, State: snapshot, Phone: s.relay.Snapshot()})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, snapshot state.Snapshot) {
	writeJSON(w, http.StatusOK, apiResponse{OK: true, State: snapshot, Phone: s.relay.Snapshot()})
}

func (s *Server) handleClaimControl(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, _ state.Snapshot) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	snapshot, err := s.store.ClaimControl(r.Context(), s.cfg.TicketID, sessionID, id.Email, time.Now())
	s.writeStateMutation(w, r, id.Email, "control_claim", snapshot, err)
}

func (s *Server) handleExtendControl(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, _ state.Snapshot) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	snapshot, err := s.store.ExtendControl(r.Context(), s.cfg.TicketID, sessionID, id.Email, time.Now())
	s.writeStateMutation(w, r, id.Email, "control_extend", snapshot, err)
}

func (s *Server) handleReleaseControl(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, _ state.Snapshot) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	snapshot, err := s.store.ReleaseControl(r.Context(), s.cfg.TicketID, sessionID, id.Email, "user_released", time.Now())
	s.writeStateMutation(w, r, id.Email, "control_release", snapshot, err)
}

func (s *Server) handleClientLog(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, _ state.Snapshot) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{OK: false, Error: "bad_request", Message: "Client log was too large."})
		return
	}
	log.Printf("ticket client log email=%s session=%s ua=%q body=%s", id.Email, sessionID, r.UserAgent(), strings.TrimSpace(string(body)))
	writeJSON(w, http.StatusOK, apiResponse{OK: true})
}

func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, snapshot state.Snapshot) {
	writeJSON(w, http.StatusOK, apiResponse{OK: true, State: snapshot, Phone: s.relay.Snapshot()})
}

func (s *Server) handleAdminMembers(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, _ state.Snapshot) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, apiResponse{OK: false, Error: "bad_request", Message: err.Error()})
			return
		}
		snapshot, err := s.store.UpsertMember(r.Context(), s.cfg.TicketID, id.Email, req.Email, req.Role)
		s.writeStateMutation(w, r, id.Email, "member_upsert", snapshot, err)
	case http.MethodDelete:
		email := strings.TrimSpace(r.URL.Query().Get("email"))
		snapshot, err := s.store.RemoveMember(r.Context(), s.cfg.TicketID, id.Email, email)
		s.writeStateMutation(w, r, id.Email, "member_remove", snapshot, err)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminRevokeControl(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, _ state.Snapshot) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	snapshot, err := s.store.RevokeControl(r.Context(), s.cfg.TicketID, id.Email, "admin_revoked", time.Now())
	s.writeStateMutation(w, r, id.Email, "control_revoke", snapshot, err)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id, sessionID, snapshot, ok := s.identifyMember(w, r)
	if !ok {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	c := &client{conn: conn, sessionID: sessionID, email: id.Email, page: "ticket"}
	s.addClient(c)
	s.relay.AddViewer()
	if heartbeat, err := s.store.HeartbeatPresence(r.Context(), state.PresenceInput{
		TicketID:    s.cfg.TicketID,
		SessionID:   sessionID,
		Email:       id.Email,
		DisplayName: id.Email,
		Page:        "ticket",
		Connected:   true,
		Now:         time.Now(),
	}); err == nil {
		s.cacheSnapshot(heartbeat)
		s.rememberControlGate(heartbeat, time.Now())
		snapshot = heartbeat
	} else {
		log.Printf("ticket presence heartbeat failed for %s: %v", id.Email, err)
	}
	c.sendJSON(context.Background(), map[string]any{"type": "state", "state": snapshot, "phone": s.relay.Snapshot()})
	defer func() {
		s.removeClient(c)
		s.relay.RemoveViewer()
		if snapshot, err := s.store.DisconnectPresence(context.Background(), s.cfg.TicketID, sessionID, time.Now()); err == nil {
			s.cacheSnapshot(snapshot)
			s.rememberControlGate(snapshot, time.Now())
		} else {
			log.Printf("ticket presence disconnect failed for %s: %v", c.email, err)
		}
		s.broadcastState()
		_ = conn.Close(websocket.StatusNormalClosure, "closed")
	}()
	for {
		typ, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		s.handleClientMessage(r.Context(), c, data)
	}
}

func (s *Server) handleClientMessage(ctx context.Context, c *client, data []byte) {
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	msgType, _ := msg["type"].(string)
	now := time.Now()
	switch msgType {
	case "heartbeat":
		snapshot, err := s.store.HeartbeatPresence(ctx, state.PresenceInput{
			TicketID:    s.cfg.TicketID,
			SessionID:   c.sessionID,
			Email:       c.email,
			DisplayName: c.email,
			Page:        c.page,
			Connected:   true,
			Now:         now,
		})
		if err != nil {
			log.Printf("ticket presence heartbeat failed for %s: %v", c.email, err)
			if cached, ok := s.cachedSnapshot(now); ok {
				c.sendJSON(ctx, map[string]any{"type": "state", "state": cached, "phone": s.relay.Snapshot()})
			}
			return
		}
		s.cacheSnapshot(snapshot)
		s.rememberControlGate(snapshot, now)
		c.sendJSON(ctx, map[string]any{"type": "state", "state": snapshot, "phone": s.relay.Snapshot()})
	case "tap":
		active, allowed := s.activeControlGateAllows(c.sessionID, c.email, now)
		if !active || !allowed {
			go func() {
				_ = s.store.Audit(context.Background(), s.cfg.TicketID, c.email, "input_ignored", map[string]any{"reason": "not_active_controller"}, time.Now())
			}()
			c.sendJSON(ctx, map[string]any{"type": "input", "accepted": false, "reason": "not_active_controller"})
			return
		}
		_ = s.relay.SendText(ctx, data)
		c.sendJSON(ctx, map[string]any{"type": "input", "accepted": true})
	case "activity":
		_ = s.relay.SendText(ctx, data)
	case "swipe", "long_press", "longpress", "hold":
		_ = s.store.Audit(ctx, s.cfg.TicketID, c.email, "input_ignored", map[string]any{"reason": msgType}, now)
		c.sendJSON(ctx, map[string]any{"type": "input", "accepted": false, "reason": "blocked_gesture"})
	default:
	}
}

func (s *Server) handlePhoneMessage(msg phone.Message) {
	if len(msg.Text) > 0 {
		s.handlePhoneText(msg.Text)
		s.broadcastText(msg.Text)
		return
	}
	if len(msg.Binary) > 0 {
		s.broadcastFrame(msg.Binary)
	}
}

func (s *Server) handlePhoneDisconnect(err error) {
	if err != nil {
		log.Printf("ticket phone disconnected: %v", err)
	}
	s.broadcastPhoneStatus("reconnecting", "Phone stream reconnecting")
}

func (s *Server) handlePhoneText(raw []byte) {
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	now := time.Now()
	if msgType, _ := msg["type"].(string); msgType == "health" {
		data, _ := msg["data"].(map[string]any)
		healthJSON := string(raw)
		if snapshot, err := s.store.UpdatePhone(context.Background(), state.PhoneInput{
			TicketID:     s.cfg.TicketID,
			BackendID:    s.cfg.Phone.BackendID,
			AttachName:   s.cfg.Phone.AttachName,
			BaseURL:      s.cfg.Phone.BaseURL,
			DesiredState: "streaming",
			HealthJSON:   healthJSON,
			Now:          now,
		}); err == nil {
			s.cacheSnapshot(snapshot)
			s.rememberControlGate(snapshot, now)
		} else {
			log.Printf("ticket phone state update failed: %v", err)
		}
		streamActive, _ := data["streamActive"].(bool)
		inactivityActive, _ := data["inactivityActive"].(bool)
		s.maybeRequestPhoneStart(data, "phone_health")
		if !streamActive && !inactivityActive {
			snapshot, err := s.store.Snapshot(context.Background(), s.cfg.TicketID, now)
			if err == nil && snapshot.ActiveControl != nil {
				_, _ = s.store.ReleaseControl(context.Background(), s.cfg.TicketID, "", "", "phone_left_ticket", now)
				s.broadcastState()
			}
		}
	}
}

func (s *Server) writeStateMutation(w http.ResponseWriter, r *http.Request, actor string, event string, snapshot state.Snapshot, err error) {
	if err != nil {
		log.Printf("ticket state mutation %s failed for %s: %v", event, actor, err)
		status := http.StatusConflict
		if errors.Is(err, state.ErrForbidden) || errors.Is(err, state.ErrNotMember) {
			status = http.StatusForbidden
		}
		writeJSON(w, status, apiResponse{OK: false, Error: errorCode(err), Message: err.Error(), State: snapshot})
		return
	}
	s.cacheSnapshot(snapshot)
	s.rememberControlGate(snapshot, time.Now())
	if err := s.store.Audit(r.Context(), s.cfg.TicketID, actor, event, nil, time.Now()); err != nil {
		log.Printf("ticket audit %s failed for %s: %v", event, actor, err)
	}
	s.broadcastState()
	writeJSON(w, http.StatusOK, apiResponse{OK: true, State: snapshot, Phone: s.relay.Snapshot()})
}

func (s *Server) withMember(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request, auth.Identity, string, state.Snapshot)) {
	id, sessionID, snapshot, ok := s.identifyMember(w, r)
	if !ok {
		return
	}
	next(w, r, id, sessionID, snapshot)
}

func (s *Server) withAdmin(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request, auth.Identity, string, state.Snapshot)) {
	id, sessionID, snapshot, ok := s.identifyMember(w, r)
	if !ok {
		return
	}
	if !snapshot.IsAdmin(id.Email) {
		writeErrorPage(w, http.StatusForbidden, "Admin access is required.")
		return
	}
	next(w, r, id, sessionID, snapshot)
}

func (s *Server) identifyMember(w http.ResponseWriter, r *http.Request) (auth.Identity, string, state.Snapshot, bool) {
	id, err := s.auth.IdentityFromRequest(r.Context(), r)
	if err != nil {
		writeErrorPage(w, http.StatusUnauthorized, "Cloudflare Access identity is required.")
		return auth.Identity{}, "", state.Snapshot{}, false
	}
	sessionID := s.sessionID(w, r)
	now := time.Now()
	snapshot, err := s.store.Snapshot(r.Context(), s.cfg.TicketID, now)
	if err != nil {
		log.Printf("ticket state lookup failed for %s: %v", id.Email, err)
		cached, ok := s.cachedSnapshot(now)
		if !ok {
			writeErrorPage(w, http.StatusServiceUnavailable, "Ticket state is unavailable.")
			return auth.Identity{}, "", state.Snapshot{}, false
		}
		snapshot = cached
	} else {
		s.cacheSnapshot(snapshot)
		s.rememberControlGate(snapshot, now)
	}
	if _, ok := snapshot.Member(id.Email); !ok {
		writeErrorPage(w, http.StatusForbidden, "This email is not linked to this ticket.")
		return auth.Identity{}, "", snapshot, false
	}
	return id, sessionID, snapshot, true
}

func (s *Server) sessionID(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie(s.cfg.CookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
		return cookie.Value
	}
	sessionID := randomID()
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    sessionID,
		Path:     "/",
		MaxAge:   int(s.cfg.CookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.PublicBaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
	return sessionID
}

func (s *Server) addClient(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c] = struct{}{}
}

func (s *Server) removeClient(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, c)
}

func (s *Server) clientSnapshot() []*client {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		out = append(out, c)
	}
	return out
}

func (s *Server) broadcastText(data []byte) {
	for _, c := range s.clientSnapshot() {
		c.sendText(context.Background(), data)
	}
}

func (s *Server) broadcastFrame(data []byte) {
	now := time.Now()
	for _, c := range s.clientSnapshot() {
		if !s.controlGateAllows(c.sessionID, c.email, now) {
			continue
		}
		c.sendBinary(context.Background(), data)
	}
}

func (s *Server) broadcastState() {
	now := time.Now()
	snapshot, err := s.store.Snapshot(context.Background(), s.cfg.TicketID, now)
	if err != nil {
		log.Printf("ticket state broadcast failed: %v", err)
		if cached, ok := s.cachedSnapshot(now); ok {
			s.rememberControlGate(cached, now)
			payload, _ := json.Marshal(map[string]any{"type": "state", "state": cached, "phone": s.relay.Snapshot()})
			s.broadcastText(payload)
		}
		return
	}
	s.cacheSnapshot(snapshot)
	s.rememberControlGate(snapshot, now)
	payload, _ := json.Marshal(map[string]any{"type": "state", "state": snapshot, "phone": s.relay.Snapshot()})
	s.broadcastText(payload)
}

func (s *Server) broadcastCachedState(now time.Time) {
	snapshot, ok := s.cachedSnapshot(now)
	if !ok {
		s.broadcastState()
		return
	}
	s.rememberControlGate(snapshot, now)
	payload, _ := json.Marshal(map[string]any{"type": "state", "state": snapshot, "phone": s.relay.Snapshot()})
	s.broadcastText(payload)
}

func (s *Server) cacheSnapshot(snapshot state.Snapshot) {
	if snapshot.Ticket.ID == "" {
		return
	}
	s.stateMu.Lock()
	s.cachedState = snapshot
	s.stateMu.Unlock()
}

func (s *Server) cachedSnapshot(now time.Time) (state.Snapshot, bool) {
	s.stateMu.RLock()
	snapshot := s.cachedState
	s.stateMu.RUnlock()
	if snapshot.Ticket.ID == "" {
		return state.Snapshot{}, false
	}
	if snapshot.ActiveControl != nil {
		control := *snapshot.ActiveControl
		snapshot.ActiveControl = &control
	}
	adjustSnapshotTime(&snapshot, now)
	return snapshot, true
}

func adjustSnapshotTime(snapshot *state.Snapshot, now time.Time) {
	snapshot.ServerTime = now.UTC().Format(time.RFC3339)
	if snapshot.ActiveControl == nil {
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, snapshot.ActiveControl.ExpiresAt)
	if err != nil || !now.Before(expiresAt) {
		snapshot.ActiveControl = nil
		return
	}
	snapshot.ActiveControl.RemainingMS = int64(expiresAt.Sub(now) / time.Millisecond)
}

func (s *Server) rememberControlGate(snapshot state.Snapshot, now time.Time) {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	if snapshot.ActiveControl == nil {
		s.gate = nil
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, snapshot.ActiveControl.ExpiresAt)
	if err != nil || !now.Before(expiresAt) {
		s.gate = nil
		return
	}
	s.gate = &controlGate{
		sessionID: snapshot.ActiveControl.SessionID,
		email:     strings.ToLower(strings.TrimSpace(snapshot.ActiveControl.Email)),
		expiresAt: expiresAt,
	}
}

func (s *Server) controlGateAllows(sessionID string, email string, now time.Time) bool {
	s.gateMu.RLock()
	gate := s.gate
	s.gateMu.RUnlock()
	if gate == nil || !now.Before(gate.expiresAt) {
		return true
	}
	return gate.sessionID == sessionID && gate.email == strings.ToLower(strings.TrimSpace(email))
}

func (s *Server) activeControlGateAllows(sessionID string, email string, now time.Time) (bool, bool) {
	s.gateMu.RLock()
	gate := s.gate
	s.gateMu.RUnlock()
	if gate == nil || !now.Before(gate.expiresAt) {
		return false, false
	}
	return true, gate.sessionID == sessionID && gate.email == strings.ToLower(strings.TrimSpace(email))
}

func (s *Server) requestOriginAllowed(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	originURL, err := url.Parse(origin)
	if err != nil || originURL.Host == "" {
		return false
	}
	allowedHosts := []string{r.Host}
	if forwardedHost := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwardedHost != "" {
		allowedHosts = append(allowedHosts, forwardedHost)
	}
	if publicURL, err := url.Parse(s.cfg.PublicBaseURL); err == nil && publicURL.Host != "" {
		allowedHosts = append(allowedHosts, publicURL.Host)
	}
	for _, host := range allowedHosts {
		if strings.EqualFold(originURL.Host, strings.TrimSpace(host)) {
			return true
		}
	}
	return false
}

func (s *Server) broadcastPhoneStatus(stateText string, message string) {
	payload, _ := json.Marshal(map[string]any{"type": "phone", "state": stateText, "message": message, "phone": s.relay.Snapshot()})
	s.broadcastText(payload)
}

func (s *Server) maybeRequestPhoneStart(data map[string]any, reason string) {
	if data == nil {
		return
	}
	if streamActive, _ := data["streamActive"].(bool); streamActive {
		return
	}
	relayHealth := s.relay.Snapshot()
	if relayHealth.Viewers <= 0 || !relayHealth.Desired || !relayHealth.Connected {
		return
	}
	now := time.Now()
	s.phoneStartMu.Lock()
	if !s.lastPhoneStartAttempt.IsZero() && now.Sub(s.lastPhoneStartAttempt) < 10*time.Second {
		s.phoneStartMu.Unlock()
		return
	}
	s.lastPhoneStartAttempt = now
	s.phoneStartMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.relay.SendJSON(ctx, map[string]any{"type": "start", "reason": reason}); err != nil {
			log.Printf("ticket phone stream restart request failed: %v", err)
		}
	}()
}

func (s *Server) maybeRequestPhoneStartFromSnapshot(snapshot state.Snapshot) {
	if snapshot.Phone == nil || strings.TrimSpace(snapshot.Phone.HealthJSON) == "" {
		return
	}
	var msg struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(snapshot.Phone.HealthJSON), &msg); err != nil || msg.Type != "health" {
		return
	}
	s.maybeRequestPhoneStart(msg.Data, "state_tick")
}

func (s *Server) stateTicker() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastRefresh time.Time
	for now := range ticker.C {
		if len(s.clientSnapshot()) > 0 {
			if lastRefresh.IsZero() || now.Sub(lastRefresh) >= 15*time.Second {
				s.broadcastState()
				if snapshot, ok := s.cachedSnapshot(now); ok {
					s.maybeRequestPhoneStartFromSnapshot(snapshot)
				}
				lastRefresh = now
				continue
			}
			if snapshot, ok := s.cachedSnapshot(now); ok {
				s.broadcastCachedState(now)
				s.maybeRequestPhoneStartFromSnapshot(snapshot)
			} else {
				s.broadcastState()
			}
		}
	}
}

func (c *client) sendJSON(ctx context.Context, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		return
	}
	c.sendText(ctx, body)
}

func (c *client) sendText(ctx context.Context, value []byte) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = c.conn.Write(ctx, websocket.MessageText, value)
}

func (c *client) sendBinary(ctx context.Context, value []byte) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = c.conn.Write(ctx, websocket.MessageBinary, value)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeErrorPage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<!doctype html><title>Ticket</title><body style=\"font-family:system-ui;margin:40px;background:#0b0f17;color:#eef3fb\"><h1>%d</h1><p>%s</p></body>", status, template.HTMLEscapeString(message))
}

func randomID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func mustJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, state.ErrForbidden):
		return "forbidden"
	case errors.Is(err, state.ErrNotMember):
		return "not_member"
	case errors.Is(err, state.ErrControlClaimed):
		return "control_claimed"
	case errors.Is(err, state.ErrNoControl):
		return "no_control"
	case errors.Is(err, state.ErrNotController):
		return "not_controller"
	case errors.Is(err, state.ErrAlreadyExtended):
		return "already_extended"
	default:
		return "error"
	}
}

const indexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Ticket</title>
  <link rel="stylesheet" href="/static/app.css">
  <script>
    window.TICKET_REMOTE_CONFIG = {{.ConfigJSON}};
  </script>
  <script defer src="/static/app.js"></script>
</head>
<body>
  <main class="shell">
    <section class="stage">
      <canvas id="screen" width="540" height="1080" aria-label="ViVi ticket stream"></canvas>
      <div id="emptyState" class="empty-state">
        <div class="empty-inner">
          <button id="startStream" class="primary" type="button">Start</button>
          <div id="emptyMessage" class="empty-message" aria-live="polite"></div>
        </div>
      </div>
      <div id="privacyOverlay" class="privacy-overlay" hidden>
        <div class="overlay-title">Controle code mode</div>
        <div id="privacyText" class="overlay-text"></div>
      </div>
    </section>
    <aside id="panel" class="panel">
      <div class="identity">
        <span id="connectionState">Connecting</span>
        <a href="/admin" class="admin-link">Admin</a>
      </div>
      <div class="control-row">
        <button id="claimControl" class="primary" type="button">Controle code</button>
        <button id="extendControl" type="button" hidden>Extend</button>
        <button id="releaseControl" type="button" hidden>End</button>
      </div>
      <div id="timer" class="timer" hidden>45s</div>
      <div id="statusLine" class="status-line"></div>
      <div id="presence" class="presence"></div>
    </aside>
  </main>
  <dialog id="claimDialog" class="claim-dialog">
    <form method="dialog">
      <h1>Private controle-code session</h1>
      <p>You get the only phone controls for 45 seconds. Others stay connected, see who claimed it and the timer, and return to general viewing when it ends.</p>
      <p>You can extend once, up to 90 seconds total.</p>
      <menu>
        <button value="cancel">Cancel</button>
        <button id="confirmClaim" value="claim" class="primary">Claim</button>
      </menu>
    </form>
  </dialog>
</body>
</html>`

const adminHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Ticket Admin</title>
  <link rel="stylesheet" href="/static/app.css">
  <script defer src="/static/app.js"></script>
</head>
<body class="admin-page">
  <main class="admin-shell" data-admin="true">
    <header class="admin-header">
      <h1>Ticket admin</h1>
      <a href="/">Stream</a>
    </header>
    <section class="admin-section">
      <form id="memberForm" class="member-form">
        <input id="memberEmail" type="email" placeholder="email@example.com" required>
        <select id="memberRole">
          <option value="member">member</option>
          <option value="admin">admin</option>
          <option value="owner">owner</option>
        </select>
        <button class="primary" type="submit">Add</button>
      </form>
      <div id="adminMembers" class="admin-list"></div>
    </section>
    <section class="admin-section">
      <button id="adminRevoke" type="button">Revoke control</button>
      <pre id="adminState" class="admin-state"></pre>
    </section>
  </main>
</body>
</html>`
