#!/usr/bin/env python3
"""Identity/token control plane for tokenized AdGuardHome DoH front door."""

from __future__ import annotations

import argparse
import json
import math
import os
import re
import secrets
import shlex
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, NoReturn
from urllib.parse import urlparse

SCHEMA = 1
DEFAULT_IDENTITY_ID = "default"
DEFAULT_WINDOW_SECONDS = 7 * 24 * 60 * 60
DEFAULT_RETENTION_DAYS = 30
IDENTITY_PATTERN = re.compile(r"^[a-z0-9][a-z0-9._-]{0,63}$")
TOKEN_PATTERN = re.compile(r"^[A-Za-z0-9._~-]{16,128}$")
TOKEN_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._~-"
NGINX_TOKEN_PATH_PATTERN = re.compile(r"^/([A-Za-z0-9._~-]+)/dns-query/?$")
DOT_LABEL_PATTERN = re.compile(r"^[a-z0-9]{1,63}$")
DOT_LABEL_ALPHABET = "abcdefghijklmnopqrstuvwxyz0123456789"

IDENTITIES_FILE = Path(
  os.environ.get("ADGUARDHOME_DOH_IDENTITIES_FILE", "/etc/pixel-stack/remote-dns/doh-identities.json")
)
USAGE_EVENTS_FILE = Path(
  os.environ.get(
    "ADGUARDHOME_DOH_USAGE_EVENTS_FILE",
    "/etc/pixel-stack/remote-dns/state/doh-usage-events.jsonl",
  )
)
USAGE_CURSOR_FILE = Path(
  os.environ.get(
    "ADGUARDHOME_DOH_USAGE_CURSOR_FILE",
    "/etc/pixel-stack/remote-dns/state/doh-usage-cursor.json",
  )
)
DOH_ACCESS_LOG_FILE = Path(
  os.environ.get(
    "ADGUARDHOME_DOH_ACCESS_LOG_FILE",
    "/var/log/adguardhome/remote-nginx-doh-access.log",
  )
)
RUNTIME_ENV_FILE = Path(
  os.environ.get("PIHOLE_REMOTE_RUNTIME_ENV_FILE", "/etc/pixel-stack/remote-dns/runtime.env")
)


def load_runtime_env_defaults() -> None:
  if not RUNTIME_ENV_FILE.is_file():
    return

  for raw_line in RUNTIME_ENV_FILE.read_text(encoding="utf-8").splitlines():
    line = raw_line.strip()
    if not line or line.startswith("#") or "=" not in line:
      continue

    key, raw_value = line.split("=", 1)
    key = key.strip()
    if not key or os.environ.get(key):
      continue

    try:
      tokens = shlex.split(raw_value, posix=True)
    except ValueError:
      tokens = [raw_value]
    os.environ[key] = tokens[0] if tokens else ""


load_runtime_env_defaults()


def env_flag(name: str, default: str = "0") -> bool:
  raw = os.environ.get(name, default)
  if raw is None:
    raw = default
  return raw.strip().lower() in ("1", "true", "yes", "on")


def dot_identity_enabled() -> bool:
  return env_flag("ADGUARDHOME_REMOTE_DOT_IDENTITY_ENABLED", "0")


def dot_identity_parent_hostname() -> str:
  return str(os.environ.get("PIHOLE_REMOTE_DOT_HOSTNAME", "") or "").strip().lower()


def max_dot_label_length(parent_hostname: str | None = None) -> int:
  hostname = (parent_hostname if parent_hostname is not None else dot_identity_parent_hostname()).strip().lower()
  if not hostname:
    return 63
  remaining = 253 - 1 - len(hostname)
  return min(63, remaining)


def configured_dot_label_length() -> int:
  raw = str(os.environ.get("ADGUARDHOME_REMOTE_DOT_IDENTITY_LABEL_LENGTH", "20") or "").strip()
  length = int(raw) if raw.isdigit() else 20
  maximum = max_dot_label_length()
  if maximum <= 0:
    fail(
      f"Configured PIHOLE_REMOTE_DOT_HOSTNAME '{dot_identity_parent_hostname()}' is too long for identity subdomains."
    )
  return min(max(1, length), maximum)


def now_epoch() -> int:
  return int(time.time())


def fail(message: str, code: int = 1) -> "NoReturn":
  print(message, file=sys.stderr)
  raise SystemExit(code)


def atomic_write(path: Path, content: str) -> None:
  path.parent.mkdir(parents=True, exist_ok=True)
  fd, tmp = tempfile.mkstemp(prefix=f"{path.name}.", suffix=".tmp", dir=str(path.parent))
  try:
    with os.fdopen(fd, "w", encoding="utf-8") as handle:
      handle.write(content)
    os.replace(tmp, path)
  finally:
    try:
      if os.path.exists(tmp):
        os.remove(tmp)
    except OSError:
      pass


def read_json(path: Path) -> Any:
  with path.open("r", encoding="utf-8") as handle:
    return json.load(handle)


def write_json(path: Path, payload: Any) -> None:
  atomic_write(path, json.dumps(payload, indent=2, sort_keys=False) + "\n")


def default_store() -> dict[str, Any]:
  return {
    "schema": SCHEMA,
    "primaryIdentityId": "",
    "identities": [],
  }


def normalize_identity_id(raw: str) -> str:
  identity_id = raw.strip().lower()
  if not IDENTITY_PATTERN.fullmatch(identity_id):
    fail(
      "Invalid identity id. Expected slug-like value (1-64 chars, lower-case a-z, 0-9, ., _, -)."
    )
  return identity_id


def validate_token(token: str) -> str:
  normalized = token.strip()
  if not TOKEN_PATTERN.fullmatch(normalized):
    fail("Invalid token. Expected 16-128 chars from [A-Za-z0-9._~-].")
  return normalized


def generate_token(length: int = 48) -> str:
  if length < 16:
    length = 16
  if length > 128:
    length = 128
  return "".join(secrets.choice(TOKEN_ALPHABET) for _ in range(length))


def normalize_dot_label(raw: Any, identity_id: str) -> str | None:
  if raw is None:
    return None
  if not isinstance(raw, str):
    fail(f"Identity '{identity_id}' dotLabel must be a string or null.")
  label = raw.strip().lower()
  if not label:
    return None
  if not DOT_LABEL_PATTERN.fullmatch(label):
    fail(f"Identity '{identity_id}' dotLabel must be lower-case letters and digits only.")
  maximum = max_dot_label_length()
  if len(label) > maximum:
    fail(f"Identity '{identity_id}' dotLabel exceeds max length {maximum} for '{dot_identity_parent_hostname() or '*'}'.")
  return label


def generate_dot_label(length: int | None = None, taken: set[str] | None = None) -> str:
  target_length = configured_dot_label_length() if length is None else max(1, min(length, max_dot_label_length()))
  reserved = taken or set()
  while True:
    label = "".join(secrets.choice(DOT_LABEL_ALPHABET) for _ in range(target_length))
    if label not in reserved:
      return label


def render_dot_hostname(dot_label: str | None) -> str | None:
  label = str(dot_label or "").strip().lower()
  if not label:
    return None
  parent_hostname = dot_identity_parent_hostname()
  if not parent_hostname:
    return None
  return f"{label}.{parent_hostname}"


def normalize_expires_epoch(raw: Any, identity_id: str) -> int | None:
  if raw is None:
    return None
  if isinstance(raw, bool):
    fail(f"Identity '{identity_id}' expiresEpochSeconds must be an integer epoch or null.")
  if not isinstance(raw, int):
    fail(f"Identity '{identity_id}' expiresEpochSeconds must be an integer epoch or null.")
  if raw <= 0:
    return None
  return raw


def is_identity_expired(entry: dict[str, Any], now: int) -> bool:
  expires = entry.get("expiresEpochSeconds")
  if not isinstance(expires, int) or expires <= 0:
    return False
  return expires <= now


def active_identities(store: dict[str, Any], now: int) -> list[dict[str, Any]]:
  entries = store.get("identities", [])
  if not isinstance(entries, list):
    return []
  return [entry for entry in entries if isinstance(entry, dict) and not is_identity_expired(entry, now)]


def resolve_effective_primary_id(store: dict[str, Any], now: int) -> str:
  configured = str(store.get("primaryIdentityId", "") or "")
  active = active_identities(store, now)
  if not active:
    return ""
  for entry in active:
    if entry.get("id") == configured:
      return configured
  return str(active[0].get("id", ""))


def load_store(allow_missing: bool = True) -> dict[str, Any]:
  if not IDENTITIES_FILE.exists():
    if allow_missing:
      return default_store()
    fail(f"Missing identity store: {IDENTITIES_FILE}")
  try:
    store = read_json(IDENTITIES_FILE)
  except json.JSONDecodeError as exc:
    fail(f"Invalid JSON in identity store {IDENTITIES_FILE}: {exc}")
  except OSError as exc:
    fail(f"Failed to read identity store {IDENTITIES_FILE}: {exc}")
  if not isinstance(store, dict):
    fail(f"Identity store {IDENTITIES_FILE} must be a JSON object.")
  return normalize_store(store)


def normalize_store(store: dict[str, Any]) -> dict[str, Any]:
  schema = store.get("schema", SCHEMA)
  if schema != SCHEMA:
    fail(f"Unsupported identity schema: {schema} (expected {SCHEMA}).")

  raw_identities = store.get("identities", [])
  if not isinstance(raw_identities, list):
    fail("Identity store field 'identities' must be an array.")

  identities: list[dict[str, Any]] = []
  seen_ids: set[str] = set()
  seen_tokens: set[str] = set()
  seen_dot_labels: set[str] = set()

  for entry in raw_identities:
    if not isinstance(entry, dict):
      fail("Identity entries must be JSON objects.")
    raw_id = entry.get("id", "")
    raw_token = entry.get("token", "")
    raw_dot_label = entry.get("dotLabel")
    created = entry.get("createdEpochSeconds", 0)
    expires = entry.get("expiresEpochSeconds")
    if not isinstance(raw_id, str):
      fail("Identity 'id' must be a string.")
    if not isinstance(raw_token, str):
      fail(f"Identity '{raw_id}' token must be a string.")
    identity_id = normalize_identity_id(raw_id)
    token = validate_token(raw_token)
    if identity_id in seen_ids:
      fail(f"Duplicate identity id in store: {identity_id}")
    if token in seen_tokens:
      fail(f"Duplicate identity token in store for identity: {identity_id}")
    if not isinstance(created, int):
      fail(f"Identity '{identity_id}' createdEpochSeconds must be an integer.")
    expires_epoch = normalize_expires_epoch(expires, identity_id)
    dot_label = normalize_dot_label(raw_dot_label, identity_id)
    seen_ids.add(identity_id)
    seen_tokens.add(token)
    if dot_label is not None:
      if dot_label in seen_dot_labels:
        fail(f"Duplicate identity dotLabel in store for identity: {identity_id}")
      seen_dot_labels.add(dot_label)
    identities.append(
      {
        "id": identity_id,
        "token": token,
        "dotLabel": dot_label,
        "createdEpochSeconds": max(0, created),
        "expiresEpochSeconds": expires_epoch,
      }
    )

  primary = store.get("primaryIdentityId", "")
  if primary is None:
    primary = ""
  if not isinstance(primary, str):
    fail("Identity store field 'primaryIdentityId' must be a string.")
  primary = primary.strip().lower()

  if identities:
    ids = {entry["id"] for entry in identities}
    if not primary:
      primary = identities[0]["id"]
    if primary not in ids:
      fail(
        f"primaryIdentityId '{primary}' does not match any identity in {IDENTITIES_FILE}."
      )
  else:
    primary = ""

  return {
    "schema": SCHEMA,
    "primaryIdentityId": primary,
    "identities": identities,
  }


def save_store(store: dict[str, Any]) -> dict[str, Any]:
  normalized = normalize_store(store)
  write_json(IDENTITIES_FILE, normalized)
  return normalized


def ensure_store_file() -> dict[str, Any]:
  if not IDENTITIES_FILE.exists():
    return save_store(default_store())
  return load_store(allow_missing=False)


def identity_runtime_view(entry: dict[str, Any]) -> dict[str, Any]:
  payload = dict(entry)
  dot_label = payload.get("dotLabel")
  if not isinstance(dot_label, str) or not dot_label:
    payload["dotLabel"] = None
    payload["dotHostname"] = None
    return payload
  payload["dotHostname"] = render_dot_hostname(dot_label)
  return payload


def ensure_dot_labels_in_store(store: dict[str, Any]) -> tuple[dict[str, Any], bool]:
  normalized = normalize_store(store)
  if not dot_identity_enabled():
    return normalized, False

  taken = {
    entry["dotLabel"]
    for entry in normalized.get("identities", [])
    if isinstance(entry, dict) and isinstance(entry.get("dotLabel"), str) and entry.get("dotLabel")
  }
  changed = False
  updated_identities: list[dict[str, Any]] = []
  for entry in normalized.get("identities", []):
    if not isinstance(entry, dict):
      continue
    current = dict(entry)
    if not current.get("dotLabel"):
      current["dotLabel"] = generate_dot_label(taken=taken)
      taken.add(current["dotLabel"])
      changed = True
    updated_identities.append(current)

  if not changed:
    return normalized, False

  updated_store = dict(normalized)
  updated_store["identities"] = updated_identities
  return save_store(updated_store), True


def token_to_identity_map(store: dict[str, Any]) -> dict[str, str]:
  return {entry["token"]: entry["id"] for entry in store["identities"]}


def find_primary_identity(store: dict[str, Any]) -> dict[str, Any] | None:
  primary = store.get("primaryIdentityId", "")
  for entry in store["identities"]:
    if entry["id"] == primary:
      return entry
  return None


def parse_expires_epoch_args(args: argparse.Namespace, now: int) -> int | None:
  expires_epoch: int | None = None
  raw_epoch = getattr(args, "expires_epoch", None)
  raw_window = getattr(args, "expires_in", None)
  if raw_epoch is not None and raw_window:
    fail("Use only one of --expires-epoch or --expires-in.")
  if raw_epoch is not None:
    if isinstance(raw_epoch, bool) or not isinstance(raw_epoch, int):
      fail("Invalid --expires-epoch value. Expected integer epoch seconds.")
    expires_epoch = raw_epoch
  elif raw_window:
    expires_epoch = now + parse_duration(raw_window)

  if expires_epoch is None:
    return None
  if expires_epoch <= now:
    fail("Invalid expiry. Expiration must be in the future.")
  return expires_epoch


def parse_duration(value: str | None) -> int:
  if not value:
    return DEFAULT_WINDOW_SECONDS
  raw = value.strip().lower()
  if not raw:
    return DEFAULT_WINDOW_SECONDS
  if raw.isdigit():
    return max(1, int(raw))
  match = re.fullmatch(r"([0-9]+)([smhdw])", raw)
  if not match:
    fail("Invalid --window value. Use formats like 3600, 90m, 24h, 7d, 1w.")
  number = int(match.group(1))
  unit = match.group(2)
  multipliers = {
    "s": 1,
    "m": 60,
    "h": 3600,
    "d": 86400,
    "w": 7 * 86400,
  }
  return max(1, number * multipliers[unit])


def parse_iso8601_epoch(value: str) -> int | None:
  text = value.strip()
  if not text:
    return None
  if text.endswith("Z"):
    text = text[:-1] + "+00:00"
  try:
    parsed = datetime.fromisoformat(text)
  except ValueError:
    return None
  if parsed.tzinfo is None:
    parsed = parsed.replace(tzinfo=timezone.utc)
  return int(parsed.timestamp())


def parse_iso8601_epoch_ms(value: str) -> int | None:
  text = value.strip()
  if not text:
    return None
  if text.endswith("Z"):
    text = text[:-1] + "+00:00"
  try:
    parsed = datetime.fromisoformat(text)
  except ValueError:
    return None
  if parsed.tzinfo is None:
    parsed = parsed.replace(tzinfo=timezone.utc)
  return int(round(parsed.timestamp() * 1000.0))


def percentile(sorted_values: list[float], pct: float) -> float:
  if not sorted_values:
    return 0.0
  if len(sorted_values) == 1:
    return float(sorted_values[0])
  rank = int(math.ceil((pct / 100.0) * len(sorted_values)))
  index = min(max(rank - 1, 0), len(sorted_values) - 1)
  return float(sorted_values[index])


def parse_doh_log_line(line: str, token_map: dict[str, str], fallback_epoch: int) -> dict[str, Any] | None:
  parts = line.rstrip("\n").split("\t")
  if len(parts) < 4:
    return None
  ts_raw, uri_raw, status_raw, request_time_raw = parts[0], parts[1], parts[2], parts[3]
  client_ip = parts[4].strip() if len(parts) >= 5 else ""
  ts_ms_raw = parts[5].strip() if len(parts) >= 6 else ""
  status = int(status_raw) if status_raw.isdigit() else 0
  try:
    request_time_ms = max(0, int(round(float(request_time_raw) * 1000)))
  except ValueError:
    request_time_ms = 0

  parsed_uri = urlparse(uri_raw)
  path = parsed_uri.path or ""

  identity_id = None
  if path in ("/dns-query", "/dns-query/"):
    identity_id = "__bare__"
  else:
    token_match = NGINX_TOKEN_PATH_PATTERN.fullmatch(path)
    if token_match:
      token = token_match.group(1)
      identity_id = token_map.get(token, "__unknown__")
  if identity_id is None:
    return None

  epoch = parse_iso8601_epoch(ts_raw) or fallback_epoch
  ts_ms = parse_iso8601_epoch_ms(ts_raw)
  if ts_ms is None:
    try:
      ts_ms = max(0, int(round(float(ts_ms_raw) * 1000)))
    except ValueError:
      ts_ms = epoch * 1000
  return {
    "ts": epoch,
    "tsMs": ts_ms,
    "identityId": identity_id,
    "status": status,
    "requestTimeMs": request_time_ms,
    "clientIp": client_ip,
  }


def read_cursor() -> dict[str, int]:
  if not USAGE_CURSOR_FILE.exists():
    return {"inode": 0, "offset": 0}
  try:
    data = read_json(USAGE_CURSOR_FILE)
  except Exception:
    return {"inode": 0, "offset": 0}
  inode = data.get("inode", 0) if isinstance(data, dict) else 0
  offset = data.get("offset", 0) if isinstance(data, dict) else 0
  if not isinstance(inode, int):
    inode = 0
  if not isinstance(offset, int):
    offset = 0
  return {"inode": max(0, inode), "offset": max(0, offset)}


def write_cursor(inode: int, offset: int) -> None:
  payload = {"inode": max(0, inode), "offset": max(0, offset)}
  write_json(USAGE_CURSOR_FILE, payload)


def append_events(events: list[dict[str, Any]]) -> None:
  if not events:
    return
  USAGE_EVENTS_FILE.parent.mkdir(parents=True, exist_ok=True)
  with USAGE_EVENTS_FILE.open("a", encoding="utf-8") as handle:
    for event in events:
      handle.write(json.dumps(event, separators=(",", ":")))
      handle.write("\n")


def load_events() -> list[dict[str, Any]]:
  if not USAGE_EVENTS_FILE.exists():
    return []
  events: list[dict[str, Any]] = []
  with USAGE_EVENTS_FILE.open("r", encoding="utf-8") as handle:
    for line in handle:
      line = line.strip()
      if not line:
        continue
      try:
        parsed = json.loads(line)
      except json.JSONDecodeError:
        continue
      if not isinstance(parsed, dict):
        continue
      ts = parsed.get("ts")
      identity_id = parsed.get("identityId")
      status = parsed.get("status")
      request_time_ms = parsed.get("requestTimeMs")
      ts_ms = parsed.get("tsMs")
      client_ip = parsed.get("clientIp")
      if not isinstance(ts, int):
        continue
      if not isinstance(identity_id, str):
        continue
      if not isinstance(status, int):
        status = 0
      if not isinstance(request_time_ms, int):
        request_time_ms = 0
      if not isinstance(ts_ms, int):
        ts_ms = ts * 1000
      if not isinstance(client_ip, str):
        client_ip = ""
      events.append(
        {
          "ts": ts,
          "tsMs": max(0, ts_ms),
          "identityId": identity_id,
          "status": status,
          "requestTimeMs": max(0, request_time_ms),
          "clientIp": client_ip.strip(),
        }
      )
  return events


def prune_events(events: list[dict[str, Any]], now: int, retention_days: int) -> list[dict[str, Any]]:
  retention_seconds = max(1, retention_days) * 86400
  min_epoch = now - retention_seconds
  kept = [event for event in events if event.get("ts", 0) >= min_epoch]
  if kept != events:
    lines = [json.dumps(event, separators=(",", ":")) for event in kept]
    atomic_write(USAGE_EVENTS_FILE, "\n".join(lines) + ("\n" if lines else ""))
  return kept


def rollup_doh_access(store: dict[str, Any], now: int, retention_days: int) -> list[dict[str, Any]]:
  token_map = token_to_identity_map(store)
  cursor = read_cursor()

  if not DOH_ACCESS_LOG_FILE.exists():
    events = load_events()
    return prune_events(events, now, retention_days)

  stat = DOH_ACCESS_LOG_FILE.stat()
  inode = int(stat.st_ino)
  start_offset = 0
  if cursor["inode"] == inode and 0 <= cursor["offset"] <= stat.st_size:
    start_offset = cursor["offset"]

  new_events: list[dict[str, Any]] = []
  final_offset = start_offset
  with DOH_ACCESS_LOG_FILE.open("r", encoding="utf-8", errors="replace") as handle:
    handle.seek(start_offset)
    for line in handle:
      parsed = parse_doh_log_line(line, token_map, now)
      if parsed is not None:
        new_events.append(parsed)
    final_offset = handle.tell()

  append_events(new_events)
  write_cursor(inode, final_offset)
  events = load_events()
  return prune_events(events, now, retention_days)


def aggregate_usage(
  events: list[dict[str, Any]],
  store: dict[str, Any],
  window_seconds: int,
  identity_filter: str | None,
  now: int,
  retention_days: int,
) -> dict[str, Any]:
  start_epoch = now - window_seconds
  filtered = [event for event in events if event["ts"] >= start_epoch]
  if identity_filter and identity_filter != "all":
    filtered = [event for event in filtered if event["identityId"] == identity_filter]

  grouped: dict[str, list[dict[str, Any]]] = {}
  for event in filtered:
    grouped.setdefault(event["identityId"], []).append(event)

  identity_rows: list[dict[str, Any]] = []
  for identity_id in sorted(grouped.keys()):
    rows = grouped[identity_id]
    statuses = {
      "2xx": 0,
      "3xx": 0,
      "4xx": 0,
      "5xx": 0,
      "other": 0,
    }
    latencies = sorted(float(entry["requestTimeMs"]) for entry in rows)
    for entry in rows:
      status = int(entry["status"])
      if 200 <= status < 300:
        statuses["2xx"] += 1
      elif 300 <= status < 400:
        statuses["3xx"] += 1
      elif 400 <= status < 500:
        statuses["4xx"] += 1
      elif 500 <= status < 600:
        statuses["5xx"] += 1
      else:
        statuses["other"] += 1
    identity_rows.append(
      {
        "id": identity_id,
        "requests": len(rows),
        "statusCounts": statuses,
        "latencyMs": {
          "p50": round(percentile(latencies, 50), 3),
          "p95": round(percentile(latencies, 95), 3),
          "p99": round(percentile(latencies, 99), 3),
        },
      }
    )

  identity_rows.sort(key=lambda row: (-row["requests"], row["id"]))
  active_identity_ids = [str(entry.get("id", "")) for entry in active_identities(store, now)]
  effective_primary = resolve_effective_primary_id(store, now)

  return {
    "generatedEpochSeconds": now,
    "windowSeconds": window_seconds,
    "retentionDays": retention_days,
    "identityFilter": identity_filter or "all",
    "activeIdentityIds": active_identity_ids,
    "primaryIdentityId": effective_primary,
    "configuredPrimaryIdentityId": str(store.get("primaryIdentityId", "")),
    "totalRequests": len(filtered),
    "identities": identity_rows,
  }


def filter_events(
  events: list[dict[str, Any]],
  window_seconds: int,
  now: int,
  identity_filter: str | None = None,
  client_ip: str | None = None,
  limit: int | None = None,
) -> list[dict[str, Any]]:
  start_epoch = now - window_seconds
  filtered = [event for event in events if event.get("ts", 0) >= start_epoch]
  if identity_filter and identity_filter != "all":
    filtered = [event for event in filtered if event.get("identityId") == identity_filter]
  if client_ip:
    normalized_ip = client_ip.strip()
    filtered = [event for event in filtered if event.get("clientIp", "") == normalized_ip]
  filtered.sort(
    key=lambda event: (
      -int(event.get("tsMs", 0)),
      -int(event.get("ts", 0)),
      str(event.get("identityId", "")),
      str(event.get("clientIp", "")),
    )
  )
  if isinstance(limit, int) and limit > 0:
    filtered = filtered[:limit]
  return filtered


def emit(payload: Any, as_json: bool) -> None:
  if as_json:
    print(json.dumps(payload, indent=2, sort_keys=False))
    return
  if isinstance(payload, dict) and "identities" in payload and "windowSeconds" in payload:
    print(
      f"window={payload['windowSeconds']}s retention={payload['retentionDays']}d "
      f"total={payload['totalRequests']} primary={payload.get('primaryIdentityId', '') or 'none'}"
    )
    rows = payload.get("identities", [])
    if not rows:
      print("no usage rows")
      return
    for row in rows:
      status = row["statusCounts"]
      latency = row["latencyMs"]
      print(
        f"id={row['id']} requests={row['requests']} "
        f"2xx={status['2xx']} 4xx={status['4xx']} 5xx={status['5xx']} "
        f"p50={latency['p50']}ms p95={latency['p95']}ms p99={latency['p99']}ms"
      )
    return
  if isinstance(payload, dict) and "identities" in payload:
    now = now_epoch()
    primary = resolve_effective_primary_id(payload, now)
    identities = payload.get("identities", [])
    if not identities:
      print("no identities")
      return
    for entry in identities:
      marker = "*" if entry["id"] == primary else " "
      expires = entry.get("expiresEpochSeconds")
      dot_hostname = entry.get("dotHostname")
      print(
        f"{marker} id={entry['id']} token={entry['token']} createdEpochSeconds={entry['createdEpochSeconds']} "
        f"expiresEpochSeconds={expires if isinstance(expires, int) and expires > 0 else 'none'} "
        f"dotHostname={dot_hostname or 'none'}"
      )
    return
  print(payload)


def apply_runtime_reload() -> bool:
  apply_raw = os.environ.get("ADGUARDHOME_DOH_IDENTITYCTL_APPLY", "1").strip().lower()
  if apply_raw in ("0", "false", "no", "off"):
    return False

  restart_entry = Path("/usr/local/bin/adguardhome-start")
  if not restart_entry.exists():
    return False

  env = dict(os.environ)
  path_segments = [segment for segment in env.get("PATH", "").split(":") if segment]
  required_segments = [
    "/system/bin",
    "/system/xbin",
    "/vendor/bin",
    "/apex/com.android.runtime/bin",
    "/usr/local/sbin",
    "/usr/local/bin",
    "/usr/sbin",
    "/usr/bin",
    "/sbin",
    "/bin",
  ]
  for segment in required_segments:
    if segment not in path_segments:
      path_segments.append(segment)
  env["PATH"] = ":".join(path_segments)

  if dot_identity_enabled():
    restart = subprocess.run(
      [str(restart_entry), "--runtime-restart-core"],
      stdout=subprocess.PIPE,
      stderr=subprocess.PIPE,
      text=True,
      env=env,
    )
    if restart.returncode == 0:
      return True

    health_detail = ""
    for _ in range(15):
      health = subprocess.run(
        [str(restart_entry), "--remote-healthcheck"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=env,
      )
      if health.returncode == 0:
        return True
      health_stderr = (health.stderr or "").strip()
      health_stdout = (health.stdout or "").strip()
      health_detail = health_stderr or health_stdout or "no output"
      time.sleep(1)

    restart_detail = (restart.stderr or "").strip() or (restart.stdout or "").strip() or "no output"
    raise RuntimeError(
      "Failed to apply updated identity routes: "
      f"runtime core restart detail: {restart_detail}; "
      f"healthcheck detail: {health_detail}"
    )

  frontend = subprocess.run(
    [str(restart_entry), "--remote-reload-frontend"],
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    text=True,
    env=env,
  )
  if frontend.returncode == 0:
    return True

  restart = subprocess.run(
    [str(restart_entry), "--remote-restart"],
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    text=True,
    env=env,
  )
  if restart.returncode == 0:
    return True

  # Runtime restarts can report a transient non-zero exit while listeners are
  # still converging. Treat the apply as successful once health checks pass.
  health_detail = ""
  for _ in range(15):
    health = subprocess.run(
      [str(restart_entry), "--remote-healthcheck"],
      stdout=subprocess.PIPE,
      stderr=subprocess.PIPE,
      text=True,
      env=env,
    )
    if health.returncode == 0:
      return True
    health_stderr = (health.stderr or "").strip()
    health_stdout = (health.stdout or "").strip()
    health_detail = health_stderr or health_stdout or "no output"
    time.sleep(1)

  frontend_detail = (frontend.stderr or "").strip() or (frontend.stdout or "").strip() or "no output"
  restart_detail = (restart.stderr or "").strip() or (restart.stdout or "").strip() or "no output"
  raise RuntimeError(
    "Failed to apply updated identity routes: "
    f"frontend reload detail: {frontend_detail}; "
    f"runtime restart detail: {restart_detail}; "
    f"healthcheck detail: {health_detail}"
  )


def command_list(args: argparse.Namespace) -> int:
  store = load_store(allow_missing=True)
  now = now_epoch()
  store = dict(store)
  configured_primary = str(store.get("primaryIdentityId", ""))
  store["configuredPrimaryIdentityId"] = configured_primary
  store["primaryIdentityId"] = resolve_effective_primary_id(store, now)
  store["dotIdentityEnabled"] = dot_identity_enabled()
  store["dotHostnameBase"] = dot_identity_parent_hostname()
  store["dotIdentityLabelLength"] = configured_dot_label_length()
  store["identities"] = [
    identity_runtime_view(entry)
    for entry in store.get("identities", [])
    if isinstance(entry, dict)
  ]
  emit(store, as_json=args.json)
  return 0


def command_create(args: argparse.Namespace) -> int:
  store = ensure_store_file()
  previous_store = json.loads(json.dumps(store))
  now = now_epoch()
  identity_id = normalize_identity_id(args.id)
  token = validate_token(args.token) if args.token else generate_token()
  expires_epoch = parse_expires_epoch_args(args, now)
  existing_ids = {entry["id"] for entry in store["identities"]}
  existing_tokens = {entry["token"] for entry in store["identities"]}
  if identity_id in existing_ids:
    fail(f"Identity already exists: {identity_id}")
  if token in existing_tokens:
    fail("Token already exists in identity store.")

  entry = {
    "id": identity_id,
    "token": token,
    "dotLabel": None,
    "createdEpochSeconds": now,
    "expiresEpochSeconds": expires_epoch,
  }
  if dot_identity_enabled():
    taken = {
      item.get("dotLabel")
      for item in store["identities"]
      if isinstance(item, dict) and isinstance(item.get("dotLabel"), str) and item.get("dotLabel")
    }
    entry["dotLabel"] = generate_dot_label(taken=taken)
  store["identities"].append(entry)
  if args.primary or not store.get("primaryIdentityId"):
    store["primaryIdentityId"] = identity_id
  store = save_store(store)
  applied = False
  try:
    applied = apply_runtime_reload()
  except RuntimeError as exc:
    save_store(previous_store)
    fail(f"{exc}. Identity store changes were reverted.")

  created_entry = next(
    (item for item in store["identities"] if isinstance(item, dict) and item.get("id") == identity_id),
    None,
  )
  created_dot_label = created_entry.get("dotLabel") if isinstance(created_entry, dict) else None

  payload = {
    "created": identity_id,
    "primaryIdentityId": resolve_effective_primary_id(store, now_epoch()),
    "configuredPrimaryIdentityId": store["primaryIdentityId"],
    "token": token,
    "dotLabel": created_dot_label,
    "dotHostname": render_dot_hostname(created_dot_label),
    "expiresEpochSeconds": expires_epoch,
    "identityCount": len(store["identities"]),
    "applied": applied,
  }
  emit(payload, as_json=args.json)
  return 0


def command_revoke(args: argparse.Namespace) -> int:
  store = load_store(allow_missing=False)
  previous_store = json.loads(json.dumps(store))
  identity_id = normalize_identity_id(args.id)
  existing = store["identities"]
  remaining = [entry for entry in existing if entry["id"] != identity_id]
  if len(remaining) == len(existing):
    fail(f"Identity not found: {identity_id}")
  if not remaining and not args.allow_empty:
    fail("Refusing to revoke the last identity without --allow-empty.")

  store["identities"] = remaining
  if not remaining:
    store["primaryIdentityId"] = ""
  elif store.get("primaryIdentityId") == identity_id:
    store["primaryIdentityId"] = remaining[0]["id"]
  store = save_store(store)
  applied = False
  try:
    applied = apply_runtime_reload()
  except RuntimeError as exc:
    save_store(previous_store)
    fail(f"{exc}. Identity store changes were reverted.")

  payload = {
    "revoked": identity_id,
    "remaining": len(store["identities"]),
    "primaryIdentityId": resolve_effective_primary_id(store, now_epoch()),
    "configuredPrimaryIdentityId": store["primaryIdentityId"],
    "applied": applied,
  }
  emit(payload, as_json=args.json)
  return 0


def command_usage(args: argparse.Namespace) -> int:
  store = load_store(allow_missing=True)
  retention_days = int(
    os.environ.get("ADGUARDHOME_DOH_USAGE_RETENTION_DAYS", str(DEFAULT_RETENTION_DAYS))
  )
  retention_days = max(1, retention_days)
  now = now_epoch()
  events = rollup_doh_access(store, now, retention_days)
  window_seconds = parse_duration(args.window)
  identity_filter = None
  if args.identity and args.identity != "all":
    raw_identity = args.identity.strip()
    if raw_identity in ("__bare__", "__unknown__"):
      identity_filter = raw_identity
    else:
      identity_filter = normalize_identity_id(raw_identity)
  payload = aggregate_usage(
    events=events,
    store=store,
    window_seconds=window_seconds,
    identity_filter=identity_filter,
    now=now,
    retention_days=retention_days,
  )
  emit(payload, as_json=args.json)
  if args.json_out:
    target = Path(args.json_out)
    write_json(target, payload)
  return 0


def command_events(args: argparse.Namespace) -> int:
  store = load_store(allow_missing=True)
  retention_days = int(
    os.environ.get("ADGUARDHOME_DOH_USAGE_RETENTION_DAYS", str(DEFAULT_RETENTION_DAYS))
  )
  retention_days = max(1, retention_days)
  now = now_epoch()
  events = rollup_doh_access(store, now, retention_days)
  window_seconds = parse_duration(args.window)
  identity_filter = None
  if args.identity and args.identity != "all":
    raw_identity = args.identity.strip()
    if raw_identity in ("__bare__", "__unknown__"):
      identity_filter = raw_identity
    else:
      identity_filter = normalize_identity_id(raw_identity)
  limit = max(1, int(args.limit)) if isinstance(args.limit, int) else None
  filtered = filter_events(
    events=events,
    window_seconds=window_seconds,
    now=now,
    identity_filter=identity_filter,
    client_ip=args.client_ip,
    limit=limit,
  )
  payload = {
    "generatedEpochSeconds": now,
    "windowSeconds": window_seconds,
    "retentionDays": retention_days,
    "identityFilter": identity_filter or "all",
    "clientIp": (args.client_ip or "").strip(),
    "totalEvents": len(filtered),
    "events": filtered,
  }
  emit(payload, as_json=args.json)
  return 0


def command_ensure_store(_: argparse.Namespace) -> int:
  ensure_store_file()
  return 0


def command_ensure_legacy(args: argparse.Namespace) -> int:
  store = ensure_store_file()
  if store["identities"]:
    save_store(store)
    return 0

  legacy_token = validate_token(args.legacy_token)
  identity_id = normalize_identity_id(args.id or DEFAULT_IDENTITY_ID)
  store["identities"] = [
    {
      "id": identity_id,
      "token": legacy_token,
      "dotLabel": generate_dot_label(taken=set()) if dot_identity_enabled() else None,
      "createdEpochSeconds": now_epoch(),
      "expiresEpochSeconds": None,
    }
  ]
  store["primaryIdentityId"] = identity_id
  save_store(store)
  return 0


def command_ensure_dot_labels(_: argparse.Namespace) -> int:
  store = ensure_store_file()
  ensure_dot_labels_in_store(store)
  return 0


def command_validate_active(_: argparse.Namespace) -> int:
  store = load_store(allow_missing=False)
  now = now_epoch()
  active = active_identities(store, now)
  if not active:
    fail("No active identities are available while tokenized DoH mode is active.")
  primary_id = resolve_effective_primary_id(store, now)
  if not primary_id:
    fail("No active identity is available for tokenized DoH mode.")
  return 0


def command_primary_token(_: argparse.Namespace) -> int:
  store = load_store(allow_missing=False)
  primary_id = resolve_effective_primary_id(store, now_epoch())
  primary = None
  for entry in store.get("identities", []):
    if isinstance(entry, dict) and entry.get("id") == primary_id:
      primary = entry
      break
  if primary is None:
    fail("No active primary identity is available.")
  print(primary["token"])
  return 0


def command_nginx_token_block(_: argparse.Namespace) -> int:
  store = load_store(allow_missing=False)
  now = now_epoch()
  active = active_identities(store, now)
  if not active:
    fail("Identity store is empty; cannot render tokenized DoH nginx block.")
  web_port = os.environ.get("PIHOLE_WEB_PORT", "8080")
  if not web_port.isdigit():
    web_port = "8080"
  blocks: list[str] = []
  for entry in active:
    token = entry["token"]
    identity_id = entry["id"]
    blocks.append(f"    # identity:{identity_id}")
    for suffix in ("", "/"):
      blocks.extend(
        [
          f"    location = /{token}/dns-query{suffix} {{",
          "      access_log /var/log/adguardhome/remote-nginx-doh-access.log pixel_doh;",
          f"      proxy_pass http://127.0.0.1:{web_port}/dns-query;",
          "      proxy_set_header Host $host;",
          "      proxy_set_header X-Forwarded-For $doh_client_ip;",
          "      proxy_set_header X-Real-IP $doh_client_ip;",
          "      proxy_set_header X-Forwarded-Proto https;",
          "    }",
        ]
      )
  print("\n".join(blocks))
  return 0


def command_nginx_dot_sni_map(args: argparse.Namespace) -> int:
  if not dot_identity_enabled():
    fail("DoT identity mode is disabled; refusing to render SNI allow map.")

  store = load_store(allow_missing=False)
  now = now_epoch()
  active = active_identities(store, now)
  if not active:
    fail("Identity store is empty; cannot render DoT SNI allow map.")

  backend = str(args.backend or "").strip()
  if not backend:
    fail("A backend host:port is required.")

  seen: set[str] = set()
  blocks = ["    default 127.0.0.1:1;"]
  for entry in active:
    dot_hostname = render_dot_hostname(entry.get("dotLabel"))
    if not dot_hostname or dot_hostname in seen:
      continue
    seen.add(dot_hostname)
    blocks.append(f"    {dot_hostname} {backend};")

  if len(blocks) == 1:
    fail("No active DoT identity hostnames available for SNI allow map.")

  print("\n".join(blocks))
  return 0


def command_adguard_client_block(_: argparse.Namespace) -> int:
  store = load_store(allow_missing=True)
  now = now_epoch()
  active = active_identities(store, now)
  blocks: list[str] = []
  for entry in active:
    dot_label = entry.get("dotLabel")
    if not isinstance(dot_label, str) or not dot_label:
      continue
    client_name = json.dumps(f"Identity {entry['id']}")
    blocks.extend(
      [
        f"    - name: {client_name}",
        "      ids:",
        f"        - {json.dumps(dot_label)}",
      ]
    )
  print("\n".join(blocks))
  return 0


def build_parser() -> argparse.ArgumentParser:
  parser = argparse.ArgumentParser(
    description="Manage encrypted DNS identities and DoH usage accounting."
  )
  sub = parser.add_subparsers(dest="command", required=True)

  list_cmd = sub.add_parser("list", help="List identities")
  list_cmd.add_argument("--json", action="store_true")
  list_cmd.set_defaults(handler=command_list)

  create_cmd = sub.add_parser("create", help="Create identity")
  create_cmd.add_argument("--id", required=True)
  create_cmd.add_argument("--token")
  create_cmd.add_argument("--expires-epoch", type=int)
  create_cmd.add_argument("--expires-in")
  create_cmd.add_argument("--primary", action="store_true")
  create_cmd.add_argument("--json", action="store_true")
  create_cmd.set_defaults(handler=command_create)

  revoke_cmd = sub.add_parser("revoke", help="Revoke identity")
  revoke_cmd.add_argument("--id", required=True)
  revoke_cmd.add_argument("--allow-empty", action="store_true")
  revoke_cmd.add_argument("--json", action="store_true")
  revoke_cmd.set_defaults(handler=command_revoke)

  usage_cmd = sub.add_parser("usage", help="Report usage")
  usage_cmd.add_argument("--identity")
  usage_cmd.add_argument("--all", action="store_true")
  usage_cmd.add_argument("--window")
  usage_cmd.add_argument("--json", action="store_true")
  usage_cmd.add_argument("--json-out")
  usage_cmd.set_defaults(handler=command_usage)

  events_cmd = sub.add_parser("events", help=argparse.SUPPRESS)
  events_cmd.add_argument("--identity")
  events_cmd.add_argument("--client-ip")
  events_cmd.add_argument("--window")
  events_cmd.add_argument("--limit", type=int, default=50000)
  events_cmd.add_argument("--json", action="store_true")
  events_cmd.set_defaults(handler=command_events)

  ensure_store_cmd = sub.add_parser("ensure-store", help=argparse.SUPPRESS)
  ensure_store_cmd.set_defaults(handler=command_ensure_store)

  ensure_dot_labels_cmd = sub.add_parser("ensure-dot-labels", help=argparse.SUPPRESS)
  ensure_dot_labels_cmd.set_defaults(handler=command_ensure_dot_labels)

  ensure_legacy_cmd = sub.add_parser("ensure-legacy", help=argparse.SUPPRESS)
  ensure_legacy_cmd.add_argument("--legacy-token", required=True)
  ensure_legacy_cmd.add_argument("--id", default=DEFAULT_IDENTITY_ID)
  ensure_legacy_cmd.set_defaults(handler=command_ensure_legacy)

  validate_cmd = sub.add_parser("validate-active", help=argparse.SUPPRESS)
  validate_cmd.set_defaults(handler=command_validate_active)

  primary_token_cmd = sub.add_parser("primary-token", help=argparse.SUPPRESS)
  primary_token_cmd.set_defaults(handler=command_primary_token)

  nginx_token_block_cmd = sub.add_parser("nginx-token-block", help=argparse.SUPPRESS)
  nginx_token_block_cmd.set_defaults(handler=command_nginx_token_block)

  nginx_dot_sni_map_cmd = sub.add_parser("nginx-dot-sni-map", help=argparse.SUPPRESS)
  nginx_dot_sni_map_cmd.add_argument("--backend", required=True)
  nginx_dot_sni_map_cmd.set_defaults(handler=command_nginx_dot_sni_map)

  adguard_client_block_cmd = sub.add_parser("adguard-client-block", help=argparse.SUPPRESS)
  adguard_client_block_cmd.set_defaults(handler=command_adguard_client_block)

  return parser


def main(argv: list[str]) -> int:
  parser = build_parser()
  args = parser.parse_args(argv)
  if getattr(args, "all", False):
    args.identity = "all"
  return int(args.handler(args))


if __name__ == "__main__":
  raise SystemExit(main(sys.argv[1:]))
