package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"satiksmebot/internal/model"
)

func TestSpacetimePayloadsExposeStableTelegramIdentity(t *testing.T) {
	createdAt := time.Date(2026, 4, 24, 10, 30, 0, 0, time.UTC)
	stop := spacetimeStopSightingPayload(model.StopSighting{
		ID:        "stop-1",
		StopID:    "1033a",
		UserID:    777001,
		CreatedAt: createdAt,
	})
	vote := spacetimeIncidentVotePayload(model.IncidentVote{
		IncidentID: "stop:1033a",
		UserID:     777001,
		Value:      model.IncidentVoteOngoing,
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
	})

	assertIdentityJSON(t, stop)
	assertIdentityJSON(t, vote)
}

func TestSpacetimeReportDumpPayloadUsesLowerCamelFields(t *testing.T) {
	createdAt := time.Date(2026, 4, 24, 11, 0, 0, 0, time.UTC)
	nextAttemptAt := createdAt.Add(30 * time.Second)
	payload := spacetimeReportDumpPayload(ReportDumpItem{
		ID:            "dump-1",
		Payload:       "Kontrole pie pieturas",
		Attempts:      2,
		CreatedAt:     createdAt,
		NextAttemptAt: nextAttemptAt,
	})

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	raw := string(body)
	for _, want := range []string{`"id"`, `"payload"`, `"attempts"`, `"createdAt"`, `"nextAttemptAt"`, `"lastAttemptAt"`, `"lastError"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("report dump payload JSON = %s, want key %s", raw, want)
		}
	}
	for _, unwanted := range []string{`"ID"`, `"Payload"`, `"CreatedAt"`, `"NextAttemptAt"`} {
		if strings.Contains(raw, unwanted) {
			t.Fatalf("report dump payload JSON = %s, did not want exported Go key %s", raw, unwanted)
		}
	}
	if !strings.Contains(raw, `"payload":"Kontrole pie pieturas"`) {
		t.Fatalf("report dump payload JSON = %s, want non-empty payload", raw)
	}
}

func TestSpacetimeChatAnalyzerPayloadUsesPrivateLowerCamelFields(t *testing.T) {
	now := time.Date(2026, 4, 27, 9, 30, 0, 0, time.UTC)
	payload := spacetimeChatAnalyzerMessagePayload(model.ChatAnalyzerMessage{
		ID:               "chat:1:2",
		ChatID:           "chat:1",
		MessageID:        2,
		SenderID:         -10042,
		SenderStableID:   "telegram:-10042",
		SenderNickname:   "Amber Scout 123",
		Text:             "raw private text",
		MessageDate:      now,
		ReceivedAt:       now,
		ReplyToMessageID: 1,
		Status:           model.ChatAnalyzerMessagePending,
	})
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	raw := string(body)
	for _, want := range []string{`"chatId"`, `"messageId":"2"`, `"senderId":"-10042"`, `"text":"raw private text"`, `"replyToMessageId":"1"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("chat analyzer payload JSON = %s, want %s", raw, want)
		}
	}
	for _, unwanted := range []string{`"ChatID"`, `"MessageID"`, `"SenderID"`} {
		if strings.Contains(raw, unwanted) {
			t.Fatalf("chat analyzer payload JSON = %s, did not want Go key %s", raw, unwanted)
		}
	}
}

func TestSpacetimeChatAnalyzerBatchPayloadUsesPrivateLowerCamelFields(t *testing.T) {
	now := time.Date(2026, 4, 28, 8, 0, 0, 0, time.UTC)
	payload := spacetimeChatAnalyzerBatchPayload(model.ChatAnalyzerBatch{
		ID:            "batch-1",
		Status:        model.ChatAnalyzerBatchCompleted,
		DryRun:        true,
		StartedAt:     now,
		FinishedAt:    now.Add(time.Second),
		MessageCount:  5,
		ReportCount:   1,
		WouldApply:    1,
		Model:         "openrouter/free",
		SelectedModel: "qwen/free-picked",
		ResultJSON:    `{"reports":[],"votes":[],"ignored":[]}`,
	})
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	raw := string(body)
	for _, want := range []string{`"id":"batch-1"`, `"dryRun":true`, `"messageCount":5`, `"wouldApply":1`, `"selectedModel":"qwen/free-picked"`, `"resultJson"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("chat analyzer batch payload JSON = %s, want %s", raw, want)
		}
	}
	for _, unwanted := range []string{`"ID"`, `"DryRun"`, `"SelectedModel"`} {
		if strings.Contains(raw, unwanted) {
			t.Fatalf("chat analyzer batch payload JSON = %s, did not want Go key %s", raw, unwanted)
		}
	}
}

func TestDecodeSpacetimeChatAnalyzerMessageAcceptsBlankProcessedAt(t *testing.T) {
	now := "2026-04-27T09:30:00Z"
	item, err := decodeSpacetimeChatAnalyzerMessage(spacetimeChatAnalyzerMessageJSON{
		ID:               "chat:1:2",
		ChatID:           "chat:1",
		MessageID:        float64(2),
		SenderID:         float64(-10042),
		SenderStableID:   "telegram:-10042",
		SenderNickname:   "Amber Scout 123",
		Text:             "raw private text",
		MessageDate:      now,
		ReceivedAt:       now,
		ReplyToMessageID: float64(1),
		Status:           string(model.ChatAnalyzerMessagePending),
		Attempts:         0,
		ProcessedAt:      "",
	})
	if err != nil {
		t.Fatalf("decodeSpacetimeChatAnalyzerMessage() error = %v", err)
	}
	if !item.ProcessedAt.IsZero() {
		t.Fatalf("ProcessedAt = %v, want zero time", item.ProcessedAt)
	}
	if item.MessageID != 2 || item.SenderID != -10042 || item.ReplyToMessageID != 1 {
		t.Fatalf("decoded item = %+v", item)
	}
}

func TestDecodeSpacetimeReportDumpPayloadAcceptsBlankLastAttemptAt(t *testing.T) {
	createdAt := "2026-04-24T11:00:00Z"
	nextAttemptAt := "2026-04-24T11:01:00Z"
	item, err := decodeSpacetimeReportDumpPayload(map[string]any{
		"item": map[string]any{
			"id":            "dump-1",
			"payload":       "Kontrole pie pieturas",
			"attempts":      0,
			"createdAt":     createdAt,
			"nextAttemptAt": nextAttemptAt,
			"lastAttemptAt": "",
			"lastError":     "",
		},
	})
	if err != nil {
		t.Fatalf("decodeSpacetimeReportDumpPayload() error = %v", err)
	}
	if item == nil {
		t.Fatalf("decodeSpacetimeReportDumpPayload() = nil")
	}
	if item.LastAttemptAt.IsZero() != true {
		t.Fatalf("LastAttemptAt = %v, want zero time", item.LastAttemptAt)
	}
	if item.ID != "dump-1" || item.Payload != "Kontrole pie pieturas" {
		t.Fatalf("decoded item = %+v", item)
	}
}

func assertIdentityJSON(t *testing.T, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if payload["stableId"] != "telegram:777001" {
		t.Fatalf("stableId = %#v, want telegram:777001 in %s", payload["stableId"], string(body))
	}
	if payload["userId"] != "777001" {
		t.Fatalf("userId = %#v, want 777001 in %s", payload["userId"], string(body))
	}
}
