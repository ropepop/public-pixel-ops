package phone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

type RelayConfig struct {
	BaseURL           string
	RequestTimeout    time.Duration
	ReconnectMinDelay time.Duration
	ReconnectMaxDelay time.Duration
	NoViewerStopDelay time.Duration
	ReadLimit         int64
}

type Message struct {
	Text   []byte
	Binary []byte
}

type Health struct {
	BaseURL     string `json:"baseUrl"`
	Viewers     int    `json:"viewers"`
	Connected   bool   `json:"connected"`
	Desired     bool   `json:"desired"`
	LastError   string `json:"lastError,omitempty"`
	LastConfig  string `json:"lastConfig,omitempty"`
	LastSeenAt  string `json:"lastSeenAt,omitempty"`
	StreamState string `json:"streamState"`
}

type Relay struct {
	cfg RelayConfig

	mu           sync.Mutex
	writeMu      sync.Mutex
	videoWriteMu sync.Mutex
	viewers      int
	desired      bool
	connected    bool
	lastError    string
	lastConfig   string
	lastSeenAt   time.Time
	conn         *websocket.Conn
	videoConn    *websocket.Conn
	cancelLoop   context.CancelFunc
	idleStop     *time.Timer
	onMessage    func(Message)
	onDisconnect func(error)
}

func NewRelay(cfg RelayConfig) *Relay {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if cfg.ReconnectMinDelay <= 0 {
		cfg.ReconnectMinDelay = 500 * time.Millisecond
	}
	if cfg.ReconnectMaxDelay <= 0 {
		cfg.ReconnectMaxDelay = 5 * time.Second
	}
	if cfg.NoViewerStopDelay <= 0 {
		cfg.NoViewerStopDelay = 10 * time.Second
	}
	if cfg.ReadLimit <= 0 {
		cfg.ReadLimit = 32 << 20
	}
	return &Relay{cfg: cfg}
}

func (r *Relay) SetHandlers(onMessage func(Message), onDisconnect func(error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onMessage = onMessage
	r.onDisconnect = onDisconnect
}

func (r *Relay) AddViewer() {
	r.mu.Lock()
	if r.idleStop != nil {
		r.idleStop.Stop()
		r.idleStop = nil
	}
	r.viewers++
	if !r.desired {
		r.desired = true
		ctx, cancel := context.WithCancel(context.Background())
		r.cancelLoop = cancel
		go r.connectLoop(ctx)
	}
	r.mu.Unlock()
}

func (r *Relay) RemoveViewer() {
	r.mu.Lock()
	if r.viewers > 0 {
		r.viewers--
	}
	if r.viewers == 0 && r.desired {
		if r.idleStop != nil {
			r.idleStop.Stop()
		}
		delay := r.cfg.NoViewerStopDelay
		r.idleStop = time.AfterFunc(delay, r.stopIfStillIdle)
	}
	r.mu.Unlock()
}

func (r *Relay) stopIfStillIdle() {
	r.mu.Lock()
	if r.viewers > 0 || !r.desired {
		r.idleStop = nil
		r.mu.Unlock()
		return
	}
	r.idleStop = nil
	r.desired = false
	if r.cancelLoop != nil {
		r.cancelLoop()
		r.cancelLoop = nil
	}
	conn := r.conn
	videoConn := r.videoConn
	r.conn = nil
	r.videoConn = nil
	r.connected = false
	r.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "no viewers")
	}
	if videoConn != nil {
		_ = videoConn.Close(websocket.StatusNormalClosure, "no viewers")
	}
	r.stopPhoneSession()
}

func (r *Relay) Close() {
	r.mu.Lock()
	if r.idleStop != nil {
		r.idleStop.Stop()
		r.idleStop = nil
	}
	if r.cancelLoop != nil {
		r.cancelLoop()
		r.cancelLoop = nil
	}
	conn := r.conn
	videoConn := r.videoConn
	r.conn = nil
	r.videoConn = nil
	r.connected = false
	r.desired = false
	r.viewers = 0
	r.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "relay closed")
	}
	if videoConn != nil {
		_ = videoConn.Close(websocket.StatusNormalClosure, "relay closed")
	}
	r.stopPhoneSession()
}

func (r *Relay) SendJSON(ctx context.Context, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return r.SendText(ctx, body)
}

func (r *Relay) SendText(ctx context.Context, body []byte) error {
	r.mu.Lock()
	conn := r.conn
	connected := r.connected
	r.mu.Unlock()
	if conn == nil || !connected {
		return fmt.Errorf("phone stream is not connected")
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	return conn.Write(ctx, websocket.MessageText, body)
}

func (r *Relay) Snapshot() Health {
	r.mu.Lock()
	defer r.mu.Unlock()
	lastSeenAt := ""
	if !r.lastSeenAt.IsZero() {
		lastSeenAt = r.lastSeenAt.UTC().Format(time.RFC3339)
	}
	streamState := "idle"
	if r.desired {
		streamState = "connecting"
	}
	if r.connected {
		streamState = "streaming"
	}
	return Health{
		BaseURL:     r.cfg.BaseURL,
		Viewers:     r.viewers,
		Connected:   r.connected,
		Desired:     r.desired,
		LastError:   r.lastError,
		LastConfig:  r.lastConfig,
		LastSeenAt:  lastSeenAt,
		StreamState: streamState,
	}
}

func (r *Relay) connectLoop(ctx context.Context) {
	delay := r.cfg.ReconnectMinDelay
	for {
		if ctx.Err() != nil || !r.shouldRun() {
			return
		}
		err := r.connectOnce(ctx)
		if ctx.Err() != nil || !r.shouldRun() {
			return
		}
		if err != nil {
			r.recordError(err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay *= 2
		if delay > r.cfg.ReconnectMaxDelay {
			delay = r.cfg.ReconnectMaxDelay
		}
	}
}

func (r *Relay) connectOnce(ctx context.Context) (retErr error) {
	controlURL, err := r.websocketURL("/api/v1/session")
	if err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()
	controlConn, _, err := websocket.Dial(dialCtx, controlURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return fmt.Errorf("dial phone control: %w", err)
	}
	videoURL, err := r.websocketURL("/api/v1/stream")
	if err != nil {
		_ = controlConn.Close(websocket.StatusInternalError, "video url failed")
		return err
	}
	videoConn, _, err := websocket.Dial(dialCtx, videoURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		_ = controlConn.Close(websocket.StatusInternalError, "video dial failed")
		return fmt.Errorf("dial phone video: %w", err)
	}
	controlConn.SetReadLimit(r.cfg.ReadLimit)
	videoConn.SetReadLimit(r.cfg.ReadLimit)
	r.mu.Lock()
	if !r.desired {
		r.mu.Unlock()
		_ = controlConn.Close(websocket.StatusNormalClosure, "relay no longer desired")
		_ = videoConn.Close(websocket.StatusNormalClosure, "relay no longer desired")
		return nil
	}
	r.conn = controlConn
	r.videoConn = videoConn
	r.connected = true
	r.lastError = ""
	r.lastSeenAt = time.Now()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.conn == controlConn {
			r.conn = nil
		}
		if r.videoConn == videoConn {
			r.videoConn = nil
		}
		r.connected = false
		onDisconnect := r.onDisconnect
		r.mu.Unlock()
		_ = controlConn.Close(websocket.StatusNormalClosure, "phone relay reconnect")
		_ = videoConn.Close(websocket.StatusNormalClosure, "phone relay reconnect")
		if onDisconnect != nil {
			onDisconnect(retErr)
		}
	}()
	if err := r.SendJSON(ctx, map[string]any{"type": "start"}); err != nil {
		return fmt.Errorf("start phone stream: %w", err)
	}
	if err := r.sendVideoJSON(ctx, map[string]any{"type": "keyframe"}); err != nil {
		return fmt.Errorf("request phone keyframe: %w", err)
	}
	errCh := make(chan error, 2)
	go func() { errCh <- r.readLoop(ctx, controlConn) }()
	go func() { errCh <- r.readLoop(ctx, videoConn) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (r *Relay) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		msgType, data, readErr := conn.Read(ctx)
		if readErr != nil {
			return readErr
		}
		r.mu.Lock()
		r.lastSeenAt = time.Now()
		if msgType == websocket.MessageText && bytes.Contains(data, []byte(`"type":"config"`)) {
			r.lastConfig = string(data)
		}
		handler := r.onMessage
		r.mu.Unlock()
		if handler != nil {
			switch msgType {
			case websocket.MessageText:
				handler(Message{Text: append([]byte(nil), data...)})
			case websocket.MessageBinary:
				handler(Message{Binary: append([]byte(nil), data...)})
			}
		}
	}
}

func (r *Relay) shouldRun() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.desired && r.viewers > 0
}

func (r *Relay) recordError(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	r.lastError = err.Error()
	handler := r.onMessage
	r.mu.Unlock()
	log.Printf("ticket phone relay: %v", err)
	if handler != nil {
		payload, _ := json.Marshal(map[string]any{
			"type":    "phone",
			"state":   "reconnecting",
			"message": err.Error(),
		})
		handler(Message{Text: payload})
	}
}

func (r *Relay) sendVideoJSON(ctx context.Context, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	r.mu.Lock()
	conn := r.videoConn
	connected := r.connected
	r.mu.Unlock()
	if conn == nil || !connected {
		return fmt.Errorf("phone video stream is not connected")
	}
	r.videoWriteMu.Lock()
	defer r.videoWriteMu.Unlock()
	return conn.Write(ctx, websocket.MessageText, body)
}

func (r *Relay) websocketURL(path string) (string, error) {
	base := strings.TrimRight(r.cfg.BaseURL, "/")
	if base == "" {
		return "", fmt.Errorf("phone base URL is empty")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported phone base URL scheme %q", parsed.Scheme)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	parsed.RawQuery = ""
	return parsed.String(), nil
}

func (r *Relay) stopPhoneSession() {
	base := strings.TrimRight(r.cfg.BaseURL, "/")
	if base == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/session/stop", nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.mu.Lock()
		idle := !r.desired && r.viewers == 0
		if idle {
			r.lastError = ""
		}
		r.mu.Unlock()
		if idle {
			log.Printf("ticket phone relay: ignored idle stop error: %v", err)
			return
		}
		r.recordError(fmt.Errorf("stop phone session: %w", err))
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		r.mu.Lock()
		r.lastError = ""
		r.mu.Unlock()
		return
	}
	r.recordError(fmt.Errorf("stop phone session status %d", resp.StatusCode))
}
