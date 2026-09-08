package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ticketremote/internal/state"
)

type coldRestartResumeStore struct{ advances chan string }

func (*coldRestartResumeStore) BeginColdRestart(context.Context, string, string, string, string) error {
	panic("reconciliation must not begin or replay a cold operation")
}
func (s *coldRestartResumeStore) AdvanceColdRestart(_ context.Context, _, _, operation, phase string) error {
	s.advances <- operation + ":" + phase
	return nil
}
func (*coldRestartResumeStore) WatchStreamControl(ctx context.Context, _ func(state.ColdRestartState)) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestColdRestartCoordinatorResumesRecordedStateWithoutReplay(t *testing.T) {
	for _, tc := range []struct {
		phase, advance string
		expired        bool
	}{
		{phase: "stopping"},
		{phase: "confirmed", advance: "asleep"},
		{phase: "failed"},
		{phase: "stopping", advance: "failed", expired: true},
	} {
		t.Run(tc.phase+tc.advance, func(t *testing.T) {
			phoneServer := httptest.NewServer(http.NotFoundHandler())
			defer phoneServer.Close()
			server, httpServer, relay := newTicketRecoveryTestServer(t, phoneServer.URL)
			defer httpServer.Close()
			defer server.Close()
			started := time.Now()
			if tc.expired {
				started = started.Add(-time.Minute)
			}
			store := &coldRestartResumeStore{advances: make(chan string, 4)}
			ctx, cancel := context.WithCancel(context.Background())
			server.coldRestartDone = make(chan struct{})
			go server.coldRestartLoop(ctx, store, state.ColdRestartState{
				OperationID: "retained-operation", Phase: tc.phase, StartedAt: started.Format(time.RFC3339Nano),
			})
			if tc.advance != "" {
				select {
				case got := <-store.advances:
					if got != "retained-operation:"+tc.advance {
						t.Errorf("unexpected progress %q", got)
					}
				case <-time.After(time.Second):
					t.Error("recorded operation did not reconcile")
				}
			}
			cancel()
			select {
			case <-server.coldRestartDone:
			case <-time.After(time.Second):
				t.Fatal("coordinator did not stop")
			}
			if !server.coldRestartBlocked.Load() {
				t.Error("barrier released before durable confirmation")
			}
			if health := relay.Snapshot(); health.Desired || health.Connected {
				t.Error("reconciliation restarted capture")
			}
			select {
			case got := <-store.advances:
				t.Errorf("unexpected extra progress %q", got)
			default:
			}
		})
	}
}

func TestColdRestartCancelsActualWarmTimerAndFencesOldViewer(t *testing.T) {
	phoneServer := httptest.NewServer(http.NotFoundHandler())
	defer phoneServer.Close()
	server, httpServer, relay := newTicketRecoveryTestServer(t, phoneServer.URL)
	defer httpServer.Close()
	defer server.Close()
	const session = "cold-test-viewer"
	oldGeneration := server.addRelayViewer(session)
	server.retainRelayViewerForPageOpen(session, time.Now(), time.Now())
	opening := server.direct.startOpeningForRun(session, newStartupRunOrigin(), "test")
	server.retainRelayViewerForOpening(session, time.Hour, "prewarm", opening)
	server.direct.mu.Lock()
	server.direct.lastConfig = []byte("old-config")
	server.direct.lastFrame = []byte("old-frame")
	server.direct.streamEpoch = 8
	server.direct.mu.Unlock()
	if len(server.streamPrewarmTimers) < 2 {
		t.Fatal("test must own both real warm and startup timers")
	}
	server.coldRestartBlocked.Store(true)
	server.quiesceForColdRestart(state.ColdRestartState{OperationID: "test", Phase: "quiescing"})
	server.retainRelayViewerForPageOpen(session, time.Now(), time.Now())
	server.retainRelayViewerForOpening(session, time.Hour, "prewarm", opening)
	server.addRelayViewer(session)
	if len(server.streamPrewarmTimers) != 0 || len(server.streamPageOpenWarmUntil) != 0 || len(server.relayViewerRefs) != 0 {
		t.Fatal("cold restart left or revived warm ownership")
	}
	health := relay.Snapshot()
	if health.Desired || health.Connected || health.Viewers != 0 {
		t.Fatalf("relay is not cold: %+v", health)
	}
	if len(server.direct.lastConfig) != 0 || len(server.direct.lastFrame) != 0 {
		t.Fatal("cold restart retained cached pictures")
	}
	server.coldRestartBlocked.Store(false)
	replacementGeneration := server.addRelayViewer(session)
	server.removeRelayViewerGeneration(session, oldGeneration)
	if relay.Snapshot().Viewers != 1 {
		t.Fatal("old page disconnected the replacement")
	}
	server.removeRelayViewerGeneration(session, replacementGeneration)
	if relay.Snapshot().Viewers != 0 {
		t.Fatal("replacement could not release its own viewer")
	}
}
