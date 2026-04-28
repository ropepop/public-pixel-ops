package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	trainapp "telegramtrainapp/internal/app"
	"telegramtrainapp/internal/config"
	"telegramtrainapp/internal/domain"
	"telegramtrainapp/internal/i18n"
	"telegramtrainapp/internal/reports"
	"telegramtrainapp/internal/ride"
	"telegramtrainapp/internal/schedule"
	"telegramtrainapp/internal/store"
)

func testSessionCookie(t *testing.T, server *Server, userID int64, language string, now time.Time) *http.Cookie {
	t.Helper()

	cookie, err := issueSessionCookie(server.sessionSecret, telegramAuth{
		AuthDate: now,
		User: telegramUser{
			ID:           userID,
			LanguageCode: language,
		},
	}, now)
	if err != nil {
		t.Fatalf("issue session cookie: %v", err)
	}
	return cookie
}

func newAuthenticatedDataServerWithTrains(t *testing.T, publicBaseURL string, now time.Time, trains []publicSnapshotTrain) (*Server, *store.SQLiteStore) {
	t.Helper()

	ctx := context.Background()
	loc, err := time.LoadLocation("Europe/Riga")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	dir := t.TempDir()
	secretPath := filepath.Join(dir, "train-session-secret")
	if err := os.WriteFile(secretPath, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	privateKeyPath := filepath.Join(dir, "spacetime-test.key")
	if err := os.WriteFile(privateKeyPath, pemEncodePKCS1PrivateKey(t), 0o600); err != nil {
		t.Fatalf("write spacetime private key: %v", err)
	}
	dbPath := filepath.Join(dir, "train-bot.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	serviceDate := now.In(loc).Format("2006-01-02")
	snapshotPath := filepath.Join(dir, serviceDate+".json")
	payload, err := json.Marshal(publicSnapshot{
		SourceVersion: "server-auth-test",
		Trains:        trains,
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := os.WriteFile(snapshotPath, payload, 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	manager := schedule.NewManager(st, dir, loc, 3)
	if err := manager.LoadToday(ctx, now.In(loc)); err != nil {
		t.Fatalf("load today: %v", err)
	}

	appSvc := trainapp.NewService(
		st,
		manager,
		ride.NewService(st),
		reports.NewService(st, 3*time.Minute, 90*time.Second),
		loc,
		true,
	)
	server, err := NewServer(config.Config{
		BotToken:                           "bot-token",
		TrainWebEnabled:                    true,
		TrainWebBindAddr:                   "127.0.0.1",
		TrainWebPort:                       9317,
		TrainWebPublicBaseURL:              publicBaseURL,
		TrainWebSessionSecretFile:          secretPath,
		TrainWebTelegramAuthMaxAgeSec:      300,
		TrainWebSpacetimeHost:              "https://stdb.example.test",
		TrainWebSpacetimeDatabase:          "train-bot",
		TrainWebSpacetimeOIDCAudience:      "train-bot-web",
		TrainWebSpacetimeJWTPrivateKeyFile: privateKeyPath,
		TrainWebSpacetimeTokenTTLSec:       24 * 60 * 60,
	}, appSvc, i18n.NewCatalog(), loc)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	server.now = func() time.Time { return now }
	return server, st
}

func TestServeHTTPStationSightingSubmissionAcceptsDirectSignedInReports(t *testing.T) {
	t.Parallel()

	server, _, now := newPublicDataServerWithStore(t, "https://example.test/pixel-stack/train")
	req := httptest.NewRequest(http.MethodPost, "/pixel-stack/train/api/v1/stations/riga/sightings", bytes.NewReader([]byte(`{"trainId":"train-next-0"}`)))
	req.AddCookie(testSessionCookie(t, server, 77, "en", now))
	res := httptest.NewRecorder()

	server.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("unexpected station sighting status: got %d body=%s", res.Code, res.Body.String())
	}

	var payload struct {
		Accepted bool `json:"accepted"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode station sighting response: %v", err)
	}
	if !payload.Accepted {
		t.Fatalf("expected accepted station sighting payload, got %+v", payload)
	}
}

func TestServeHTTPCheckInRoutesReturnDeferredNotice(t *testing.T) {
	t.Parallel()

	server, _, now := newPublicDataServerWithStore(t, "https://example.test/pixel-stack/train")
	cookie := testSessionCookie(t, server, 77, "lv", now)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/pixel-stack/train/api/v1/checkins/current"},
		{method: http.MethodPut, path: "/pixel-stack/train/api/v1/checkins/current"},
		{method: http.MethodPost, path: "/pixel-stack/train/api/v1/checkins/current/undo"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.AddCookie(cookie)
		res := httptest.NewRecorder()
		server.ServeHTTP(res, req)
		if res.Code != http.StatusGone {
			t.Fatalf("%s %s unexpected status: got %d body=%s", tc.method, tc.path, res.Code, res.Body.String())
		}
		var payload struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s %s decode deferred payload: %v", tc.method, tc.path, err)
		}
		if payload.Error != "removed" || payload.Message == "" {
			t.Fatalf("%s %s expected retired payload, got %+v", tc.method, tc.path, payload)
		}
	}
}

func TestServeHTTPRouteCheckInLifecycle(t *testing.T) {
	t.Parallel()

	server, _, now := newPublicDataServerWithStore(t, "https://example.test/pixel-stack/train")
	cookie := testSessionCookie(t, server, 77, "lv", now)

	routesReq := httptest.NewRequest(http.MethodGet, "/pixel-stack/train/api/v1/public/route-checkin-routes", nil)
	routesRes := httptest.NewRecorder()
	server.ServeHTTP(routesRes, routesReq)
	if routesRes.Code != http.StatusOK {
		t.Fatalf("unexpected route catalog status: got %d body=%s", routesRes.Code, routesRes.Body.String())
	}
	var routesPayload struct {
		Routes                 []domain.RouteCheckInRoute `json:"routes"`
		DefaultDurationMinutes int                        `json:"defaultDurationMinutes"`
		MinDurationMinutes     int                        `json:"minDurationMinutes"`
		MaxDurationMinutes     int                        `json:"maxDurationMinutes"`
	}
	if err := json.Unmarshal(routesRes.Body.Bytes(), &routesPayload); err != nil {
		t.Fatalf("decode route catalog: %v", err)
	}
	if len(routesPayload.Routes) == 0 || routesPayload.DefaultDurationMinutes != 120 || routesPayload.MinDurationMinutes != 30 || routesPayload.MaxDurationMinutes != 480 {
		t.Fatalf("unexpected route catalog payload: %+v", routesPayload)
	}

	startBody, err := json.Marshal(map[string]any{
		"routeId":         routesPayload.Routes[0].ID,
		"durationMinutes": 240,
	})
	if err != nil {
		t.Fatalf("marshal route checkin body: %v", err)
	}
	startReq := httptest.NewRequest(http.MethodPost, "/pixel-stack/train/api/v1/route-checkins/current", bytes.NewReader(startBody))
	startReq.AddCookie(cookie)
	startRes := httptest.NewRecorder()
	server.ServeHTTP(startRes, startReq)
	if startRes.Code != http.StatusOK {
		t.Fatalf("unexpected route checkin start status: got %d body=%s", startRes.Code, startRes.Body.String())
	}
	var startPayload struct {
		RouteCheckIn     *domain.RouteCheckIn `json:"routeCheckIn"`
		RemainingSeconds int                  `json:"remainingSeconds"`
	}
	if err := json.Unmarshal(startRes.Body.Bytes(), &startPayload); err != nil {
		t.Fatalf("decode route checkin start: %v", err)
	}
	if startPayload.RouteCheckIn == nil || startPayload.RouteCheckIn.RouteID != routesPayload.Routes[0].ID {
		t.Fatalf("unexpected route checkin start payload: %+v", startPayload)
	}
	if startPayload.RemainingSeconds != 240*60 {
		t.Fatalf("expected four hour route watch, got %+v", startPayload)
	}

	currentReq := httptest.NewRequest(http.MethodGet, "/pixel-stack/train/api/v1/route-checkins/current", nil)
	currentReq.AddCookie(cookie)
	currentRes := httptest.NewRecorder()
	server.ServeHTTP(currentRes, currentReq)
	if currentRes.Code != http.StatusOK {
		t.Fatalf("unexpected current route checkin status: got %d body=%s", currentRes.Code, currentRes.Body.String())
	}
	var currentPayload struct {
		RouteCheckIn *domain.RouteCheckIn `json:"routeCheckIn"`
	}
	if err := json.Unmarshal(currentRes.Body.Bytes(), &currentPayload); err != nil {
		t.Fatalf("decode current route checkin: %v", err)
	}
	if currentPayload.RouteCheckIn == nil || currentPayload.RouteCheckIn.RouteID != routesPayload.Routes[0].ID || len(currentPayload.RouteCheckIn.StationNames) == 0 {
		t.Fatalf("unexpected current route checkin payload: %+v", currentPayload)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/pixel-stack/train/api/v1/route-checkins/current", nil)
	deleteReq.AddCookie(cookie)
	deleteRes := httptest.NewRecorder()
	server.ServeHTTP(deleteRes, deleteReq)
	if deleteRes.Code != http.StatusOK {
		t.Fatalf("unexpected route checkin delete status: got %d body=%s", deleteRes.Code, deleteRes.Body.String())
	}
	var deletePayload struct {
		RouteCheckIn *domain.RouteCheckIn `json:"routeCheckIn"`
	}
	if err := json.Unmarshal(deleteRes.Body.Bytes(), &deletePayload); err != nil {
		t.Fatalf("decode route checkin delete: %v", err)
	}
	if deletePayload.RouteCheckIn != nil {
		t.Fatalf("expected route checkin cleared, got %+v", deletePayload)
	}
}

func TestServeHTTPSubscriptionRouteIsNotExposed(t *testing.T) {
	t.Parallel()

	server, _, now := newPublicDataServerWithStore(t, "https://example.test/pixel-stack/train")
	cookie := testSessionCookie(t, server, 77, "lv", now)

	req := httptest.NewRequest(http.MethodPut, "/pixel-stack/train/api/v1/trains/train-next-0/subscription", bytes.NewReader([]byte(`{"enabled":true}`)))
	req.AddCookie(cookie)
	res := httptest.NewRecorder()

	server.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("expected removed subscription route to return 404, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestServeHTTPSettingsAndMeOmitLegacyGlobalStationSightings(t *testing.T) {
	t.Parallel()

	server, _, now := newPublicDataServerWithStore(t, "https://example.test/pixel-stack/train")
	cookie := testSessionCookie(t, server, 88, "en", now)

	patchReq := httptest.NewRequest(http.MethodPatch, "/pixel-stack/train/api/v1/settings", bytes.NewReader([]byte(`{"alertsEnabled":true,"globalStationSightingsEnabled":true,"alertStyle":"DETAILED","language":"lv"}`)))
	patchReq.AddCookie(cookie)
	patchRes := httptest.NewRecorder()

	server.ServeHTTP(patchRes, patchReq)

	if patchRes.Code != http.StatusOK {
		t.Fatalf("unexpected settings patch status: got %d body=%s", patchRes.Code, patchRes.Body.String())
	}

	var settingsPayload map[string]any
	if err := json.Unmarshal(patchRes.Body.Bytes(), &settingsPayload); err != nil {
		t.Fatalf("decode settings patch response: %v", err)
	}
	if settingsPayload["alertsEnabled"] != true {
		t.Fatalf("expected alertsEnabled true in response, got %+v", settingsPayload)
	}
	if settingsPayload["alertStyle"] != "DETAILED" || settingsPayload["language"] != "LV" {
		t.Fatalf("expected settings normalization in response, got %+v", settingsPayload)
	}
	if _, exists := settingsPayload["globalStationSightingsEnabled"]; exists {
		t.Fatalf("expected legacy globalStationSightingsEnabled to be omitted, got %+v", settingsPayload)
	}

	meReq := httptest.NewRequest(http.MethodGet, "/pixel-stack/train/api/v1/me", nil)
	meReq.AddCookie(cookie)
	meRes := httptest.NewRecorder()

	server.ServeHTTP(meRes, meReq)

	if meRes.Code != http.StatusOK {
		t.Fatalf("unexpected /me status: got %d body=%s", meRes.Code, meRes.Body.String())
	}

	var mePayload map[string]any
	if err := json.Unmarshal(meRes.Body.Bytes(), &mePayload); err != nil {
		t.Fatalf("decode /me response: %v", err)
	}
	settings, ok := mePayload["settings"].(map[string]any)
	if !ok {
		t.Fatalf("expected settings map in /me response, got %+v", mePayload)
	}
	if settings["language"] != "LV" {
		t.Fatalf("expected /me settings language LV, got %+v", settings)
	}
	if _, exists := settings["globalStationSightingsEnabled"]; exists {
		t.Fatalf("expected /me settings to omit legacy globalStationSightingsEnabled, got %+v", settings)
	}
	if _, exists := mePayload["favorites"]; exists {
		t.Fatalf("expected /me to omit retired favorites payload, got %+v", mePayload)
	}
	if _, exists := mePayload["currentRide"]; exists {
		t.Fatalf("expected /me to omit retired currentRide payload, got %+v", mePayload)
	}
}

func TestServeHTTPTrainReportAllowsDirectSignedInReportWithoutRide(t *testing.T) {
	t.Parallel()

	server, _, now := newPublicDataServerWithStore(t, "https://example.test/pixel-stack/train")
	req := httptest.NewRequest(http.MethodPost, "/pixel-stack/train/api/v1/trains/train-next-0/reports", bytes.NewReader([]byte(`{"signal":"INSPECTION_STARTED"}`)))
	req.AddCookie(testSessionCookie(t, server, 77, "en", now))
	res := httptest.NewRecorder()

	server.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("unexpected train report status: got %d body=%s", res.Code, res.Body.String())
	}

	var payload struct {
		Accepted          bool    `json:"accepted"`
		Deduped           bool    `json:"deduped"`
		CooldownRemaining float64 `json:"cooldownRemaining"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode train report response: %v", err)
	}
	if !payload.Accepted {
		t.Fatalf("expected accepted train report payload, got %+v", payload)
	}

	duplicateReq := httptest.NewRequest(http.MethodPost, "/pixel-stack/train/api/v1/trains/train-next-0/reports", bytes.NewReader([]byte(`{"signal":"INSPECTION_STARTED"}`)))
	duplicateReq.AddCookie(testSessionCookie(t, server, 77, "en", now))
	duplicateRes := httptest.NewRecorder()
	server.ServeHTTP(duplicateRes, duplicateReq)
	if duplicateRes.Code != http.StatusOK {
		t.Fatalf("unexpected duplicate train report status: got %d body=%s", duplicateRes.Code, duplicateRes.Body.String())
	}
	payload = struct {
		Accepted          bool    `json:"accepted"`
		Deduped           bool    `json:"deduped"`
		CooldownRemaining float64 `json:"cooldownRemaining"`
	}{}
	if err := json.Unmarshal(duplicateRes.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode duplicate train report response: %v", err)
	}
	if payload.Accepted || !payload.Deduped {
		t.Fatalf("expected duplicate report to be deduped without accepting, got %+v", payload)
	}

	cooldownReq := httptest.NewRequest(http.MethodPost, "/pixel-stack/train/api/v1/trains/train-next-0/reports", bytes.NewReader([]byte(`{"signal":"INSPECTION_IN_MY_CAR"}`)))
	cooldownReq.AddCookie(testSessionCookie(t, server, 77, "en", now))
	cooldownRes := httptest.NewRecorder()
	server.ServeHTTP(cooldownRes, cooldownReq)
	if cooldownRes.Code != http.StatusOK {
		t.Fatalf("unexpected cooldown train report status: got %d body=%s", cooldownRes.Code, cooldownRes.Body.String())
	}
	payload = struct {
		Accepted          bool    `json:"accepted"`
		Deduped           bool    `json:"deduped"`
		CooldownRemaining float64 `json:"cooldownRemaining"`
	}{}
	if err := json.Unmarshal(cooldownRes.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode cooldown train report response: %v", err)
	}
	if payload.Accepted || payload.Deduped || payload.CooldownRemaining <= 0 {
		t.Fatalf("expected different signal inside cooldown to be rate limited, got %+v", payload)
	}
}
