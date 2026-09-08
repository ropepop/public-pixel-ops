package phone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const (
	DefaultRequestTimeout     = 10 * time.Second
	DefaultReconnectMinDelay  = 500 * time.Millisecond
	DefaultReconnectMaxDelay  = 5 * time.Second
	DefaultNoViewerStopDelay  = 10 * time.Second
	DefaultLivenessIdle       = 4 * time.Second
	DefaultLivenessTimeout    = 3 * time.Second
	DefaultReconnectReset     = 10 * time.Second
	DefaultClockProbeInterval = 2 * time.Second
	ClockProbeIDMaxBytes      = 64
	CaptureDemandTTL          = 2500 * time.Millisecond
	maxProtocolGeneration     = uint64(9_007_199_254_740_991)

	// MaxVideoPayloadBytes bounds encoded H.264 only. MaxVideoMessageBytes adds
	// the largest supported frame envelope so the relay can enforce a total
	// WebSocket message bound before the envelope is parsed.
	MaxVideoPayloadBytes int64 = 2 * 1024 * 1024
	MaxVideoMessageBytes int64 = MaxVideoPayloadBytes + 93
)

type RelayConfig struct {
	BackendID          string
	AttachName         string
	BaseURL            string
	RequestTimeout     time.Duration
	ReconnectMinDelay  time.Duration
	ReconnectMaxDelay  time.Duration
	NoViewerStopDelay  time.Duration
	LivenessIdle       time.Duration
	LivenessTimeout    time.Duration
	ReconnectReset     time.Duration
	ClockProbeInterval time.Duration
}

type Message struct {
	Text                 []byte
	Binary               []byte
	ClockProbe           *ClockProbeResult
	ConnectionGeneration uint64
}

// ClockProbeResult is a validated four-timestamp exchange. ServerSendUnixMicros
// is the relay's t0, the two phone fields use Android monotonic time, and
// ServerReceiveUnixMicros is the relay's t3.
type ClockProbeResult struct {
	ProbeID                  string
	ServerSendUnixMicros     int64
	PhoneReceiveUptimeMicros int64
	PhoneSendUptimeMicros    int64
	ServerReceiveUnixMicros  int64
}

// CaptureDemandReceipt identifies one successfully written, connection-scoped
// ordinary capture opportunity. Proof, keyframe, startup, and prewarm capture
// paths do not use this protocol.
type CaptureDemandReceipt struct {
	StreamEpoch          uint64
	Generation           uint64
	ConnectionGeneration uint64
	SentAt               time.Time
	ExpiresAt            time.Time
}

type outstandingClockProbe struct {
	conn                 *websocket.Conn
	serverSendUnixMicros int64
}

type Health struct {
	BackendID   string `json:"backendId"`
	AttachName  string `json:"attachName"`
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

	mu                        sync.Mutex
	videoWriteMu              sync.Mutex
	viewers                   int
	desired                   bool
	connected                 bool
	lastError                 string
	lastConfig                string
	lastSeenAt                time.Time
	videoConn                 *websocket.Conn
	dialAttemptGeneration     uint64
	dialWebsocket             func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	reconnectJitter           func(time.Duration) time.Duration
	clockProbeCounter         uint64
	outstandingClockProbes    map[string]outstandingClockProbe
	videoConnectionGeneration uint64
	captureDemandGeneration   uint64
	cancelLoop                context.CancelFunc
	loopContext               context.Context
	idleStop                  *time.Timer
	onMessage                 func(Message)
	onDisconnect              func(error)
}

func NewRelay(cfg RelayConfig) *Relay {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	if cfg.ReconnectMinDelay <= 0 {
		cfg.ReconnectMinDelay = DefaultReconnectMinDelay
	}
	if cfg.ReconnectMaxDelay <= 0 {
		cfg.ReconnectMaxDelay = DefaultReconnectMaxDelay
	}
	if cfg.NoViewerStopDelay < 0 {
		cfg.NoViewerStopDelay = DefaultNoViewerStopDelay
	}
	if cfg.LivenessIdle <= 0 {
		cfg.LivenessIdle = DefaultLivenessIdle
	}
	if cfg.LivenessTimeout <= 0 {
		cfg.LivenessTimeout = DefaultLivenessTimeout
	}
	if cfg.ReconnectReset <= 0 {
		cfg.ReconnectReset = DefaultReconnectReset
	}
	if cfg.ClockProbeInterval <= 0 {
		cfg.ClockProbeInterval = DefaultClockProbeInterval
	}
	return &Relay{
		cfg:                    cfg,
		dialWebsocket:          websocket.Dial,
		reconnectJitter:        jitterReconnectDelay,
		outstandingClockProbes: make(map[string]outstandingClockProbe),
	}
}

type Backend struct {
	ID         string
	AttachName string
	BaseURL    string
}

func (r *Relay) SetHandlers(onMessage func(Message), onDisconnect func(error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onMessage = onMessage
	r.onDisconnect = onDisconnect
}

func (r *Relay) AddViewer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelIdleStopLocked()
	r.viewers++
	r.desired = true
	if r.cancelLoop == nil {
		go r.connectLoop(r.newConnectLoopLocked())
	}
}

func (r *Relay) RemoveViewer() {
	r.mu.Lock()
	if r.viewers > 0 {
		r.viewers--
	}
	if r.viewers == 0 && r.desired {
		if r.idleStop != nil {
			r.idleStop.Stop()
			r.idleStop = nil
		}
		if r.cfg.NoViewerStopDelay == 0 {
			go r.stopIfStillIdle()
		} else {
			r.idleStop = time.AfterFunc(r.cfg.NoViewerStopDelay, r.stopIfStillIdle)
		}
	}
	r.mu.Unlock()
}

func (r *Relay) stopIfStillIdle() {
	r.mu.Lock()
	r.idleStop = nil
	if r.viewers > 0 || !r.desired {
		r.mu.Unlock()
		return
	}
	r.desired = false
	conn := r.detachConnectionLocked()
	r.mu.Unlock()
	closeRetiredConnection(conn)
}

// Detach and cancel the old owner before a replacement can be admitted.
// Retiring that socket afterwards cannot change the replacement's state.
func (r *Relay) detachConnectionLocked() *websocket.Conn {
	if r.cancelLoop != nil {
		r.cancelLoop()
	}
	r.cancelLoop = nil
	r.loopContext = nil
	conn := r.videoConn
	r.videoConn = nil
	r.connected = false
	return conn
}

func (r *Relay) cancelIdleStopLocked() {
	if r.idleStop != nil {
		r.idleStop.Stop()
		r.idleStop = nil
	}
}

func closeRetiredConnection(conn *websocket.Conn) {
	if conn != nil {
		_ = conn.CloseNow()
	}
}

func (r *Relay) Close() {
	r.mu.Lock()
	r.cancelIdleStopLocked()
	r.desired = false
	r.viewers = 0
	conn := r.detachConnectionLocked()
	r.mu.Unlock()
	closeRetiredConnection(conn)
}

func (r *Relay) SwitchBackend(backend Backend) {
	cleanBaseURL := strings.TrimRight(strings.TrimSpace(backend.BaseURL), "/")
	r.mu.Lock()
	same := r.cfg.BackendID == strings.TrimSpace(backend.ID) && r.cfg.BaseURL == cleanBaseURL
	r.cfg.AttachName = strings.TrimSpace(backend.AttachName)
	if same {
		r.mu.Unlock()
		return
	}
	r.cancelIdleStopLocked()
	conn := r.detachConnectionLocked()
	r.lastError = ""
	r.lastConfig = ""
	r.lastSeenAt = time.Time{}
	r.cfg.BackendID = strings.TrimSpace(backend.ID)
	r.cfg.BaseURL = cleanBaseURL
	if r.desired && r.viewers > 0 {
		go r.connectLoop(r.newConnectLoopLocked())
	}
	r.mu.Unlock()
	closeRetiredConnection(conn)
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
		BackendID:   r.cfg.BackendID,
		AttachName:  r.cfg.AttachName,
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

// newConnectLoopLocked assigns one owner before a loop can start or finish.
func (r *Relay) newConnectLoopLocked() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	r.loopContext = ctx
	r.cancelLoop = cancel
	return ctx
}

func (r *Relay) finishConnectLoop(ctx context.Context) {
	r.mu.Lock()
	if r.loopContext != ctx {
		r.mu.Unlock()
		return
	}
	if r.cancelLoop != nil {
		r.cancelLoop()
	}
	r.cancelLoop = nil
	r.loopContext = nil
	// A viewer may arrive after the loop decides to exit but before this cleanup.
	// Transfer ownership here so that viewer cannot be left without a retry loop.
	var next context.Context
	if r.desired && r.viewers > 0 {
		next = r.newConnectLoopLocked()
	}
	r.mu.Unlock()
	if next != nil {
		go r.connectLoop(next)
	}
}

func (r *Relay) connectLoop(ctx context.Context) {
	defer r.finishConnectLoop(ctx)
	delay := r.cfg.ReconnectMinDelay
	for {
		if ctx.Err() != nil || !r.shouldRun() {
			return
		}
		connectedFor, err := r.connectOnceMeasured(ctx)
		if ctx.Err() != nil || !r.shouldRun() {
			return
		}
		if err != nil {
			r.recordError(err)
		}
		if connectedFor >= r.cfg.ReconnectReset {
			delay = r.cfg.ReconnectMinDelay
		}
		wait := delay
		if r.reconnectJitter != nil {
			wait = r.reconnectJitter(delay)
		}
		timer := time.NewTimer(wait)
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

func jitterReconnectDelay(delay time.Duration) time.Duration {
	if delay <= 1 {
		return delay
	}
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(delay-half)+1))
}

func (r *Relay) connectOnceMeasured(ctx context.Context) (connectedFor time.Duration, retErr error) {
	videoURL, err := r.websocketURL("/api/v1/stream")
	if err != nil {
		return 0, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()
	r.mu.Lock()
	r.dialAttemptGeneration++
	dialAttemptGeneration := r.dialAttemptGeneration
	r.mu.Unlock()
	dialWebsocket := r.dialWebsocket
	if dialWebsocket == nil {
		dialWebsocket = websocket.Dial
	}
	videoConn, _, err := dialWebsocket(dialCtx, videoURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return 0, fmt.Errorf("dial phone video: %w", err)
	}
	videoConn.SetReadLimit(MaxVideoMessageBytes)
	r.mu.Lock()
	if !r.desired || r.dialAttemptGeneration != dialAttemptGeneration || ctx.Err() != nil {
		r.mu.Unlock()
		_ = videoConn.CloseNow()
		return 0, nil
	}
	r.videoConn = videoConn
	r.connected = true
	r.lastError = ""
	r.lastSeenAt = time.Now()
	r.outstandingClockProbes = make(map[string]outstandingClockProbe)
	r.videoConnectionGeneration++
	if r.videoConnectionGeneration == 0 || r.videoConnectionGeneration > maxProtocolGeneration {
		r.videoConnectionGeneration = 1
	}
	r.captureDemandGeneration = 0
	r.mu.Unlock()
	connectedAt := time.Now()
	defer func() {
		connectedFor = time.Since(connectedAt)
		r.mu.Lock()
		wasCurrent := r.videoConn == videoConn
		if r.videoConn == videoConn {
			r.videoConn = nil
		}
		if wasCurrent {
			r.connected = false
		}
		for probeID, probe := range r.outstandingClockProbes {
			if probe.conn == videoConn {
				delete(r.outstandingClockProbes, probeID)
			}
		}
		var onDisconnect func(error)
		if wasCurrent {
			onDisconnect = r.onDisconnect
		}
		r.mu.Unlock()
		// The connection has already ended or been superseded. Do not wait for a
		// close handshake before publishing disconnect and starting recovery.
		_ = videoConn.CloseNow()
		if onDisconnect != nil {
			onDisconnect(retErr)
		}
	}()
	connectionCtx, cancelConnection := context.WithCancel(ctx)
	defer cancelConnection()
	activity := make(chan struct{}, 1)
	errCh := make(chan error, 2)
	go func() { errCh <- r.readLoop(connectionCtx, videoConn, activity) }()
	go func() { errCh <- r.livenessLoop(connectionCtx, videoConn, activity) }()
	select {
	case <-ctx.Done():
		return connectedFor, ctx.Err()
	case err := <-errCh:
		return connectedFor, err
	}
}

func (r *Relay) readLoop(ctx context.Context, conn *websocket.Conn, activity chan<- struct{}) error {
	for {
		msgType, reader, readErr := conn.Reader(ctx)
		if readErr != nil {
			return readErr
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, MaxVideoMessageBytes+1))
		if readErr != nil {
			return readErr
		}
		if int64(len(data)) > MaxVideoMessageBytes {
			return fmt.Errorf("phone media message exceeds %d bytes", MaxVideoMessageBytes)
		}
		r.mu.Lock()
		if r.videoConn != conn {
			r.mu.Unlock()
			return nil
		}
		r.lastSeenAt = time.Now()
		if msgType == websocket.MessageText && bytes.Contains(data, []byte(`"type":"config"`)) {
			r.lastConfig = string(data)
		}
		handler := r.onMessage
		connectionGeneration := r.videoConnectionGeneration
		r.mu.Unlock()
		select {
		case activity <- struct{}{}:
		default:
		}
		message := Message{
			ConnectionGeneration: connectionGeneration,
		}
		switch msgType {
		case websocket.MessageText:
			probe, recognized := r.consumeClockProbeResult(conn, data, time.Now())
			if recognized {
				if probe == nil {
					continue
				}
				message.ClockProbe = probe
			} else {
				message.Text = data
			}
		case websocket.MessageBinary:
			message.Binary = data
		default:
			continue
		}
		if handler != nil {
			handler(message)
		}
	}
}

func (r *Relay) livenessLoop(ctx context.Context, conn *websocket.Conn, activity <-chan struct{}) error {
	// TSF3 frames need a bounded clock mapping. Probe as soon as the connection
	// is current so the first 1 FPS frame is not deterministically discarded.
	if err := r.sendClockProbe(ctx, conn); err != nil {
		return fmt.Errorf("initial phone clock probe: %w", err)
	}
	timer := time.NewTimer(r.cfg.LivenessIdle)
	defer timer.Stop()
	probeTicker := time.NewTicker(r.cfg.ClockProbeInterval)
	defer probeTicker.Stop()
	reset := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(r.cfg.LivenessIdle)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-activity:
			reset()
		case <-probeTicker.C:
			if err := r.sendClockProbe(ctx, conn); err != nil {
				return fmt.Errorf("phone clock probe: %w", err)
			}
		case <-timer.C:
			pingCtx, cancel := context.WithTimeout(ctx, r.cfg.LivenessTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return fmt.Errorf("phone media liveness: %w", err)
			}
			timer.Reset(r.cfg.LivenessIdle)
		}
	}
}

func (r *Relay) sendClockProbe(ctx context.Context, conn *websocket.Conn) error {
	writeCtx, cancel := context.WithTimeout(ctx, r.cfg.LivenessTimeout)
	defer cancel()
	r.videoWriteMu.Lock()
	defer r.videoWriteMu.Unlock()
	// Sample t0 after waiting for the only application-data writer and as close
	// as practical to the actual WebSocket write.
	serverSendUnixMicros := time.Now().UnixMicro()
	r.mu.Lock()
	if serverSendUnixMicros <= 0 || r.videoConn != conn {
		r.mu.Unlock()
		return context.Canceled
	}
	r.clockProbeCounter++
	probeID := fmt.Sprintf("p-%016x", r.clockProbeCounter)
	if len(r.outstandingClockProbes) >= 4 {
		for existingID, existing := range r.outstandingClockProbes {
			if existing.conn == conn {
				delete(r.outstandingClockProbes, existingID)
			}
		}
	}
	r.outstandingClockProbes[probeID] = outstandingClockProbe{
		conn: conn, serverSendUnixMicros: serverSendUnixMicros,
	}
	r.mu.Unlock()

	payload, err := json.Marshal(map[string]any{
		"type":                 "clock_probe",
		"probeId":              probeID,
		"serverSendUnixMicros": serverSendUnixMicros,
	})
	if err != nil {
		r.mu.Lock()
		delete(r.outstandingClockProbes, probeID)
		r.mu.Unlock()
		return err
	}
	err = conn.Write(writeCtx, websocket.MessageText, payload)
	if err != nil {
		r.mu.Lock()
		delete(r.outstandingClockProbes, probeID)
		r.mu.Unlock()
	}
	return err
}

// SendCaptureDemand writes one strict, additive ordinary-capture opportunity
// to the current private media socket. Unknown text remains harmless to older
// Pixel releases, which continue their legacy periodic capture behavior.
func (r *Relay) SendCaptureDemand(ctx context.Context, streamEpoch uint64) (CaptureDemandReceipt, error) {
	if streamEpoch == 0 || streamEpoch > maxProtocolGeneration {
		return CaptureDemandReceipt{}, fmt.Errorf("capture demand stream epoch is invalid")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.videoWriteMu.Lock()
	defer r.videoWriteMu.Unlock()

	r.mu.Lock()
	conn := r.videoConn
	connectionGeneration := r.videoConnectionGeneration
	if conn == nil || !r.connected || !r.desired || connectionGeneration == 0 {
		r.mu.Unlock()
		return CaptureDemandReceipt{}, fmt.Errorf("phone media socket is not current")
	}
	if r.captureDemandGeneration >= maxProtocolGeneration {
		r.mu.Unlock()
		return CaptureDemandReceipt{}, fmt.Errorf("capture demand generation exhausted")
	}
	r.captureDemandGeneration++
	generation := r.captureDemandGeneration
	writeTimeout := r.cfg.LivenessTimeout
	r.mu.Unlock()

	payload, err := json.Marshal(struct {
		Type        string `json:"type"`
		Version     int    `json:"version"`
		StreamEpoch uint64 `json:"streamEpoch"`
		Generation  uint64 `json:"generation"`
		TTLMillis   int64  `json:"ttlMillis"`
	}{
		Type: "capture_demand", Version: 1, StreamEpoch: streamEpoch,
		Generation: generation, TTLMillis: CaptureDemandTTL.Milliseconds(),
	})
	if err != nil {
		return CaptureDemandReceipt{}, err
	}
	if writeTimeout <= 0 || writeTimeout > CaptureDemandTTL {
		writeTimeout = CaptureDemandTTL
	}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	sentAt := time.Now()
	err = conn.Write(writeCtx, websocket.MessageText, payload)
	cancel()
	if err != nil {
		return CaptureDemandReceipt{}, err
	}
	r.mu.Lock()
	stillCurrent := r.videoConn == conn && r.connected && r.desired && r.videoConnectionGeneration == connectionGeneration
	r.mu.Unlock()
	if !stillCurrent {
		return CaptureDemandReceipt{}, fmt.Errorf("phone media socket changed during capture demand")
	}
	return CaptureDemandReceipt{
		StreamEpoch: streamEpoch, Generation: generation, ConnectionGeneration: connectionGeneration,
		SentAt: sentAt, ExpiresAt: sentAt.Add(CaptureDemandTTL),
	}, nil
}

func (r *Relay) consumeClockProbeResult(conn *websocket.Conn, data []byte, receivedAt time.Time) (*ClockProbeResult, bool) {
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &kind); err != nil || kind.Type != "clock_probe_result" {
		return nil, false
	}
	var payload struct {
		Type                     string `json:"type"`
		ProbeID                  string `json:"probeId"`
		ServerSendUnixMicros     int64  `json:"serverSendUnixMicros"`
		PhoneReceiveUptimeMicros int64  `json:"phoneReceiveUptimeMicros"`
		PhoneSendUptimeMicros    int64  `json:"phoneSendUptimeMicros"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, true
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, true
	}
	if payload.Type != "clock_probe_result" || !validClockProbeID(payload.ProbeID) ||
		payload.ServerSendUnixMicros <= 0 || payload.PhoneReceiveUptimeMicros <= 0 ||
		payload.PhoneSendUptimeMicros < payload.PhoneReceiveUptimeMicros || receivedAt.UnixMicro() <= 0 {
		return nil, true
	}
	r.mu.Lock()
	outstanding, ok := r.outstandingClockProbes[payload.ProbeID]
	if ok && outstanding.conn == conn {
		delete(r.outstandingClockProbes, payload.ProbeID)
	}
	r.mu.Unlock()
	if !ok || outstanding.conn != conn || outstanding.serverSendUnixMicros != payload.ServerSendUnixMicros {
		return nil, true
	}
	return &ClockProbeResult{
		ProbeID:                  payload.ProbeID,
		ServerSendUnixMicros:     payload.ServerSendUnixMicros,
		PhoneReceiveUptimeMicros: payload.PhoneReceiveUptimeMicros,
		PhoneSendUptimeMicros:    payload.PhoneSendUptimeMicros,
		ServerReceiveUnixMicros:  receivedAt.UnixMicro(),
	}, true
}

func validClockProbeID(value string) bool {
	if value == "" || len(value) > ClockProbeIDMaxBytes {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._:-", char) {
			continue
		}
		return false
	}
	return true
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
	if handler != nil {
		payload, _ := json.Marshal(map[string]any{
			"type":    "phone",
			"state":   "reconnecting",
			"message": err.Error(),
		})
		handler(Message{Text: payload})
	}
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
