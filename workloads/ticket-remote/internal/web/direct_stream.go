package web

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"ticketremote/internal/phone"
)

type directStreamHub struct {
	mu sync.Mutex

	activeVideoClients int
	videoConnections   uint64
	phoneReconnects    uint64

	codec       string
	transport   string
	width       int
	height      int
	rootCapture bool

	framesForwarded    uint64
	keyframesForwarded uint64
	lastConfigAt       time.Time
	lastFrameAt        time.Time
	lastKeyFrameAt     time.Time
	lastVideoClientAt  time.Time

	lastBrowserDecodeError string
	lastBrowserEvent       clientTelemetryEvent
	recentBrowserEvents    []clientTelemetryEvent
}

type clientTelemetryEvent struct {
	Event  string `json:"event"`
	Detail string `json:"detail,omitempty"`
	At     string `json:"at"`
}

func newDirectStreamHub() *directStreamHub {
	return &directStreamHub{}
}

func (h *directStreamHub) addVideoClient() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activeVideoClients++
	h.videoConnections++
	h.lastVideoClientAt = time.Now()
}

func (h *directStreamHub) removeVideoClient() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activeVideoClients > 0 {
		h.activeVideoClients--
	}
}

func (h *directStreamHub) recordPhoneReconnect() {
	h.mu.Lock()
	h.phoneReconnects++
	h.mu.Unlock()
}

func (h *directStreamHub) setConfig(raw []byte) {
	var payload struct {
		Type        string `json:"type"`
		Codec       string `json:"codec"`
		Transport   string `json:"transport"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		RootCapture bool   `json:"rootCapture"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Type != "config" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.codec = payload.Codec
	h.transport = payload.Transport
	h.width = payload.Width
	h.height = payload.Height
	h.rootCapture = payload.RootCapture
	h.lastConfigAt = time.Now()
}

func (h *directStreamHub) recordFrame(frame []byte) {
	if len(frame) == 0 {
		return
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.framesForwarded++
	h.lastFrameAt = now
	if frame[0] == 1 {
		h.keyframesForwarded++
		h.lastKeyFrameAt = now
	}
}

func (h *directStreamHub) recordClientTelemetry(event, detail string) {
	event = trimLogField(event, 96)
	detail = trimLogField(detail, 500)
	if event == "" {
		return
	}
	telemetry := clientTelemetryEvent{
		Event:  event,
		Detail: detail,
		At:     time.Now().UTC().Format(time.RFC3339),
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastBrowserEvent = telemetry
	if strings.Contains(event, "decode") || strings.Contains(event, "decoder") {
		h.lastBrowserDecodeError = detail
	}
	h.recentBrowserEvents = append(h.recentBrowserEvents, telemetry)
	if len(h.recentBrowserEvents) > 12 {
		h.recentBrowserEvents = append([]clientTelemetryEvent(nil), h.recentBrowserEvents[len(h.recentBrowserEvents)-12:]...)
	}
}

func (h *directStreamHub) snapshot(now time.Time, phoneHealth phone.Health) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return map[string]any{
		"path":                     "cloudflare_tunnel_websocket",
		"codec":                    h.codec,
		"transport":                h.transport,
		"width":                    h.width,
		"height":                   h.height,
		"rootCapture":              h.rootCapture,
		"activeVideoClients":       h.activeVideoClients,
		"videoConnections":         h.videoConnections,
		"phoneReconnects":          h.phoneReconnects,
		"framesForwarded":          h.framesForwarded,
		"keyframesForwarded":       h.keyframesForwarded,
		"lastConfigAt":             timeString(h.lastConfigAt),
		"lastConfigAgoMillis":      ageSinceMillis(now, h.lastConfigAt),
		"lastFrameAt":              timeString(h.lastFrameAt),
		"lastFrameAgoMillis":       ageSinceMillis(now, h.lastFrameAt),
		"lastKeyFrameAt":           timeString(h.lastKeyFrameAt),
		"lastKeyFrameAgoMillis":    ageSinceMillis(now, h.lastKeyFrameAt),
		"lastVideoClientAt":        timeString(h.lastVideoClientAt),
		"lastVideoClientAgoMillis": ageSinceMillis(now, h.lastVideoClientAt),
		"phoneConnected":           phoneHealth.Connected,
		"phoneDesired":             phoneHealth.Desired,
		"phoneViewers":             phoneHealth.Viewers,
		"phoneStreamState":         phoneHealth.StreamState,
		"phoneLastError":           phoneHealth.LastError,
		"browserDecodeError":       h.lastBrowserDecodeError,
		"lastBrowserEvent":         h.lastBrowserEvent,
		"recentBrowserEvents":      append([]clientTelemetryEvent(nil), h.recentBrowserEvents...),
	}
}

func ageSinceMillis(now time.Time, at time.Time) int64 {
	if at.IsZero() {
		return -1
	}
	return int64(now.Sub(at) / time.Millisecond)
}

func timeString(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

func trimLogField(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
