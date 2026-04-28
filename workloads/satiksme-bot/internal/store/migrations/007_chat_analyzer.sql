CREATE TABLE IF NOT EXISTS chat_analyzer_checkpoints (
  chat_id TEXT PRIMARY KEY,
  last_message_id INTEGER NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS chat_analyzer_messages (
  id TEXT PRIMARY KEY,
  chat_id TEXT NOT NULL,
  message_id INTEGER NOT NULL,
  sender_id INTEGER NOT NULL,
  sender_stable_id TEXT NOT NULL,
  sender_nickname TEXT NOT NULL,
  raw_text TEXT NOT NULL,
  message_date TEXT NOT NULL,
  received_at TEXT NOT NULL,
  reply_to_message_id INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending',
  attempts INTEGER NOT NULL DEFAULT 0,
  analysis_json TEXT NOT NULL DEFAULT '',
  applied_action_id TEXT NOT NULL DEFAULT '',
  applied_target_key TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  processed_at TEXT NOT NULL DEFAULT '',
  UNIQUE(chat_id, message_id)
);

CREATE INDEX IF NOT EXISTS chat_analyzer_messages_pending_idx
ON chat_analyzer_messages(status, received_at ASC, message_id ASC);

CREATE INDEX IF NOT EXISTS chat_analyzer_messages_sender_idx
ON chat_analyzer_messages(chat_id, sender_id, received_at DESC);

CREATE INDEX IF NOT EXISTS chat_analyzer_messages_target_idx
ON chat_analyzer_messages(applied_target_key, processed_at DESC);
