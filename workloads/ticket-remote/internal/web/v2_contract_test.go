package web

import (
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"
)

func TestV2RejectsOldFramesAndKeepsTheExactFreshnessBoundary(t *testing.T) {
	old := make([]byte, 30)
	binary.BigEndian.PutUint32(old, 0x54534632)
	if parseTSF3(old).ok {
		t.Fatal("retired frame protocol accepted")
	}
	if _, fresh := frameVisualAge(tsf3Metadata{version: 3, timestamp: 10000}, time.Now()); fresh {
		t.Fatal("unmapped phone timestamp granted freshness")
	}

	now := time.Now().Truncate(time.Microsecond)
	for _, item := range []struct {
		age   time.Duration
		fresh bool
	}{{3000 * time.Millisecond, true}, {3001 * time.Millisecond, false}} {
		_, fresh := frameVisualAge(tsf3Metadata{version: 3, timestamp: uint64(now.Add(-item.age).UnixMicro())}, now)
		if fresh != item.fresh {
			t.Fatalf("age %v fresh=%v", item.age, fresh)
		}
	}
}

func TestV2PresentationRequiresWrittenEvidence(t *testing.T) {
	for _, item := range []struct {
		name                                   string
		version                                int
		epoch, generation, received, presented uint64
		valid                                  bool
	}{
		{"current", 2, 7, 4, 11, 11, true},
		{"old_feedback", 1, 7, 4, 11, 11, false},
		{"old_config", 2, 7, 3, 11, 11, false},
		{"other_epoch", 2, 6, 4, 11, 11, false},
		{"unwritten", 2, 7, 4, 12, 12, false},
		{"unproved_picture", 2, 7, 4, 11, 10, false},
	} {
		t.Run(item.name, func(t *testing.T) {
			viewer := &client{videoEpoch: 7, videoConfigGeneration: 4, videoWrittenSequence: 11,
				videoWrittenEvidence: []uint64{11}}
			raw, err := json.Marshal(streamFeedback{Type: "stream_feedback", Version: item.version, Epoch: item.epoch,
				ConfigGeneration: item.generation, ReceivedSequence: item.received, DecodedSequence: item.received,
				RenderedSequence: item.received, PresentedSequence: item.presented, Visibility: "visible", AgeKnown: true})
			if err != nil {
				t.Fatal(err)
			}
			result := viewer.acceptStreamFeedbackOutcome(raw)
			if result.presented != item.valid {
				t.Fatalf("presentation=%v wanted %v", result.presented, item.valid)
			}
		})
	}
}

func TestV2EarlyReceiptDoesNotOutrunPresentationEvidence(t *testing.T) {
	viewer := &client{videoEpoch: 7, videoConfigGeneration: 4}
	frame := queuedVideoFrame{meta: tsf3Metadata{ok: true, keyFrame: true, epoch: 7, sequence: 11}, configGeneration: 4}
	viewer.videoInFlight = &frame
	feedback := streamFeedback{Type: "stream_feedback", Version: 2, Epoch: 7, ConfigGeneration: 4,
		ReceivedSequence: 11, DecodedSequence: 11, RenderedSequence: 11, PresentedSequence: 11, Visibility: "visible"}
	raw, err := json.Marshal(feedback)
	if err != nil {
		t.Fatal(err)
	}
	first := viewer.acceptStreamFeedbackOutcome(raw)
	if !first.receiptReleased || first.presented {
		t.Fatalf("early receipt must release transport credit without claiming written evidence: %+v", first)
	}
	viewer.noteVideoFrameWrittenAt(&frame, time.Now())
	if viewer.videoReceiptSequence != 0 {
		t.Fatal("writer armed a receipt timeout after the exact frame was acknowledged")
	}
	if !viewer.acceptStreamFeedbackOutcome(raw).presented {
		t.Fatal("repeated presentation was lost after the successful write was recorded")
	}
	feedback.ConfigGeneration = 3
	raw, err = json.Marshal(feedback)
	if err != nil {
		t.Fatal(err)
	}
	if stale := viewer.acceptStreamFeedbackOutcome(raw); stale.receiptReleased || stale.presented {
		t.Fatalf("retired configuration affected the current viewer: %+v", stale)
	}
}
