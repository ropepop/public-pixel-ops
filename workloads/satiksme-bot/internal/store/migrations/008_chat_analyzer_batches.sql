ALTER TABLE chat_analyzer_messages
ADD COLUMN batch_id TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS chat_analyzer_batches (
  id TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  dry_run INTEGER NOT NULL DEFAULT 1,
  started_at TEXT NOT NULL,
  finished_at TEXT NOT NULL DEFAULT '',
  message_count INTEGER NOT NULL DEFAULT 0,
  report_count INTEGER NOT NULL DEFAULT 0,
  vote_count INTEGER NOT NULL DEFAULT 0,
  ignored_count INTEGER NOT NULL DEFAULT 0,
  would_apply_count INTEGER NOT NULL DEFAULT 0,
  applied_count INTEGER NOT NULL DEFAULT 0,
  error_count INTEGER NOT NULL DEFAULT 0,
  model TEXT NOT NULL DEFAULT '',
  selected_model TEXT NOT NULL DEFAULT '',
  result_json TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS chat_analyzer_messages_batch_idx
ON chat_analyzer_messages(batch_id, message_id ASC);

CREATE INDEX IF NOT EXISTS chat_analyzer_batches_started_idx
ON chat_analyzer_batches(started_at DESC);
