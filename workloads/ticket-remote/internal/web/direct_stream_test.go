package web

import (
	"encoding/json"

	"testing"
	"time"

	"ticketremote/internal/phone"
)

func TestWarmEncoderReuseDoesNotPermitExpiredPictureReplay(t *testing.T) {
	hub := newDirectStreamHub()
	hub.streamEpoch = 7
	hub.lastFrameEpoch = 7
	hub.lastConfig = []byte(`{"streamEpoch":7}`)
	hub.lastFrame = []byte("expired-picture")
	hub.lastFrameReceivedAt = time.Now().Add(-23 * time.Minute)
	health := phone.Health{Desired: true, Connected: true, StreamState: "streaming"}
	if !hub.warmEncoderReusable(time.Now(), health) {
		t.Fatal("idle connected encoder should be reused")
	}
	config, frame := hub.warmStart()
	if len(config) == 0 || len(frame) != 0 {
		t.Fatal("reuse must send configuration without replaying the expired picture")
	}
	health.Connected = false
	if hub.warmEncoderReusable(time.Now(), health) {
		t.Fatal("disconnected phone cannot be reused")
	}
	health.Connected = true
	hub.streamEpoch++
	if hub.warmEncoderReusable(time.Now(), health) {
		t.Fatal("previous epoch cannot establish warm reuse")
	}
}

func testCanonicalSourceConfig(raw []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	payload["codec"] = canonicalStreamCodec
	payload["transport"] = canonicalStreamTransport
	payload["captureMode"] = canonicalStreamCaptureMode
	payload["captureSource"] = canonicalStreamCaptureSource
	payload["captureMethod"] = canonicalStreamCaptureMethod
	payload["rootCapture"] = true
	payload["width"] = canonicalStreamWidth
	payload["height"] = canonicalStreamHeight
	payload["sourceWidth"] = canonicalStreamSourceWidth
	payload["sourceHeight"] = canonicalStreamSourceHeight
	payload["sourceLeftCrop"] = canonicalStreamLeftCrop
	payload["sourceTopCrop"] = canonicalStreamTopCrop
	payload["sourceRightCrop"] = canonicalStreamRightCrop
	payload["sourceBottomCrop"] = canonicalStreamBottomCrop
	payload["sourceVisibleWidth"] = canonicalStreamVisibleWidth
	payload["sourceVisibleHeight"] = canonicalStreamVisibleHeight
	payload["bitrate"] = canonicalStreamBitrate
	payload["qualityProfile"] = canonicalStreamQualityProfile
	payload["colorCorrection"] = canonicalStreamColorCorrection
	payload["colorStandard"] = canonicalStreamColorStandard
	strict, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return strict
}

func testAllIntraConfig(raw []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(testCanonicalSourceConfig(raw), &payload); err != nil {
		return nil
	}
	payload["frameDependencyMode"] = frameDependencyModeAllIntra
	payload["fps"] = 1
	payload["sourceFps"] = 1
	payload["keyframeIntervalFrames"] = 1
	strict, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return strict
}

func TestDirectStreamTSF3RewritesEveryStageToWallClock(t *testing.T) {
	hub := newDirectStreamHub()
	hub.addVideoClient()
	if !hub.setConfig(testAllIntraConfig([]byte(`{"type":"config","streamEpoch":7,"phoneUptimeMillis":10000,"frameEnvelope":"tsf3"}`))) {
		t.Fatal("strict TSF3 config should be accepted")
	}
	recordTestBoundedPhoneClock(t, hub, 10_000_000)
	original := testTSF3Frame(7, 1, true, 10_000_000)
	forwarded, ok := hub.recordFrameForBroadcast(original)
	if !ok {
		t.Fatal("fresh calibrated TSF3 frame should be forwarded")
	}
	meta := parseTSF3(forwarded)
	if !meta.ok || meta.version != 3 || meta.headerBytes != tsf3HeaderBytes {
		t.Fatalf("forwarded TSF3 metadata invalid: %#v", meta)
	}
	if meta.captureAttemptID != 101 || meta.codecGeneration != 5 {
		t.Fatalf("TSF3 identity fields changed: %#v", meta)
	}
	stages := []uint64{meta.captureStartMicros, meta.captureCompleteMicros, meta.codecInputMicros, meta.codecOutputMicros, meta.recordEmissionMicros}
	if stages[0] < wallClockMicrosFloor {
		t.Fatalf("TSF3 stages were not rewritten to UTC wall time: %#v", stages)
	}
	for index := 1; index < len(stages); index++ {
		if stages[index]-stages[index-1] != 1_000 {
			t.Fatalf("TSF3 stage spacing changed: %#v", stages)
		}
	}
	if meta.calibrationGeneration == 0 || meta.uncertaintyMicros > uint64(phoneClockUncertaintyMax/time.Microsecond) {
		t.Fatalf("TSF3 calibration evidence invalid: generation=%d uncertainty=%d", meta.calibrationGeneration, meta.uncertaintyMicros)
	}
	if got := forwarded[tsf3HeaderBytes:]; string(got) != string(original[tsf3HeaderBytes:]) {
		t.Fatalf("TSF3 payload changed: got=%x want=%x", got, original[tsf3HeaderBytes:])
	}
}

func TestBoundedPhoneClockMappingUsesNTPInterval(t *testing.T) {
	offset, uncertainty, ok := boundedPhoneClockMapping(phone.ClockProbeResult{
		ServerSendUnixMicros:     1_000_000,
		PhoneReceiveUptimeMicros: 100_000,
		PhoneSendUptimeMicros:    102_000,
		ServerReceiveUnixMicros:  1_010_000,
	})
	if !ok || offset != 904_000 || uncertainty != 4_000 {
		t.Fatalf("bounded mapping = offset %d uncertainty %d ok=%t", offset, uncertainty, ok)
	}
}

func TestDirectStreamTSF3RequiresFreshBoundedClockProbe(t *testing.T) {
	hub := newDirectStreamHub()
	if !hub.setConfig(testAllIntraConfig([]byte(`{"type":"config","streamEpoch":7,"phoneUptimeMillis":10000,"frameEnvelope":"tsf3"}`))) {
		t.Fatal("strict TSF3 config should be accepted")
	}
	frame := testTSF3Frame(7, 1, true, 10_000_000)
	if _, accepted := hub.recordFrameForBroadcast(frame); accepted {
		t.Fatal("one-way config clock sample granted TSF3 freshness authority")
	}
	recordTestBoundedPhoneClock(t, hub, 10_000_000)
	if _, accepted := hub.recordFrameForBroadcast(frame); !accepted {
		t.Fatal("fresh bounded four-timestamp probe did not grant TSF3 authority")
	}
	hub.mu.Lock()
	hub.lastBoundedPhoneClockAt = time.Now().Add(-(phoneClockCalibrationMaxAge + time.Millisecond))
	hub.mu.Unlock()
	if _, accepted := hub.recordFrameForBroadcast(testTSF3Frame(7, 2, true, 10_001_000)); accepted {
		t.Fatal("accepted frames prolonged expired bounded TSF3 clock authority")
	}
}

func TestServerAppliesInitialPhoneClockProbeBeforeFirstTSF3Frame(t *testing.T) {
	hub := newDirectStreamHub()
	if !hub.setConfig(testAllIntraConfig([]byte(`{"type":"config","streamEpoch":7,"phoneUptimeMillis":10000,"frameEnvelope":"tsf3"}`))) {
		t.Fatal("strict TSF3 config should be accepted")
	}
	server := &Server{direct: hub, clients: map[*client]struct{}{}}
	captureUptimeMicros := int64(10_000_000)
	nowMicros := time.Now().UnixMicro()
	server.handlePhoneMessage(phone.Message{ClockProbe: &phone.ClockProbeResult{
		ProbeID:                  "initial-probe",
		ServerSendUnixMicros:     nowMicros - 2_000,
		PhoneReceiveUptimeMicros: captureUptimeMicros + 5_000,
		PhoneSendUptimeMicros:    captureUptimeMicros + 6_000,
		ServerReceiveUnixMicros:  nowMicros,
	}})
	server.handlePhoneMessage(phone.Message{Binary: testTSF3Frame(7, 1, true, uint64(captureUptimeMicros))})

	status := hub.snapshot(time.Now(), phone.Health{})
	if status["framesForwarded"] != uint64(1) || status["lastFrameSequence"] != uint64(1) {
		t.Fatalf("initial bounded probe did not authorize the first TSF3 frame: %#v", status)
	}
}

func TestDirectStreamRejectsUnboundedClockProbeUncertainty(t *testing.T) {
	hub := newDirectStreamHub()
	now := time.Now().UnixMicro()
	if hub.recordBoundedPhoneClock(phone.ClockProbeResult{
		ServerSendUnixMicros:     now - int64(5*time.Second/time.Microsecond),
		PhoneReceiveUptimeMicros: 10_000_000,
		PhoneSendUptimeMicros:    10_001_000,
		ServerReceiveUnixMicros:  now,
	}) {
		t.Fatal("clock probe wider than the uncertainty budget was accepted")
	}
	snapshot := hub.snapshot(time.Now(), phone.Health{})
	if snapshot["phoneClockBoundedCalibrated"] != false || snapshot["phoneClockProbeRejections"] != uint64(1) {
		t.Fatalf("rejected bounded probe diagnostics missing: %#v", snapshot)
	}
}

func TestPhoneHealthCannotRestartPhoneOrRepublishCommandResults(t *testing.T) {
	// A media relay has no command/state writer in this test. Health and retained
	// legacy events must remain harmless even when the phone reports inactivity.
	server := &Server{direct: newDirectStreamHub()}
	for _, message := range []string{
		`{"type":"health","data":{"streamActive":false,"phoneUptimeMillis":10000}}`,
		`{"type":"ticket_state_event","ticketState":"generated_result","requestId":"old-request"}`,
	} {
		if !server.handlePhoneText([]byte(message)) {
			t.Fatalf("private phone state escaped onto the media socket: %s", message)
		}
	}
}

func TestOneWayPhoneClockCanOnlyRevokeBoundedAuthority(t *testing.T) {
	hub := newDirectStreamHub()
	recordTestBoundedPhoneClock(t, hub, 10_000_000)
	probeAt, offset, uncertainty := hub.lastBoundedPhoneClockAt, hub.phoneClockOffsetMicros, hub.phoneClockUncertaintyMicros
	hub.recordPhoneClock(11000, time.Now())
	if !hub.phoneClockBounded || hub.lastBoundedPhoneClockAt != probeAt ||
		hub.phoneClockOffsetMicros != offset || hub.phoneClockUncertaintyMicros != uncertainty {
		t.Fatal("one-way health changed the bounded clock mapping")
	}
	hub.recordPhoneClock(1000, time.Now())
	if hub.phoneClockBounded || !hub.lastBoundedPhoneClockAt.IsZero() {
		t.Fatal("phone restart retained authority from the previous boot")
	}
	hub.recordPhoneClock(2000, time.Now())
	if hub.phoneClockBounded {
		t.Fatal("one-way health restored authority without a new probe")
	}
}

func testTSF3Frame(epoch uint64, sequence uint64, keyFrame bool, captureStartMicros uint64) []byte {
	frame := make([]byte, tsf3HeaderBytes)
	copy(frame[0:4], []byte("TSF3"))
	if keyFrame {
		frame[4] = tsf3FlagKeyframe
	}
	values := []uint64{
		epoch,
		sequence,
		101,
		5,
		captureStartMicros,
		captureStartMicros + 1_000,
		captureStartMicros + 2_000,
		captureStartMicros + 3_000,
		captureStartMicros + 4_000,
		77,
		88,
	}
	for index, value := range values {
		start := 5 + index*8
		putUint64(frame[start:start+8], value)
	}
	return append(frame, 0x65, 0x88)
}

func recordTestBoundedPhoneClock(t *testing.T, hub *directStreamHub, captureUptimeMicros int64) {
	t.Helper()
	nowMicros := time.Now().UnixMicro()
	if !hub.recordBoundedPhoneClock(phone.ClockProbeResult{
		ProbeID:                  "test-probe",
		ServerSendUnixMicros:     nowMicros - 2_000,
		PhoneReceiveUptimeMicros: captureUptimeMicros + 5_000,
		PhoneSendUptimeMicros:    captureUptimeMicros + 6_000,
		ServerReceiveUnixMicros:  nowMicros,
	}) {
		t.Fatal("bounded phone clock fixture was rejected")
	}
}

func putUint64(dst []byte, value uint64) {
	for i := 7; i >= 0; i-- {
		dst[i] = byte(value)
		value >>= 8
	}
}
