package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpeningHoldRejectsLatePageWorkAndPreservesPageWarmth(t *testing.T) {
	phoneServer := httptest.NewServer(http.NotFoundHandler())
	defer phoneServer.Close()
	server, httpServer, relay := newTicketRecoveryTestServer(t, phoneServer.URL)
	defer httpServer.Close()
	defer server.Close()
	const session = "opening-owner"
	opening := server.direct.startOpeningForRun(session, newStartupRunOrigin(), "test")
	server.retainRelayViewerForOpening(session, time.Hour, "prewarm", opening)
	server.retainRelayViewerForOpening(session, time.Hour, "public_open_grace", opening)
	if got := relay.Snapshot().Viewers; got != 1 {
		t.Fatalf("prewarm and socket grace counted as %d viewers", got)
	}
	server.direct.completeOpening(opening)
	server.releaseOpeningWarm(session, opening)
	server.retainRelayViewerForOpening(session, time.Hour, "prewarm", opening)
	if got := relay.Snapshot().Viewers; got != 0 {
		t.Fatalf("completed page revived its startup hold: %d", got)
	}
	now := time.Now()
	server.retainRelayViewerForPageOpen(session, now, now)
	replacement := server.direct.startOpeningForRun(session, newStartupRunOrigin(), "replacement")
	server.retainRelayViewerForOpening(session, time.Hour, "prewarm", replacement)
	server.releaseOpeningWarm(session, opening)
	server.retainRelayViewerForOpening(session, time.Hour, "public_open_grace", opening)
	server.mu.Lock()
	owner, references := server.streamPrewarmOwners[session], server.relayViewerRefs[session]
	server.mu.Unlock()
	if owner != replacement || references != 2 {
		t.Fatalf("old page changed the replacement or page warmth: owner=%q refs=%d", owner, references)
	}
	server.direct.completeOpening(replacement)
	server.releaseOpeningWarm(session, replacement)
	if got := relay.Snapshot().Viewers; got != 1 {
		t.Fatalf("first picture released the independent page warm hold: %d", got)
	}
}

func TestExpiredOpeningTimerCannotRemoveItsReplacement(t *testing.T) {
	phoneServer := httptest.NewServer(http.NotFoundHandler())
	defer phoneServer.Close()
	server, httpServer, relay := newTicketRecoveryTestServer(t, phoneServer.URL)
	defer httpServer.Close()
	defer server.Close()
	const session = "timer-owner"
	opening := server.direct.startOpeningForRun(session, newStartupRunOrigin(), "test")
	server.retainRelayViewerForOpening(session, time.Hour, "prewarm", opening)
	server.startupLeaseMu.Lock()
	server.mu.Lock()
	server.streamPrewarmTimers[session].Reset(time.Millisecond)
	server.mu.Unlock()
	// Let the old timer become runnable behind the ownership lock, then replace
	// it before allowing its callback to inspect the current lease.
	time.Sleep(20 * time.Millisecond)
	server.retainRelayLeaseForDuration(session, session, time.Hour, true, "prewarm", true, "replacement")
	server.startupLeaseMu.Unlock()
	time.Sleep(20 * time.Millisecond)
	server.mu.Lock()
	owner, references := server.streamPrewarmOwners[session], server.relayViewerRefs[session]
	server.mu.Unlock()
	if owner != "replacement" || references != 1 || relay.Snapshot().Viewers != 1 {
		t.Fatalf("expired timer retired the replacement: owner=%q refs=%d", owner, references)
	}
}
