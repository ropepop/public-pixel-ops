package web

import (
	"testing"
	"time"

	"ticketremote/internal/state"
)

func TestAdjustSnapshotTimeExpiresControlLocally(t *testing.T) {
	now := time.Date(2026, 4, 30, 15, 0, 0, 0, time.UTC)
	snapshot := state.Snapshot{
		ActiveControl: &state.ControlSession{
			ExpiresAt: now.Add(-time.Second).Format(time.RFC3339),
		},
	}

	adjustSnapshotTime(&snapshot, now)

	if snapshot.ActiveControl != nil {
		t.Fatalf("expected expired control to be hidden, got %#v", snapshot.ActiveControl)
	}
	if snapshot.ServerTime != now.Format(time.RFC3339) {
		t.Fatalf("server time = %q", snapshot.ServerTime)
	}
}

func TestAdjustSnapshotTimeUpdatesRemainingControlTime(t *testing.T) {
	now := time.Date(2026, 4, 30, 15, 0, 0, 0, time.UTC)
	snapshot := state.Snapshot{
		ActiveControl: &state.ControlSession{
			ExpiresAt: now.Add(12*time.Second + 500*time.Millisecond).Format(time.RFC3339),
		},
	}

	adjustSnapshotTime(&snapshot, now)

	if snapshot.ActiveControl == nil {
		t.Fatal("expected active control")
	}
	if snapshot.ActiveControl.RemainingMS != 12000 {
		t.Fatalf("remaining ms = %d", snapshot.ActiveControl.RemainingMS)
	}
}

func TestActiveControlGateAllowsOnlyController(t *testing.T) {
	now := time.Date(2026, 4, 30, 15, 0, 0, 0, time.UTC)
	server := &Server{}
	server.gate = &controlGate{
		sessionID: "controller-session",
		email:     "ticket@jolkins.id.lv",
		expiresAt: now.Add(45 * time.Second),
	}

	active, allowed := server.activeControlGateAllows("controller-session", "ticket@jolkins.id.lv", now)
	if !active || !allowed {
		t.Fatalf("controller active=%v allowed=%v", active, allowed)
	}

	active, allowed = server.activeControlGateAllows("other-session", "ticket@jolkins.id.lv", now)
	if !active || allowed {
		t.Fatalf("other session active=%v allowed=%v", active, allowed)
	}
}

func TestActiveControlGateRejectsWhenNoActiveControl(t *testing.T) {
	server := &Server{}
	active, allowed := server.activeControlGateAllows("session", "ticket@jolkins.id.lv", time.Now())
	if active || allowed {
		t.Fatalf("expected inactive gate, got active=%v allowed=%v", active, allowed)
	}
}
