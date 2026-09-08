package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ticketremote/internal/config"
	"ticketremote/internal/phone"
	"ticketremote/internal/state"
)

func (s *Server) activePhoneBackend() config.PhoneBackend {
	s.backendMu.RLock()
	defer s.backendMu.RUnlock()
	return config.PhoneBackend{
		ID:         s.cfg.Phone.BackendID,
		AttachName: s.cfg.Phone.AttachName,
		BaseURL:    s.cfg.Phone.BaseURL,
	}
}

func (s *Server) configuredPhoneBackends() []config.PhoneBackend {
	s.backendMu.RLock()
	defer s.backendMu.RUnlock()
	return append([]config.PhoneBackend(nil), s.cfg.Phone.Backends...)
}

func (s *Server) setActivePhoneBackend(backend config.PhoneBackend) {
	s.backendMu.Lock()
	defer s.backendMu.Unlock()
	s.cfg.Phone.BackendID = backend.ID
	s.cfg.Phone.AttachName = backend.AttachName
	s.cfg.Phone.BaseURL = strings.TrimRight(backend.BaseURL, "/")
}

func (s *Server) withActivePhoneBackend(snapshot state.Snapshot, health phone.Health) state.Snapshot {
	backend := s.activePhoneBackend()
	if backend.ID == "" {
		return snapshot
	}
	desiredState := health.StreamState
	if desiredState == "" {
		desiredState = "idle"
	}
	if snapshot.Phone != nil && snapshot.Phone.ID == backend.ID {
		phoneState := *snapshot.Phone
		if phoneState.AttachName == "" {
			phoneState.AttachName = backend.AttachName
		}
		if phoneState.BaseURL == "" {
			phoneState.BaseURL = backend.BaseURL
		}
		if phoneState.DesiredState == "" {
			phoneState.DesiredState = desiredState
		}
		if phoneState.LastError == "" {
			phoneState.LastError = health.LastError
		}
		if phoneState.LastSeenAt == "" {
			phoneState.LastSeenAt = health.LastSeenAt
		}
		snapshot.Phone = &phoneState
		return snapshot
	}
	snapshot.Phone = &state.PhoneBackend{
		ID:           backend.ID,
		AttachName:   backend.AttachName,
		BaseURL:      backend.BaseURL,
		DesiredState: desiredState,
		LastError:    health.LastError,
		LastSeenAt:   health.LastSeenAt,
	}
	return snapshot
}

func (s *Server) withFreshActivePhoneHealth(ctx context.Context, snapshot state.Snapshot, health phone.Health) state.Snapshot {
	snapshot = s.withActivePhoneBackend(snapshot, health)
	backend := s.activePhoneBackend()
	if backend.ID == "" || strings.TrimSpace(backend.BaseURL) == "" || snapshot.Phone == nil || snapshot.Phone.ID != backend.ID {
		return snapshot
	}
	healthJSON, err := fetchPhoneBackendHealthJSON(ctx, backend)
	if err != nil {
		s.recordRuntimeErrorAsync("admin_phone_health_refresh_failed", backend.ID, err, map[string]any{"backendId": backend.ID})
		return snapshot
	}
	phoneState := *snapshot.Phone
	phoneState.HealthJSON = healthJSON
	phoneState.LastError = ""
	phoneState.LastSeenAt = time.Now().UTC().Format(time.RFC3339)
	snapshot.Phone = &phoneState
	return snapshot
}

func fetchPhoneBackendHealthJSON(ctx context.Context, backend config.PhoneBackend) (string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(backend.BaseURL), "/")
	if baseURL == "" {
		return "", fmt.Errorf("base URL is empty")
	}
	if healthJSON, err := fetchPhoneBackendHealthJSONAt(ctx, baseURL+"/api/v1/upstream/health"); err == nil {
		return healthJSON, nil
	}
	return fetchPhoneBackendHealthJSONAt(ctx, baseURL+"/api/v1/health")
}

func fetchPhoneBackendHealthJSONAt(ctx context.Context, healthURL string) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("health returned %d", resp.StatusCode)
	}
	if !json.Valid(body) {
		return "", fmt.Errorf("health returned invalid JSON")
	}
	return string(body), nil
}

func (s *Server) probePhoneBackend(ctx context.Context, backend config.PhoneBackend) (bool, int, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(backend.BaseURL), "/")
	if baseURL == "" {
		return false, 0, fmt.Errorf("base URL is empty")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, baseURL+"/api/v1/health", nil)
	if err != nil {
		return false, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, resp.StatusCode, fmt.Errorf("health returned %d", resp.StatusCode)
	}
	return true, resp.StatusCode, nil
}
