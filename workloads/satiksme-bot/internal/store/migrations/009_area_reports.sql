CREATE TABLE IF NOT EXISTS area_reports (
  id TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL,
  latitude REAL NOT NULL,
  longitude REAL NOT NULL,
  radius_meters INTEGER NOT NULL,
  description TEXT NOT NULL,
  scope_key TEXT NOT NULL,
  is_hidden INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS area_reports_user_scope_idx
ON area_reports(user_id, scope_key, created_at DESC);

CREATE INDEX IF NOT EXISTS area_reports_visible_idx
ON area_reports(is_hidden, created_at DESC);
