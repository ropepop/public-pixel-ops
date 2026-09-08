package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ticketremote/internal/auth"
	"ticketremote/internal/config"
	"ticketremote/internal/state"
)

const (
	latestTicketScheduleMaxFormBytes = 4 * 1024
	latestTicketScheduleHorizon      = 90 * 24 * time.Hour
	latestTicketScheduleIDPrefix     = "latest_reselect_"
	manualTicketRedetectPurpose      = "ticket_action_v3_redetect_latest"
	phoneLocalMinuteLayout           = "2006-01-02T15:04"
)

type latestTicketScheduleRequest struct {
	Action     string `json:"action"`
	Date       string `json:"date"`
	Time       string `json:"time"`
	ScheduleID string `json:"scheduleId"`
	BackendID  string `json:"backendId"`
}

type latestTicketSchedulePageView struct {
	ID             string
	BackendID      string
	Purpose        string
	Status         string
	CommandID      string
	ScheduledAt    string
	PhoneLocalTime string
	PhoneTimeZone  string
	ResultReason   string
	ResultPhase    string
	ProofSource    string
	RequestedBy    string
	CreatedAt      string
	UpdatedAt      string
	TriggeredAt    string
	CompletedAt    string
	Cancelable     bool
}

func (s *Server) handleAdminTicketReselectLatestSchedule(w http.ResponseWriter, r *http.Request, id auth.Identity, _ string, snapshot state.Snapshot) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	req, err := decodeLatestTicketScheduleRequest(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{OK: false, Error: "bad_request", Message: err.Error()})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(req.Action), "cancel") {
		handleRetiredTicketRoute(w)
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, apiResponse{OK: false, Error: "state_unavailable", Message: "Ticket state is unavailable."})
		return
	}
	scheduler, ok := s.store.(state.LatestTicketReselectScheduler)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, apiResponse{OK: false, Error: "schedule_unavailable", Message: "Latest-ticket scheduling is not available."})
		return
	}

	now := time.Now()
	s.cancelLatestTicketReselectSchedule(w, r, id, snapshot, scheduler, req, now)
}

func (s *Server) cancelLatestTicketReselectSchedule(
	w http.ResponseWriter,
	r *http.Request,
	id auth.Identity,
	snapshot state.Snapshot,
	scheduler state.LatestTicketReselectScheduler,
	req latestTicketScheduleRequest,
	now time.Time,
) {
	scheduleID := strings.TrimSpace(req.ScheduleID)
	if scheduleID == "" || len(scheduleID) > 120 {
		writeJSON(w, http.StatusBadRequest, apiResponse{OK: false, Error: "bad_schedule_id", Message: "A valid schedule ID is required."})
		return
	}
	backendID := strings.TrimSpace(req.BackendID)
	ticketID := s.cfg.TicketID
	if scheduled, ok := findLatestTicketReselectSchedule(snapshot, scheduleID); ok {
		if !isManualTicketRedetectSchedule(scheduled) {
			writeJSON(w, http.StatusConflict, apiResponse{OK: false, Error: "cancel_failed", Message: "Only a manual scheduled redetection can be cancelled."})
			return
		}
		if strings.TrimSpace(scheduled.BackendID) != "" {
			backendID = strings.TrimSpace(scheduled.BackendID)
		}
		if strings.TrimSpace(scheduled.TicketID) != "" {
			ticketID = strings.TrimSpace(scheduled.TicketID)
		}
	}
	if backendID == "" {
		backendID = strings.TrimSpace(s.activePhoneBackend().ID)
	}
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, apiResponse{OK: false, Error: "phone_backend_unavailable", Message: "The schedule has no phone backend."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), streamControlWriteTimeout)
	defer cancel()
	if err := scheduler.CancelLatestTicketReselect(ctx, state.LatestTicketReselectScheduleCancelInput{
		ScheduleID: scheduleID,
		TicketID:   ticketID,
		BackendID:  backendID,
		Now:        now,
	}); err != nil {
		s.recordRuntimeErrorAsync("latest_ticket_reselect_cancel_failed", scheduleID, err, map[string]any{"backendId": backendID})
		writeJSON(w, http.StatusConflict, apiResponse{OK: false, Error: "cancel_failed", Message: "The latest-ticket schedule could not be cancelled."})
		return
	}
	s.recordAuditAsync(s.cfg.TicketID, id.Email, "latest_ticket_reselect_cancelled", map[string]any{
		"scheduleId": scheduleID,
		"backendId":  backendID,
	}, now)
	if redirectAdminForm(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"scheduleId": scheduleID,
		"backendId":  backendID,
		"status":     "canceled",
	})
}

func decodeLatestTicketScheduleRequest(w http.ResponseWriter, r *http.Request) (latestTicketScheduleRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, latestTicketScheduleMaxFormBytes)
	var req latestTicketScheduleRequest
	if adminFormRequest(r) {
		if err := r.ParseForm(); err != nil {
			return req, fmt.Errorf("read schedule form: %w", err)
		}
		req.Action = r.Form.Get("action")
		req.Date = r.Form.Get("date")
		req.Time = r.Form.Get("time")
		req.ScheduleID = r.Form.Get("scheduleId")
		req.BackendID = r.Form.Get("backendId")
		return req, nil
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return req, fmt.Errorf("read schedule request: %w", err)
	}
	return req, nil
}

func (s *Server) phoneTimeZone() string {
	zone := strings.TrimSpace(s.cfg.Phone.TimeZone)
	if zone == "" {
		zone = config.DefaultPhoneTimeZone
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return config.DefaultPhoneTimeZone
	}
	return zone
}

func (s *Server) phoneSchedulePageData(snapshot state.Snapshot, now time.Time) map[string]any {
	zone := s.phoneTimeZone()
	location, err := time.LoadLocation(zone)
	if err != nil {
		location = time.UTC
	}
	localNow := now.In(location)
	defaultLocal := localNow.Add(15 * time.Minute).Truncate(5 * time.Minute)
	activeBackend := s.activePhoneBackend()
	pending := pendingLatestTicketReselectSchedule(snapshot, activeBackend.ID, now)
	latest := latestTicketReselectScheduleStatus(snapshot, activeBackend.ID)
	if pending != nil {
		latest = pending
	}
	canSchedule := true
	if latest != nil {
		switch strings.ToLower(strings.TrimSpace(latest.Status)) {
		case "queued", "running":
			canSchedule = false
		}
	}
	return map[string]any{
		"PhoneTimeZone":                   zone,
		"PhoneLocalNow":                   localNow.Format("Mon, 2 Jan 2006 15:04:05 MST"),
		"PhoneLocalDate":                  localNow.Format("2006-01-02"),
		"PhoneLocalMaxDate":               localNow.Add(latestTicketScheduleHorizon).Format("2006-01-02"),
		"ScheduleDefaultDate":             defaultLocal.Format("2006-01-02"),
		"ScheduleDefaultTime":             defaultLocal.Format("15:04"),
		"LatestTicketSchedule":            latest,
		"CanScheduleLatestTicketReselect": canSchedule,
	}
}

func pendingLatestTicketReselectSchedule(snapshot state.Snapshot, backendID string, now time.Time) *latestTicketSchedulePageView {
	var selected *latestTicketSchedulePageView
	var selectedAt time.Time
	for _, item := range snapshot.LatestTicketReselectSchedules {
		if !isManualTicketRedetectSchedule(item) {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(item.Status))
		if status != "pending" {
			continue
		}
		if strings.TrimSpace(backendID) != "" && strings.TrimSpace(item.BackendID) != strings.TrimSpace(backendID) {
			continue
		}
		scheduledAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(item.ScheduledAt))
		if err != nil || !scheduledAt.After(now) {
			continue
		}
		if selected != nil && !scheduledAt.Before(selectedAt) {
			continue
		}
		selectedAt = scheduledAt
		selected = latestTicketReselectScheduleView(item)
		selected.Cancelable = true
	}
	return selected
}

func latestTicketReselectScheduleStatus(snapshot state.Snapshot, backendID string) *latestTicketSchedulePageView {
	var selected state.LatestTicketReselectSchedule
	var selectedTime time.Time
	found := false
	for _, item := range snapshot.LatestTicketReselectSchedules {
		if !isManualTicketRedetectSchedule(item) {
			continue
		}
		if strings.TrimSpace(backendID) != "" && strings.TrimSpace(item.BackendID) != strings.TrimSpace(backendID) {
			continue
		}
		itemTime := latestTicketReselectScheduleSortTime(item)
		if found && itemTime.Before(selectedTime) {
			continue
		}
		if found && itemTime.Equal(selectedTime) && strings.TrimSpace(item.ID) <= strings.TrimSpace(selected.ID) {
			continue
		}
		selected = item
		selectedTime = itemTime
		found = true
	}
	if !found {
		return nil
	}
	return latestTicketReselectScheduleView(selected)
}

func latestTicketReselectScheduleSortTime(item state.LatestTicketReselectSchedule) time.Time {
	for _, value := range []string{item.UpdatedAt, item.CompletedAt, item.TriggeredAt, item.CreatedAt, item.ScheduledAt} {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func latestTicketReselectScheduleView(item state.LatestTicketReselectSchedule) *latestTicketSchedulePageView {
	scheduledAt := strings.TrimSpace(item.ScheduledAt)
	if parsed, err := time.Parse(time.RFC3339Nano, scheduledAt); err == nil {
		scheduledAt = parsed.UTC().Format(time.RFC3339)
	}
	return &latestTicketSchedulePageView{
		ID:             item.ID,
		BackendID:      item.BackendID,
		Purpose:        item.Purpose,
		Status:         item.Status,
		CommandID:      item.CommandID,
		ScheduledAt:    scheduledAt,
		PhoneLocalTime: strings.Replace(strings.TrimSpace(item.PhoneLocalTime), "T", " ", 1),
		PhoneTimeZone:  item.PhoneTimeZone,
		ResultReason:   item.ResultReason,
		ResultPhase:    item.ResultPhase,
		ProofSource:    item.ProofSource,
		RequestedBy:    item.RequestedBy,
		CreatedAt:      item.CreatedAt,
		UpdatedAt:      item.UpdatedAt,
		TriggeredAt:    item.TriggeredAt,
		CompletedAt:    item.CompletedAt,
	}
}

func findLatestTicketReselectSchedule(snapshot state.Snapshot, scheduleID string) (state.LatestTicketReselectSchedule, bool) {
	for _, item := range snapshot.LatestTicketReselectSchedules {
		if strings.TrimSpace(item.ID) == strings.TrimSpace(scheduleID) {
			return item, true
		}
	}
	return state.LatestTicketReselectSchedule{}, false
}

func isManualTicketRedetectSchedule(item state.LatestTicketReselectSchedule) bool {
	return strings.TrimSpace(item.Purpose) == manualTicketRedetectPurpose
}
