package web

import (
	"strings"
	"time"
)

func (s *Server) tryAddClient(c *client) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) >= maxBrowserSocketConnections {
		return false
	}
	identityConnections := 0
	sessionConnections := 0
	for existing := range s.clients {
		if strings.EqualFold(strings.TrimSpace(existing.email), strings.TrimSpace(c.email)) {
			identityConnections++
		}
		if strings.TrimSpace(existing.sessionID) == strings.TrimSpace(c.sessionID) {
			sessionConnections++
		}
	}
	if identityConnections >= maxBrowserSocketsPerIdentity || sessionConnections >= maxBrowserSocketsPerSession {
		return false
	}
	s.clients[c] = struct{}{}
	return true
}

func (s *Server) removeClient(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, c)
}

func (s *Server) addRelayViewer(sessionID string) uint64 {
	s.streamLifecycleMu.Lock()
	defer s.streamLifecycleMu.Unlock()
	s.addRelayViewerLocked(sessionID)
	return s.relayViewerGeneration
}

func (s *Server) addRelayViewerLocked(sessionID string) {
	if s.coldRestartBlocked.Load() {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID != "" {
		s.mu.Lock()
		previous := s.relayViewerRefs[sessionID]
		s.relayViewerRefs[sessionID] = previous + 1
		s.mu.Unlock()
		if previous > 0 {
			return
		}
	}
	s.relay.AddViewer()
	viewers := s.relay.Snapshot().Viewers
	s.cancelIdleStreamDesiredRelease()
	s.publishStreamDesiredStateAsync(true, viewers, "relay_viewer_added", "ticket_remote_relay")
	s.publishRelayCurrentReportAsync("relay_viewer_added")
}

// Prewarm and socket startup share the same bounded hold. The opening ID is
// checked while installing it, so a completed or superseded page cannot revive it.
func (s *Server) retainRelayViewerForOpening(sessionID string, hold time.Duration, kind, openingID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || hold <= 0 {
		return
	}
	s.startupLeaseMu.Lock()
	defer s.startupLeaseMu.Unlock()
	s.streamLifecycleMu.Lock()
	defer s.streamLifecycleMu.Unlock()
	added := false
	install := func() {
		added = s.retainRelayLeaseForDuration(sessionID, sessionID, hold, true, kind, openingID != "", openingID)
	}
	if openingID != "" {
		s.direct.withActiveOpening(openingID, install)
	} else {
		s.direct.withoutActiveOpeningForSession(sessionID, install)
	}
	if added {
		s.addRelayViewerLocked(sessionID)
	}
}

func (s *Server) releaseOpeningWarm(sessionID, openingID string) {
	s.startupLeaseMu.Lock()
	defer s.startupLeaseMu.Unlock()
	released := false
	release := func() {
		released = s.retainRelayLeaseForDuration(sessionID, sessionID, 0, false, "release", false, openingID)
	}
	if openingID != "" {
		release()
	} else {
		s.direct.withoutActiveOpeningForSession(sessionID, release)
	}
	if released {
		s.removeRelayViewer(sessionID)
	}
}

func (s *Server) retainRelayLeaseForDuration(leaseID, sessionID string, hold time.Duration, retain bool, reason string, allowOwnerReplacement bool, requestedTraceID string) bool {
	if retain && s.coldRestartBlocked.Load() {
		return false
	}
	sessionID = strings.TrimSpace(sessionID)
	leaseID = strings.TrimSpace(leaseID)
	if sessionID == "" || leaseID == "" {
		return false
	}
	if retain {
		if hold <= 0 {
			hold = streamPrewarmHold
		}
	}
	shouldChangeViewer := false
	var timer *time.Timer

	s.mu.Lock()
	if s.streamPrewarmTimers == nil {
		s.streamPrewarmTimers = map[string]*time.Timer{}
	}
	if s.streamPrewarmOwners == nil {
		s.streamPrewarmOwners = map[string]string{}
	}
	if retain {
		reason = cleanStreamControlText(reason, "prewarm")
		existing := s.streamPrewarmTimers[leaseID]
		existingOwner := s.streamPrewarmOwners[leaseID]
		if existing != nil && existingOwner != requestedTraceID && !allowOwnerReplacement {
			s.mu.Unlock()
			return false
		}
		if existing != nil {
			existing.Stop()
		} else {
			shouldChangeViewer = true
		}
		timer = time.AfterFunc(hold, func() {
			s.startupLeaseMu.Lock()
			defer s.startupLeaseMu.Unlock()
			shouldRemoveViewer := false
			s.mu.Lock()
			if s.streamPrewarmTimers[leaseID] == timer {
				delete(s.streamPrewarmTimers, leaseID)
				delete(s.streamPrewarmOwners, leaseID)
				if reason == "page_open_warm" {
					delete(s.streamPageOpenWarmUntil, sessionID)
				}
				shouldRemoveViewer = true
			}
			s.mu.Unlock()
			if shouldRemoveViewer {
				if reason == "public_open_grace" {
					s.recordRuntimeEventForSourceAsync("ticket_remote_relay", "info", "public_open_grace_expired", safeRuntimeTraceID("browser", sessionID), map[string]any{
						"holdMillis": durationMillis(hold),
					})
				}
				s.removeRelayViewer(sessionID)
			}
		})
		s.streamPrewarmTimers[leaseID] = timer
		s.streamPrewarmOwners[leaseID] = requestedTraceID
	} else {
		if existing := s.streamPrewarmTimers[leaseID]; existing != nil {
			existingOwner := s.streamPrewarmOwners[leaseID]
			if existingOwner != requestedTraceID {
				s.mu.Unlock()
				return false
			}
			existing.Stop()
			delete(s.streamPrewarmTimers, leaseID)
			delete(s.streamPrewarmOwners, leaseID)
			shouldChangeViewer = true
		}
	}
	s.mu.Unlock()
	return shouldChangeViewer
}

// Page warmth uses the existing timed lease owner with a distinct timer key.
// Its reference still belongs to the real session, so socket/grace references
// remain independently releasable without counting another viewer.
func (s *Server) retainRelayViewerForPageOpen(sessionID string, openedAt, now time.Time) {
	sessionID = strings.TrimSpace(sessionID)
	deadline := openedAt.Add(streamPageOpenWarmHold)
	if sessionID == "" || openedAt.IsZero() || !deadline.After(now) {
		return
	}
	s.startupLeaseMu.Lock()
	defer s.startupLeaseMu.Unlock()
	s.streamLifecycleMu.Lock()
	defer s.streamLifecycleMu.Unlock()
	hold := time.Until(deadline)
	if s.coldRestartBlocked.Load() {
		return
	}
	if hold <= 0 {
		return
	}
	s.mu.Lock()
	if !deadline.After(s.streamPageOpenWarmUntil[sessionID]) {
		s.mu.Unlock()
		return
	}
	if s.streamPageOpenWarmUntil == nil {
		s.streamPageOpenWarmUntil = map[string]time.Time{}
	}
	s.streamPageOpenWarmUntil[sessionID] = deadline
	s.mu.Unlock()
	if s.retainRelayLeaseForDuration("page_open_warm:"+sessionID, sessionID, hold, true, "page_open_warm", false, "") {
		s.addRelayViewerLocked(sessionID)
	}
}

func (s *Server) pageOpenWarmSnapshot(now time.Time) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	var latest time.Time
	for _, deadline := range s.streamPageOpenWarmUntil {
		if deadline.After(now) {
			count++
			if deadline.After(latest) {
				latest = deadline
			}
		}
	}
	expiresAt := ""
	if !latest.IsZero() {
		expiresAt = latest.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{"retainedSessions": count, "expiresAt": expiresAt}
}

func (s *Server) removeRelayViewer(sessionID string) {
	s.streamLifecycleMu.Lock()
	defer s.streamLifecycleMu.Unlock()
	s.removeRelayViewerLocked(sessionID)
}

func (s *Server) removeRelayViewerGeneration(sessionID string, generation uint64) {
	s.streamLifecycleMu.Lock()
	defer s.streamLifecycleMu.Unlock()
	if generation != s.relayViewerGeneration {
		return
	}
	s.removeRelayViewerLocked(sessionID)
}

func (s *Server) removeRelayViewerLocked(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		s.relay.RemoveViewer()
		viewers := s.relay.Snapshot().Viewers
		s.recordRuntimeEventForSourceAsync("ticket_remote_relay", "info", "relay_viewer_removed", "", map[string]any{
			"viewerCount": viewers,
			"session":     false,
		})
		if viewers > 0 {
			s.publishStreamDesiredStateAsync(true, viewers, "relay_viewer_removed", "ticket_remote_relay")
		} else {
			s.scheduleIdleStreamDesiredRelease("relay_viewer_removed")
		}
		s.publishRelayCurrentReportAsync("relay_viewer_removed")
		return
	}
	removeFromRelay := false
	viewerCount := 0
	s.mu.Lock()
	if count, ok := s.relayViewerRefs[sessionID]; !ok {
		removeFromRelay = false
	} else if count <= 1 {
		delete(s.relayViewerRefs, sessionID)
		removeFromRelay = true
	} else {
		s.relayViewerRefs[sessionID] = count - 1
	}
	viewerCount = len(s.relayViewerRefs)
	s.mu.Unlock()
	if removeFromRelay {
		s.relay.RemoveViewer()
		s.recordRuntimeEventForSourceAsync("ticket_remote_relay", "info", "relay_viewer_removed", safeRuntimeTraceID("browser", sessionID), map[string]any{
			"viewerCount": viewerCount,
			"session":     true,
		})
		if viewerCount > 0 {
			s.publishStreamDesiredStateAsync(true, viewerCount, "relay_viewer_removed", "ticket_remote_relay")
		} else {
			s.scheduleIdleStreamDesiredRelease("relay_viewer_removed")
		}
		s.publishRelayCurrentReportAsync("relay_viewer_removed")
	}
}
