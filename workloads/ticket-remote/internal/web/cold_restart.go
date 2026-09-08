package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ticketremote/internal/auth"
	"ticketremote/internal/state"
)

const coldShutdownDeadline = 15 * time.Second

func (s *Server) startColdRestartControl() error {
	store, ok := s.store.(state.ColdRestartStore)
	if !ok {
		return nil
	}
	initialCtx, initialCancel := context.WithTimeout(context.Background(), stateLookupTimeout)
	snapshot, err := s.store.Snapshot(initialCtx, s.cfg.TicketID, time.Now())
	initialCancel()
	if err != nil {
		return fmt.Errorf("read initial stream control: %w", err)
	}
	s.coldRestartState = snapshot.ColdRestart
	s.coldRestartBlocked.Store(snapshot.ColdRestart.Blocks())
	ctx, cancel := context.WithCancel(context.Background())
	s.coldRestartCancel = cancel
	s.coldRestartDone = make(chan struct{})
	go s.coldRestartLoop(ctx, store, snapshot.ColdRestart)
	return nil
}

func (s *Server) coldRestartSnapshot() state.ColdRestartState {
	s.coldRestartMu.Lock()
	defer s.coldRestartMu.Unlock()
	return s.coldRestartState
}

func (s *Server) coldRestartLoop(ctx context.Context, store state.ColdRestartStore, initial state.ColdRestartState) {
	defer close(s.coldRestartDone)
	updates := make(chan state.ColdRestartState, 1)
	watcherDone := make(chan struct{})
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer func() { cancelWatch(); <-watcherDone }()
	go func() {
		defer close(watcherDone)
		for watchCtx.Err() == nil {
			_ = store.WatchStreamControl(watchCtx, func(value state.ColdRestartState) {
				select {
				case updates <- value:
				case <-watchCtx.Done():
				}
			})
			timer := time.NewTimer(time.Second)
			select {
			case <-timer.C:
			case <-watchCtx.Done():
				timer.Stop()
				return
			}
		}
	}()
	var deadline *time.Timer
	var deadlineC <-chan time.Time
	defer func() {
		if deadline != nil {
			deadline.Stop()
		}
	}()
	quiesced := ""
	hadViewers := false
	advance := func(value state.ColdRestartState, phase string) {
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := store.AdvanceColdRestart(writeCtx, s.cfg.TicketID, s.activePhoneBackend().ID, value.OperationID, phase); err != nil {
			s.recordRuntimeErrorAsync("cold_restart_progress_failed", value.OperationID, err, map[string]any{"phase": phase})
		}
	}
	apply := func(value state.ColdRestartState) {
		s.coldRestartMu.Lock()
		s.coldRestartState = value
		s.coldRestartMu.Unlock()
		s.coldRestartBlocked.Store(value.Blocks())
		if deadline != nil {
			deadline.Stop()
			deadlineC = nil
		}
		if value.Blocks() && value.Phase != "failed" {
			started, err := time.Parse(time.RFC3339Nano, value.StartedAt)
			if err != nil || time.Since(started) >= coldShutdownDeadline {
				advance(value, "failed")
				return
			}
			deadline = time.NewTimer(time.Until(started.Add(coldShutdownDeadline)))
			deadlineC = deadline.C
		}
		if value.Blocks() && quiesced != value.OperationID {
			hadViewers = s.direct.activeVideoClientCount() > 0
			s.quiesceForColdRestart(value)
			quiesced = value.OperationID
		}
		switch value.Phase {
		case "quiescing":
			advance(value, "stopping")
		case "confirmed":
			// The phone acknowledgement proves teardown. The relay independently
			// proves all warm holds, cached pictures, and delivery ownership ended.
			s.quiesceForColdRestart(value)
			health := s.relay.Snapshot()
			config, frame := s.direct.warmStart()
			if health.Desired || health.Connected || health.Viewers != 0 || len(config) != 0 || len(frame) != 0 {
				advance(value, "failed")
			} else if hadViewers {
				advance(value, "reloading")
			} else {
				advance(value, "asleep")
			}
		}
	}
	apply(initial)
	for {
		select {
		case <-ctx.Done():
			return
		case value := <-updates:
			current := s.coldRestartSnapshot()
			if value.OperationID == current.OperationID && value.Phase == current.Phase {
				continue
			}
			apply(value)
		case <-deadlineC:
			deadlineC = nil
			advance(s.coldRestartSnapshot(), "failed")
		}
	}
}

func (s *Server) quiesceForColdRestart(value state.ColdRestartState) {
	s.startupLeaseMu.Lock()
	s.streamLifecycleMu.Lock()
	s.cancelIdleStreamDesiredRelease()
	s.fenceOrdinaryCaptureDemand(0)
	s.streamDesiredMu.Lock()
	pending := s.streamDesiredPending
	s.streamDesiredPending = nil
	s.streamDesiredMu.Unlock()
	completeStreamDesiredState(supersededResult(pending), streamDesiredStateResult{})
	s.mu.Lock()
	for key, timer := range s.streamPrewarmTimers {
		timer.Stop()
		delete(s.streamPrewarmTimers, key)
	}
	s.streamPrewarmOwners = map[string]string{}
	s.streamPageOpenWarmUntil = nil
	s.relayViewerRefs = map[string]int{}
	s.relayViewerGeneration++
	s.mu.Unlock()
	s.relay.Close()
	s.direct.mu.Lock()
	s.direct.clearFrameCacheLocked()
	s.direct.lastConfig = nil
	s.direct.streamEpoch = 0
	s.direct.allIntraConfigValid = false
	s.direct.mu.Unlock()
	s.streamLifecycleMu.Unlock()
	s.startupLeaseMu.Unlock()
	// The durable browser subscription owns pause/reload. Retire every old media
	// socket so no queued configuration or frame survives the cold boundary.
	for _, c := range s.clientSnapshot() {
		_ = c.conn.CloseNow()
	}
	s.publishRelayCurrentReportAsync("cold_restart_quiesced")
}

func (s *Server) handleColdRestart(w http.ResponseWriter, r *http.Request, identity auth.Identity, _ string, _ state.Snapshot) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	store, ok := s.store.(state.ColdRestartStore)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, apiResponse{Error: "cold_restart_unavailable"})
		return
	}
	var input struct {
		OperationID string `json:"operationId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&input); err != nil || input.OperationID == "" || len(input.OperationID) > 100 {
		writeJSON(w, http.StatusBadRequest, apiResponse{Error: "invalid_cold_restart_id"})
		return
	}
	if err := store.BeginColdRestart(r.Context(), s.cfg.TicketID, s.activePhoneBackend().ID, input.OperationID, identity.Email); err != nil {
		writeJSON(w, http.StatusConflict, apiResponse{Error: "cold_restart_rejected", Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}
