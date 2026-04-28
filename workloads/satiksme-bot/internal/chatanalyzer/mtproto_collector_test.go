package chatanalyzer

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"satiksmebot/internal/model"
)

func TestTelegramMessageToAnalyzerMessageUsesTelegramUserIdentity(t *testing.T) {
	receivedAt := time.Date(2026, 4, 27, 8, 0, 0, 0, time.UTC)
	item := telegramMessageToAnalyzerMessage("channel:42", &tg.Message{
		ID:      10,
		Date:    int(receivedAt.Unix()),
		FromID:  &tg.PeerUser{UserID: 777001},
		PeerID:  &tg.PeerChannel{ChannelID: 42},
		Message: "kontrole",
	}, receivedAt)

	if item.SenderID != 777001 {
		t.Fatalf("sender id = %d, want Telegram user id 777001", item.SenderID)
	}
	if got, want := item.SenderStableID, model.TelegramStableID(777001); got != want {
		t.Fatalf("sender stable id = %q, want %q", got, want)
	}
	if got, want := item.SenderNickname, model.GenericNickname(777001); got != want {
		t.Fatalf("sender nickname = %q, want %q", got, want)
	}
}

func TestTelegramMessageToAnalyzerMessageDoesNotUseGroupAsReporter(t *testing.T) {
	receivedAt := time.Date(2026, 4, 27, 8, 0, 0, 0, time.UTC)
	item := telegramMessageToAnalyzerMessage("channel:42", &tg.Message{
		ID:      11,
		Date:    int(receivedAt.Unix()),
		FromID:  &tg.PeerChannel{ChannelID: 42},
		PeerID:  &tg.PeerChannel{ChannelID: 42},
		Message: "kontrole",
	}, receivedAt)

	if item.SenderID != 0 {
		t.Fatalf("sender id = %d, want 0 for non-user sender", item.SenderID)
	}
	if _, ok := model.ChatAnalyzerReporterUserID(item.SenderID); ok {
		t.Fatalf("non-user sender unexpectedly resolved to a reporter user id")
	}
}
