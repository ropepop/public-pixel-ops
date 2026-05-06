package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ticketremote/internal/auth"
	"ticketremote/internal/state"
)

const (
	setupPackageVivi       = "com.pv.vivi"
	setupPackageAccrescent = "app.accrescent.client"
	setupPackageAurora     = "com.aurora.store"
	setupPackageController = "lv.jolkins.pixelorchestrator"
	setupTextMaxRunes      = 256
)

type simulatorSetupRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

type commandSimulatorSetupRunner struct {
	adbPath string
	target  string
	timeout time.Duration
}

func (r commandSimulatorSetupRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	timeout := r.timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	adbPath := strings.TrimSpace(r.adbPath)
	if adbPath == "" {
		adbPath = "adb"
	}
	target := strings.TrimSpace(r.target)
	if target == "" {
		return nil, fmt.Errorf("simulator ADB target is empty")
	}
	_, _ = runCommand(runCtx, adbPath, "connect", target)
	if _, err := runCommand(runCtx, adbPath, "-s", target, "wait-for-device"); err != nil {
		return nil, err
	}
	fullArgs := append([]string{"-s", target}, args...)
	return runCommand(runCtx, adbPath, fullArgs...)
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return out, fmt.Errorf("%w: %s", err, detail)
		}
		return out, err
	}
	return out, nil
}

type simulatorSetupStatus struct {
	OK        bool                             `json:"ok"`
	BackendID string                           `json:"backendId"`
	Connected bool                             `json:"connected"`
	Display   simulatorSetupDisplay            `json:"display"`
	Packages  map[string]simulatorSetupPackage `json:"packages"`
	Message   string                           `json:"message,omitempty"`
	Error     string                           `json:"error,omitempty"`
}

type simulatorSetupDisplay struct {
	Width   int `json:"width"`
	Height  int `json:"height"`
	Density int `json:"density,omitempty"`
}

type simulatorSetupPackage struct {
	Package   string `json:"package"`
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
}

type simulatorSetupInputRequest struct {
	Type       string `json:"type"`
	X          int    `json:"x"`
	Y          int    `json:"y"`
	StartX     int    `json:"startX"`
	StartY     int    `json:"startY"`
	EndX       int    `json:"endX"`
	EndY       int    `json:"endY"`
	DurationMS int    `json:"durationMs"`
	Key        string `json:"key"`
	Text       string `json:"text"`
}

func (s *Server) handleAdminPhoneSetupStatus(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, _ state.Snapshot) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	status := s.simulatorSetupStatus(r.Context())
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleAdminPhoneSetupScreenshot(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, _ state.Snapshot) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	png, err := s.runSimulatorSetupADB(r.Context(), "exec-out", "screencap", "-p")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, apiResponse{OK: false, Error: "screenshot_failed", Message: err.Error()})
		return
	}
	writeNoStoreHeaders(w)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	_, _ = w.Write(png)
}

func (s *Server) handleAdminPhoneSetupInput(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, _ state.Snapshot) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req simulatorSetupInputRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{OK: false, Error: "bad_request", Message: err.Error()})
		return
	}
	args, auditPayload, err := s.simulatorSetupInputArgs(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{OK: false, Error: "invalid_input", Message: err.Error()})
		return
	}
	if _, err := s.runSimulatorSetupADB(r.Context(), args...); err != nil {
		writeJSON(w, http.StatusBadGateway, apiResponse{OK: false, Error: "input_failed", Message: err.Error()})
		return
	}
	_ = s.store.Audit(r.Context(), s.cfg.TicketID, id.Email, "simulator_setup_input", auditPayload, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminPhoneSetupOpen(w http.ResponseWriter, r *http.Request, id auth.Identity, sessionID string, _ state.Snapshot) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2048)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{OK: false, Error: "bad_request", Message: err.Error()})
		return
	}
	target := strings.TrimSpace(req.Target)
	args, err := simulatorSetupOpenArgs(target)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{OK: false, Error: "invalid_target", Message: err.Error()})
		return
	}
	if _, err := s.runSimulatorSetupADB(r.Context(), args...); err != nil {
		writeJSON(w, http.StatusBadGateway, apiResponse{OK: false, Error: "open_failed", Message: err.Error()})
		return
	}
	_ = s.store.Audit(r.Context(), s.cfg.TicketID, id.Email, "simulator_setup_open", map[string]any{"target": target}, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) runSimulatorSetupADB(ctx context.Context, args ...string) ([]byte, error) {
	if s.setupRunner == nil {
		return nil, fmt.Errorf("simulator setup runner is not configured")
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	return s.setupRunner.Run(ctx, args...)
}

func (s *Server) simulatorSetupStatus(ctx context.Context) simulatorSetupStatus {
	status := simulatorSetupStatus{
		OK:        true,
		BackendID: s.cfg.SimulatorSetup.BackendID,
		Packages:  map[string]simulatorSetupPackage{},
		Message:   "Owner simulator control is available before ViVi is ready.",
	}
	if _, err := s.runSimulatorSetupADB(ctx, "get-state"); err != nil {
		status.Connected = false
		status.Error = err.Error()
		return status
	}
	status.Connected = true
	status.Display = s.simulatorSetupDisplay(ctx)
	for label, pkg := range map[string]string{
		"vivi":       setupPackageVivi,
		"accrescent": setupPackageAccrescent,
		"aurora":     setupPackageAurora,
		"controller": setupPackageController,
	} {
		status.Packages[label] = s.simulatorPackageStatus(ctx, pkg)
	}
	if pkg := status.Packages["vivi"]; !pkg.Installed {
		status.Message = "ViVi is not installed yet; owner simulator controls are enabled."
	}
	return status
}

func (s *Server) simulatorPackageStatus(ctx context.Context, pkg string) simulatorSetupPackage {
	out, err := s.runSimulatorSetupADB(ctx, "shell", "pm", "path", pkg)
	if err != nil {
		return simulatorSetupPackage{Package: pkg, Installed: false}
	}
	return simulatorSetupPackage{
		Package:   pkg,
		Installed: true,
		Path:      strings.TrimSpace(string(out)),
	}
}

func (s *Server) simulatorSetupDisplay(ctx context.Context) simulatorSetupDisplay {
	display := simulatorSetupDisplay{Width: 720, Height: 1280}
	if out, err := s.runSimulatorSetupADB(ctx, "shell", "wm", "size"); err == nil {
		if width, height, ok := parseWmSize(string(out)); ok {
			display.Width = width
			display.Height = height
		}
	}
	if out, err := s.runSimulatorSetupADB(ctx, "shell", "wm", "density"); err == nil {
		if density, ok := parseWmDensity(string(out)); ok {
			display.Density = density
		}
	}
	return display
}

func (s *Server) simulatorSetupInputArgs(ctx context.Context, req simulatorSetupInputRequest) ([]string, map[string]any, error) {
	display := s.simulatorSetupDisplay(ctx)
	inBounds := func(x, y int) bool {
		return x >= 0 && y >= 0 && x < display.Width && y < display.Height
	}
	inputType := strings.ToLower(strings.TrimSpace(req.Type))
	switch inputType {
	case "tap":
		if !inBounds(req.X, req.Y) {
			return nil, nil, fmt.Errorf("tap coordinates are outside the simulator screen")
		}
		return []string{"shell", "input", "tap", strconv.Itoa(req.X), strconv.Itoa(req.Y)}, map[string]any{"type": "tap", "x": req.X, "y": req.Y}, nil
	case "swipe", "drag":
		if !inBounds(req.StartX, req.StartY) || !inBounds(req.EndX, req.EndY) {
			return nil, nil, fmt.Errorf("swipe coordinates are outside the simulator screen")
		}
		duration := req.DurationMS
		if duration == 0 {
			duration = 300
		}
		if duration < 50 || duration > 1000 {
			return nil, nil, fmt.Errorf("swipe duration must be between 50 and 1000 ms")
		}
		dx := req.EndX - req.StartX
		if dx < 0 {
			dx = -dx
		}
		dy := req.EndY - req.StartY
		if dy < 0 {
			dy = -dy
		}
		if dx > 900 || dy > 900 {
			return nil, nil, fmt.Errorf("swipe is too long for simulator control")
		}
		return []string{"shell", "input", "swipe", strconv.Itoa(req.StartX), strconv.Itoa(req.StartY), strconv.Itoa(req.EndX), strconv.Itoa(req.EndY), strconv.Itoa(duration)}, map[string]any{"type": inputType, "startX": req.StartX, "startY": req.StartY, "endX": req.EndX, "endY": req.EndY, "durationMs": duration}, nil
	case "long_press":
		if !inBounds(req.X, req.Y) {
			return nil, nil, fmt.Errorf("long-press coordinates are outside the simulator screen")
		}
		duration := req.DurationMS
		if duration == 0 {
			duration = 650
		}
		if duration < 400 || duration > 1500 {
			return nil, nil, fmt.Errorf("long-press duration must be between 400 and 1500 ms")
		}
		return []string{"shell", "input", "swipe", strconv.Itoa(req.X), strconv.Itoa(req.Y), strconv.Itoa(req.X), strconv.Itoa(req.Y), strconv.Itoa(duration)}, map[string]any{"type": "long_press", "x": req.X, "y": req.Y, "durationMs": duration}, nil
	case "key":
		key, err := simulatorSetupKey(req.Key)
		if err != nil {
			return nil, nil, err
		}
		return []string{"shell", "input", "keyevent", key}, map[string]any{"type": "key", "key": strings.ToLower(strings.TrimSpace(req.Key))}, nil
	case "text":
		textArg, err := simulatorSetupTextArg(req.Text)
		if err != nil {
			return nil, nil, err
		}
		return []string{"shell", "input", "text", textArg}, map[string]any{"type": "text", "length": len([]rune(req.Text))}, nil
	default:
		return nil, nil, fmt.Errorf("unsupported simulator control input type")
	}
}

func simulatorSetupKey(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "back":
		return "KEYCODE_BACK", nil
	case "home":
		return "KEYCODE_HOME", nil
	case "enter":
		return "KEYCODE_ENTER", nil
	case "app_switch", "app-switch", "overview", "recents":
		return "KEYCODE_APP_SWITCH", nil
	case "wake", "wake_up", "wakeup":
		return "KEYCODE_WAKEUP", nil
	case "delete", "backspace", "del":
		return "KEYCODE_DEL", nil
	case "space":
		return "KEYCODE_SPACE", nil
	case "tab":
		return "KEYCODE_TAB", nil
	case "escape", "esc":
		return "KEYCODE_ESCAPE", nil
	default:
		return "", fmt.Errorf("unsupported simulator control key")
	}
}

func simulatorSetupTextArg(text string) (string, error) {
	runes := []rune(text)
	if len(runes) == 0 || len(runes) > setupTextMaxRunes {
		return "", fmt.Errorf("text must be 1-%d printable ASCII characters", setupTextMaxRunes)
	}
	for _, r := range runes {
		if r < 0x20 || r > 0x7e {
			return "", fmt.Errorf("text contains unsupported characters")
		}
		switch r {
		case '%', '\'', '"', '\\', ';', '&', '|', '<', '>', '`', '$':
			return "", fmt.Errorf("text contains unsupported characters")
		}
	}
	return strings.ReplaceAll(text, " ", "%s"), nil
}

func simulatorSetupOpenArgs(target string) ([]string, error) {
	switch strings.TrimSpace(target) {
	case "home":
		return []string{"shell", "input", "keyevent", "KEYCODE_HOME"}, nil
	case "accrescent":
		return []string{"shell", "monkey", "-p", setupPackageAccrescent, "1"}, nil
	case "aurora-vivi":
		return []string{"shell", "am", "start", "-a", "android.intent.action.VIEW", "-d", "market://details?id=com.pv.vivi", "-p", setupPackageAurora}, nil
	case "controller":
		return []string{"shell", "am", "start", "-n", setupPackageController + "/.app.MainActivity"}, nil
	default:
		return nil, fmt.Errorf("unsupported simulator control target")
	}
}

func parseWmSize(value string) (int, int, bool) {
	re := regexp.MustCompile(`(?m)(?:Physical|Override) size:\s*([0-9]+)x([0-9]+)`)
	matches := re.FindAllStringSubmatch(value, -1)
	if len(matches) == 0 {
		return 0, 0, false
	}
	last := matches[len(matches)-1]
	width, widthErr := strconv.Atoi(last[1])
	height, heightErr := strconv.Atoi(last[2])
	return width, height, widthErr == nil && heightErr == nil && width > 0 && height > 0
}

func parseWmDensity(value string) (int, bool) {
	re := regexp.MustCompile(`(?m)(?:Physical|Override) density:\s*([0-9]+)`)
	matches := re.FindAllStringSubmatch(value, -1)
	if len(matches) == 0 {
		return 0, false
	}
	density, err := strconv.Atoi(matches[len(matches)-1][1])
	return density, err == nil && density > 0
}
