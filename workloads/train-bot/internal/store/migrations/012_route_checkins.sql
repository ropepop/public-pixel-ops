CREATE TABLE IF NOT EXISTS route_checkins (
  user_id INTEGER PRIMARY KEY,
  route_id TEXT NOT NULL,
  route_name TEXT NOT NULL,
  station_ids_json TEXT NOT NULL,
  checked_in_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_route_checkins_active_expiry
  ON route_checkins(is_active, expires_at);
