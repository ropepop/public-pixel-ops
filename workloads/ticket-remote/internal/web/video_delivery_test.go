package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestVideoSocketKeepsNewestPendingPictureUntilReceipt(t *testing.T) {
	ready := make(chan *client, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		viewer := &client{conn: conn}
		defer viewer.stopVideoWriter()
		defer conn.CloseNow()
		ready <- viewer
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			viewer.acceptStreamFeedbackOutcome(data)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	viewer := <-ready
	frame := func(sequence uint64) []byte {
		return testTSF3Frame(7, sequence, true, uint64(time.Now().Add(-20*time.Millisecond).UnixMicro()))
	}
	viewer.enqueueConfigAndKeyframe([]byte(`{"type":"config","streamEpoch":7}`), frame(1), nil)
	kind, data, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageText {
		t.Fatalf("configuration did not precede the picture: %v %v", kind, err)
	}
	var config struct {
		Generation uint64 `json:"feedbackConfigGeneration"`
	}
	if err := json.Unmarshal(data, &config); err != nil || config.Generation == 0 {
		t.Fatalf("missing configuration generation: %s", data)
	}
	kind, data, err = conn.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || parseTSF3(data).sequence != 1 {
		t.Fatalf("first picture was not delivered: %v", err)
	}
	viewer.enqueueVideoFrame(frame(2))
	viewer.enqueueVideoFrame(frame(3))
	viewer.enqueueVideoFrame(frame(2))
	// Control traffic still passes while binary delivery waits for the receipt.
	viewer.enqueueControl([]byte(`{"type":"clock_probe_result"}`))
	kind, _, err = conn.Read(ctx)
	if err != nil || kind != websocket.MessageText {
		t.Fatalf("a picture bypassed receipt credit or blocked control traffic: %v %v", kind, err)
	}
	ack, _ := json.Marshal(streamFeedback{Type: "stream_feedback", Version: 2, Epoch: 7,
		ConfigGeneration: config.Generation, ReceivedSequence: 1})
	if err := conn.Write(ctx, websocket.MessageText, ack); err != nil {
		t.Fatal(err)
	}
	kind, data, err = conn.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || parseTSF3(data).sequence != 3 {
		t.Fatalf("receipt did not release only the newest picture: %v", err)
	}
}

func TestVideoConfigurationRetiresPendingAndLateWriteEvidence(t *testing.T) {
	viewer := &client{}
	viewer.enqueueControl([]byte(`{"type":"config","streamEpoch":7}`))
	viewer.enqueueVideoFrame(testTSF3Frame(7, 11, true, uint64(time.Now().UnixMicro())))
	if _, ok := viewer.nextVideoWriteItem(); !ok {
		t.Fatal("missing configuration")
	}
	old, ok := viewer.nextVideoWriteItem()
	if !ok || old.frame == nil {
		t.Fatal("missing first picture")
	}
	viewer.enqueueControl([]byte(`{"type":"config","streamEpoch":8}`))
	viewer.enqueueVideoFrame(testTSF3Frame(7, 12, true, uint64(time.Now().UnixMicro())))
	viewer.noteVideoFrameWrittenAt(old.frame, time.Now())
	if viewer.videoPending != nil || viewer.videoWrittenSequence != 0 || viewer.videoReceiptSequence != 0 {
		t.Fatal("retired configuration affected current delivery")
	}
	viewer.videoReceiptDeadlineAt = time.Now().Add(-time.Millisecond)
	viewer.videoReceiptSequence = 1
	if !viewer.closeExpiredVideoReceipt() {
		t.Fatal("missing receipt did not close the viewer")
	}
	viewer.enqueueControl([]byte(`{"type":"config","streamEpoch":9}`))
	if viewer.videoEpoch != 8 || viewer.videoWriterHasRunnableWork() {
		t.Fatal("closed viewer was revived")
	}
	if videoWriteFailureReason(nil, true) != "write_timeout" {
		t.Fatal("late successful write was accepted after its deadline")
	}
}

// A warm encoder may have no picture fresh enough to replay. Configuration
// must still establish capture credit so the first new picture can be made.
func TestWarmViewerWithoutFreshCacheCanRequestCapture(t *testing.T) {
	hub := newDirectStreamHub()
	if !hub.setConfig(testAllIntraConfig([]byte(`{"type":"config","streamEpoch":7,"phoneUptimeMillis":10000,"frameEnvelope":"tsf3"}`))) {
		t.Fatal("config rejected")
	}
	config, frame := hub.warmStart()
	if len(frame) != 0 {
		t.Fatal("unexpected cached picture")
	}
	viewer := &client{videoV2Visibility: "visible"}
	viewer.enqueueConfigAndKeyframe(config, frame, nil)
	item, ok := viewer.nextVideoWriteItem()
	if !ok || item.control == nil {
		t.Fatal("missing configuration")
	}
	viewer.noteVideoConfigWritten(*item.control)
	if !viewer.canUseOrdinaryCapture(7) {
		t.Fatal("warm viewer cannot request its first fresh picture")
	}
}
