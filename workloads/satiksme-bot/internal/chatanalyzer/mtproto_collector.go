package chatanalyzer

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"

	"satiksmebot/internal/model"
	"satiksmebot/internal/store"
)

type MTProtoCollectorConfig struct {
	APIID       int
	APIHash     string
	SessionFile string
	ChatID      string
	Store       store.ChatAnalyzerStore
	BatchLimit  int
	Now         func() time.Time
}

type MTProtoCollector struct {
	apiID       int
	apiHash     string
	sessionFile string
	chatID      string
	store       store.ChatAnalyzerStore
	batchLimit  int
	now         func() time.Time
}

func NewMTProtoCollector(cfg MTProtoCollectorConfig) *MTProtoCollector {
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	limit := cfg.BatchLimit
	if limit <= 0 {
		limit = 25
	}
	return &MTProtoCollector{
		apiID:       cfg.APIID,
		apiHash:     strings.TrimSpace(cfg.APIHash),
		sessionFile: strings.TrimSpace(cfg.SessionFile),
		chatID:      strings.TrimSpace(cfg.ChatID),
		store:       cfg.Store,
		batchLimit:  limit,
		now:         now,
	}
}

func (c *MTProtoCollector) Collect(ctx context.Context) ([]model.ChatAnalyzerMessage, error) {
	if c == nil || c.apiID <= 0 || c.apiHash == "" || c.sessionFile == "" || c.chatID == "" || c.store == nil {
		return nil, fmt.Errorf("telegram chat collector is not configured")
	}
	var out []model.ChatAnalyzerMessage
	client := telegram.NewClient(c.apiID, c.apiHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: filepath.Clean(c.sessionFile)},
		NoUpdates:      true,
	})
	err := client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if status == nil || !status.Authorized {
			return fmt.Errorf("telegram account session is not authorized; run chat-analyzer-session first")
		}
		peer, checkpointKey, err := c.resolvePeer(ctx, client.API())
		if err != nil {
			return err
		}
		lastID, found, err := c.store.GetChatAnalyzerCheckpoint(ctx, checkpointKey)
		if err != nil {
			return err
		}
		if !found {
			latest, err := c.fetchMessages(ctx, client.API(), peer, 0, 1)
			if err != nil {
				return err
			}
			if maxID := maxTelegramMessageID(latest); maxID > 0 {
				return c.store.SetChatAnalyzerCheckpoint(ctx, checkpointKey, maxID, c.now())
			}
			return nil
		}
		messages, err := c.fetchMessages(ctx, client.API(), peer, int(lastID), c.batchLimit)
		if err != nil {
			return err
		}
		now := c.now().UTC()
		out = make([]model.ChatAnalyzerMessage, 0, len(messages))
		for _, msg := range messages {
			if int64(msg.ID) <= lastID || strings.TrimSpace(msg.Message) == "" {
				continue
			}
			item := telegramMessageToAnalyzerMessage(checkpointKey, msg, now)
			out = append(out, item)
		}
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].MessageID < out[j].MessageID
		})
		return nil
	})
	return out, err
}

func (c *MTProtoCollector) fetchMessages(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, minID, limit int) ([]*tg.Message, error) {
	result, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  peer,
		MinID: minID,
		Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	return messagesFromHistory(result), nil
}

func messagesFromHistory(result tg.MessagesMessagesClass) []*tg.Message {
	var raw []tg.MessageClass
	switch value := result.(type) {
	case *tg.MessagesMessages:
		raw = value.Messages
	case *tg.MessagesMessagesSlice:
		raw = value.Messages
	case *tg.MessagesChannelMessages:
		raw = value.Messages
	default:
		return nil
	}
	out := make([]*tg.Message, 0, len(raw))
	for _, item := range raw {
		if msg, ok := item.(*tg.Message); ok {
			out = append(out, msg)
		}
	}
	return out
}

func maxTelegramMessageID(messages []*tg.Message) int64 {
	var maxID int64
	for _, msg := range messages {
		if msg != nil && int64(msg.ID) > maxID {
			maxID = int64(msg.ID)
		}
	}
	return maxID
}

func telegramMessageToAnalyzerMessage(chatID string, msg *tg.Message, receivedAt time.Time) model.ChatAnalyzerMessage {
	senderID := peerTelegramUserID(msg.FromID)
	replyToID := int64(0)
	if reply, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
		replyToID = int64(reply.ReplyToMsgID)
	}
	messageDate := time.Unix(int64(msg.Date), 0).UTC()
	if messageDate.IsZero() {
		messageDate = receivedAt
	}
	return model.ChatAnalyzerMessage{
		ID:               fmt.Sprintf("%s:%d", chatID, msg.ID),
		ChatID:           chatID,
		MessageID:        int64(msg.ID),
		SenderID:         senderID,
		SenderStableID:   model.ChatAnalyzerStableID(senderID),
		SenderNickname:   model.ChatAnalyzerReporterNickname(senderID),
		Text:             msg.Message,
		MessageDate:      messageDate,
		ReceivedAt:       receivedAt,
		ReplyToMessageID: replyToID,
		Status:           model.ChatAnalyzerMessagePending,
	}
}

func peerTelegramUserID(peer tg.PeerClass) int64 {
	switch value := peer.(type) {
	case *tg.PeerUser:
		return value.UserID
	default:
		return 0
	}
}

func (c *MTProtoCollector) resolvePeer(ctx context.Context, api *tg.Client) (tg.InputPeerClass, string, error) {
	raw := strings.TrimSpace(c.chatID)
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "chat:"):
		id, err := strconv.ParseInt(strings.TrimSpace(raw[len("chat:"):]), 10, 64)
		if err != nil || id == 0 {
			return nil, "", fmt.Errorf("invalid telegram chat descriptor %q", raw)
		}
		return &tg.InputPeerChat{ChatID: absInt64(id)}, "chat:" + strconv.FormatInt(absInt64(id), 10), nil
	case strings.HasPrefix(lower, "channel:"):
		parts := strings.Split(raw, ":")
		if len(parts) != 3 {
			return nil, "", fmt.Errorf("channel descriptor must be channel:<id>:<accessHash>")
		}
		id, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || id == 0 {
			return nil, "", fmt.Errorf("invalid channel id in %q", raw)
		}
		hash, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		if err != nil || hash == 0 {
			return nil, "", fmt.Errorf("invalid channel access hash in %q", raw)
		}
		id = absInt64(id)
		return &tg.InputPeerChannel{ChannelID: id, AccessHash: hash}, "channel:" + strconv.FormatInt(id, 10), nil
	case strings.HasPrefix(raw, "@") || strings.Contains(lower, "t.me/"):
		return resolveUsernamePeer(ctx, api, raw)
	default:
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id == 0 {
			return nil, "", fmt.Errorf("unsupported telegram chat descriptor %q", raw)
		}
		if id < -1000000000000 {
			return nil, "", fmt.Errorf("telegram supergroup/channel numeric ids need channel:<id>:<accessHash> from the session setup command")
		}
		id = absInt64(id)
		return &tg.InputPeerChat{ChatID: id}, "chat:" + strconv.FormatInt(id, 10), nil
	}
}

func resolveUsernamePeer(ctx context.Context, api *tg.Client, raw string) (tg.InputPeerClass, string, error) {
	username := strings.TrimSpace(raw)
	username = strings.TrimPrefix(username, "https://t.me/")
	username = strings.TrimPrefix(username, "http://t.me/")
	username = strings.TrimPrefix(username, "t.me/")
	username = strings.TrimPrefix(username, "@")
	username = strings.Trim(username, "/")
	if username == "" {
		return nil, "", fmt.Errorf("telegram username is empty")
	}
	resolved, err := api.ContactsResolveUsername(ctx, username)
	if err != nil {
		return nil, "", err
	}
	switch peer := resolved.Peer.(type) {
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: peer.ChatID}, "chat:" + strconv.FormatInt(peer.ChatID, 10), nil
	case *tg.PeerChannel:
		for _, chat := range resolved.Chats {
			channel, ok := chat.(*tg.Channel)
			if !ok || channel.ID != peer.ChannelID {
				continue
			}
			return &tg.InputPeerChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash}, "channel:" + strconv.FormatInt(channel.ID, 10), nil
		}
		return nil, "", fmt.Errorf("resolved channel %q without access hash", username)
	default:
		return nil, "", fmt.Errorf("telegram descriptor %q did not resolve to a group or channel", raw)
	}
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
