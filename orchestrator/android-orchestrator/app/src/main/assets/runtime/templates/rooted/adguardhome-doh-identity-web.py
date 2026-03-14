#!/usr/bin/env python3
"""Web control plane for encrypted DNS identities inside AdGuardHome runtime."""

from __future__ import annotations

import argparse
import copy
from collections import Counter
from datetime import datetime, timezone
import json
import ipaddress
import os
import re
import shlex
import subprocess
import sys
import threading
import time
from dataclasses import dataclass
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.parse import parse_qs, quote, unquote, urlencode, urlparse
from urllib.request import Request, urlopen

SETTINGS_HASH_PREFIXES = (
  "#settings",
  "#dns",
  "#encryption",
  "#clients",
  "#dhcp",
)

IDENTITY_ID_PATTERN = re.compile(r"^[a-z0-9][a-z0-9._-]{0,63}$")
INTERNAL_QUERYLOG_CLIENTS_DEFAULT = "127.0.0.1,::1"
INTERNAL_PROBE_DOMAINS_DEFAULT = "example.com"
INTERNAL_LOOPBACK_DOMAIN_SUFFIXES_DEFAULT = "argotunnel.com"
QUERYLOG_LIMIT_DEFAULT = 1000
QUERYLOG_LIMIT_MIN = 1
QUERYLOG_LIMIT_MAX = 10000
QUERYLOG_FILTER_SCAN_PAGE_LIMIT = 500
QUERYLOG_FILTER_SCAN_PAGES_MAX = 50
QUERYLOG_IDENTITY_MAX_SKEW_MS = 2000
QUERYLOG_IDENTITY_MAX_DURATION_SKEW_MS = 25
QUERYLOG_DOT_SESSION_MAX_SKEW_MS = 2000
QUERYLOG_EVENT_WINDOW_DEFAULT = "30d"
IPINFO_CACHE_TTL_SECONDS = 30 * 86400
IPINFO_LOOKUP_TIMEOUT_SECONDS = 4
BURST_CACHE_TTL_SECONDS = 2.0
DOT_ACCESS_LOG_FILE_DEFAULT = "/var/log/adguardhome/remote-nginx-dot-access.log"
IDENTITY_LABELS = {
  "__bare__": "Bare path",
  "__unknown__": "Unknown token",
}


def mask_token(token: str) -> str:
  if len(token) <= 4:
    return "*" * len(token)
  if len(token) <= 10:
    return f"{token[:2]}{'*' * (len(token) - 4)}{token[-2:]}"
  return f"{token[:4]}...{token[-4:]}"


def normalize_bool(value: Any) -> bool:
  if isinstance(value, bool):
    return value
  if isinstance(value, (int, float)):
    return value != 0
  if isinstance(value, str):
    return value.strip().lower() in ("1", "true", "yes", "on")
  return False


def normalize_optional_epoch(value: Any) -> int | None:
  if value is None:
    return None
  if isinstance(value, bool):
    raise ValueError("epoch must be an integer or null")
  if isinstance(value, int):
    return value if value > 0 else None
  if isinstance(value, str):
    text = value.strip()
    if not text:
      return None
    if text.isdigit():
      parsed = int(text)
      return parsed if parsed > 0 else None
  raise ValueError("epoch must be an integer or null")


def normalize_origin(origin_or_referer: str) -> str:
  raw = origin_or_referer.strip()
  if not raw:
    return ""
  parsed = urlparse(raw)
  if parsed.scheme and parsed.netloc:
    return f"{parsed.scheme}://{parsed.netloc}"
  return ""


@dataclass
class ServerConfig:
  host: str
  port: int
  identityctl: str
  adguard_web_port: int
  skip_session_check: bool


class IdentityWebError(RuntimeError):
  def __init__(self, status: int, message: str):
    super().__init__(message)
    self.status = status
    self.message = message


class QuerylogFetchError(RuntimeError):
  def __init__(self, querylog_status: str, message: str):
    super().__init__(message)
    self.querylog_status = querylog_status
    self.message = message


class IdentityWebServer(ThreadingHTTPServer):
  daemon_threads = True
  allow_reuse_address = True

  def __init__(self, server_address: tuple[str, int], handler_cls: type[BaseHTTPRequestHandler], config: ServerConfig):
    super().__init__(server_address, handler_cls)
    self.config = config
    self.burst_cache_lock = threading.Lock()
    self.burst_cache: dict[str, dict[str, Any]] = {}


class IdentityWebHandler(BaseHTTPRequestHandler):
  server: IdentityWebServer

  def log_message(self, fmt: str, *args: Any) -> None:
    sys.stderr.write("[identity-web] %s\n" % (fmt % args))

  def _send_bytes(self, status: int, payload: bytes, content_type: str) -> None:
    self.send_response(status)
    self.send_header("Content-Type", content_type)
    self.send_header("Cache-Control", "no-store")
    self.send_header("Content-Length", str(len(payload)))
    self.end_headers()
    self.wfile.write(payload)

  def _send_json(self, status: int, payload: dict[str, Any]) -> None:
    body = json.dumps(payload, separators=(",", ":"), sort_keys=False).encode("utf-8")
    self._send_bytes(status, body, "application/json; charset=utf-8")

  def _send_text(self, status: int, payload: str, content_type: str = "text/plain; charset=utf-8") -> None:
    self._send_bytes(status, payload.encode("utf-8"), content_type)

  def _redirect(self, location: str) -> None:
    self.send_response(HTTPStatus.FOUND)
    self.send_header("Location", location)
    self.send_header("Cache-Control", "no-store")
    self.end_headers()

  def _parse_path(self) -> tuple[str, dict[str, list[str]]]:
    parsed = urlparse(self.path)
    return parsed.path, parse_qs(parsed.query, keep_blank_values=True)

  def _read_body_bytes(self, limit_bytes: int = 1024 * 1024) -> bytes:
    length_raw = self.headers.get("Content-Length", "")
    if not length_raw.isdigit():
      raise IdentityWebError(HTTPStatus.BAD_REQUEST, "Missing or invalid Content-Length.")
    length = int(length_raw)
    if length < 0 or length > limit_bytes:
      raise IdentityWebError(HTTPStatus.BAD_REQUEST, "Request body is too large.")
    return self.rfile.read(length)

  def _read_json_body(self) -> dict[str, Any]:
    payload_raw = self._read_body_bytes()
    try:
      payload = json.loads(payload_raw.decode("utf-8"))
    except Exception as exc:
      raise IdentityWebError(HTTPStatus.BAD_REQUEST, f"Invalid JSON body: {exc}") from exc
    if not isinstance(payload, dict):
      raise IdentityWebError(HTTPStatus.BAD_REQUEST, "JSON body must be an object.")
    return payload

  def _session_authenticated(self) -> bool:
    if self.server.config.skip_session_check:
      return True
    cookie = self.headers.get("Cookie", "").strip()
    if not cookie:
      return False
    req = Request(
      url=f"http://127.0.0.1:{self.server.config.adguard_web_port}/control/status",
      headers={"Cookie": cookie},
      method="GET",
    )
    try:
      with urlopen(req, timeout=3) as response:
        return int(getattr(response, "status", 0)) == 200
    except HTTPError:
      return False
    except URLError:
      return False
    except TimeoutError:
      return False
    except OSError:
      return False

  def _require_session(self, is_api: bool) -> bool:
    if self._session_authenticated():
      return True
    if is_api:
      self._send_json(HTTPStatus.UNAUTHORIZED, {"error": "Unauthorized"})
    else:
      self._redirect("/login.html")
    return False

  def _require_same_origin(self) -> bool:
    host = self.headers.get("Host", "").split(",", 1)[0].strip()
    if not host:
      self._send_json(HTTPStatus.FORBIDDEN, {"error": "Origin validation failed."})
      return False

    proto = self.headers.get("X-Forwarded-Proto", "https").split(",", 1)[0].strip() or "https"
    expected = f"{proto}://{host}"

    origin = normalize_origin(self.headers.get("Origin", ""))
    referer = normalize_origin(self.headers.get("Referer", ""))

    if origin:
      if origin == expected:
        return True
      self._send_json(HTTPStatus.FORBIDDEN, {"error": "Origin validation failed."})
      return False

    if referer:
      if referer == expected:
        return True
      self._send_json(HTTPStatus.FORBIDDEN, {"error": "Origin validation failed."})
      return False

    self._send_json(HTTPStatus.FORBIDDEN, {"error": "Origin validation failed."})
    return False

  def _identityctl_json(self, args: list[str], apply_changes: bool = True) -> dict[str, Any]:
    env = dict(os.environ)
    if not apply_changes:
      env["ADGUARDHOME_DOH_IDENTITYCTL_APPLY"] = "0"
    proc = subprocess.run(
      [self.server.config.identityctl, *args],
      stdout=subprocess.PIPE,
      stderr=subprocess.PIPE,
      text=True,
      env=env,
    )
    if proc.returncode != 0:
      detail = (proc.stderr or proc.stdout or "identity command failed").strip()
      raise IdentityWebError(HTTPStatus.BAD_REQUEST, detail)
    output = (proc.stdout or "").strip()
    if not output:
      return {}
    try:
      payload = json.loads(output)
    except json.JSONDecodeError as exc:
      raise IdentityWebError(
        HTTPStatus.INTERNAL_SERVER_ERROR,
        f"identity command returned invalid JSON: {exc}",
      ) from exc
    if not isinstance(payload, dict):
      raise IdentityWebError(HTTPStatus.INTERNAL_SERVER_ERROR, "identity command returned non-object JSON.")
    return payload

  def _request_memo(self) -> dict[str, Any]:
    memo = getattr(self, "_pixel_request_memo", None)
    if isinstance(memo, dict):
      return memo
    memo = {}
    self._pixel_request_memo = memo
    return memo

  def _request_memoize(self, key: str, loader):
    memo = self._request_memo()
    if key not in memo:
      memo[key] = loader()
    return memo[key]

  def _flush_request_state(self) -> None:
    memo = getattr(self, "_pixel_request_memo", None)
    if not isinstance(memo, dict):
      return
    whois_state = memo.get("whois_state")
    if not isinstance(whois_state, dict):
      return
    if not whois_state.get("dirty"):
      return
    cache_payload = whois_state.get("cache")
    if isinstance(cache_payload, dict):
      self._save_ipinfo_cache(cache_payload)
    whois_state["dirty"] = False

  def _json_clone(self, payload: Any) -> Any:
    return copy.deepcopy(payload)

  def _burst_cache_key(self, namespace: str, params: Any) -> str:
    return f"{namespace}:{json.dumps(params, sort_keys=True, separators=(',', ':'))}"

  def _purge_burst_cache_locked(self, now_monotonic: float) -> None:
    stale_keys = [
      key
      for key, entry in self.server.burst_cache.items()
      if entry.get("state") == "ready" and float(entry.get("expires_at", 0.0)) <= now_monotonic
    ]
    for key in stale_keys:
      self.server.burst_cache.pop(key, None)

  def _burst_cache_get(self, namespace: str, params: Any, loader):
    key = self._burst_cache_key(namespace, params)
    wait_timeout = BURST_CACHE_TTL_SECONDS + 1.0

    while True:
      event: threading.Event | None = None
      leader = False
      with self.server.burst_cache_lock:
        now_monotonic = time.monotonic()
        self._purge_burst_cache_locked(now_monotonic)
        entry = self.server.burst_cache.get(key)
        if isinstance(entry, dict) and entry.get("state") == "ready":
          return self._json_clone(entry.get("value"))
        if isinstance(entry, dict) and entry.get("state") == "pending":
          started_at = float(entry.get("started_at", now_monotonic))
          if now_monotonic - started_at <= wait_timeout:
            pending_event = entry.get("event")
            if isinstance(pending_event, threading.Event):
              event = pending_event
          else:
            self.server.burst_cache.pop(key, None)
        if event is None:
          event = threading.Event()
          self.server.burst_cache[key] = {
            "state": "pending",
            "event": event,
            "started_at": now_monotonic,
            "expires_at": now_monotonic + BURST_CACHE_TTL_SECONDS,
          }
          leader = True
      if leader:
        try:
          value = loader()
        except Exception:
          with self.server.burst_cache_lock:
            current = self.server.burst_cache.get(key)
            if isinstance(current, dict) and current.get("event") is event:
              self.server.burst_cache.pop(key, None)
          event.set()
          raise
        stored_value = self._json_clone(value)
        with self.server.burst_cache_lock:
          current = self.server.burst_cache.get(key)
          if isinstance(current, dict) and current.get("event") is event:
            current["state"] = "ready"
            current["value"] = stored_value
            current["expires_at"] = time.monotonic() + BURST_CACHE_TTL_SECONDS
        event.set()
        return self._json_clone(stored_value)
      if event.wait(wait_timeout):
        continue

  def _burst_cache_invalidate(self, namespaces: list[str]) -> None:
    prefixes = tuple(f"{namespace}:" for namespace in namespaces)
    with self.server.burst_cache_lock:
      for key in list(self.server.burst_cache.keys()):
        if key.startswith(prefixes):
          self.server.burst_cache.pop(key, None)

  def _schedule_runtime_reload(self) -> bool:
    disable_reload = os.environ.get("ADGUARDHOME_DOH_IDENTITY_WEB_DISABLE_RELOAD", "0").strip().lower()
    if disable_reload in ("1", "true", "yes", "on"):
      return False

    restart_entry = os.environ.get(
      "ADGUARDHOME_DOH_IDENTITY_WEB_RESTART_ENTRY",
      "/usr/local/bin/adguardhome-start",
    ).strip()
    restart_mode = os.environ.get(
      "ADGUARDHOME_DOH_IDENTITY_WEB_RESTART_MODE",
      "--remote-reload-frontend",
    ).strip()
    if not restart_entry:
      return False
    if not restart_mode:
      restart_mode = "--remote-reload-frontend"
    if not restart_mode.startswith("--"):
      restart_mode = f"--{restart_mode}"
    if not os.path.exists(restart_entry) or not os.access(restart_entry, os.X_OK):
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

    try:
      shell_bin = "/bin/sh"
      if not os.path.exists(shell_bin):
        shell_bin = "/usr/bin/sh"
      if not os.path.exists(shell_bin):
        return False
      subprocess.Popen(
        [shell_bin, "-c", f"sleep 1; exec '{restart_entry}' {shlex.quote(restart_mode)}"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        env=env,
        start_new_session=True,
      )
      return True
    except OSError:
      return False

  def _list_identities(self) -> dict[str, Any]:
    raw = self._identityctl_json(["list", "--json"])
    primary = str(raw.get("primaryIdentityId", ""))
    configured_primary = str(raw.get("configuredPrimaryIdentityId", ""))
    dot_identity_enabled = normalize_bool(raw.get("dotIdentityEnabled", False))
    dot_hostname_base = str(raw.get("dotHostnameBase", "") or "").strip()
    dot_identity_label_length = raw.get("dotIdentityLabelLength", 0)
    now = int(time.time())
    identities_raw = raw.get("identities", [])
    identities: list[dict[str, Any]] = []
    if isinstance(identities_raw, list):
      for entry in identities_raw:
        if not isinstance(entry, dict):
          continue
        token = str(entry.get("token", ""))
        created = entry.get("createdEpochSeconds", 0)
        if not isinstance(created, int):
          created = 0
        expires = entry.get("expiresEpochSeconds")
        if not isinstance(expires, int) or expires <= 0:
          expires = None
        is_expired = bool(isinstance(expires, int) and expires <= now)
        identities.append(
          {
            "id": str(entry.get("id", "")),
            "token": token,
            "tokenMasked": mask_token(token),
            "dotLabel": str(entry.get("dotLabel", "") or "") or None,
            "dotHostname": str(entry.get("dotHostname", "") or "") or None,
            "createdEpochSeconds": max(0, created),
            "expiresEpochSeconds": expires,
            "isExpired": is_expired,
          }
        )
    return {
      "primaryIdentityId": primary,
      "configuredPrimaryIdentityId": configured_primary,
      "dotIdentityEnabled": dot_identity_enabled,
      "dotHostnameBase": dot_hostname_base,
      "dotIdentityLabelLength": int(dot_identity_label_length) if isinstance(dot_identity_label_length, int) else 0,
      "identities": identities,
    }

  def _normalize_usage_payload(self, payload: dict[str, Any]) -> dict[str, Any]:
    identities = payload.get("identities", [])
    if isinstance(identities, list):
      for row in identities:
        if not isinstance(row, dict):
          continue
        if "requestCount" not in row and "requests" in row:
          row["requestCount"] = row.get("requests")
    return payload

  def _usage_payload(self, identity: str, window: str) -> dict[str, Any]:
    args = ["usage", "--json"]
    if identity and identity != "all":
      args.extend(["--identity", identity])
    else:
      args.append("--all")
    if window:
      args.extend(["--window", window])
    payload = self._identityctl_json(args)
    payload = self._augment_usage_payload_with_dot_querylog(payload, identity, window)
    return self._normalize_usage_payload(payload)

  def _cached_identities_payload(self) -> dict[str, Any]:
    return self._burst_cache_get("identities", {}, self._list_identities)

  def _cached_usage_payload(self, identity: str, window: str) -> dict[str, Any]:
    return self._burst_cache_get(
      "usage",
      {"identity": identity or "all", "window": window},
      lambda: self._usage_payload(identity, window),
    )

  def _identity_querylog_metadata_lookup(self) -> dict[str, Any]:
    def load_lookup() -> dict[str, Any]:
      payload = self._cached_identities_payload()
      identities = payload.get("identities", [])
      known_ids: set[str] = set()
      by_client_name: dict[str, str] = {}
      by_dot_label: dict[str, str] = {}
      by_dot_hostname: dict[str, str] = {}
      if isinstance(identities, list):
        for entry in identities:
          if not isinstance(entry, dict):
            continue
          identity_id = str(entry.get("id", "") or "").strip()
          if not identity_id:
            continue
          known_ids.add(identity_id)
          by_client_name[f"Identity {identity_id}".lower()] = identity_id
          dot_label = str(entry.get("dotLabel", "") or "").strip().lower()
          if dot_label:
            by_dot_label[dot_label] = identity_id
          dot_hostname = str(entry.get("dotHostname", "") or "").strip().lower()
          if dot_hostname:
            by_dot_hostname[dot_hostname] = identity_id
      return {
        "knownIds": known_ids,
        "byClientName": by_client_name,
        "byDotLabel": by_dot_label,
        "byDotHostname": by_dot_hostname,
      }

    return self._request_memoize("identity_querylog_metadata_lookup", load_lookup)

  def _normalize_csv_unique(self, raw: str, lowercase: bool = False) -> list[str]:
    out: list[str] = []
    seen: set[str] = set()
    for token in raw.split(","):
      value = token.strip()
      if lowercase:
        value = value.lower()
      if not value or value in seen:
        continue
      seen.add(value)
      out.append(value)
    return out

  def _querylog_internal_clients(self) -> tuple[list[str], str]:
    raw = os.environ.get(
      "ADGUARDHOME_DOH_IDENTITY_WEB_INTERNAL_QUERYLOG_CLIENTS",
      INTERNAL_QUERYLOG_CLIENTS_DEFAULT,
    ).strip()
    clients = self._normalize_csv_unique(raw, lowercase=False)
    if not clients:
      clients = self._normalize_csv_unique(INTERNAL_QUERYLOG_CLIENTS_DEFAULT, lowercase=False)
    return clients, ",".join(clients)

  def _querylog_probe_domains(self) -> tuple[list[str], str]:
    raw = os.environ.get(
      "ADGUARDHOME_DOH_IDENTITY_WEB_INTERNAL_PROBE_DOMAINS",
      INTERNAL_PROBE_DOMAINS_DEFAULT,
    ).strip()
    domains = self._normalize_csv_unique(raw, lowercase=True)
    if not domains:
      domains = self._normalize_csv_unique(INTERNAL_PROBE_DOMAINS_DEFAULT, lowercase=True)
    return domains, ",".join(domains)

  def _default_querylog_limit(self) -> int:
    raw = os.environ.get("ADGUARDHOME_DOH_IDENTITY_WEB_QUERYLOG_LIMIT_DEFAULT", str(QUERYLOG_LIMIT_DEFAULT)).strip()
    if raw.isdigit():
      value = int(raw)
    else:
      value = QUERYLOG_LIMIT_DEFAULT
    return max(QUERYLOG_LIMIT_MIN, min(QUERYLOG_LIMIT_MAX, value))

  def _parse_querylog_limit(self, query: dict[str, list[str]]) -> int:
    raw = (query.get("limit") or [str(self._default_querylog_limit())])[0].strip()
    if not raw.isdigit():
      raise IdentityWebError(
        HTTPStatus.BAD_REQUEST,
        f"Invalid querylog limit. Expected integer {QUERYLOG_LIMIT_MIN}-{QUERYLOG_LIMIT_MAX}.",
      )
    value = int(raw)
    if value < QUERYLOG_LIMIT_MIN or value > QUERYLOG_LIMIT_MAX:
      raise IdentityWebError(
        HTTPStatus.BAD_REQUEST,
        f"Invalid querylog limit. Expected integer {QUERYLOG_LIMIT_MIN}-{QUERYLOG_LIMIT_MAX}.",
      )
    return value

  def _parse_querylog_view(self, query: dict[str, list[str]]) -> str:
    raw = (query.get("view") or ["user_only"])[0].strip().lower()
    if raw not in ("user_only", "all"):
      raise IdentityWebError(HTTPStatus.BAD_REQUEST, "Invalid querylog view. Expected user_only or all.")
    return raw

  def _querylog_payload(self, limit: int) -> dict[str, Any]:
    fixture_file = os.environ.get("ADGUARDHOME_DOH_IDENTITY_WEB_QUERYLOG_JSON_FILE", "").strip()
    if fixture_file:
      try:
        with open(fixture_file, "r", encoding="utf-8") as handle:
          raw = handle.read()
      except OSError as exc:
        raise QuerylogFetchError("fetch_failed", f"Failed to read querylog fixture: {exc}") from exc
      try:
        payload = json.loads(raw)
      except json.JSONDecodeError as exc:
        raise QuerylogFetchError("invalid_json", f"Querylog fixture contains invalid JSON: {exc}") from exc
      if not isinstance(payload, dict) or not isinstance(payload.get("data"), list):
        raise QuerylogFetchError("invalid_json", "Querylog fixture missing data array.")
      return payload

    headers = {"Accept": "application/json"}
    cookie = self.headers.get("Cookie", "").strip()
    if cookie:
      headers["Cookie"] = cookie
    req = Request(
      url=f"http://127.0.0.1:{self.server.config.adguard_web_port}/control/querylog?limit={limit}",
      headers=headers,
      method="GET",
    )
    try:
      with urlopen(req, timeout=5) as response:
        body = response.read().decode("utf-8", errors="replace")
    except HTTPError as exc:
      if exc.code in (HTTPStatus.UNAUTHORIZED, HTTPStatus.FORBIDDEN):
        raise QuerylogFetchError("unauthorized", "AdGuard querylog request was unauthorized.") from exc
      raise QuerylogFetchError("fetch_failed", f"AdGuard querylog request failed with status {exc.code}.") from exc
    except URLError as exc:
      raise QuerylogFetchError("fetch_failed", f"AdGuard querylog request failed: {exc}") from exc
    except TimeoutError as exc:
      raise QuerylogFetchError("fetch_failed", f"AdGuard querylog request timed out: {exc}") from exc
    except OSError as exc:
      raise QuerylogFetchError("fetch_failed", f"AdGuard querylog request failed: {exc}") from exc

    try:
      payload = json.loads(body)
    except json.JSONDecodeError as exc:
      raise QuerylogFetchError("invalid_json", f"AdGuard querylog response contains invalid JSON: {exc}") from exc
    if not isinstance(payload, dict) or not isinstance(payload.get("data"), list):
      raise QuerylogFetchError("invalid_json", "AdGuard querylog response missing data array.")
    return payload

  def _querylog_rows(self, limit: int) -> list[dict[str, Any]]:
    payload = self._querylog_payload(limit)
    rows = payload.get("data")
    if not isinstance(rows, list):
      raise QuerylogFetchError("invalid_json", "AdGuard querylog response missing data array.")
    out: list[dict[str, Any]] = []
    for row in rows:
      if isinstance(row, dict):
        out.append(row)
    return out

  def _querylog_proto(self, row: dict[str, Any]) -> str:
    raw = str(row.get("client_proto", "")).strip().lower()
    return raw if raw else "plain"

  def _querylog_qname(self, row: dict[str, Any]) -> str:
    question = row.get("question")
    if isinstance(question, dict):
      return str(question.get("name", "")).strip().lower()
    return ""

  def _querylog_top_clients(self, rows: list[dict[str, Any]]) -> str:
    if not rows:
      return "none"
    counts: Counter[tuple[str, str]] = Counter()
    for row in rows:
      client = str(row.get("client", "")).strip() or "unknown"
      proto = self._querylog_proto(row)
      counts[(client, proto)] += 1
    ranked = sorted(counts.items(), key=lambda item: (-item[1], item[0][0], item[0][1]))[:10]
    if not ranked:
      return "none"
    return ";".join(f"{client}:{proto}:{count}" for ((client, proto), count) in ranked)

  def _querylog_count_doh(self, rows: list[dict[str, Any]]) -> int:
    return sum(1 for row in rows if self._querylog_proto(row) == "doh")

  def _querylog_count_client_doh(self, rows: list[dict[str, Any]], client_ip: str) -> int:
    if not client_ip:
      return 0
    return sum(1 for row in rows if str(row.get("client", "")).strip() == client_ip and self._querylog_proto(row) == "doh")

  def _querylog_effective_rows(self, rows: list[dict[str, Any]], include_internal: bool) -> tuple[list[dict[str, Any]], list[dict[str, Any]], list[dict[str, Any]], str, str, Counter[str]]:
    internal_clients, internal_clients_csv = self._querylog_internal_clients()
    internal_probe_domains, internal_probe_domains_csv = self._querylog_probe_domains()
    internal_client_set = set(internal_clients)
    internal_probe_set = set(internal_probe_domains)

    internal_rows: list[dict[str, Any]] = []
    user_rows: list[dict[str, Any]] = []
    internal_probe_counts: Counter[str] = Counter()
    for row in rows:
      client = str(row.get("client", "")).strip()
      is_internal = client in internal_client_set
      if is_internal:
        internal_rows.append(row)
      else:
        user_rows.append(row)
      qname = self._querylog_qname(row)
      if is_internal and qname and qname in internal_probe_set:
        internal_probe_counts[qname] += 1

    effective_rows = rows if include_internal else user_rows
    return effective_rows, user_rows, internal_rows, internal_clients_csv, internal_probe_domains_csv, internal_probe_counts

  def _summarize_querylog(self, rows: list[dict[str, Any]], view_mode: str, limit: int) -> dict[str, Any]:
    include_internal = view_mode == "all"
    effective_rows, user_rows, internal_rows, internal_clients_csv, internal_probe_domains_csv, internal_probe_counts = self._querylog_effective_rows(
      rows,
      include_internal,
    )
    total_doh_count = self._querylog_count_doh(effective_rows)
    gateway_ip = os.environ.get(
      "ADGUARDHOME_DOH_IDENTITY_WEB_LAN_GATEWAY_IP",
      os.environ.get("ADGUARDHOME_REMOTE_ROUTER_LAN_IP", ""),
    ).strip()
    gateway_doh_count = self._querylog_count_client_doh(effective_rows, gateway_ip)
    if total_doh_count <= 0:
      gateway_share_pct = "0.00"
    else:
      gateway_share_pct = f"{(gateway_doh_count * 100.0) / total_doh_count:.2f}"
    if internal_probe_counts:
      internal_probe_domain_counts = ";".join(
        f"{domain}:{count}" for domain, count in sorted(internal_probe_counts.items(), key=lambda item: (-item[1], item[0]))
      )
    else:
      internal_probe_domain_counts = "none"

    return {
      "querylog_status": "ok",
      "querylog_view_mode": view_mode,
      "querylog_limit": limit,
      "include_internal_querylog": 1 if include_internal else 0,
      "internal_querylog_clients": internal_clients_csv,
      "internal_probe_domains": internal_probe_domains_csv,
      "total_query_count": len(effective_rows),
      "total_doh_count": total_doh_count,
      "gateway_doh_count": gateway_doh_count,
      "gateway_share_pct": gateway_share_pct,
      "user_total_count": len(user_rows),
      "user_doh_count": self._querylog_count_doh(user_rows),
      "internal_total_count": len(internal_rows),
      "internal_doh_count": self._querylog_count_doh(internal_rows),
      "top_clients": self._querylog_top_clients(effective_rows),
      "top_clients_user": self._querylog_top_clients(user_rows),
      "top_clients_internal": self._querylog_top_clients(internal_rows),
      "internal_probe_domain_counts": internal_probe_domain_counts,
    }

  def _identity_label(self, identity_id: str) -> str:
    normalized = str(identity_id or "").strip()
    return IDENTITY_LABELS.get(normalized, normalized or "Unknown identity")

  def _querylog_internal_loopback_suffixes(self) -> list[str]:
    raw = os.environ.get(
      "ADGUARDHOME_DOH_IDENTITY_WEB_INTERNAL_LOOPBACK_DOMAIN_SUFFIXES",
      INTERNAL_LOOPBACK_DOMAIN_SUFFIXES_DEFAULT,
    ).strip()
    return self._normalize_csv_unique(raw, lowercase=True)

  def _fixture_payload(self, env_var: str) -> Any | None:
    fixture_file = os.environ.get(env_var, "").strip()
    if not fixture_file:
      return None
    try:
      with open(fixture_file, "r", encoding="utf-8") as handle:
        return json.loads(handle.read())
    except OSError as exc:
      raise IdentityWebError(HTTPStatus.BAD_GATEWAY, f"Failed to read fixture {fixture_file}: {exc}") from exc
    except json.JSONDecodeError as exc:
      raise IdentityWebError(HTTPStatus.BAD_GATEWAY, f"Fixture {fixture_file} contains invalid JSON: {exc}") from exc

  def _adguard_json(
    self,
    path: str,
    *,
    query: dict[str, list[str]] | None = None,
    method: str = "GET",
    body: bytes | None = None,
    fixture_env: str | None = None,
  ) -> Any:
    if fixture_env:
      fixture = self._fixture_payload(fixture_env)
      if fixture is not None:
        return fixture

    url = f"http://127.0.0.1:{self.server.config.adguard_web_port}{path}"
    if query:
      encoded = urlencode([(key, item) for key, values in query.items() for item in values])
      if encoded:
        url = f"{url}?{encoded}"

    headers = {"Accept": "application/json"}
    cookie = self.headers.get("Cookie", "").strip()
    if cookie:
      headers["Cookie"] = cookie
    if body is not None:
      headers["Content-Type"] = self.headers.get("Content-Type", "application/json")

    req = Request(url=url, headers=headers, method=method.upper(), data=body)
    try:
      with urlopen(req, timeout=5) as response:
        raw = response.read().decode("utf-8", errors="replace")
    except HTTPError as exc:
      detail = f"AdGuard request failed with status {exc.code} for {path}."
      raise IdentityWebError(HTTPStatus.BAD_GATEWAY, detail) from exc
    except URLError as exc:
      raise IdentityWebError(HTTPStatus.BAD_GATEWAY, f"AdGuard request failed for {path}: {exc}") from exc
    except TimeoutError as exc:
      raise IdentityWebError(HTTPStatus.BAD_GATEWAY, f"AdGuard request timed out for {path}: {exc}") from exc
    except OSError as exc:
      raise IdentityWebError(HTTPStatus.BAD_GATEWAY, f"AdGuard request failed for {path}: {exc}") from exc

    try:
      return json.loads(raw)
    except json.JSONDecodeError as exc:
      raise IdentityWebError(HTTPStatus.BAD_GATEWAY, f"AdGuard response for {path} contains invalid JSON: {exc}") from exc

  def _status_payload(self) -> dict[str, Any]:
    def load_payload() -> dict[str, Any]:
      payload = self._adguard_json("/control/status", fixture_env="ADGUARDHOME_DOH_IDENTITY_WEB_STATUS_JSON_FILE")
      if not isinstance(payload, dict):
        raise IdentityWebError(HTTPStatus.BAD_GATEWAY, "AdGuard status response must be a JSON object.")
      return payload

    return self._request_memoize("status_payload", load_payload)

  def _stats_payload_raw(self, query: dict[str, list[str]]) -> dict[str, Any]:
    payload = self._adguard_json("/control/stats", query=query, fixture_env="ADGUARDHOME_DOH_IDENTITY_WEB_STATS_JSON_FILE")
    if not isinstance(payload, dict):
      raise IdentityWebError(HTTPStatus.BAD_GATEWAY, "AdGuard stats response must be a JSON object.")
    return payload

  def _clients_payload_raw(self) -> dict[str, Any]:
    payload = self._adguard_json("/control/clients", fixture_env="ADGUARDHOME_DOH_IDENTITY_WEB_CLIENTS_JSON_FILE")
    if not isinstance(payload, dict):
      raise IdentityWebError(HTTPStatus.BAD_GATEWAY, "AdGuard clients response must be a JSON object.")
    return payload

  def _client_search_payload_raw(self, body: bytes) -> Any:
    return self._adguard_json(
      "/control/clients/search",
      method="POST",
      body=body,
      fixture_env="ADGUARDHOME_DOH_IDENTITY_WEB_CLIENTS_SEARCH_JSON_FILE",
    )

  def _adguard_querylog_payload(self, query: dict[str, list[str]]) -> dict[str, Any]:
    fixture = self._fixture_payload("ADGUARDHOME_DOH_IDENTITY_WEB_QUERYLOG_JSON_FILE")
    if fixture is not None:
      if not isinstance(fixture, dict) or not isinstance(fixture.get("data"), list):
        raise IdentityWebError(HTTPStatus.BAD_GATEWAY, "Querylog fixture missing data array.")
      older_than = ((query.get("older_than") or [""])[0]).strip()
      older_than_ms = self._parse_querylog_row_time_ms({"time": older_than})
      rows = [row for row in fixture.get("data", []) if isinstance(row, dict)]
      rows.sort(key=self._parse_querylog_row_time_ms, reverse=True)
      if older_than_ms > 0:
        rows = [row for row in rows if self._parse_querylog_row_time_ms(row) < older_than_ms]
      limit = self._parse_querylog_limit(query)
      page_rows = rows[:limit]
      response = dict(fixture)
      response["data"] = page_rows
      response["oldest"] = self._querylog_oldest_value(page_rows, fallback=older_than if older_than_ms > 0 else "")
      return response
    payload = self._adguard_json("/control/querylog", query=query)
    if not isinstance(payload, dict) or not isinstance(payload.get("data"), list):
      raise IdentityWebError(HTTPStatus.BAD_GATEWAY, "AdGuard querylog response missing data array.")
    return payload

  def _configured_router_lan_ip(self) -> str:
    return os.environ.get(
      "ADGUARDHOME_DOH_IDENTITY_WEB_LAN_GATEWAY_IP",
      os.environ.get("ADGUARDHOME_REMOTE_ROUTER_LAN_IP", ""),
    ).strip()

  def _is_ip(self, value: str) -> bool:
    try:
      ipaddress.ip_address(value.strip())
      return True
    except ValueError:
      return False

  def _is_loopback_ip(self, value: str) -> bool:
    try:
      return ipaddress.ip_address(value.strip()).is_loopback
    except ValueError:
      return False

  def _is_public_ip(self, value: str) -> bool:
    try:
      parsed = ipaddress.ip_address(value.strip())
    except ValueError:
      return False
    return not (
      parsed.is_private or
      parsed.is_loopback or
      parsed.is_link_local or
      parsed.is_multicast or
      parsed.is_reserved or
      parsed.is_unspecified
    )

  def _detect_device_lan_ip(self, status_payload: dict[str, Any] | None = None) -> str:
    override = os.environ.get("ADGUARDHOME_DOH_IDENTITY_WEB_DEVICE_LAN_IP", "").strip()
    router_lan_ip = self._configured_router_lan_ip()
    if override:
      try:
        parsed = ipaddress.ip_address(override)
        if parsed.version == 4 and parsed.is_private and not parsed.is_loopback and override != router_lan_ip:
          return override
      except ValueError:
        pass

    status = status_payload if isinstance(status_payload, dict) else self._status_payload()
    dns_addresses = status.get("dns_addresses")
    if not isinstance(dns_addresses, list):
      return ""
    for raw in dns_addresses:
      candidate = str(raw or "").strip()
      try:
        parsed = ipaddress.ip_address(candidate)
      except ValueError:
        continue
      if parsed.version != 4 or not parsed.is_private or parsed.is_loopback:
        continue
      if candidate == router_lan_ip:
        continue
      return candidate
    return ""

  def _ipinfo_token(self) -> str:
    token_file = os.environ.get(
      "ADGUARDHOME_DOH_IDENTITY_WEB_IPINFO_LITE_TOKEN_FILE",
      os.environ.get("ADGUARDHOME_IPINFO_LITE_TOKEN_FILE", ""),
    ).strip()
    if not token_file:
      return ""
    try:
      with open(token_file, "r", encoding="utf-8") as handle:
        return handle.readline().strip()
    except OSError:
      return ""

  def _ipinfo_cache_file(self) -> str:
    return os.environ.get(
      "ADGUARDHOME_DOH_IDENTITY_WEB_IPINFO_CACHE_FILE",
      "/tmp/pixel-stack-ipinfo-lite-cache.json",
    ).strip()

  def _whois_request_state(self) -> dict[str, Any]:
    memo = self._request_memo()
    state = memo.get("whois_state")
    if isinstance(state, dict):
      return state
    state = {
      "cache": self._load_ipinfo_cache(),
      "dirty": False,
      "memo": {},
    }
    memo["whois_state"] = state
    return state

  def _load_ipinfo_cache(self) -> dict[str, Any]:
    cache_file = self._ipinfo_cache_file()
    if not cache_file or not os.path.exists(cache_file):
      return {}
    try:
      with open(cache_file, "r", encoding="utf-8") as handle:
        payload = json.loads(handle.read())
    except (OSError, json.JSONDecodeError):
      return {}
    return payload if isinstance(payload, dict) else {}

  def _save_ipinfo_cache(self, payload: dict[str, Any]) -> None:
    cache_file = self._ipinfo_cache_file()
    if not cache_file:
      return
    tmpfile = f"{cache_file}.tmp"
    os.makedirs(os.path.dirname(cache_file), exist_ok=True)
    try:
      with open(tmpfile, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, separators=(",", ":"), sort_keys=False)
      os.replace(tmpfile, cache_file)
    except OSError:
      try:
        if os.path.exists(tmpfile):
          os.remove(tmpfile)
      except OSError:
        pass

  def _normalize_whois_info(self, payload: Any) -> dict[str, str]:
    if not isinstance(payload, dict):
      return {}
    out: dict[str, str] = {}
    country = str(payload.get("country", "") or "").strip()
    orgname = str(payload.get("orgname", "") or "").strip()
    if country:
      out["country"] = country
    if orgname:
      out["orgname"] = orgname
    return out

  def _lookup_ipinfo_lite(self, ip: str, *, allow_network: bool = True) -> dict[str, str]:
    normalized_ip = ip.strip()
    if not normalized_ip or not self._is_public_ip(normalized_ip):
      return {}

    request_state = self._whois_request_state()
    memoized = request_state.get("memo", {}).get(normalized_ip)
    if isinstance(memoized, dict):
      return dict(memoized)

    now = int(time.time())
    cache = request_state.get("cache")
    if not isinstance(cache, dict):
      cache = self._load_ipinfo_cache()
      request_state["cache"] = cache
    cached = cache.get(normalized_ip)
    if isinstance(cached, dict):
      cached_at = cached.get("cachedAtEpochSeconds", 0)
      whois_info = self._normalize_whois_info(cached.get("whois_info", {}))
      if isinstance(cached_at, int) and now - cached_at < IPINFO_CACHE_TTL_SECONDS and whois_info:
        request_state.setdefault("memo", {})[normalized_ip] = whois_info
        return dict(whois_info)
      if not allow_network and whois_info:
        request_state.setdefault("memo", {})[normalized_ip] = whois_info
        return dict(whois_info)

    if not allow_network:
      request_state.setdefault("memo", {})[normalized_ip] = {}
      return {}

    token = self._ipinfo_token()
    if not token:
      if isinstance(cached, dict):
        whois_info = self._normalize_whois_info(cached.get("whois_info", {}))
        request_state.setdefault("memo", {})[normalized_ip] = whois_info
        return dict(whois_info)
      return {}

    req = Request(
      url=f"https://api.ipinfo.io/lite/{quote(normalized_ip)}?token={quote(token)}",
      headers={"Accept": "application/json"},
      method="GET",
    )
    try:
      with urlopen(req, timeout=IPINFO_LOOKUP_TIMEOUT_SECONDS) as response:
        payload = json.loads(response.read().decode("utf-8", errors="replace"))
      if not isinstance(payload, dict):
        raise ValueError("invalid response shape")
      country = str(payload.get("country_code") or payload.get("country") or "").strip()
      orgname = str(
        payload.get("as_name") or
        payload.get("name") or
        payload.get("as_domain") or
        ""
      ).strip()
      whois_info = self._normalize_whois_info({"country": country, "orgname": orgname})
      if whois_info:
        cache[normalized_ip] = {
          "cachedAtEpochSeconds": now,
          "whois_info": whois_info,
        }
        request_state["dirty"] = True
      request_state.setdefault("memo", {})[normalized_ip] = whois_info
      return dict(whois_info)
    except Exception:
      if isinstance(cached, dict):
        whois_info = self._normalize_whois_info(cached.get("whois_info", {}))
        request_state.setdefault("memo", {})[normalized_ip] = whois_info
        return dict(whois_info)
      return {}

  def _merged_whois_info(self, ip: str, existing: Any, *, allow_network: bool = True) -> dict[str, str]:
    normalized = self._normalize_whois_info(existing)
    if normalized:
      return normalized
    return self._lookup_ipinfo_lite(ip, allow_network=allow_network)

  def _identity_event_window_for_seconds(self, window_seconds: int) -> str:
    if window_seconds <= 24 * 3600:
      return "24h"
    if window_seconds <= 7 * 86400:
      return "7d"
    return QUERYLOG_EVENT_WINDOW_DEFAULT

  def _querylog_event_window_for_rows(self, rows: list[dict[str, Any]]) -> str:
    row_times = [self._parse_querylog_row_time_ms(row) for row in rows]
    valid_times = [value for value in row_times if value > 0]
    if not valid_times:
      return QUERYLOG_EVENT_WINDOW_DEFAULT
    now_ms = int(time.time() * 1000)
    oldest_ms = min(valid_times)
    age_seconds = max(0, (now_ms - oldest_ms) // 1000)
    return self._identity_event_window_for_seconds(age_seconds)

  def _parse_querylog_row_time_ms(self, row: dict[str, Any]) -> int:
    raw = str(row.get("time", "") or "").strip()
    if not raw:
      return 0
    text = raw[:-1] + "+00:00" if raw.endswith("Z") else raw
    try:
      parsed = datetime.fromisoformat(text)
    except ValueError:
      return 0
    if parsed.tzinfo is None:
      parsed = parsed.replace(tzinfo=timezone.utc)
    return int(round(parsed.timestamp() * 1000.0))

  def _parse_duration_seconds(self, raw: str, default_seconds: int = 7 * 86400) -> int:
    value = str(raw or "").strip().lower()
    if not value:
      return default_seconds
    match = re.fullmatch(r"(\d+)([smhdw])", value)
    if not match:
      return default_seconds
    amount = int(match.group(1))
    unit = match.group(2)
    multiplier = {
      "s": 1,
      "m": 60,
      "h": 3600,
      "d": 86400,
      "w": 7 * 86400,
    }.get(unit, 86400)
    return max(1, amount * multiplier)

  def _querylog_elapsed_ms(self, row: dict[str, Any]) -> int:
    raw = str(row.get("elapsedMs", "") or "").strip()
    if not raw:
      return 0
    try:
      return max(0, int(round(float(raw))))
    except ValueError:
      return 0

  def _querylog_status(self, row: dict[str, Any]) -> str:
    return str(row.get("status", "") or "").strip().upper()

  def _is_internal_loopback_row(self, row: dict[str, Any]) -> bool:
    client = str(row.get("client", "")).strip()
    if not self._is_loopback_ip(client):
      return False
    qname = self._querylog_qname(row)
    if not qname:
      return True
    probe_domains = set(self._querylog_probe_domains()[0])
    if qname in probe_domains:
      return True
    for suffix in self._querylog_internal_loopback_suffixes():
      if qname == suffix or qname.endswith(f".{suffix}"):
        return True
    return False

  def _dot_access_log_file(self) -> str:
    return os.environ.get(
      "ADGUARDHOME_DOT_ACCESS_LOG_FILE",
      os.environ.get("ADGUARDHOME_DOH_IDENTITY_WEB_DOT_ACCESS_LOG_FILE", DOT_ACCESS_LOG_FILE_DEFAULT),
    ).strip()

  def _parse_dot_access_log_line(self, line: str, metadata_lookup: dict[str, Any] | None = None) -> dict[str, Any] | None:
    parts = line.rstrip("\n").split("\t")
    if len(parts) < 5:
      return None

    lookup = metadata_lookup if isinstance(metadata_lookup, dict) else self._identity_querylog_metadata_lookup()
    by_dot_label = lookup.get("byDotLabel")
    by_dot_hostname = lookup.get("byDotHostname")
    if not isinstance(by_dot_label, dict):
      by_dot_label = {}
    if not isinstance(by_dot_hostname, dict):
      by_dot_hostname = {}

    ts_raw, sni_raw, _status_raw, session_time_raw, client_ip_raw = parts[:5]
    sni = str(sni_raw or "").strip().lower()
    client_ip = str(client_ip_raw or "").strip()
    if not sni or not client_ip:
      return None

    identity_id = by_dot_hostname.get(sni, "")
    if not identity_id:
      dot_label = sni.split(".", 1)[0].strip().lower()
      identity_id = by_dot_label.get(dot_label, "")
    if not identity_id:
      return None

    end_ms = 0
    if len(parts) >= 6:
      try:
        end_ms = max(0, int(round(float(parts[5].strip()) * 1000.0)))
      except ValueError:
        end_ms = 0
    if end_ms <= 0:
      end_ms = self._parse_querylog_row_time_ms({"time": ts_raw})
    if end_ms <= 0:
      return None

    try:
      duration_ms = max(0, int(round(float(session_time_raw.strip()) * 1000.0)))
    except ValueError:
      duration_ms = 0
    return {
      "identityId": identity_id,
      "clientIp": client_ip,
      "startMs": max(0, end_ms - duration_ms),
      "endMs": end_ms,
      "durationMs": duration_ms,
    }

  def _dot_sessions(self, window: str = QUERYLOG_EVENT_WINDOW_DEFAULT) -> list[dict[str, Any]]:
    def load_sessions() -> list[dict[str, Any]]:
      log_file = self._dot_access_log_file()
      if not log_file or not os.path.exists(log_file):
        return []

      metadata_lookup = self._identity_querylog_metadata_lookup()
      floor_ms = int(time.time() * 1000) - (self._parse_duration_seconds(window, default_seconds=30 * 86400) * 1000)
      sessions: list[dict[str, Any]] = []
      try:
        with open(log_file, "r", encoding="utf-8", errors="replace") as handle:
          for line in handle:
            parsed = self._parse_dot_access_log_line(line, metadata_lookup=metadata_lookup)
            if not parsed:
              continue
            if int(parsed.get("endMs", 0)) < floor_ms:
              continue
            sessions.append(parsed)
      except OSError:
        return []

      sessions.sort(key=lambda entry: (str(entry.get("identityId", "")), int(entry.get("startMs", 0)), int(entry.get("endMs", 0))))
      return sessions

    return self._request_memoize(
      f"dot_sessions:{window}",
      lambda: self._burst_cache_get("dot_sessions", {"window": window}, load_sessions),
    )

  def _dot_sessions_by_identity(self, window: str = QUERYLOG_EVENT_WINDOW_DEFAULT) -> dict[str, list[dict[str, Any]]]:
    def load_index() -> dict[str, list[dict[str, Any]]]:
      indexed: dict[str, list[dict[str, Any]]] = {}
      for entry in self._dot_sessions(window):
        identity_id = str(entry.get("identityId", "") or "").strip()
        if not identity_id:
          continue
        indexed.setdefault(identity_id, []).append(dict(entry))
      return indexed

    return self._request_memoize(
      f"dot_sessions_by_identity:{window}",
      lambda: self._burst_cache_get("dot_sessions_by_identity", {"window": window}, load_index),
    )

  def _identity_events(self, window: str = QUERYLOG_EVENT_WINDOW_DEFAULT) -> list[dict[str, Any]]:
    def load_events() -> list[dict[str, Any]]:
      payload = self._identityctl_json(
        ["events", "--window", window, "--limit", "50000", "--json"],
        apply_changes=False,
      )
      events = payload.get("events", [])
      out: list[dict[str, Any]] = []
      if isinstance(events, list):
        for entry in events:
          if not isinstance(entry, dict):
            continue
          client_ip = str(entry.get("clientIp", "") or "").strip()
          identity_id = str(entry.get("identityId", "") or "").strip()
          ts_ms = entry.get("tsMs")
          if not client_ip or not identity_id or not isinstance(ts_ms, int) or ts_ms <= 0:
            continue
          out.append(
            {
              "clientIp": client_ip,
              "identityId": identity_id,
              "tsMs": ts_ms,
              "requestTimeMs": int(entry.get("requestTimeMs", 0)) if isinstance(entry.get("requestTimeMs", 0), int) else 0,
            }
          )
      out.sort(key=lambda entry: (entry["clientIp"], entry["tsMs"]))
      return out

    return self._request_memoize(
      f"identity_events:{window}",
      lambda: self._burst_cache_get("identity_events", {"window": window}, load_events),
    )

  def _identity_events_by_client(self, window: str = QUERYLOG_EVENT_WINDOW_DEFAULT) -> dict[str, list[dict[str, Any]]]:
    def load_index() -> dict[str, list[dict[str, Any]]]:
      indexed: dict[str, list[dict[str, Any]]] = {}
      for entry in self._identity_events(window):
        client_ip = str(entry.get("clientIp", "") or "").strip()
        if not client_ip:
          continue
        indexed.setdefault(client_ip, []).append(dict(entry))
      return indexed

    return self._request_memoize(
      f"identity_events_by_client:{window}",
      lambda: self._burst_cache_get("identity_events_by_client", {"window": window}, load_index),
    )

  def _identity_event_times_ms(self, identity_id: str, window: str = QUERYLOG_EVENT_WINDOW_DEFAULT) -> list[int]:
    normalized_identity = str(identity_id or "").strip()

    def load_times() -> list[int]:
      times: list[int] = []
      for entry in self._identity_events(window):
        if str(entry.get("identityId", "") or "").strip() != normalized_identity:
          continue
        ts_ms = entry.get("tsMs")
        if isinstance(ts_ms, int) and ts_ms > 0:
          times.append(ts_ms)
      times.sort(reverse=True)
      return times

    return self._request_memoize(
      f"identity_event_times_ms:{window}:{normalized_identity}",
      lambda: self._burst_cache_get(
        "identity_event_times_ms",
        {"window": window, "identity": normalized_identity},
        load_times,
      ),
    )

  def _match_identity_event(
    self,
    client_ip: str,
    row_time_ms: int,
    elapsed_ms: int,
    events_by_client: dict[str, list[dict[str, Any]]],
  ) -> str:
    candidates = events_by_client.get(client_ip)
    if not candidates or row_time_ms <= 0:
      return ""

    matches: list[tuple[int, int, int]] = []
    for idx, entry in enumerate(candidates):
      diff = abs(int(entry.get("tsMs", 0)) - row_time_ms)
      if diff > QUERYLOG_IDENTITY_MAX_SKEW_MS:
        continue
      request_time_ms = int(entry.get("requestTimeMs", 0))
      duration_diff = abs(request_time_ms - elapsed_ms) if request_time_ms > 0 and elapsed_ms > 0 else 0
      if request_time_ms > 0 and elapsed_ms > 0 and duration_diff > QUERYLOG_IDENTITY_MAX_DURATION_SKEW_MS:
        continue
      matches.append((idx, diff, duration_diff))

    if not matches:
      return ""

    matches.sort(key=lambda item: (item[1], item[2], item[0]))
    best_idx, best_diff, best_duration_diff = matches[0]
    if len(matches) > 1:
      second_idx, second_diff, second_duration_diff = matches[1]
      if (best_diff, best_duration_diff) == (second_diff, second_duration_diff):
        return ""

    identity_id = str(candidates.pop(best_idx).get("identityId", "") or "").strip()
    return identity_id

  def _match_dot_session_client_ip(
    self,
    identity_id: str,
    row_time_ms: int,
    sessions_by_identity: dict[str, list[dict[str, Any]]],
  ) -> str:
    normalized_identity = str(identity_id or "").strip()
    candidates = sessions_by_identity.get(normalized_identity)
    if not candidates or row_time_ms <= 0:
      return ""

    matches: list[tuple[int, int, int, str]] = []
    for entry in candidates:
      start_ms = int(entry.get("startMs", 0))
      end_ms = int(entry.get("endMs", 0))
      if start_ms > 0 and row_time_ms + QUERYLOG_DOT_SESSION_MAX_SKEW_MS < start_ms:
        continue
      if end_ms > 0 and row_time_ms - QUERYLOG_DOT_SESSION_MAX_SKEW_MS > end_ms:
        continue
      duration_ms = int(entry.get("durationMs", 0))
      if start_ms > 0:
        distance_ms = min(abs(row_time_ms - start_ms), abs(row_time_ms - end_ms))
      else:
        distance_ms = abs(row_time_ms - end_ms)
      client_ip = str(entry.get("clientIp", "") or "").strip()
      if not client_ip:
        continue
      matches.append((distance_ms, duration_ms, -end_ms, client_ip))

    if not matches:
      return ""

    matches.sort(key=lambda item: (item[0], item[1], item[2], item[3]))
    best = matches[0]
    if len(matches) > 1 and best[:3] == matches[1][:3] and best[3] != matches[1][3]:
      return ""
    return best[3]

  def _querylog_identity_from_metadata(self, row: dict[str, Any], metadata_lookup: dict[str, Any] | None = None) -> str:
    client_info = row.get("client_info")
    if not isinstance(client_info, dict):
      return ""

    lookup = metadata_lookup if isinstance(metadata_lookup, dict) else self._identity_querylog_metadata_lookup()
    known_ids = lookup.get("knownIds")
    by_client_name = lookup.get("byClientName")
    by_dot_label = lookup.get("byDotLabel")
    by_dot_hostname = lookup.get("byDotHostname")
    if not isinstance(known_ids, set):
      known_ids = set()
    if not isinstance(by_client_name, dict):
      by_client_name = {}
    if not isinstance(by_dot_label, dict):
      by_dot_label = {}
    if not isinstance(by_dot_hostname, dict):
      by_dot_hostname = {}

    client_name = str(client_info.get("name", "") or "").strip()
    if client_name:
      identity_id = by_client_name.get(client_name.lower(), "")
      if identity_id:
        return identity_id
      if client_name.startswith("Identity "):
        candidate = client_name[len("Identity "):].strip().lower()
        if IDENTITY_ID_PATTERN.fullmatch(candidate) and candidate in known_ids:
          return candidate

    disallowed_rule = str(client_info.get("disallowed_rule", "") or "").strip().lower()
    if disallowed_rule:
      identity_id = by_dot_hostname.get(disallowed_rule, "")
      if identity_id:
        return identity_id
      identity_id = by_dot_label.get(disallowed_rule, "")
      if identity_id:
        return identity_id

    return ""

  def _set_querylog_row_identity(self, row: dict[str, Any], identity_id: str) -> None:
    normalized_identity = str(identity_id or "").strip()
    if not normalized_identity:
      return
    row["pixelIdentityId"] = normalized_identity
    row["pixelIdentity"] = {
      "id": normalized_identity,
      "label": self._identity_label(normalized_identity),
    }

  def _identity_filter_scan_floor_ms(self, identity_filter: str, limit: int) -> int:
    normalized_identity = str(identity_filter or "").strip()
    if not normalized_identity or limit <= 0:
      return 0
    event_times_ms = self._identity_event_times_ms(normalized_identity)
    if not event_times_ms:
      return 0
    floor_index = min(len(event_times_ms), limit) - 1
    return max(0, int(event_times_ms[floor_index]) - QUERYLOG_IDENTITY_MAX_SKEW_MS)

  def _enrich_querylog_rows(
    self,
    rows: list[dict[str, Any]],
    *,
    status_payload: dict[str, Any] | None = None,
    events_by_client: dict[str, list[dict[str, Any]]] | None = None,
    dot_sessions_by_identity: dict[str, list[dict[str, Any]]] | None = None,
  ) -> list[dict[str, Any]]:
    device_lan_ip = self._detect_device_lan_ip(status_payload=status_payload)
    indexed_events = events_by_client if isinstance(events_by_client, dict) else {}
    indexed_dot_sessions = dot_sessions_by_identity if isinstance(dot_sessions_by_identity, dict) else {}
    metadata_lookup = self._identity_querylog_metadata_lookup()
    enriched: list[dict[str, Any]] = []
    for raw_row in rows:
      row = self._json_clone(raw_row)
      original_client = str(row.get("client", "") or "").strip()
      if not original_client:
        enriched.append(row)
        continue

      client_info = row.get("client_info")
      if not isinstance(client_info, dict):
        client_info = {}
        row["client_info"] = client_info

      identity_id = self._querylog_identity_from_metadata(row, metadata_lookup=metadata_lookup)
      if identity_id:
        self._set_querylog_row_identity(row, identity_id)
      elif self._querylog_proto(row) == "doh":
        identity_id = self._match_identity_event(
          client_ip=original_client,
          row_time_ms=self._parse_querylog_row_time_ms(row),
          elapsed_ms=self._querylog_elapsed_ms(row),
          events_by_client=indexed_events,
        )
        if identity_id:
          self._set_querylog_row_identity(row, identity_id)

      resolved_client = original_client
      if self._querylog_proto(row) == "dot" and identity_id and self._is_loopback_ip(original_client):
        dot_client_ip = self._match_dot_session_client_ip(
          identity_id=identity_id,
          row_time_ms=self._parse_querylog_row_time_ms(row),
          sessions_by_identity=indexed_dot_sessions,
        )
        if dot_client_ip and dot_client_ip != original_client:
          row["pixelOriginalClient"] = original_client
          row["client"] = dot_client_ip
          resolved_client = dot_client_ip
      elif device_lan_ip and self._is_loopback_ip(original_client) and not self._is_internal_loopback_row(row):
        row["pixelOriginalClient"] = original_client
        row["client"] = device_lan_ip
        resolved_client = device_lan_ip

      client_info["whois"] = self._merged_whois_info(
        resolved_client,
        client_info.get("whois", {}),
        allow_network=False,
      )

      enriched.append(row)
    return enriched

  def _identity_matches_filter(self, row: dict[str, Any], identity_filter: str) -> bool:
    if not identity_filter:
      return True
    return str(row.get("pixelIdentityId", "") or "").strip() == identity_filter

  def _querylog_oldest_value(self, rows: list[dict[str, Any]], fallback: str = "") -> str:
    for row in reversed(rows):
      value = str(row.get("time", "") or "").strip()
      if value:
        return value
    return fallback

  def _querylog_time_from_ms(self, epoch_ms: int) -> str:
    if epoch_ms <= 0:
      return ""
    try:
      return datetime.fromtimestamp(epoch_ms / 1000.0, tz=timezone.utc).isoformat().replace("+00:00", "Z")
    except (OverflowError, OSError, ValueError):
      return ""

  def _querylog_proxy_payload(self, query: dict[str, list[str]], identity_filter: str) -> dict[str, Any]:
    limit = self._parse_querylog_limit(query)
    backend_limit = min(
      QUERYLOG_LIMIT_MAX,
      max(limit, QUERYLOG_FILTER_SCAN_PAGE_LIMIT),
    )
    base_query = {
      "search": [((query.get("search") or [""])[0]).strip()],
      "response_status": [((query.get("response_status") or ["all"])[0]).strip() or "all"],
      "older_than": [((query.get("older_than") or [""])[0]).strip()],
      "limit": [str(limit if not identity_filter else backend_limit)],
    }

    status_payload = self._status_payload()

    if not identity_filter:
      payload = self._adguard_querylog_payload(base_query)
      rows = [row for row in payload.get("data", []) if isinstance(row, dict)]
      enrichment_window = self._querylog_event_window_for_rows(rows)
      events_by_client = self._identity_events_by_client(window=enrichment_window)
      dot_sessions_by_identity = self._dot_sessions_by_identity(window=enrichment_window)
      payload["data"] = self._enrich_querylog_rows(
        rows,
        status_payload=status_payload,
        events_by_client=events_by_client,
        dot_sessions_by_identity=dot_sessions_by_identity,
      )
      payload["pixelIdentityRequested"] = ""
      return payload

    scan_floor_ms = 0
    scan_floor_ready = False
    identity_event_times_ms: list[int] = []
    matched_rows: list[dict[str, Any]] = []
    scan_query = dict(base_query)
    pages_scanned = 0
    last_older_than = scan_query["older_than"][0]
    last_backend_cursor = ""
    while len(matched_rows) < limit and pages_scanned < QUERYLOG_FILTER_SCAN_PAGES_MAX:
      payload = self._adguard_querylog_payload(scan_query)
      raw_rows = [row for row in payload.get("data", []) if isinstance(row, dict)]
      if not raw_rows:
        payload_oldest = str(payload.get("oldest", "") or "").strip()
        if payload_oldest:
          last_backend_cursor = payload_oldest
        break
      enrichment_window = self._querylog_event_window_for_rows(raw_rows)
      events_by_client = self._identity_events_by_client(window=enrichment_window)
      dot_sessions_by_identity = self._dot_sessions_by_identity(window=enrichment_window)
      enriched_rows = self._enrich_querylog_rows(
        raw_rows,
        status_payload=status_payload,
        events_by_client=events_by_client,
        dot_sessions_by_identity=dot_sessions_by_identity,
      )
      matched_rows.extend(row for row in enriched_rows if self._identity_matches_filter(row, identity_filter))
      pages_scanned += 1
      payload_oldest = str(payload.get("oldest", "") or "").strip()
      if payload_oldest:
        last_backend_cursor = payload_oldest
      oldest_row_ms = self._parse_querylog_row_time_ms(raw_rows[-1])
      if not scan_floor_ready and len(matched_rows) < limit:
        identity_event_times_ms = self._identity_event_times_ms(identity_filter)
        if identity_event_times_ms:
          floor_index = min(len(identity_event_times_ms), limit) - 1
          scan_floor_ms = max(0, int(identity_event_times_ms[floor_index]) - QUERYLOG_IDENTITY_MAX_SKEW_MS)
        scan_floor_ready = True
        if not matched_rows and identity_event_times_ms and oldest_row_ms > 0:
          newest_event_ms = int(identity_event_times_ms[0])
          if newest_event_ms > 0 and oldest_row_ms > newest_event_ms + QUERYLOG_IDENTITY_MAX_SKEW_MS:
            jump_cursor = self._querylog_time_from_ms(newest_event_ms + QUERYLOG_IDENTITY_MAX_SKEW_MS + 1)
            if jump_cursor and jump_cursor != last_older_than:
              last_older_than = jump_cursor
              last_backend_cursor = jump_cursor
              scan_query["older_than"] = [jump_cursor]
              continue
      if len(raw_rows) < backend_limit:
        break
      if scan_floor_ms > 0:
        if oldest_row_ms > 0 and oldest_row_ms <= scan_floor_ms:
          break
      next_older_than = str(raw_rows[-1].get("time", "") or "").strip()
      if not next_older_than or next_older_than == last_older_than:
        break
      last_older_than = next_older_than
      last_backend_cursor = next_older_than
      scan_query["older_than"] = [next_older_than]

    returned_rows = matched_rows[:limit]
    return {
      "data": returned_rows,
      "oldest": self._querylog_oldest_value(returned_rows, fallback=last_backend_cursor),
      "pixelIdentityRequested": identity_filter,
      "pixelQuerylogPagesScanned": pages_scanned,
      "pixelQuerylogMatchCount": len(matched_rows),
    }

  def _cached_querylog_proxy_payload(self, query: dict[str, list[str]], identity_filter: str) -> dict[str, Any]:
    return self._burst_cache_get(
      "adguard_querylog",
      {"query": query, "identity_filter": identity_filter},
      lambda: self._querylog_proxy_payload(query, identity_filter),
    )

  def _window_seconds_for_stats(self, query: dict[str, list[str]]) -> int:
    raw = ((query.get("interval") or ["24_hours"])[0]).strip().lower()
    if raw in ("24h", "24_hours", "day"):
      return 24 * 3600
    if raw in ("7d", "7_days", "week"):
      return 7 * 86400
    return 24 * 3600

  def _querylog_rows_for_window(self, window_seconds: int) -> list[dict[str, Any]]:
    now_ms = int(time.time() * 1000)
    min_time_ms = now_ms - (window_seconds * 1000)
    status_payload = self._status_payload()
    enrichment_window = self._identity_event_window_for_seconds(window_seconds)
    events_by_client = self._identity_events_by_client(window=enrichment_window)
    dot_sessions_by_identity = self._dot_sessions_by_identity(window=enrichment_window)
    collected: list[dict[str, Any]] = []
    older_than = ""

    for _ in range(QUERYLOG_FILTER_SCAN_PAGES_MAX):
      payload = self._adguard_querylog_payload(
        {
          "search": [""],
          "response_status": ["all"],
          "older_than": [older_than],
          "limit": [str(QUERYLOG_FILTER_SCAN_PAGE_LIMIT)],
        }
      )
      raw_rows = [row for row in payload.get("data", []) if isinstance(row, dict)]
      if not raw_rows:
        break
      enriched_rows = self._enrich_querylog_rows(
        raw_rows,
        status_payload=status_payload,
        events_by_client=events_by_client,
        dot_sessions_by_identity=dot_sessions_by_identity,
      )
      for row in enriched_rows:
        row_time_ms = self._parse_querylog_row_time_ms(row)
        if row_time_ms and row_time_ms < min_time_ms:
          return collected
        collected.append(row)
      if len(raw_rows) < QUERYLOG_FILTER_SCAN_PAGE_LIMIT:
        break
      older_than = str(raw_rows[-1].get("time", "") or "").strip()
      if not older_than:
        break
    return collected

  def _cached_querylog_rows_for_window(self, window_seconds: int) -> list[dict[str, Any]]:
    return self._burst_cache_get(
      "querylog_rows_window",
      {"windowSeconds": window_seconds},
      lambda: self._querylog_rows_for_window(window_seconds),
    )

  def _dot_usage_counts(self, window_seconds: int, identity_filter: str) -> Counter[str]:
    def load_counts() -> dict[str, int]:
      now_ms = int(time.time() * 1000)
      min_time_ms = now_ms - (window_seconds * 1000)
      older_than = ""
      counts: Counter[str] = Counter()
      metadata_lookup = self._identity_querylog_metadata_lookup()

      for _ in range(QUERYLOG_FILTER_SCAN_PAGES_MAX):
        payload = self._adguard_querylog_payload(
          {
            "search": [""],
            "response_status": ["all"],
            "older_than": [older_than],
            "limit": [str(QUERYLOG_FILTER_SCAN_PAGE_LIMIT)],
          }
        )
        raw_rows = [row for row in payload.get("data", []) if isinstance(row, dict)]
        if not raw_rows:
          break
        for row in raw_rows:
          row_time_ms = self._parse_querylog_row_time_ms(row)
          if row_time_ms and row_time_ms < min_time_ms:
            return dict(counts)
          if self._querylog_proto(row) != "dot":
            continue
          identity_id = self._querylog_identity_from_metadata(row, metadata_lookup=metadata_lookup)
          if not identity_id:
            continue
          if identity_filter and identity_filter != "all" and identity_id != identity_filter:
            continue
          counts[identity_id] += 1
        if len(raw_rows) < QUERYLOG_FILTER_SCAN_PAGE_LIMIT:
          break
        older_than = str(raw_rows[-1].get("time", "") or "").strip()
        if not older_than:
          break
      return dict(counts)

    return Counter(
      self._burst_cache_get(
        "dot_usage_counts",
        {"windowSeconds": window_seconds, "identity": identity_filter or "all"},
        load_counts,
      )
    )

  def _augment_usage_payload_with_dot_querylog(self, payload: dict[str, Any], identity: str, window: str) -> dict[str, Any]:
    if not isinstance(payload, dict):
      return payload

    dot_counts = self._dot_usage_counts(self._parse_duration_seconds(window or QUERYLOG_EVENT_WINDOW_DEFAULT), identity or "all")
    if not dot_counts:
      return payload

    identities = payload.get("identities", [])
    rows: list[dict[str, Any]] = []
    seen_ids: set[str] = set()
    if isinstance(identities, list):
      for raw_row in identities:
        if not isinstance(raw_row, dict):
          continue
        row = self._json_clone(raw_row)
        identity_id = str(row.get("id", "") or "").strip()
        if not identity_id:
          continue
        seen_ids.add(identity_id)
        doh_count = row.get("requests", 0)
        if not isinstance(doh_count, int):
          doh_count = 0
        dot_count = int(dot_counts.pop(identity_id, 0))
        row["dohRequestCount"] = max(0, doh_count)
        row["dotRequestCount"] = max(0, dot_count)
        row["requestCount"] = max(0, doh_count + dot_count)
        rows.append(row)

    zero_status_counts = {"2xx": 0, "3xx": 0, "4xx": 0, "5xx": 0, "other": 0}
    zero_latency = {"p50": 0, "p95": 0, "p99": 0}
    for identity_id, dot_count in dot_counts.items():
      if not identity_id or identity_id in seen_ids:
        continue
      rows.append(
        {
          "id": identity_id,
          "requests": 0,
          "requestCount": int(dot_count),
          "dohRequestCount": 0,
          "dotRequestCount": int(dot_count),
          "statusCounts": dict(zero_status_counts),
          "latencyMs": dict(zero_latency),
        }
      )

    rows.sort(key=lambda row: (-int(row.get("requestCount", row.get("requests", 0)) or 0), str(row.get("id", ""))))
    payload["identities"] = rows
    doh_total_requests = payload.get("totalRequests", 0)
    if not isinstance(doh_total_requests, int):
      doh_total_requests = 0
    payload["dohTotalRequests"] = max(0, doh_total_requests)
    payload["dotTotalRequests"] = sum(int(row.get("dotRequestCount", 0) or 0) for row in rows)
    payload["totalRequestCount"] = payload["dohTotalRequests"] + payload["dotTotalRequests"]
    return payload

  def _stats_top_clients_from_querylog(self, window_seconds: int) -> list[dict[str, int]]:
    counts: Counter[str] = Counter()
    rows = self._cached_querylog_rows_for_window(window_seconds)
    effective_rows, _, _, _, _, _ = self._querylog_effective_rows(rows, False)
    for row in effective_rows:
      client = str(row.get("client", "") or "").strip()
      if not client:
        continue
      counts[client] += 1
    ranked = sorted(counts.items(), key=lambda item: (-item[1], item[0]))[:10]
    return [{client: count} for client, count in ranked]

  def _stats_proxy_payload(self, query: dict[str, list[str]]) -> dict[str, Any]:
    payload = json.loads(json.dumps(self._stats_payload_raw(query)))
    payload["top_clients"] = self._stats_top_clients_from_querylog(self._window_seconds_for_stats(query))
    return payload

  def _cached_stats_proxy_payload(self, query: dict[str, list[str]]) -> dict[str, Any]:
    return self._burst_cache_get(
      "adguard_stats",
      {"query": query},
      lambda: self._stats_proxy_payload(query),
    )

  def _minimal_client_entry(self, client_id: str) -> dict[str, Any]:
    return {
      "disallowed": False,
      "whois_info": {},
      "safe_search": None,
      "blocked_services_schedule": None,
      "name": "",
      "blocked_services": None,
      "ids": [client_id],
      "tags": None,
      "upstreams": None,
      "filtering_enabled": False,
      "parental_enabled": False,
      "safebrowsing_enabled": False,
      "safesearch_enabled": False,
      "use_global_blocked_services": False,
      "use_global_settings": False,
      "ignore_querylog": None,
      "ignore_statistics": None,
      "upstreams_cache_size": 0,
      "upstreams_cache_enabled": None,
    }

  def _clients_proxy_payload(self) -> dict[str, Any]:
    payload = json.loads(json.dumps(self._clients_payload_raw()))
    auto_clients = payload.get("auto_clients")
    if isinstance(auto_clients, list):
      for row in auto_clients:
        if not isinstance(row, dict):
          continue
        ip = str(row.get("ip", "") or "").strip()
        row["whois_info"] = self._merged_whois_info(ip, row.get("whois_info", {}))
    return payload

  def _clients_search_proxy_payload(self, body: bytes) -> Any:
    requested_ids: list[str] = []
    try:
      request_payload = json.loads(body.decode("utf-8"))
    except Exception as exc:
      raise IdentityWebError(HTTPStatus.BAD_REQUEST, f"Invalid clients/search JSON body: {exc}") from exc
    if isinstance(request_payload, dict):
      clients = request_payload.get("clients")
      if isinstance(clients, list):
        for entry in clients:
          if not isinstance(entry, dict):
            continue
          client_id = str(entry.get("id", "") or "").strip()
          if client_id:
            requested_ids.append(client_id)

    raw_payload = self._client_search_payload_raw(body)
    existing_map: dict[str, dict[str, Any]] = {}
    if isinstance(raw_payload, list):
      for item in raw_payload:
        if not isinstance(item, dict):
          continue
        for client_id, entry in item.items():
          if isinstance(entry, dict):
            existing_map[str(client_id)] = json.loads(json.dumps(entry))

    response: list[dict[str, Any]] = []
    for client_id in requested_ids:
      entry = existing_map.get(client_id, self._minimal_client_entry(client_id))
      entry["whois_info"] = self._merged_whois_info(client_id, entry.get("whois_info", {}))
      response.append({client_id: entry})
    return response

  def _build_html(self) -> str:
    settings_prefixes_json = json.dumps(list(SETTINGS_HASH_PREFIXES))
    querylog_limit_default = QUERYLOG_LIMIT_DEFAULT
    return f"""<!doctype html>
<html lang=\"en\">
<head>
  <meta charset=\"utf-8\">
  <meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">
  <title>DNS identities</title>
  <style>
    :root {{
      --bg: #f6f8fb;
      --panel: #ffffff;
      --text: #1f2937;
      --muted: #6b7280;
      --border: #d8dee8;
      --primary: #3f78d1;
      --danger: #cc4b37;
      --success: #2f8c5a;
      --warning: #9a6700;
    }}
    * {{ box-sizing: border-box; }}
    body {{ margin: 0; padding: 24px; background: var(--bg); color: var(--text); font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }}
    .wrap {{ max-width: 1080px; margin: 0 auto; }}
    .header {{ display: flex; align-items: center; gap: 10px; margin-bottom: 16px; }}
    .title {{ font-size: 26px; font-weight: 700; margin: 0; }}
    .grow {{ flex: 1; }}
    .btn {{ border: 1px solid transparent; border-radius: 6px; padding: 6px 12px; font-size: 14px; font-weight: 600; cursor: pointer; text-decoration: none; display: inline-flex; align-items: center; gap: 6px; }}
    .btn:disabled {{ opacity: 0.5; cursor: not-allowed; }}
    .btn-outline-primary {{ background: #fff; border-color: var(--primary); color: var(--primary); }}
    .btn-primary {{ background: var(--primary); border-color: var(--primary); color: #fff; }}
    .btn-danger {{ background: var(--danger); border-color: var(--danger); color: #fff; }}
    .btn-outline-danger {{ background: #fff; border-color: var(--danger); color: var(--danger); }}
    .btn-outline-secondary {{ background: #fff; border-color: var(--border); color: var(--text); }}
    .btn-sm {{ font-size: 13px; padding: 5px 9px; }}
    .panel {{ background: var(--panel); border: 1px solid var(--border); border-radius: 10px; padding: 16px; margin-bottom: 14px; }}
    .panel h2 {{ margin: 0 0 12px; font-size: 18px; }}
    .hint {{ margin: 0 0 14px; color: var(--muted); font-size: 14px; }}
    .status {{ min-height: 20px; margin-bottom: 10px; font-size: 13px; color: var(--muted); }}
    .status.error {{ color: var(--danger); }}
    .status.ok {{ color: var(--success); }}
    .status.warn {{ color: var(--warning); }}
    .grid {{ display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 10px; align-items: end; }}
    .field label {{ display: block; font-size: 12px; color: var(--muted); margin-bottom: 4px; }}
    .field input, .field select {{ width: 100%; border: 1px solid var(--border); border-radius: 6px; padding: 8px 10px; font-size: 14px; }}
    .checkbox {{ display: inline-flex; align-items: center; gap: 6px; font-size: 14px; color: var(--muted); }}
    .table-scroll {{ overflow-x: auto; -webkit-overflow-scrolling: touch; }}
    .table-scroll table {{ min-width: 680px; }}
    .table-scroll--wide table {{ min-width: 940px; }}
    table {{ width: 100%; border-collapse: collapse; font-size: 14px; }}
    th, td {{ border-top: 1px solid var(--border); padding: 10px 8px; vertical-align: middle; text-align: left; }}
    th {{ border-top: none; color: var(--muted); font-size: 12px; text-transform: uppercase; letter-spacing: 0.03em; }}
    .mono {{ font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, "Courier New", monospace; font-size: 12px; }}
    .row-actions {{ display: flex; gap: 6px; flex-wrap: wrap; }}
    .pill {{ display: inline-block; border: 1px solid var(--border); border-radius: 999px; padding: 2px 8px; font-size: 12px; color: var(--muted); }}
    .querylog-grid {{ display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; margin-top: 10px; }}
    .metric-card {{ border: 1px solid var(--border); border-radius: 8px; padding: 10px; background: #fbfdff; }}
    .metric-title {{ margin: 0 0 8px; font-size: 13px; color: var(--muted); text-transform: uppercase; letter-spacing: 0.03em; }}
    .metric-line {{ display: flex; justify-content: space-between; gap: 8px; padding: 3px 0; border-top: 1px solid #eef2f8; font-size: 13px; }}
    .metric-line:first-child {{ border-top: 0; }}
    .metric-key {{ color: var(--muted); }}
    .metric-value {{ text-align: right; word-break: break-word; }}
    @media (max-width: 900px) {{
      body {{ padding: 12px; }}
      .panel {{ padding: 14px; }}
      .grid {{ grid-template-columns: 1fr; }}
      .querylog-grid {{ grid-template-columns: 1fr; }}
      .header {{ flex-wrap: wrap; }}
      .header > .btn {{ flex: 1 1 auto; justify-content: center; }}
      .row-actions {{ min-width: 180px; }}
      .table-scroll {{ margin: 0 -2px; padding-bottom: 4px; }}
    }}
  </style>
</head>
<body>
  <div class=\"wrap\">
    <div class=\"header\">
      <h1 class=\"title\">DNS identities</h1>
      <span class=\"grow\"></span>
      <a class=\"btn btn-outline-primary btn-sm\" id=\"back-link\" href=\"/#settings\">Back to settings</a>
      <button class=\"btn btn-outline-secondary btn-sm\" id=\"refresh-btn\" type=\"button\">Refresh</button>
    </div>
    <p class=\"hint\">Manage encrypted DNS identities. DoH uses per-identity path tokens, and DoT can use randomized Private DNS hostnames when enabled.</p>
    <div class=\"status\" id=\"status\"></div>

    <section class=\"panel\">
      <h2>Identities</h2>
      <p class=\"hint\">Revocation requires confirmation. Revoke is blocked when only one identity remains.</p>
      <div class=\"table-scroll table-scroll--wide\">
        <table>
          <thead>
            <tr>
              <th>Identity</th>
              <th>DoH token</th>
              <th>DoT hostname</th>
              <th>Created</th>
              <th>Expiration</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody id=\"identities-body\"></tbody>
        </table>
      </div>
    </section>

    <section class=\"panel\">
      <h2>Create identity</h2>
      <div class=\"grid\">
        <div class=\"field\">
          <label for=\"create-id\">Identity id</label>
          <input id=\"create-id\" name=\"create-id\" autocomplete=\"off\" placeholder=\"iphone\">
        </div>
        <div class=\"field\">
          <label for=\"create-token\">DoH token (optional override)</label>
          <input id=\"create-token\" name=\"create-token\" autocomplete=\"off\" placeholder=\"auto-generated when empty\">
        </div>
        <div class=\"field\">
          <label for=\"create-expiration\">Expiration</label>
          <select id=\"create-expiration\" name=\"create-expiration\">
            <option value=\"none\" selected>No expiry</option>
            <option value=\"7d\">7 days</option>
            <option value=\"30d\">30 days</option>
            <option value=\"90d\">90 days</option>
            <option value=\"custom\">Custom</option>
          </select>
        </div>
        <div class=\"field\">
          <label for=\"create-expiration-custom\">Custom expiration</label>
          <input id=\"create-expiration-custom\" name=\"create-expiration-custom\" type=\"datetime-local\" disabled>
        </div>
        <div class=\"field\">
          <label for=\"usage-window\">Default usage window</label>
          <select id=\"usage-window\" name=\"usage-window\">
            <option value=\"1h\">1 hour</option>
            <option value=\"24h\">24 hours</option>
            <option value=\"7d\" selected>7 days</option>
            <option value=\"30d\">30 days</option>
            <option value=\"custom\">Custom</option>
          </select>
        </div>
        <div class=\"field\">
          <label for=\"usage-window-custom\">Custom window</label>
          <input id=\"usage-window-custom\" name=\"usage-window-custom\" autocomplete=\"off\" placeholder=\"example: 12h\" disabled>
        </div>
      </div>
      <p class=\"hint\" id=\"dot-mode-hint\">DoT hostname assignment is disabled.</p>
      <div style=\"margin-top:10px; display:flex; align-items:center; gap:10px;\">
        <label class=\"checkbox\"><input id=\"create-primary\" type=\"checkbox\">Set as primary identity</label>
        <button class=\"btn btn-primary btn-sm\" id=\"create-btn\" type=\"button\">Create</button>
      </div>
    </section>

    <section class=\"panel\">
      <h2>Usage</h2>
      <div class=\"grid\">
        <div class=\"field\">
          <label for=\"usage-identity\">Identity filter</label>
          <select id=\"usage-identity\" name=\"usage-identity\">
            <option value=\"all\">All identities</option>
          </select>
        </div>
        <div class=\"field\">
          <label>Actions</label>
          <button class=\"btn btn-outline-primary btn-sm\" id=\"usage-refresh-btn\" type=\"button\">Refresh usage</button>
        </div>
      </div>
      <div class=\"table-scroll\" style=\"margin-top:10px;\">
        <table>
          <thead>
            <tr>
              <th>Identity</th>
              <th>Requests</th>
              <th>Status buckets</th>
              <th>Latency (ms)</th>
            </tr>
          </thead>
          <tbody id=\"usage-body\"></tbody>
        </table>
      </div>
    </section>

    <section class=\"panel\">
      <h2>Querylog Visibility</h2>
      <p class=\"hint\">Default view hides internal loopback watchdog/admin/maintenance rows from effective metrics.</p>
      <div class=\"grid\">
        <div class=\"field\">
          <label for=\"querylog-limit\">Querylog row limit</label>
          <input id=\"querylog-limit\" name=\"querylog-limit\" autocomplete=\"off\" inputmode=\"numeric\" value=\"{querylog_limit_default}\">
        </div>
        <div class=\"field\">
          <label for=\"querylog-view-mode\">Effective view mode</label>
          <input id=\"querylog-view-mode\" name=\"querylog-view-mode\" value=\"user_only\" disabled>
        </div>
        <div class=\"field\">
          <label for=\"querylog-status\">Querylog status</label>
          <input id=\"querylog-status\" name=\"querylog-status\" value=\"pending\" disabled>
        </div>
        <div class=\"field\">
          <label>Actions</label>
          <button class=\"btn btn-outline-primary btn-sm\" id=\"querylog-refresh-btn\" type=\"button\">Refresh querylog</button>
        </div>
      </div>
      <div style=\"margin-top:10px; display:flex; align-items:center; gap:10px;\">
        <label class=\"checkbox\"><input id=\"querylog-include-internal\" type=\"checkbox\">Include internal querylog</label>
      </div>
      <div class=\"status\" id=\"querylog-status-line\"></div>
      <div class=\"querylog-grid\">
        <div class=\"metric-card\">
          <h3 class=\"metric-title\">Effective metrics</h3>
          <div class=\"metric-line\"><span class=\"metric-key\">total_query_count</span><span class=\"metric-value\" id=\"querylog-total-query-count\">0</span></div>
          <div class=\"metric-line\"><span class=\"metric-key\">total_doh_count</span><span class=\"metric-value\" id=\"querylog-total-doh-count\">0</span></div>
          <div class=\"metric-line\"><span class=\"metric-key\">gateway_doh_count</span><span class=\"metric-value\" id=\"querylog-gateway-doh-count\">0</span></div>
          <div class=\"metric-line\"><span class=\"metric-key\">gateway_share_pct</span><span class=\"metric-value\" id=\"querylog-gateway-share-pct\">0.00</span></div>
          <div class=\"metric-line\"><span class=\"metric-key\">top_clients</span><span class=\"metric-value mono\" id=\"querylog-top-clients\">none</span></div>
        </div>
        <div class=\"metric-card\">
          <h3 class=\"metric-title\">User metrics</h3>
          <div class=\"metric-line\"><span class=\"metric-key\">user_total_count</span><span class=\"metric-value\" id=\"querylog-user-total-count\">0</span></div>
          <div class=\"metric-line\"><span class=\"metric-key\">user_doh_count</span><span class=\"metric-value\" id=\"querylog-user-doh-count\">0</span></div>
          <div class=\"metric-line\"><span class=\"metric-key\">top_clients_user</span><span class=\"metric-value mono\" id=\"querylog-top-clients-user\">none</span></div>
        </div>
        <div class=\"metric-card\">
          <h3 class=\"metric-title\">Internal metrics</h3>
          <div class=\"metric-line\"><span class=\"metric-key\">internal_total_count</span><span class=\"metric-value\" id=\"querylog-internal-total-count\">0</span></div>
          <div class=\"metric-line\"><span class=\"metric-key\">internal_doh_count</span><span class=\"metric-value\" id=\"querylog-internal-doh-count\">0</span></div>
          <div class=\"metric-line\"><span class=\"metric-key\">top_clients_internal</span><span class=\"metric-value mono\" id=\"querylog-top-clients-internal\">none</span></div>
          <div class=\"metric-line\"><span class=\"metric-key\">internal_probe_domain_counts</span><span class=\"metric-value mono\" id=\"querylog-internal-probe-domain-counts\">none</span></div>
        </div>
      </div>
    </section>
  </div>

  <script>
    (() => {{
      const SETTINGS_HASH_PREFIXES = {settings_prefixes_json};
      const identityPattern = /^[a-z0-9][a-z0-9._-]{{0,63}}$/;
      const tokenPattern = /^[A-Za-z0-9._~-]{{16,128}}$/;

      const state = {{
        primaryIdentityId: "",
        dotIdentityEnabled: false,
        dotHostnameBase: "",
        dotIdentityLabelLength: 0,
        identities: [],
        revealed: Object.create(null),
        querylogSummary: null,
        revokeArmedId: "",
        revokeArmTimer: null,
      }};

      const $ = (id) => document.getElementById(id);
      const statusEl = $("status");
      const identitiesBody = $("identities-body");
      const usageBody = $("usage-body");
      const querylogStatusLineEl = $("querylog-status-line");

      function setStatus(message, kind = "") {{
        statusEl.textContent = message || "";
        statusEl.className = kind ? `status ${{kind}}` : "status";
      }}

      function setQuerylogStatus(message, kind = "") {{
        querylogStatusLineEl.textContent = message || "";
        querylogStatusLineEl.className = kind ? `status ${{kind}}` : "status";
      }}

      function querylogValue(value, fallback = "none") {{
        if (value === null || value === undefined) return fallback;
        const text = String(value);
        return text.length ? text : fallback;
      }}

      function formatEpoch(epoch) {{
        if (!Number.isFinite(epoch) || epoch <= 0) return "-";
        const date = new Date(epoch * 1000);
        return date.toLocaleString();
      }}

      function expirationLabel(entry) {{
        const expiresEpoch = Number(entry?.expiresEpochSeconds || 0);
        if (!Number.isFinite(expiresEpoch) || expiresEpoch <= 0) {{
          return "No expiry";
        }}
        const base = formatEpoch(expiresEpoch);
        return entry?.isExpired ? `${{base}} (Expired)` : base;
      }}

      function dotHostnameLabel(entry) {{
        const hostname = String(entry?.dotHostname || "");
        return hostname || "Disabled";
      }}

      function updateDotModeHint() {{
        const hint = $("dot-mode-hint");
        if (!hint) {{
          return;
        }}
        if (state.dotIdentityEnabled && state.dotHostnameBase) {{
          const labelLength = Number(state.dotIdentityLabelLength || 0);
          const prefix = labelLength > 0 ? `${{labelLength}}-character random labels` : "random labels";
          hint.textContent = `DoT hostname mode is enabled. New identities get ${{prefix}} under *.${{state.dotHostnameBase}}.`;
          return;
        }}
        hint.textContent = "DoT hostname assignment is disabled.";
      }}

      function normalizeReturnTarget(raw) {{
        if (!raw) return "/#settings";
        if (raw.startsWith("/#")) return raw;
        if (raw.startsWith("/")) return raw;
        return "/#settings";
      }}

      function querylogRouteHref(identityId) {{
        const url = new URL("/", window.location.origin);
        const normalizedIdentityId = String(identityId || "");
        if (normalizedIdentityId) {{
          url.searchParams.set("pixel_identity", normalizedIdentityId);
        }}
        const hashParams = new URLSearchParams();
        hashParams.set("response_status", "all");
        if (normalizedIdentityId) {{
          hashParams.set("pixel_identity", normalizedIdentityId);
        }}
        url.hash = `#logs?${{hashParams.toString()}}`;
        return `${{url.pathname}}${{url.search}}${{url.hash}}`;
      }}

      function getUsageWindow() {{
        const select = $("usage-window");
        const custom = $("usage-window-custom");
        if (select.value === "custom") {{
          return custom.value.trim();
        }}
        return select.value;
      }}

      async function api(path, options = {{}}) {{
        const init = Object.assign({{ credentials: "same-origin" }}, options);
        init.headers = Object.assign({{}}, options.headers || {{}});
        if (init.body && !init.headers["Content-Type"]) {{
          init.headers["Content-Type"] = "application/json";
        }}
        const response = await fetch(path, init);
        const text = await response.text();
        let payload = {{}};
        if (text) {{
          try {{
            payload = JSON.parse(text);
          }} catch (_err) {{
            payload = {{ error: text }};
          }}
        }}
        if (response.status === 401) {{
          window.location.href = "/login.html";
          throw new Error("Session expired");
        }}
        if (!response.ok) {{
          throw new Error(payload.error || `Request failed (${{response.status}})`);
        }}
        return payload;
      }}

      function updateUsageIdentityOptions() {{
        const select = $("usage-identity");
        const current = select.value;
        const options = [
          {{ value: "all", label: "All identities" }},
          ...state.identities.map((entry) => ({{ value: entry.id, label: entry.id }})),
        ];
        select.innerHTML = "";
        for (const opt of options) {{
          const node = document.createElement("option");
          node.value = opt.value;
          node.textContent = opt.label;
          select.appendChild(node);
        }}
        if (options.some((opt) => opt.value === current)) {{
          select.value = current;
        }}
      }}

      function clearRevokeArm(shouldRender = true) {{
        const hadArm = Boolean(state.revokeArmedId);
        state.revokeArmedId = "";
        if (state.revokeArmTimer) {{
          clearTimeout(state.revokeArmTimer);
          state.revokeArmTimer = null;
        }}
        if (hadArm && shouldRender) {{
          renderIdentities();
        }}
      }}

      function armRevoke(identityId) {{
        clearRevokeArm(false);
        state.revokeArmedId = identityId;
        state.revokeArmTimer = setTimeout(() => {{
          if (state.revokeArmedId !== identityId) {{
            return;
          }}
          clearRevokeArm();
          setStatus(`Revocation confirmation expired for ${{identityId}}.`, "warn");
        }}, 2000);
        renderIdentities();
        setStatus(`Press Revoke again for ${{identityId}} within 2s to confirm.`, "warn");
      }}

      function renderIdentities() {{
        identitiesBody.innerHTML = "";
        const identityCount = state.identities.length;
        for (const entry of state.identities) {{
          const tr = document.createElement("tr");

          const tdId = document.createElement("td");
          tdId.textContent = entry.id;
          if (entry.id === state.primaryIdentityId) {{
            const badge = document.createElement("span");
            badge.className = "pill";
            badge.style.marginLeft = "8px";
            badge.textContent = "primary";
            tdId.appendChild(badge);
          }}

          const tdToken = document.createElement("td");
          tdToken.className = "mono";
          const shown = state.revealed[entry.id] ? entry.token : entry.tokenMasked;
          tdToken.textContent = shown;

          const tdDot = document.createElement("td");
          tdDot.className = "mono";
          tdDot.textContent = dotHostnameLabel(entry);

          const tdCreated = document.createElement("td");
          tdCreated.textContent = formatEpoch(entry.createdEpochSeconds);

          const tdExpiration = document.createElement("td");
          tdExpiration.textContent = expirationLabel(entry);
          if (entry.isExpired) {{
            const badge = document.createElement("span");
            badge.className = "pill";
            badge.style.marginLeft = "8px";
            badge.textContent = "expired";
            tdExpiration.appendChild(badge);
          }}

          const tdActions = document.createElement("td");
          const actions = document.createElement("div");
          actions.className = "row-actions";

          const revealBtn = document.createElement("button");
          revealBtn.className = "btn btn-outline-secondary btn-sm";
          revealBtn.type = "button";
          revealBtn.textContent = state.revealed[entry.id] ? "Mask" : "Reveal";
          revealBtn.addEventListener("click", () => {{
            state.revealed[entry.id] = !state.revealed[entry.id];
            renderIdentities();
          }});

          const copyBtn = document.createElement("button");
          copyBtn.className = "btn btn-outline-secondary btn-sm";
          copyBtn.type = "button";
          copyBtn.textContent = "Copy token";
          copyBtn.addEventListener("click", async () => {{
            try {{
              await navigator.clipboard.writeText(entry.token);
              setStatus(`DoH token copied for ${{entry.id}}`, "ok");
            }} catch (_err) {{
              setStatus("Clipboard write failed", "error");
            }}
          }});

          const copyDotBtn = document.createElement("button");
          copyDotBtn.className = "btn btn-outline-secondary btn-sm";
          copyDotBtn.type = "button";
          copyDotBtn.textContent = "Copy DoT";
          copyDotBtn.disabled = !entry.dotHostname;
          if (copyDotBtn.disabled) {{
            copyDotBtn.title = "DoT hostname assignment is disabled for this identity.";
          }}
          copyDotBtn.addEventListener("click", async () => {{
            if (!entry.dotHostname) {{
              return;
            }}
            try {{
              await navigator.clipboard.writeText(entry.dotHostname);
              setStatus(`DoT hostname copied for ${{entry.id}}`, "ok");
            }} catch (_err) {{
              setStatus("Clipboard write failed", "error");
            }}
          }});

          const revokeBtn = document.createElement("button");
          const revokeArmed = state.revokeArmedId === entry.id;
          revokeBtn.className = "btn btn-outline-danger btn-sm";
          revokeBtn.type = "button";
          revokeBtn.textContent = "Revoke";
          revokeBtn.disabled = identityCount <= 1;
          if (revokeBtn.disabled) {{
            revokeBtn.title = "At least one identity must remain.";
          }}
          revokeBtn.addEventListener("click", async () => {{
            if (revokeBtn.disabled) {{
              return;
            }}
            if (!revokeArmed) {{
              armRevoke(entry.id);
              return;
            }}
            clearRevokeArm(false);
            const originalText = revokeBtn.textContent;
            revokeBtn.disabled = true;
            revokeBtn.textContent = "Revoking...";
            try {{
              await revokeIdentity(entry.id);
            }} finally {{
              revokeBtn.textContent = originalText;
            }}
          }});

          actions.appendChild(revealBtn);
          actions.appendChild(copyBtn);
          actions.appendChild(copyDotBtn);
          actions.appendChild(revokeBtn);
          tdActions.appendChild(actions);

          tr.appendChild(tdId);
          tr.appendChild(tdToken);
          tr.appendChild(tdDot);
          tr.appendChild(tdCreated);
          tr.appendChild(tdExpiration);
          tr.appendChild(tdActions);
          identitiesBody.appendChild(tr);
        }}
      }}

      function renderUsage(payload) {{
        usageBody.innerHTML = "";
        const rows = Array.isArray(payload.identities) ? payload.identities : [];
        if (!rows.length) {{
          const tr = document.createElement("tr");
          const td = document.createElement("td");
          td.colSpan = 4;
          td.textContent = "No usage for selected window.";
          tr.appendChild(td);
          usageBody.appendChild(tr);
          return;
        }}

        for (const row of rows) {{
          const tr = document.createElement("tr");
          const requests = row.requestCount ?? row.requests ?? 0;
          const status = row.statusCounts || {{}};
          const latency = row.latencyMs || {{}};
          const identityId = String(row.id || "");

          const tdId = document.createElement("td");
          if (identityId) {{
            const link = document.createElement("a");
            link.href = querylogRouteHref(identityId);
            link.textContent = identityId;
            tdId.appendChild(link);
          }} else {{
            tdId.textContent = "";
          }}

          const tdReq = document.createElement("td");
          tdReq.textContent = String(requests);

          const tdStatus = document.createElement("td");
          tdStatus.textContent = `2xx=${{status["2xx"] || 0}} 3xx=${{status["3xx"] || 0}} 4xx=${{status["4xx"] || 0}} 5xx=${{status["5xx"] || 0}}`;

          const tdLatency = document.createElement("td");
          tdLatency.textContent = `p50=${{latency.p50 || 0}} p95=${{latency.p95 || 0}} p99=${{latency.p99 || 0}}`;

          tr.appendChild(tdId);
          tr.appendChild(tdReq);
          tr.appendChild(tdStatus);
          tr.appendChild(tdLatency);
          usageBody.appendChild(tr);
        }}
      }}

      function renderQuerylogSummary() {{
        const payload = state.querylogSummary || {{}};
        $("querylog-view-mode").value = querylogValue(payload.querylog_view_mode, "user_only");
        $("querylog-status").value = querylogValue(payload.querylog_status, "pending");
        $("querylog-total-query-count").textContent = querylogValue(payload.total_query_count, "0");
        $("querylog-total-doh-count").textContent = querylogValue(payload.total_doh_count, "0");
        $("querylog-gateway-doh-count").textContent = querylogValue(payload.gateway_doh_count, "0");
        $("querylog-gateway-share-pct").textContent = querylogValue(payload.gateway_share_pct, "0.00");
        $("querylog-top-clients").textContent = querylogValue(payload.top_clients, "none");
        $("querylog-user-total-count").textContent = querylogValue(payload.user_total_count, "0");
        $("querylog-user-doh-count").textContent = querylogValue(payload.user_doh_count, "0");
        $("querylog-top-clients-user").textContent = querylogValue(payload.top_clients_user, "none");
        $("querylog-internal-total-count").textContent = querylogValue(payload.internal_total_count, "0");
        $("querylog-internal-doh-count").textContent = querylogValue(payload.internal_doh_count, "0");
        $("querylog-top-clients-internal").textContent = querylogValue(payload.top_clients_internal, "none");
        $("querylog-internal-probe-domain-counts").textContent = querylogValue(payload.internal_probe_domain_counts, "none");
      }}

      function querylogLimitValue() {{
        const raw = $("querylog-limit").value.trim();
        if (!raw) {{
          $("querylog-limit").value = "{querylog_limit_default}";
          return {querylog_limit_default};
        }}
        const parsed = Number.parseInt(raw, 10);
        if (!Number.isFinite(parsed) || parsed < 1 || parsed > 10000) {{
          throw new Error("Querylog limit must be an integer between 1 and 10000.");
        }}
        $("querylog-limit").value = String(parsed);
        return parsed;
      }}

      async function refreshQuerylogSummary() {{
        const includeInternal = $("querylog-include-internal").checked;
        const limit = querylogLimitValue();
        const query = new URLSearchParams();
        query.set("view", includeInternal ? "all" : "user_only");
        query.set("limit", String(limit));
        const payload = await api(`/pixel-stack/identity/api/v1/querylog/summary?${{query.toString()}}`);
        state.querylogSummary = payload;
        renderQuerylogSummary();
      }}

      async function loadIdentities() {{
        const payload = await api("/pixel-stack/identity/api/v1/identities");
        state.primaryIdentityId = payload.primaryIdentityId || "";
        state.dotIdentityEnabled = Boolean(payload.dotIdentityEnabled);
        state.dotHostnameBase = String(payload.dotHostnameBase || "");
        state.dotIdentityLabelLength = Number(payload.dotIdentityLabelLength || 0);
        state.identities = Array.isArray(payload.identities) ? payload.identities : [];
        if (state.revokeArmedId && !state.identities.some((entry) => entry.id === state.revokeArmedId)) {{
          clearRevokeArm(false);
        }}
        for (const entry of state.identities) {{
          if (typeof state.revealed[entry.id] !== "boolean") {{
            state.revealed[entry.id] = false;
          }}
        }}
        updateDotModeHint();
        renderIdentities();
        updateUsageIdentityOptions();
      }}

      async function refreshUsage() {{
        const identity = $("usage-identity").value || "all";
        const windowValue = getUsageWindow();
        const query = new URLSearchParams();
        query.set("identity", identity);
        if (windowValue) {{
          query.set("window", windowValue);
        }}
        const payload = await api(`/pixel-stack/identity/api/v1/usage?${{query.toString()}}`);
        renderUsage(payload);
      }}

      async function revokeIdentity(identityId) {{
        try {{
          setStatus(`Revoking ${{identityId}}...`);
          clearRevokeArm(false);
          const payload = await api(`/pixel-stack/identity/api/v1/identities/${{encodeURIComponent(identityId)}}`, {{ method: "DELETE" }});
          state.identities = state.identities.filter((entry) => entry.id !== identityId);
          delete state.revealed[identityId];
          state.primaryIdentityId = payload?.primaryIdentityId || state.primaryIdentityId;
          renderIdentities();
          updateUsageIdentityOptions();
          const applied = payload?.applied !== false;
          const suffix = applied ? "" : " (saved; runtime reload pending)";
          setStatus(`Revoked ${{identityId}}${{suffix}}`, applied ? "ok" : "warn");
          try {{
            await loadIdentities();
            await refreshUsage();
          }} catch (refreshErr) {{
            setStatus(
              `Revoked ${{identityId}}${{suffix}} (refresh issue: ${{refreshErr?.message || "unknown"}})`,
              "warn",
            );
          }}
        }} catch (err) {{
          setStatus(err.message || "Revoke failed", "error");
        }}
      }}

      async function createIdentity() {{
        const idInput = $("create-id");
        const tokenInput = $("create-token");
        const expirationSelect = $("create-expiration");
        const expirationCustomInput = $("create-expiration-custom");
        const idValue = idInput.value.trim().toLowerCase();
        const tokenValue = tokenInput.value.trim();
        const primary = $("create-primary").checked;

        if (!identityPattern.test(idValue)) {{
          setStatus("Identity id must be lower-case slug format (1-64 chars).", "error");
          return;
        }}
        if (tokenValue && !tokenPattern.test(tokenValue)) {{
          setStatus("Token must match [A-Za-z0-9._~-] and be 16-128 chars.", "error");
          return;
        }}

        const body = {{ id: idValue, primary }};
        if (tokenValue) {{
          body.token = tokenValue;
        }}
        if (expirationSelect.value === "7d") {{
          body.expiresEpochSeconds = Math.floor(Date.now() / 1000) + (7 * 86400);
        }} else if (expirationSelect.value === "30d") {{
          body.expiresEpochSeconds = Math.floor(Date.now() / 1000) + (30 * 86400);
        }} else if (expirationSelect.value === "90d") {{
          body.expiresEpochSeconds = Math.floor(Date.now() / 1000) + (90 * 86400);
        }} else if (expirationSelect.value === "custom") {{
          const raw = expirationCustomInput.value.trim();
          if (!raw) {{
            setStatus("Custom expiration requires a date and time.", "error");
            return;
          }}
          const parsedMs = Date.parse(raw);
          if (!Number.isFinite(parsedMs)) {{
            setStatus("Custom expiration must be a valid date and time.", "error");
            return;
          }}
          const epoch = Math.floor(parsedMs / 1000);
          const nowEpoch = Math.floor(Date.now() / 1000);
          if (epoch <= nowEpoch) {{
            setStatus("Expiration must be in the future.", "error");
            return;
          }}
          body.expiresEpochSeconds = epoch;
        }} else {{
          body.expiresEpochSeconds = null;
        }}

        try {{
          setStatus(`Creating ${{idValue}}...`);
          const payload = await api("/pixel-stack/identity/api/v1/identities", {{
            method: "POST",
            body: JSON.stringify(body),
          }});
          idInput.value = "";
          tokenInput.value = "";
          expirationSelect.value = "none";
          expirationCustomInput.value = "";
          $("create-primary").checked = false;
          syncCreateExpirationCustomToggle();
          await loadIdentities();
          await refreshUsage();
          const applied = payload?.applied !== false;
          const suffix = applied ? "" : " (saved; runtime reload pending)";
          const dotSuffix = payload?.dotHostname ? `; DoT hostname: ${{payload.dotHostname}}` : "";
          setStatus(`Created ${{payload.created}}${{suffix}}${{dotSuffix}}`, applied ? "ok" : "warn");
        }} catch (err) {{
          setStatus(err.message || "Create failed", "error");
        }}
      }}

      function syncWindowCustomToggle() {{
        const select = $("usage-window");
        const custom = $("usage-window-custom");
        custom.disabled = select.value !== "custom";
      }}

      function syncCreateExpirationCustomToggle() {{
        const select = $("create-expiration");
        const custom = $("create-expiration-custom");
        custom.disabled = select.value !== "custom";
      }}

      async function refreshPageData() {{
        const querylogPromise = refreshQuerylogSummary();
        await loadIdentities();
        await Promise.all([querylogPromise, refreshUsage()]);
      }}

      async function initialize() {{
        const params = new URLSearchParams(window.location.search);
        const returnTarget = normalizeReturnTarget(params.get("return") || "");
        $("back-link").setAttribute("href", returnTarget);

        $("refresh-btn").addEventListener("click", async () => {{
          try {{
            setStatus("Refreshing...");
            await refreshPageData();
            setStatus("Refreshed", "ok");
            setQuerylogStatus("Querylog refreshed", "ok");
          }} catch (err) {{
            setStatus(err.message || "Refresh failed", "error");
            setQuerylogStatus(err.message || "Querylog refresh failed", "error");
          }}
        }});
        $("usage-refresh-btn").addEventListener("click", async () => {{
          try {{
            setStatus("Refreshing usage...");
            await refreshUsage();
            setStatus("Usage refreshed", "ok");
          }} catch (err) {{
            setStatus(err.message || "Usage refresh failed", "error");
          }}
        }});
        $("querylog-refresh-btn").addEventListener("click", async () => {{
          try {{
            setQuerylogStatus("Refreshing querylog...");
            await refreshQuerylogSummary();
            setQuerylogStatus("Querylog refreshed", "ok");
          }} catch (err) {{
            setQuerylogStatus(err.message || "Querylog refresh failed", "error");
          }}
        }});
        $("querylog-include-internal").addEventListener("change", async () => {{
          try {{
            setQuerylogStatus("Applying querylog view...");
            await refreshQuerylogSummary();
            setQuerylogStatus("Querylog view updated", "ok");
          }} catch (err) {{
            setQuerylogStatus(err.message || "Querylog refresh failed", "error");
          }}
        }});
        $("create-btn").addEventListener("click", createIdentity);
        $("usage-window").addEventListener("change", syncWindowCustomToggle);
        $("create-expiration").addEventListener("change", syncCreateExpirationCustomToggle);

        syncWindowCustomToggle();
        syncCreateExpirationCustomToggle();
        renderQuerylogSummary();

        try {{
          await refreshPageData();
          setStatus("Loaded", "ok");
          setQuerylogStatus("Loaded", "ok");
        }} catch (err) {{
          setStatus(err.message || "Load failed", "error");
          setQuerylogStatus(err.message || "Querylog load failed", "error");
        }}
      }}

      initialize();
    }})();
  </script>
</body>
</html>
"""

  def _build_bootstrap_js(self) -> str:
    script = """(() => {
  if (window.__pixelStackIdentityBootstrapInstalled) {
    return;
  }
  window.__pixelStackIdentityBootstrapInstalled = true;

  const state = window.__pixelStackAdguardIdentity = window.__pixelStackAdguardIdentity || {
    dashboardUsagePayload: null,
    lastNativeQuerylogRequestUrl: "",
    querylogRows: [],
    querylogSessionKey: "",
    lastQuerylogPayload: null,
  };

  const normalizeUrl = (value) => {
    try {
      if (typeof value === "string") {
        return new URL(value, window.location.origin);
      }
      if (value && typeof value.url === "string") {
        return new URL(value.url, window.location.origin);
      }
    } catch (_error) {
      return null;
    }
    return null;
  };

  const currentHash = () => String(window.location.hash || "");
  const currentSearch = () => String(window.location.search || "");
  const hashParams = () => {
    const hash = currentHash();
    const idx = hash.indexOf("?");
    return new URLSearchParams(idx >= 0 ? hash.slice(idx + 1) : "");
  };
  const searchParams = () => new URLSearchParams(currentSearch());
  const currentRoute = () => {
    const hash = currentHash();
    if (!hash) {
      return "#";
    }
    const idx = hash.indexOf("?");
    return idx >= 0 ? hash.slice(0, idx) : hash;
  };
  const querylogIdentityFromHash = () => hashParams().get("pixel_identity") || "";
  const querylogIdentityFromSearch = () => searchParams().get("pixel_identity") || "";
  const resolvedQuerylogIdentity = () => querylogIdentityFromHash() || querylogIdentityFromSearch() || "";
  const isLogsRoute = () => currentRoute().toLowerCase() === "#logs";
  const requestUrlFromInput = (input) => {
    if (typeof Request !== "undefined" && input instanceof Request) {
      return input.url;
    }
    if (input && typeof input.url === "string") {
      return input.url;
    }
    return String(input || "");
  };
  const readResponseText = (response) => {
    const readable = response && typeof response.clone === "function" ? response.clone() : response;
    if (readable && typeof readable.text === "function") {
      return readable.text();
    }
    if (readable && typeof readable.json === "function") {
      return readable.json().then((payload) => JSON.stringify(payload));
    }
    return Promise.reject(new Error("response body unavailable"));
  };
  const querylogProxyUrl = (requestUrl) => {
    const parsed = normalizeUrl(requestUrl);
    if (!parsed || parsed.pathname !== "/control/querylog") {
      return null;
    }
    const proxy = new URL("/pixel-stack/identity/api/v1/adguard/querylog", window.location.origin);
    parsed.searchParams.forEach((value, key) => {
      proxy.searchParams.set(key, value);
    });
    const identity = resolvedQuerylogIdentity();
    if (identity) {
      proxy.searchParams.set("identity", identity);
    }
    return proxy;
  };
  const shouldRewriteNativeQuerylog = (requestUrl) => {
    const parsed = normalizeUrl(requestUrl);
    // Always proxy Logs-page querylog requests so native rows and modal details use enriched client IPs.
    return Boolean(parsed && parsed.pathname === "/control/querylog" && isLogsRoute());
  };

  const recordNativeQuerylog = (requestUrl, rawText) => {
    try {
      const payload = JSON.parse(rawText);
      if (!payload || !Array.isArray(payload.data)) {
        return;
      }
      const parsed = normalizeUrl(requestUrl);
      if (!parsed) {
        return;
      }
      state.lastNativeQuerylogRequestUrl = parsed.toString();
      window.dispatchEvent(new CustomEvent("pixelstack:native-querylog-updated", {
        detail: { requestUrl: parsed.toString() },
      }));
    } catch (_error) {
    }
  };

  const dispatchNativeDashboardUpdate = () => {
    window.dispatchEvent(new CustomEvent("pixelstack:native-dashboard-updated"));
  };

  const originalFetch = typeof window.fetch === "function" ? window.fetch.bind(window) : null;
  if (originalFetch) {
    window.fetch = async (input, init) => {
      const originalUrl = requestUrlFromInput(input);
      const originalPath = normalizeUrl(originalUrl)?.pathname || "";
      const proxyUrl = shouldRewriteNativeQuerylog(originalUrl) ? querylogProxyUrl(originalUrl) : null;
      const actualInput = proxyUrl
        ? (
            typeof Request !== "undefined" && input instanceof Request
              ? new Request(proxyUrl.toString(), input)
              : proxyUrl.toString()
          )
        : input;
      const response = await originalFetch(actualInput, init);
      if (
        originalPath === "/control/stats" ||
        originalPath === "/control/stats/config" ||
        originalPath === "/control/clients/search"
      ) {
        readResponseText(response).then(() => dispatchNativeDashboardUpdate()).catch(() => {});
      }
      if (originalPath === "/control/querylog") {
        readResponseText(response).then((text) => recordNativeQuerylog(originalUrl, text)).catch(() => {});
      }
      return response;
    };
  }

  const originalOpen = XMLHttpRequest.prototype.open;
  const originalSend = XMLHttpRequest.prototype.send;

  XMLHttpRequest.prototype.open = function(method, url, ...rest) {
    const originalUrl = typeof url === "string" ? url : String(url || "");
    this.__pixelStackOriginalUrl = originalUrl;
    const proxyUrl = shouldRewriteNativeQuerylog(originalUrl) ? querylogProxyUrl(originalUrl) : null;
    return originalOpen.call(this, method, proxyUrl ? proxyUrl.toString() : url, ...rest);
  };

  XMLHttpRequest.prototype.send = function(body) {
    const originalPath = normalizeUrl(this.__pixelStackOriginalUrl || "")?.pathname || "";
    if (
      originalPath === "/control/stats" ||
      originalPath === "/control/stats/config" ||
      originalPath === "/control/clients/search"
    ) {
      this.addEventListener("loadend", () => {
        dispatchNativeDashboardUpdate();
      }, { once: true });
    }
    if (originalPath === "/control/querylog") {
      this.addEventListener("loadend", () => {
        if (typeof this.responseText === "string" && this.responseText) {
          recordNativeQuerylog(this.__pixelStackOriginalUrl, this.responseText);
        }
      }, { once: true });
    }
    return originalSend.call(this, body);
  };
})();"""
    return script

  def _build_inject_js(self) -> str:
    settings_prefixes_json = json.dumps(list(SETTINGS_HASH_PREFIXES))
    identity_labels_json = json.dumps(IDENTITY_LABELS)
    script = """(() => {
  if (window.__pixelStackDohIdentityInjected) {
    return;
  }
  window.__pixelStackDohIdentityInjected = true;

  const SETTINGS_PREFIXES = __SETTINGS_PREFIXES__;
  const IDENTITY_LABELS = __IDENTITY_LABELS__;
  const BUTTON_ID = "pixel-stack-doh-identities-btn";
  const DASHBOARD_CARD_ID = "pixel-stack-top-identities-card";
  const QUERYLOG_FILTER_ID = "pixel-stack-querylog-identity";
  const STYLE_ID = "pixel-stack-doh-identity-styles";
  const WAIT_TIMEOUT_MS = 15000;
  const WAIT_INTERVAL_MS = 75;
  const DASHBOARD_NATIVE_REFRESH_DELAY_MS = 16;
  const DASHBOARD_REFRESH_THROTTLE_MS = 500;
  const DASHBOARD_DESKTOP_MEDIA_QUERY = "(min-width: 992px)";
  const DASHBOARD_SUMMARY_VARIANTS = {
    "#logs": "dns",
    "#logs?response_status=blocked": "blocked",
    "#logs?response_status=blocked_safebrowsing": "safebrowsing",
    "#logs?response_status=blocked_parental": "adult",
  };
  const DASHBOARD_SUMMARY_ORDER = ["dns", "blocked", "safebrowsing", "adult"];
  const state = window.__pixelStackAdguardIdentity = window.__pixelStackAdguardIdentity || {
    lastNativeQuerylogRequestUrl: "",
    querylogRows: [],
    querylogSessionKey: "",
    lastQuerylogPayload: null,
  };
  const routeState = {
    waiters: new Map(),
    dashboardActive: false,
    dashboardLoaded: false,
    dashboardLastUsageFetchAt: 0,
    dashboardRefreshQueued: false,
    dashboardRefreshNonce: 0,
    dashboardRouteToken: 0,
    querylogActive: false,
    querylogRefreshNonce: 0,
    querylogRouteToken: 0,
    querylogEnrichmentPromise: null,
    querylogEnrichmentRequestUrl: "",
    querylogOptions: null,
    querylogOptionsPromise: null,
  };

  const api = async (path, init) => {
    const response = await fetch(path, init);
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    return await response.json();
  };

  const identityLabel = (identityId) => IDENTITY_LABELS[identityId] || identityId || "Unknown identity";

  const ensureStyles = () => {
    if (document.getElementById(STYLE_ID)) {
      return;
    }
    const style = document.createElement("style");
    style.id = STYLE_ID;
    style.textContent = `
      .pixel-stack-settings-actions { margin-left: auto; display: flex; align-items: center; gap: 8px; }
      .pixel-stack-identity-chip {
        display: inline-flex;
        align-items: center;
        margin-top: 6px;
        padding: 2px 8px;
        border-radius: 999px;
        background: rgba(47, 140, 90, 0.12);
        color: #2f8c5a;
        font-size: 11px;
        font-weight: 600;
      }
      .pixel-stack-identities-table { width: 100%; border-collapse: collapse; }
      .pixel-stack-identities-table th,
      .pixel-stack-identities-table td { padding: 8px 0; border-top: 1px solid rgba(0, 0, 0, 0.05); }
      .pixel-stack-identities-table th { padding-top: 0; border-top: 0; font-size: 12px; color: var(--gray-600, #6c757d); text-transform: uppercase; }
      .pixel-stack-identities-table td:last-child,
      .pixel-stack-identities-table th:last-child { text-align: right; }
      .pixel-stack-identities-empty { color: var(--gray-600, #6c757d); padding: 8px 0 0; }
      .pixel-stack-querylog-filter { min-width: 220px; }
      .pixel-stack-summary-row {
        --pixel-stack-summary-full-height: 10rem;
        --pixel-stack-summary-half-height: 5rem;
        --pixel-stack-summary-quarter-height: 2.5rem;
      }
      .pixel-stack-summary-row > .pixel-stack-summary-col { display: flex; flex-direction: column; }
      .pixel-stack-summary-card.card--full {
        overflow: hidden;
        height: var(--pixel-stack-summary-card-height, calc(100% - 1.5rem));
        min-height: var(--pixel-stack-summary-card-height, calc(100% - 1.5rem));
      }
      .pixel-stack-summary-card .card-wrap { height: 100%; }
      .pixel-stack-summary-card .card-title-stats,
      .pixel-stack-summary-card .card-title-stats a {
        display: block;
        overflow: hidden;
        text-overflow: ellipsis;
      }
      .pixel-stack-summary-card--dns .card-wrap { position: relative; }
      .pixel-stack-summary-card--dns .card-body-stats { position: relative; z-index: 2; padding-bottom: 0.75rem; }
      .pixel-stack-summary-card--dns .card-value-stats,
      .pixel-stack-summary-card--dns .card-title-stats,
      .pixel-stack-summary-card--dns .card-title-stats a { text-shadow: 0 1px 2px rgba(255, 255, 255, 0.92), 0 0 0.8rem rgba(255, 255, 255, 0.72); }
      .pixel-stack-summary-card--dns .card-chart-bg { position: absolute; inset: 0; height: 100%; min-height: 100%; z-index: 1; }
      .pixel-stack-summary-card--compact .card-body-stats { padding: 0.75rem 0.9rem 1.15rem; }
      .pixel-stack-summary-card--compact .card-value-stats { font-size: 1.75rem; line-height: 1; }
      .pixel-stack-summary-card--compact .card-title-stats { font-size: 0.8rem; line-height: 1.1; max-width: calc(100% - 3.5rem); }
      .pixel-stack-summary-card--compact .card-value-percent { top: 0.65rem; right: 0.75rem; font-size: 0.8rem; }
      .pixel-stack-summary-card--compact .card-chart-bg { height: 1.25rem; min-height: 1.25rem; }
      .pixel-stack-summary-card--blocked { --pixel-stack-summary-card-height: var(--pixel-stack-summary-half-height); }
      .pixel-stack-summary-card--blocked .card-wrap { position: relative; }
      .pixel-stack-summary-card--blocked .card-body-stats { position: relative; z-index: 2; padding-bottom: 0.75rem; }
      .pixel-stack-summary-card--blocked .card-value-percent { z-index: 2; }
      .pixel-stack-summary-card--blocked .card-value-stats,
      .pixel-stack-summary-card--blocked .card-title-stats,
      .pixel-stack-summary-card--blocked .card-title-stats a,
      .pixel-stack-summary-card--blocked .card-value-percent { text-shadow: 0 1px 2px rgba(255, 255, 255, 0.92), 0 0 0.8rem rgba(255, 255, 255, 0.72); }
      .pixel-stack-summary-card--blocked .card-chart-bg { position: absolute; inset: 0; height: 100%; min-height: 100%; z-index: 1; }
      .pixel-stack-summary-card--safebrowsing,
      .pixel-stack-summary-card--adult { --pixel-stack-summary-card-height: var(--pixel-stack-summary-quarter-height); }
      .pixel-stack-summary-card--safebrowsing .card-body-stats,
      .pixel-stack-summary-card--adult .card-body-stats { padding: 0.45rem 0.7rem 0.9rem; }
      .pixel-stack-summary-card--safebrowsing .card-value-stats,
      .pixel-stack-summary-card--adult .card-value-stats { font-size: 1.1rem; line-height: 1; }
      .pixel-stack-summary-card--safebrowsing .card-title-stats,
      .pixel-stack-summary-card--adult .card-title-stats { font-size: 0.68rem; line-height: 1.05; max-width: 100%; }
      .pixel-stack-summary-card--safebrowsing .card-value-percent,
      .pixel-stack-summary-card--adult .card-value-percent { display: none; }
      .pixel-stack-summary-card--safebrowsing .card-chart-bg,
      .pixel-stack-summary-card--adult .card-chart-bg { height: 0.8rem; min-height: 0.8rem; }
      @media (min-width: 992px) {
        .pixel-stack-dashboard-desktop-surface {
          --pixel-stack-dashboard-gap: 1.5rem;
          --pixel-stack-dashboard-gap-sm: 0.75rem;
          --pixel-stack-dashboard-section-gap: 1.5rem;
        }
        .pixel-stack-dashboard-toolbar,
        .pixel-stack-summary-row,
        .pixel-stack-dashboard-masonry-row,
        .pixel-stack-dashboard-later-row {
          margin-top: 0 !important;
          margin-bottom: 0 !important;
        }
        .pixel-stack-dashboard-toolbar {
          display: flex;
          flex-wrap: wrap;
          align-items: center;
          column-gap: var(--pixel-stack-dashboard-gap);
          row-gap: var(--pixel-stack-dashboard-gap-sm);
          margin-bottom: var(--pixel-stack-dashboard-section-gap) !important;
        }
        .pixel-stack-summary-row {
          --pixel-stack-summary-grid-gap: var(--pixel-stack-dashboard-gap-sm);
          display: grid;
          grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
          grid-template-rows: repeat(4, minmax(0, calc((var(--pixel-stack-summary-full-height) - (var(--pixel-stack-summary-grid-gap) * 3)) / 4)));
          row-gap: var(--pixel-stack-summary-grid-gap);
          column-gap: var(--pixel-stack-dashboard-gap);
          margin-bottom: var(--pixel-stack-dashboard-section-gap) !important;
        }
        .pixel-stack-summary-row > .pixel-stack-summary-col {
          width: auto;
          max-width: none;
          flex: none;
          padding-top: 0;
          padding-bottom: 0;
        }
        .pixel-stack-summary-col--dns { grid-column: 1; grid-row: 1 / span 4; }
        .pixel-stack-summary-col--blocked { grid-column: 2; grid-row: 1 / span 2; }
        .pixel-stack-summary-col--safebrowsing { grid-column: 2; grid-row: 3 / span 1; }
        .pixel-stack-summary-col--adult { grid-column: 2; grid-row: 4 / span 1; }
        .pixel-stack-summary-row > .pixel-stack-summary-col > .pixel-stack-summary-card.card--full {
          height: 100%;
          min-height: 100%;
        }
        .pixel-stack-dashboard-masonry-row {
          display: block;
        }
        .pixel-stack-dashboard-masonry-columns {
          display: grid;
          grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
          align-items: flex-start;
          gap: var(--pixel-stack-dashboard-gap);
        }
        .pixel-stack-dashboard-masonry-col {
          min-width: 0;
          display: flex;
          flex-direction: column;
          gap: var(--pixel-stack-dashboard-section-gap);
        }
        .pixel-stack-dashboard-masonry-col > .pixel-stack-dashboard-masonry-item {
          width: 100%;
          max-width: none;
          flex: none;
        }
        .pixel-stack-dashboard-masonry-item > .card + .card {
          margin-top: var(--pixel-stack-dashboard-section-gap);
        }
        .pixel-stack-dashboard-later-row {
          margin-top: var(--pixel-stack-dashboard-section-gap) !important;
        }
        .pixel-stack-dashboard-later-row + .pixel-stack-dashboard-later-row {
          margin-top: var(--pixel-stack-dashboard-section-gap) !important;
        }
      }
    `;
    document.head.appendChild(style);
  };

  const classTokens = (node) => String((node && node.className) || "").split(/\\s+/).filter(Boolean);
  const hasClass = (node, className) => classTokens(node).includes(className);
  const removeClass = (node, className) => {
    if (!node || !className || !hasClass(node, className)) {
      return;
    }
    node.className = classTokens(node).filter((token) => token !== className).join(" ");
  };
  const addClass = (node, className) => {
    if (!node || !className || hasClass(node, className)) {
      return;
    }
    node.className = [...classTokens(node), className].join(" ");
  };
  const siblingElements = (node) => {
    const parent = node && node.parentElement;
    return parent ? Array.from(parent.children || []) : [];
  };
  const previousElementSibling = (node) => {
    const siblings = siblingElements(node);
    const index = siblings.indexOf(node);
    return index > 0 ? siblings[index - 1] : null;
  };
  const followingElementSiblings = (node) => {
    const siblings = siblingElements(node);
    const index = siblings.indexOf(node);
    return index >= 0 ? siblings.slice(index + 1) : [];
  };
  const normalizedSummaryHref = (href) => {
    if (!href) {
      return "";
    }
    try {
      return String(new URL(String(href), window.location.origin).hash || "");
    } catch (_error) {
      return String(href || "").trim();
    }
  };
  const dashboardSummaryDescriptor = (card) => {
    if (!card || !hasClass(card, "card--full")) {
      return null;
    }
    const titleLink = (card.querySelector(".card-title-stats") && card.querySelector(".card-title-stats").querySelector("a")) || card.querySelector("a");
    const variant = DASHBOARD_SUMMARY_VARIANTS[normalizedSummaryHref(titleLink && titleLink.getAttribute("href"))];
    const column = card.parentElement;
    const row = column && column.parentElement;
    if (!variant || !column || !row) {
      return null;
    }
    return { variant, card, column, row };
  };
  const dashboardSummaryDescriptors = () => {
    const descriptors = Array.from(document.querySelectorAll(".card")).map((card) => dashboardSummaryDescriptor(card)).filter(Boolean);
    const grouped = new Map();
    for (const descriptor of descriptors) {
      const rowDescriptors = grouped.get(descriptor.row) || [];
      rowDescriptors.push(descriptor);
      grouped.set(descriptor.row, rowDescriptors);
    }
    for (const rowDescriptors of grouped.values()) {
      const byVariant = new Map();
      for (const descriptor of rowDescriptors) {
        if (!byVariant.has(descriptor.variant)) {
          byVariant.set(descriptor.variant, descriptor);
        }
      }
      if (byVariant.size === DASHBOARD_SUMMARY_ORDER.length) {
        return DASHBOARD_SUMMARY_ORDER.map((variant) => byVariant.get(variant)).filter(Boolean);
      }
    }
    return [];
  };
  const decorateDashboardSummaryCards = () => {
    const descriptors = dashboardSummaryDescriptors();
    if (descriptors.length !== DASHBOARD_SUMMARY_ORDER.length) {
      return null;
    }
    const row = descriptors[0].row;
    addClass(row, "pixel-stack-summary-row");
    for (const descriptor of descriptors) {
      addClass(descriptor.column, "pixel-stack-summary-col");
      addClass(descriptor.column, `pixel-stack-summary-col--${descriptor.variant}`);
      addClass(descriptor.card, "pixel-stack-summary-card");
      addClass(descriptor.card, `pixel-stack-summary-card--${descriptor.variant}`);
      if (descriptor.variant !== "dns") {
        addClass(descriptor.card, "pixel-stack-summary-card--compact");
      }
      if (descriptor.variant === "safebrowsing" || descriptor.variant === "adult") {
        addClass(descriptor.card, "pixel-stack-summary-card--compact-quarter");
      }
    }
    return row;
  };
  const dashboardToolbarRow = (summaryRow = dashboardSummaryDescriptors()[0] && dashboardSummaryDescriptors()[0].row) => {
    let candidate = previousElementSibling(summaryRow);
    while (candidate) {
      if (
        candidate.querySelector &&
        (candidate.querySelector("h1") || candidate.querySelector(".page-title") || candidate.querySelector(".btn") || candidate.querySelector("button"))
      ) {
        return candidate;
      }
      candidate = previousElementSibling(candidate);
    }
    return null;
  };
  const dashboardCardsRow = () => Array.from(document.querySelectorAll(".row")).find((row) => (
    hasClass(row, "row-cards") &&
    hasClass(row, "dashboard")
  ));
  const dashboardDesktopMediaQuery = () => {
    if (!window.matchMedia || typeof window.matchMedia !== "function") {
      return null;
    }
    if (!state.dashboardDesktopMediaQuery) {
      state.dashboardDesktopMediaQuery = window.matchMedia(DASHBOARD_DESKTOP_MEDIA_QUERY);
    }
    return state.dashboardDesktopMediaQuery;
  };
  const isDesktopDashboardViewport = () => {
    const query = dashboardDesktopMediaQuery();
    return query ? Boolean(query.matches) : false;
  };
  const masonryItemOrder = (item) => Number(item && item.getAttribute && item.getAttribute("data-pixel-stack-dashboard-order")) || 0;
  const dashboardMasonryItems = (row) => {
    if (!row) {
      return [];
    }
    const directItems = Array.from(row.children || []).filter((child) => hasClass(child, "col-lg-6"));
    if (directItems.length) {
      return directItems;
    }
    return Array.from(row.querySelectorAll(".pixel-stack-dashboard-masonry-item"));
  };
  const dashboardItemHeight = (item) => {
    if (!item || typeof item.getBoundingClientRect !== "function") {
      return 0;
    }
    const rect = item.getBoundingClientRect();
    return Number(rect && rect.height) || 0;
  };
  const restoreDashboardMasonryLayout = (row = dashboardCardsRow()) => {
    if (!row) {
      return null;
    }
    const wrapper = row.querySelector(".pixel-stack-dashboard-masonry-columns");
    const items = Array.from(row.querySelectorAll(".pixel-stack-dashboard-masonry-item")).sort((left, right) => (
      masonryItemOrder(left) - masonryItemOrder(right)
    ));
    if (wrapper) {
      for (const item of items) {
        row.appendChild(item);
      }
      wrapper.remove();
    }
    removeClass(row, "pixel-stack-dashboard-masonry-row");
    for (const item of items) {
      removeClass(item, "pixel-stack-dashboard-masonry-item");
    }
    return row;
  };
  const decorateDashboardMasonryLayout = () => {
    const row = dashboardCardsRow();
    if (!row) {
      return null;
    }
    if (!isDashboardRoute() || !isDesktopDashboardViewport()) {
      return restoreDashboardMasonryLayout(row);
    }
    restoreDashboardMasonryLayout(row);
    const items = dashboardMasonryItems(row);
    if (!items.length) {
      return row;
    }
    items.forEach((item, index) => {
      if (!item.getAttribute("data-pixel-stack-dashboard-order")) {
        item.setAttribute("data-pixel-stack-dashboard-order", String(index));
      }
      addClass(item, "pixel-stack-dashboard-masonry-item");
    });
    const wrapper = document.createElement("div");
    wrapper.className = "pixel-stack-dashboard-masonry-columns";
    const leftColumn = document.createElement("div");
    leftColumn.className = "pixel-stack-dashboard-masonry-col";
    const rightColumn = document.createElement("div");
    rightColumn.className = "pixel-stack-dashboard-masonry-col";
    wrapper.appendChild(leftColumn);
    wrapper.appendChild(rightColumn);
    addClass(row, "pixel-stack-dashboard-masonry-row");
    row.appendChild(wrapper);
    let leftHeight = 0;
    let rightHeight = 0;
    items.sort((left, right) => masonryItemOrder(left) - masonryItemOrder(right)).forEach((item) => {
      const targetColumn = leftHeight <= rightHeight ? leftColumn : rightColumn;
      targetColumn.appendChild(item);
      const height = dashboardItemHeight(item);
      if (targetColumn === leftColumn) {
        leftHeight += height;
      } else {
        rightHeight += height;
      }
    });
    return row;
  };
  const decorateDashboardDesktopSpacing = () => {
    const summaryRow = decorateDashboardSummaryCards();
    const masonryRow = dashboardCardsRow();
    const dashboardSurface = (summaryRow && summaryRow.parentElement) || (masonryRow && masonryRow.parentElement);
    const toolbarRow = dashboardToolbarRow(summaryRow);
    if (!isDashboardRoute() || !isDesktopDashboardViewport()) {
      removeClass(dashboardSurface, "pixel-stack-dashboard-desktop-surface");
      removeClass(toolbarRow, "pixel-stack-dashboard-toolbar");
      removeClass(summaryRow, "pixel-stack-dashboard-summary-section");
      removeClass(masonryRow, "pixel-stack-dashboard-masonry-section");
      followingElementSiblings(masonryRow).forEach((row) => removeClass(row, "pixel-stack-dashboard-later-row"));
      return null;
    }
    addClass(dashboardSurface, "pixel-stack-dashboard-desktop-surface");
    addClass(toolbarRow, "pixel-stack-dashboard-toolbar");
    addClass(summaryRow, "pixel-stack-dashboard-summary-section");
    addClass(masonryRow, "pixel-stack-dashboard-masonry-section");
    followingElementSiblings(masonryRow).forEach((row) => {
      if (row && hasClass(row, "row") && row.querySelector && row.querySelector(".card")) {
        addClass(row, "pixel-stack-dashboard-later-row");
      }
    });
    return dashboardSurface;
  };
  const ensureDashboardMasonryBreakpointListener = () => {
    if (state.dashboardDesktopMediaQueryListening) {
      return;
    }
    const query = dashboardDesktopMediaQuery();
    if (!query) {
      return;
    }
    const handler = () => {
      if (isDashboardRoute()) {
        recoverDashboardRoute(false);
      } else {
        restoreDashboardMasonryLayout();
      }
    };
    if (typeof query.addEventListener === "function") {
      query.addEventListener("change", handler);
    } else if (typeof query.addListener === "function") {
      query.addListener(handler);
    } else {
      return;
    }
    state.dashboardDesktopMediaQueryListening = true;
  };

  const clearWait = (key) => {
    const handle = routeState.waiters.get(key);
    if (typeof handle === "number") {
      window.clearTimeout(handle);
    }
    routeState.waiters.delete(key);
  };

  const waitForElement = (key, getter, onReady) => {
    clearWait(key);
    const deadline = Date.now() + WAIT_TIMEOUT_MS;
    const tick = () => {
      const node = typeof getter === "function" ? getter() : document.querySelector(getter);
      if (node) {
        routeState.waiters.delete(key);
        onReady(node);
        return;
      }
      if (Date.now() >= deadline) {
        routeState.waiters.delete(key);
        return;
      }
      const handle = window.setTimeout(tick, WAIT_INTERVAL_MS);
      routeState.waiters.set(key, handle);
    };
    tick();
  };

  const currentHash = () => String(window.location.hash || "");
  const currentSearch = () => String(window.location.search || "");
  const hashParams = () => {
    const hash = currentHash();
    const idx = hash.indexOf("?");
    return new URLSearchParams(idx >= 0 ? hash.slice(idx + 1) : "");
  };
  const searchParams = () => new URLSearchParams(currentSearch());
  const currentRoute = () => {
    const hash = currentHash();
    if (!hash) {
      return "#";
    }
    const idx = hash.indexOf("?");
    return idx >= 0 ? hash.slice(0, idx) : hash;
  };
  const querylogIdentityFromHash = () => hashParams().get("pixel_identity") || "";
  const querylogIdentityFromSearch = () => searchParams().get("pixel_identity") || "";
  const resolvedQuerylogIdentity = () => querylogIdentityFromHash() || querylogIdentityFromSearch() || "";
  const updateSearchParam = (key, value) => {
    if (!window.history || typeof window.history.replaceState !== "function") {
      return;
    }
    try {
      const current = new URL(`${window.location.pathname}${currentSearch()}${currentHash()}`, window.location.origin);
      if (value) {
        current.searchParams.set(key, value);
      } else {
        current.searchParams.delete(key);
      }
      const nextUrl = `${current.pathname}${current.search}${current.hash}`;
      const currentUrl = `${window.location.pathname}${currentSearch()}${currentHash()}`;
      if (nextUrl !== currentUrl) {
        window.history.replaceState(window.history.state ?? null, "", nextUrl);
      }
    } catch (_error) {
    }
  };
  const updateHashParam = (key, value) => {
    const route = currentRoute() || "#logs";
    const params = hashParams();
    if (value) {
      params.set(key, value);
    } else {
      params.delete(key);
    }
    const nextHash = `${route}${params.toString() ? `?${params.toString()}` : ""}`;
    if (window.location.hash !== nextHash) {
      window.location.hash = nextHash;
    }
  };
  const syncQuerylogIdentityLink = (value = resolvedQuerylogIdentity()) => {
    updateSearchParam("pixel_identity", value || "");
  };
  const setQuerylogIdentity = (value) => {
    syncQuerylogIdentityLink(value || "");
    updateHashParam("pixel_identity", value || "");
  };
  const querylogRouteHref = (identityId) => {
    const url = new URL("/", window.location.origin);
    const normalizedIdentityId = String(identityId || "");
    if (normalizedIdentityId) {
      url.searchParams.set("pixel_identity", normalizedIdentityId);
    }
    const params = new URLSearchParams();
    params.set("response_status", "all");
    if (normalizedIdentityId) {
      params.set("pixel_identity", normalizedIdentityId);
    }
    url.hash = `#logs?${params.toString()}`;
    return `${url.pathname}${url.search}${url.hash}`;
  };
  const identityPageHref = () => {
    const url = new URL("/pixel-stack/identity", window.location.origin);
    url.searchParams.set("return", `${window.location.pathname}${currentSearch()}${window.location.hash || "#settings"}`);
    return `${url.pathname}${url.search}`;
  };

  const isSettingsRoute = () => {
    const hash = currentHash().toLowerCase();
    return SETTINGS_PREFIXES.some((prefix) => (
      hash === prefix ||
      hash.startsWith(`${prefix}/`) ||
      hash.startsWith(`${prefix}?`)
    ));
  };
  const isDashboardRoute = () => {
    const route = currentRoute().toLowerCase();
    return route === "" || route === "#";
  };
  const isLogsRoute = () => currentRoute().toLowerCase() === "#logs";
  const mountSettingsRoute = () => {
    waitForElement("pixel-stack-settings", ".page-header", () => {
      if (!isSettingsRoute()) {
        return;
      }
      ensureButton();
    });
  };

  const resetSettingsRoute = () => {
    clearWait("pixel-stack-settings");
    removeButton();
  };

  const ensureButton = () => {
    const header = document.querySelector(".page-header");
    if (!header) {
      return;
    }
    let actionWrap = header.querySelector(".pixel-stack-settings-actions");
    if (!actionWrap) {
      actionWrap = document.createElement("div");
      actionWrap.className = "pixel-stack-settings-actions";
      header.appendChild(actionWrap);
    }
    let button = document.getElementById(BUTTON_ID);
    if (!button) {
      button = document.createElement("a");
      button.id = BUTTON_ID;
      button.className = "btn btn-outline-primary btn-sm";
      button.textContent = "DNS identities";
      actionWrap.appendChild(button);
    }
    button.setAttribute("href", identityPageHref());
  };

  const removeButton = () => {
    const button = document.getElementById(BUTTON_ID);
    if (button) {
      const parent = button.parentElement;
      button.remove();
      if (parent && !parent.children.length) {
        parent.remove();
      }
    }
  };

  const topClientsCard = () => Array.from(document.querySelectorAll(".card")).find((card) => {
    const title = card.querySelector(".card-title");
    return title && String(title.textContent || "").trim() === "Top clients";
  });

  const renderDashboardIdentities = (body, payload) => {
    const rows = payload && Array.isArray(payload.identities) ? payload.identities : [];
    body.innerHTML = "";
    if (!rows.length) {
      body.innerHTML = `<tr><td colspan="2" class="pixel-stack-identities-empty">No identity traffic in the last 24 hours.</td></tr>`;
      return;
    }
    for (const row of rows) {
      const identityId = String(row.id || "");
      const requestCount = Number(row.requestCount ?? row.requests ?? 0);
      const tr = document.createElement("tr");
      const tdIdentity = document.createElement("td");
      const link = document.createElement("a");
      link.setAttribute("href", querylogRouteHref(identityId));
      link.textContent = identityLabel(identityId);
      tdIdentity.appendChild(link);
      const tdCount = document.createElement("td");
      tdCount.textContent = requestCount.toLocaleString();
      tr.appendChild(tdIdentity);
      tr.appendChild(tdCount);
      body.appendChild(tr);
    }
  };

  const ensureDashboardCard = () => {
    const anchorCard = topClientsCard();
    if (!anchorCard || !anchorCard.parentElement) {
      return null;
    }
    let card = document.getElementById(DASHBOARD_CARD_ID);
    if (!card) {
      card = document.createElement("div");
      card.id = DASHBOARD_CARD_ID;
      card.className = "card";
      card.innerHTML = `
        <div class="card-header with-border">
          <div class="card-inner">
            <div class="card-title">Top identities</div>
            <div class="card-subtitle">for the last 24 hours</div>
          </div>
          <div class="card-options">
            <button type="button" class="btn btn-icon btn-outline-primary btn-sm" title="Refresh" data-pixel-refresh-identities="1">
              <svg class="icons icon12"><use xlink:href="#refresh"></use></svg>
            </button>
          </div>
        </div>
        <div class="card-table">
          <table class="pixel-stack-identities-table">
            <thead>
              <tr><th>Identity</th><th>Requests count</th></tr>
            </thead>
            <tbody data-pixel-identities-body="1">
              <tr><td colspan="2" class="pixel-stack-identities-empty">Loading...</td></tr>
            </tbody>
          </table>
        </div>
      `;
      anchorCard.insertAdjacentElement("afterend", card);
      const refreshButton = card.querySelector("[data-pixel-refresh-identities='1']");
      if (refreshButton) {
        refreshButton.addEventListener("click", () => {
          void loadDashboardIdentities(true);
        });
      }
    }
    const body = card.querySelector("[data-pixel-identities-body='1']");
    if (body && state.dashboardUsagePayload) {
      renderDashboardIdentities(body, state.dashboardUsagePayload);
      decorateDashboardMasonryLayout();
      decorateDashboardDesktopSpacing();
    }
    return card;
  };

  const scheduleDashboardUsageRefresh = (routeToken, delayMs = 0) => {
    clearWait("pixel-stack-dashboard-usage-refresh");
    const handle = window.setTimeout(() => {
      routeState.waiters.delete("pixel-stack-dashboard-usage-refresh");
      if (!isDashboardRoute() || routeState.dashboardRouteToken !== routeToken) {
        return;
      }
      void loadDashboardIdentities(true, routeToken);
    }, Math.max(0, delayMs));
    routeState.waiters.set("pixel-stack-dashboard-usage-refresh", handle);
  };

  let dashboardFetchPromise = null;
  const loadDashboardIdentities = async (force = false, routeToken = routeState.dashboardRouteToken) => {
    if (!isDashboardRoute()) {
      return;
    }
    if (routeToken !== routeState.dashboardRouteToken) {
      return;
    }
    const card = ensureDashboardCard();
    if (!card) {
      return;
    }
    if (dashboardFetchPromise) {
      if (force) {
        routeState.dashboardRefreshQueued = true;
      }
      return dashboardFetchPromise;
    }
    const now = Date.now();
    if (
      force &&
      routeState.dashboardLastUsageFetchAt &&
      state.dashboardUsagePayload &&
      (now - routeState.dashboardLastUsageFetchAt) < DASHBOARD_REFRESH_THROTTLE_MS
    ) {
      scheduleDashboardUsageRefresh(routeToken, DASHBOARD_REFRESH_THROTTLE_MS - (now - routeState.dashboardLastUsageFetchAt));
      return;
    }
    if (routeState.dashboardLoaded && !force) {
      return;
    }
    const body = card.querySelector("[data-pixel-identities-body='1']");
    routeState.dashboardLastUsageFetchAt = now;
    const activeRouteToken = routeToken;
    dashboardFetchPromise = (async () => {
      try {
        const payload = await api("/pixel-stack/identity/api/v1/usage?identity=all&window=24h");
        if (!isDashboardRoute() || routeState.dashboardRouteToken !== activeRouteToken || !body.isConnected) {
          return;
        }
        state.dashboardUsagePayload = payload;
        renderDashboardIdentities(body, payload);
        decorateDashboardMasonryLayout();
        decorateDashboardDesktopSpacing();
        routeState.dashboardLoaded = true;
      } catch (_error) {
        routeState.dashboardLoaded = false;
        body.innerHTML = `<tr><td colspan="2" class="pixel-stack-identities-empty">Unable to load identities.</td></tr>`;
        decorateDashboardMasonryLayout();
        decorateDashboardDesktopSpacing();
      } finally {
        dashboardFetchPromise = null;
        if (routeState.dashboardRefreshQueued && isDashboardRoute() && routeState.dashboardRouteToken === activeRouteToken) {
          routeState.dashboardRefreshQueued = false;
          const remainingThrottleMs = Math.max(
            0,
            DASHBOARD_REFRESH_THROTTLE_MS - (Date.now() - routeState.dashboardLastUsageFetchAt),
          );
          scheduleDashboardUsageRefresh(activeRouteToken, remainingThrottleMs);
        }
      }
    })();
    return dashboardFetchPromise;
  };

  const scheduleDashboardMount = (routeToken) => {
    clearWait("pixel-stack-dashboard");
    const deadline = Date.now() + WAIT_TIMEOUT_MS;
    const tick = () => {
      if (!isDashboardRoute() || routeState.dashboardRouteToken !== routeToken) {
        routeState.waiters.delete("pixel-stack-dashboard");
        return;
      }
      decorateDashboardSummaryCards();
      const anchorCard = topClientsCard();
      if (!anchorCard) {
        if (Date.now() >= deadline) {
          routeState.waiters.delete("pixel-stack-dashboard");
          return;
        }
        const handle = window.setTimeout(tick, WAIT_INTERVAL_MS);
        routeState.waiters.set("pixel-stack-dashboard", handle);
        return;
      }
      routeState.waiters.delete("pixel-stack-dashboard");
      const card = ensureDashboardCard();
      if (!card) {
        if (Date.now() >= deadline) {
          return;
        }
        const handle = window.setTimeout(tick, WAIT_INTERVAL_MS);
        routeState.waiters.set("pixel-stack-dashboard", handle);
        return;
      }
      decorateDashboardMasonryLayout();
      decorateDashboardDesktopSpacing();
      void loadDashboardIdentities(false, routeToken);
    };
    tick();
  };

  const scheduleDashboardRecovery = (routeToken, forceRefresh = false) => {
    clearWait("pixel-stack-dashboard-refresh");
    const refreshNonce = routeState.dashboardRefreshNonce;
    const tick = () => {
      if (!isDashboardRoute() || routeState.dashboardRouteToken !== routeToken || routeState.dashboardRefreshNonce !== refreshNonce) {
        routeState.waiters.delete("pixel-stack-dashboard-refresh");
        return;
      }
      decorateDashboardSummaryCards();
      const card = ensureDashboardCard();
      if (!card) {
        const handle = window.setTimeout(tick, WAIT_INTERVAL_MS);
        routeState.waiters.set("pixel-stack-dashboard-refresh", handle);
        return;
      }
      routeState.waiters.delete("pixel-stack-dashboard-refresh");
      if (state.dashboardUsagePayload) {
        const body = card.querySelector("[data-pixel-identities-body='1']");
        if (body) {
          renderDashboardIdentities(body, state.dashboardUsagePayload);
        }
      }
      decorateDashboardMasonryLayout();
      decorateDashboardDesktopSpacing();
      void loadDashboardIdentities(forceRefresh, routeToken);
    };
    tick();
  };

  const mountDashboardRoute = () => {
    if (!routeState.dashboardActive) {
      routeState.dashboardActive = true;
      routeState.dashboardLoaded = false;
      routeState.dashboardRouteToken += 1;
    }
    scheduleDashboardMount(routeState.dashboardRouteToken);
  };

  const resetDashboardRoute = () => {
    clearWait("pixel-stack-dashboard");
    clearWait("pixel-stack-dashboard-native-refresh");
    clearWait("pixel-stack-dashboard-usage-refresh");
    routeState.dashboardActive = false;
    routeState.dashboardLoaded = false;
    routeState.dashboardLastUsageFetchAt = 0;
    routeState.dashboardRefreshQueued = false;
    routeState.dashboardRouteToken += 1;
    dashboardFetchPromise = null;
    decorateDashboardDesktopSpacing();
    restoreDashboardMasonryLayout();
    removeDashboardCard();
  };

  const removeDashboardCard = () => {
    const card = document.getElementById(DASHBOARD_CARD_ID);
    if (card) {
      card.remove();
    }
  };

  const loadQuerylogOptions = async () => {
    if (Array.isArray(routeState.querylogOptions)) {
      return routeState.querylogOptions;
    }
    if (routeState.querylogOptionsPromise) {
      return routeState.querylogOptionsPromise;
    }
    routeState.querylogOptionsPromise = (async () => {
      const identitiesPayload = await api("/pixel-stack/identity/api/v1/identities");
      const options = [{ id: "", label: "All identities" }];
      const seen = new Set([""]);
      const identities = Array.isArray(identitiesPayload.identities) ? identitiesPayload.identities : [];
      for (const entry of identities) {
        const identityId = String(entry.id || "");
        if (!identityId || seen.has(identityId)) {
          continue;
        }
        seen.add(identityId);
        options.push({ id: identityId, label: identityLabel(identityId) });
      }
      routeState.querylogOptions = options;
      return options;
    })();
    try {
      return await routeState.querylogOptionsPromise;
    } finally {
      routeState.querylogOptionsPromise = null;
    }
  };

  const querylogProxyUrl = (sourceUrl = "") => {
    let parsed = null;
    if (sourceUrl) {
      try {
        parsed = new URL(sourceUrl, window.location.origin);
      } catch (_error) {
        parsed = null;
      }
    }
    if (!parsed || parsed.pathname !== "/control/querylog") {
      parsed = new URL("/control/querylog", window.location.origin);
      const params = hashParams();
      parsed.searchParams.set("search", params.get("search") || "");
      parsed.searchParams.set("response_status", params.get("response_status") || "all");
      parsed.searchParams.set("older_than", params.get("older_than") || "");
      parsed.searchParams.set("limit", params.get("limit") || "20");
    }
    const proxy = new URL("/pixel-stack/identity/api/v1/adguard/querylog", window.location.origin);
    parsed.searchParams.forEach((value, key) => {
      proxy.searchParams.set(key, value);
    });
    const identity = resolvedQuerylogIdentity();
    if (identity) {
      proxy.searchParams.set("identity", identity);
    } else {
      proxy.searchParams.delete("identity");
    }
    return proxy.toString();
  };

  const querylogSessionKeyForUrl = (requestUrl, payload = null) => {
    let parsed = null;
    try {
      parsed = new URL(requestUrl, window.location.origin);
    } catch (_error) {
      return "";
    }
    return JSON.stringify({
      search: parsed.searchParams.get("search") || "",
      responseStatus: parsed.searchParams.get("response_status") || "all",
      identity: (payload && payload.pixelIdentityRequested) || resolvedQuerylogIdentity() || "",
    });
  };

  const ingestEnrichedQuerylog = (requestUrl, payload) => {
    if (!payload || !Array.isArray(payload.data)) {
      return;
    }
    let parsed = null;
    try {
      parsed = new URL(requestUrl, window.location.origin);
    } catch (_error) {
      return;
    }
    const nextSessionKey = querylogSessionKeyForUrl(requestUrl, payload);
    const append = Boolean(parsed.searchParams.get("older_than")) && state.querylogSessionKey === nextSessionKey;
    const batch = payload.data.map((row) => ({
      identityId: String(row.pixelIdentityId || ""),
      identityLabel: String((row.pixelIdentity && row.pixelIdentity.label) || ""),
    }));
    state.querylogRows = append ? state.querylogRows.concat(batch) : batch;
    state.querylogSessionKey = nextSessionKey;
    state.lastQuerylogPayload = payload;
    window.dispatchEvent(new CustomEvent("pixelstack:querylog-updated"));
  };

  const refreshQuerylogEnrichment = async (sourceUrl = "", routeToken = routeState.querylogRouteToken) => {
    if (!isLogsRoute()) {
      return;
    }
    if (routeToken !== routeState.querylogRouteToken) {
      return;
    }
    const requestUrl = querylogProxyUrl(sourceUrl || state.lastNativeQuerylogRequestUrl || "");
    if (routeState.querylogEnrichmentPromise && routeState.querylogEnrichmentRequestUrl === requestUrl) {
      return routeState.querylogEnrichmentPromise;
    }
    routeState.querylogEnrichmentRequestUrl = requestUrl;
    routeState.querylogEnrichmentPromise = (async () => {
      try {
        const payload = await api(requestUrl);
        if (!isLogsRoute() || routeState.querylogRouteToken !== routeToken) {
          return;
        }
        ingestEnrichedQuerylog(requestUrl, payload);
      } finally {
        if (routeState.querylogEnrichmentRequestUrl === requestUrl) {
          routeState.querylogEnrichmentPromise = null;
        }
      }
    })();
    return routeState.querylogEnrichmentPromise;
  };

  const scheduleQuerylogRecovery = (routeToken, requestUrl = "") => {
    clearWait("pixel-stack-querylog-refresh");
    const refreshNonce = routeState.querylogRefreshNonce;
    const deadline = Date.now() + WAIT_TIMEOUT_MS;
    const tick = () => {
      if (!isLogsRoute() || routeState.querylogRouteToken !== routeToken || routeState.querylogRefreshNonce !== refreshNonce) {
        routeState.waiters.delete("pixel-stack-querylog-refresh");
        return;
      }
      const form = document.querySelector("form.form-control--container");
      if (!form) {
        const handle = window.setTimeout(tick, WAIT_INTERVAL_MS);
        routeState.waiters.set("pixel-stack-querylog-refresh", handle);
        return;
      }
      void ensureQuerylogFilter();
      renderQuerylogIdentityChips();
      void refreshQuerylogEnrichment(requestUrl || state.lastNativeQuerylogRequestUrl || "", routeToken);
      const needsRowRecovery = Array.isArray(state.querylogRows) && state.querylogRows.length > 0 && document.querySelectorAll("[data-testid='querylog_cell']").length === 0;
      const needsFilterRecovery = !document.getElementById(`${QUERYLOG_FILTER_ID}-wrap`);
      if ((needsRowRecovery || needsFilterRecovery) && Date.now() < deadline) {
        const handle = window.setTimeout(tick, WAIT_INTERVAL_MS);
        routeState.waiters.set("pixel-stack-querylog-refresh", handle);
        return;
      }
      routeState.waiters.delete("pixel-stack-querylog-refresh");
    };
    tick();
  };

  const buildQuerylogOptions = (currentValue) => {
    const baseOptions = Array.isArray(routeState.querylogOptions) ? routeState.querylogOptions : [{ id: "", label: "All identities" }];
    const options = baseOptions.map((option) => ({ id: option.id, label: option.label }));
    const seen = new Set(options.map((option) => option.id));
    const extraIds = [];
    const payloadRows = state.lastQuerylogPayload && Array.isArray(state.lastQuerylogPayload.data)
      ? state.lastQuerylogPayload.data
      : [];
    for (const row of payloadRows) {
      const identityId = String(row.pixelIdentityId || "");
      if (!identityId || seen.has(identityId)) {
        continue;
      }
      seen.add(identityId);
      extraIds.push(identityId);
    }
    extraIds.sort((left, right) => identityLabel(left).localeCompare(identityLabel(right)));
    for (const identityId of extraIds) {
      options.push({ id: identityId, label: identityLabel(identityId) });
    }
    if (currentValue && !seen.has(currentValue)) {
      options.push({ id: currentValue, label: identityLabel(currentValue) });
    }
    return options;
  };

  const ensureQuerylogFilter = async () => {
    if (!isLogsRoute()) {
      return;
    }
    if (!Array.isArray(routeState.querylogOptions)) {
      await loadQuerylogOptions();
      if (!isLogsRoute()) {
        return;
      }
    }
    const form = document.querySelector("form.form-control--container");
    if (!form) {
      return;
    }
    let field = document.getElementById(`${QUERYLOG_FILTER_ID}-wrap`);
    if (!field) {
      field = document.createElement("div");
      field.id = `${QUERYLOG_FILTER_ID}-wrap`;
      field.className = "field__select pixel-stack-querylog-filter";
      field.innerHTML = `<select id="${QUERYLOG_FILTER_ID}" class="form-control custom-select custom-select--logs custom-select__arrow--left form-control--transparent d-sm-block"></select>`;
      form.appendChild(field);
      field.querySelector("select").addEventListener("change", (event) => {
        setQuerylogIdentity(event.target.value || "");
      });
    }
    const select = field.querySelector("select");
    const currentValue = resolvedQuerylogIdentity();
    syncQuerylogIdentityLink(currentValue);
    const options = buildQuerylogOptions(currentValue);
    select.innerHTML = "";
    for (const option of options) {
      const node = document.createElement("option");
      node.value = option.id;
      node.textContent = option.label;
      if (option.id === currentValue) {
        node.selected = true;
      }
      select.appendChild(node);
    }
  };

  const mountQuerylogRoute = () => {
    if (!routeState.querylogActive) {
      routeState.querylogActive = true;
      routeState.querylogRouteToken += 1;
    }
    waitForElement("pixel-stack-querylog", () => document.querySelector("form.form-control--container"), () => {
      if (!isLogsRoute()) {
        return;
      }
      void ensureQuerylogFilter();
      renderQuerylogIdentityChips();
      void refreshQuerylogEnrichment("", routeState.querylogRouteToken);
    });
  };

  const resetQuerylogRoute = () => {
    clearWait("pixel-stack-querylog");
    routeState.querylogActive = false;
    routeState.querylogRouteToken += 1;
    routeState.querylogEnrichmentPromise = null;
    routeState.querylogEnrichmentRequestUrl = "";
    routeState.querylogOptions = null;
    routeState.querylogOptionsPromise = null;
    state.querylogRows = [];
    state.querylogSessionKey = "";
    state.lastQuerylogPayload = null;
    removeQuerylogFilter();
  };

  const removeQuerylogFilter = () => {
    const field = document.getElementById(`${QUERYLOG_FILTER_ID}-wrap`);
    if (field) {
      field.remove();
    }
  };

  const renderQuerylogIdentityChips = () => {
    if (!isLogsRoute()) {
      return;
    }
    const rows = document.querySelectorAll("[data-testid='querylog_cell']");
    rows.forEach((row, index) => {
      const clientCell = row.querySelector(".logs__cell--client");
      if (!clientCell) {
        return;
      }
      let chip = clientCell.querySelector(".pixel-stack-identity-chip");
      const entry = Array.isArray(state.querylogRows) ? state.querylogRows[index] : null;
      if (!entry || !entry.identityId) {
        if (chip) {
          chip.remove();
        }
        return;
      }
      if (!chip) {
        chip = document.createElement("div");
        chip.className = "pixel-stack-identity-chip";
        const menuButton = clientCell.querySelector("button");
        if (menuButton && menuButton.parentElement === clientCell) {
          clientCell.insertBefore(chip, menuButton);
        } else {
          clientCell.appendChild(chip);
        }
      }
      chip.textContent = entry.identityLabel || identityLabel(entry.identityId);
      chip.dataset.identityId = entry.identityId;
    });
  };

  const recoverSettingsRoute = () => {
    if (!isSettingsRoute()) {
      return;
    }
    if (!document.getElementById(BUTTON_ID)) {
      mountSettingsRoute();
    }
  };

  const recoverDashboardRoute = (forceRefresh = false) => {
    if (!isDashboardRoute()) {
      return;
    }
    routeState.dashboardRefreshNonce += 1;
    scheduleDashboardRecovery(routeState.dashboardRouteToken, forceRefresh);
  };

  const scheduleNativeDashboardRefresh = () => {
    clearWait("pixel-stack-dashboard-native-refresh");
    const handle = window.setTimeout(() => {
      routeState.waiters.delete("pixel-stack-dashboard-native-refresh");
      recoverSettingsRoute();
      recoverDashboardRoute(true);
    }, DASHBOARD_NATIVE_REFRESH_DELAY_MS);
    routeState.waiters.set("pixel-stack-dashboard-native-refresh", handle);
  };

  const recoverQuerylogRoute = (requestUrl = "") => {
    if (!isLogsRoute()) {
      return;
    }
    routeState.querylogRefreshNonce += 1;
    scheduleQuerylogRecovery(routeState.querylogRouteToken, requestUrl);
  };

  const sync = () => {
    ensureStyles();
    ensureDashboardMasonryBreakpointListener();
    if (isSettingsRoute()) {
      mountSettingsRoute();
    } else {
      resetSettingsRoute();
    }
    if (isDashboardRoute()) {
      decorateDashboardSummaryCards();
      decorateDashboardDesktopSpacing();
      mountDashboardRoute();
    } else {
      resetDashboardRoute();
    }
    if (isLogsRoute()) {
      mountQuerylogRoute();
    } else {
      resetQuerylogRoute();
    }
  };

  window.addEventListener("hashchange", sync, { passive: true });
  window.addEventListener("popstate", sync, { passive: true });
  window.addEventListener("pixelstack:querylog-updated", () => {
    if (!isLogsRoute()) {
      return;
    }
    void ensureQuerylogFilter();
    renderQuerylogIdentityChips();
  }, { passive: true });
  window.addEventListener("pixelstack:native-dashboard-updated", () => {
    scheduleNativeDashboardRefresh();
  }, { passive: true });
  window.addEventListener("pixelstack:native-querylog-updated", (event) => {
    const requestUrl = event && event.detail && typeof event.detail.requestUrl === "string"
      ? event.detail.requestUrl
      : "";
    if (requestUrl) {
      state.lastNativeQuerylogRequestUrl = requestUrl;
    }
    if (!isLogsRoute()) {
      return;
    }
    recoverQuerylogRoute(requestUrl);
  }, { passive: true });
  document.addEventListener("DOMContentLoaded", sync, { once: true });
  sync();
})();"""
    return script.replace("__SETTINGS_PREFIXES__", settings_prefixes_json).replace("__IDENTITY_LABELS__", identity_labels_json)

  def _handle_api_get(self, path: str, query: dict[str, list[str]]) -> None:
    if path == "/pixel-stack/identity/api/v1/identities":
      payload = self._cached_identities_payload()
      self._send_json(HTTPStatus.OK, payload)
      return

    if path == "/pixel-stack/identity/api/v1/usage":
      identity = (query.get("identity") or ["all"])[0].strip()
      window = (query.get("window") or [""])[0].strip()
      self._send_json(HTTPStatus.OK, self._cached_usage_payload(identity, window))
      return

    if path == "/pixel-stack/identity/api/v1/querylog/summary":
      view_mode = self._parse_querylog_view(query)
      limit = self._parse_querylog_limit(query)
      try:
        rows = self._querylog_rows(limit)
      except QuerylogFetchError as exc:
        self._send_json(HTTPStatus.BAD_GATEWAY, {"error": exc.message, "querylog_status": exc.querylog_status})
        return
      self._send_json(HTTPStatus.OK, self._summarize_querylog(rows, view_mode=view_mode, limit=limit))
      return

    if path == "/pixel-stack/identity/api/v1/adguard/querylog":
      identity_filter = (query.get("identity") or [""])[0].strip()
      proxy_query = {
        "search": [((query.get("search") or [""])[0]).strip()],
        "response_status": [((query.get("response_status") or ["all"])[0]).strip() or "all"],
        "older_than": [((query.get("older_than") or [""])[0]).strip()],
        "limit": [((query.get("limit") or [str(self._default_querylog_limit())])[0]).strip() or str(self._default_querylog_limit())],
      }
      self._send_json(HTTPStatus.OK, self._cached_querylog_proxy_payload(proxy_query, identity_filter=identity_filter))
      return

    if path == "/pixel-stack/identity/api/v1/adguard/stats":
      self._send_json(HTTPStatus.OK, self._cached_stats_proxy_payload(query))
      return

    if path == "/pixel-stack/identity/api/v1/adguard/clients":
      self._send_json(HTTPStatus.OK, self._clients_proxy_payload())
      return

    self._send_json(HTTPStatus.NOT_FOUND, {"error": "Not found"})

  def _handle_api_post(self, path: str) -> None:
    if path == "/pixel-stack/identity/api/v1/adguard/clients/search":
      body = self._read_body_bytes()
      self._send_json(HTTPStatus.OK, self._clients_search_proxy_payload(body))
      return

    if path != "/pixel-stack/identity/api/v1/identities":
      self._send_json(HTTPStatus.NOT_FOUND, {"error": "Not found"})
      return

    payload = self._read_json_body()
    identity_id = str(payload.get("id", "")).strip().lower()
    token = str(payload.get("token", "")).strip()
    primary = normalize_bool(payload.get("primary", False))
    raw_expires = payload.get("expiresEpochSeconds", None)
    now = int(time.time())

    if not IDENTITY_ID_PATTERN.fullmatch(identity_id):
      raise IdentityWebError(
        HTTPStatus.BAD_REQUEST,
        "Invalid identity id. Expected lower-case slug value (1-64 chars).",
      )
    try:
      expires_epoch = normalize_optional_epoch(raw_expires)
    except ValueError as exc:
      raise IdentityWebError(HTTPStatus.BAD_REQUEST, f"Invalid expiresEpochSeconds: {exc}") from exc
    if isinstance(expires_epoch, int) and expires_epoch <= now:
      raise IdentityWebError(HTTPStatus.BAD_REQUEST, "Invalid expiresEpochSeconds: must be in the future.")

    args = ["create", "--id", identity_id, "--json"]
    if token:
      args.extend(["--token", token])
    if primary:
      args.append("--primary")
    if isinstance(expires_epoch, int):
      args.extend(["--expires-epoch", str(expires_epoch)])

    response = self._identityctl_json(args, apply_changes=False)
    scheduled = self._schedule_runtime_reload()
    normalized = {
      "created": str(response.get("created", "")),
      "token": str(response.get("token", "")),
      "dotLabel": str(response.get("dotLabel", "") or "") or None,
      "dotHostname": str(response.get("dotHostname", "") or "") or None,
      "primaryIdentityId": str(response.get("primaryIdentityId", "")),
      "configuredPrimaryIdentityId": str(response.get("configuredPrimaryIdentityId", "")),
      "expiresEpochSeconds": response.get("expiresEpochSeconds"),
      "identityCount": int(response.get("identityCount", 0)) if isinstance(response.get("identityCount", 0), int) else 0,
      "applied": normalize_bool(response.get("applied", False)) or scheduled,
    }
    self._burst_cache_invalidate(["identities", "usage"])
    self._send_json(HTTPStatus.OK, normalized)

  def _handle_api_delete(self, path: str) -> None:
    prefix = "/pixel-stack/identity/api/v1/identities/"
    if not path.startswith(prefix):
      self._send_json(HTTPStatus.NOT_FOUND, {"error": "Not found"})
      return

    identity_id = unquote(path[len(prefix):]).strip().lower()
    if not IDENTITY_ID_PATTERN.fullmatch(identity_id):
      raise IdentityWebError(HTTPStatus.BAD_REQUEST, "Invalid identity id.")

    response = self._identityctl_json(["revoke", "--id", identity_id, "--json"], apply_changes=False)
    scheduled = self._schedule_runtime_reload()
    normalized = {
      "revoked": str(response.get("revoked", "")),
      "remaining": int(response.get("remaining", 0)) if isinstance(response.get("remaining", 0), int) else 0,
      "primaryIdentityId": str(response.get("primaryIdentityId", "")),
      "applied": normalize_bool(response.get("applied", False)) or scheduled,
    }
    self._burst_cache_invalidate(["identities", "usage"])
    self._send_json(HTTPStatus.OK, normalized)

  def do_GET(self) -> None:
    try:
      path, query = self._parse_path()

      if path == "/pixel-stack/identity/bootstrap.js":
        self._send_text(HTTPStatus.OK, self._build_bootstrap_js(), "application/javascript; charset=utf-8")
        return

      if path == "/pixel-stack/identity/inject.js":
        self._send_text(HTTPStatus.OK, self._build_inject_js(), "application/javascript; charset=utf-8")
        return

      if path in ("/pixel-stack/identity", "/pixel-stack/identity/"):
        if not self._require_session(is_api=False):
          return
        if path.endswith("/"):
          return_target = quote((query.get("return") or ["/#settings"])[0], safe="/%#?=&")
          self._redirect(f"/pixel-stack/identity?return={return_target}")
          return
        self._send_text(HTTPStatus.OK, self._build_html(), "text/html; charset=utf-8")
        return

      if path.startswith("/pixel-stack/identity/api/"):
        if not self._require_session(is_api=True):
          return
        try:
          self._handle_api_get(path, query)
        except IdentityWebError as exc:
          self._send_json(exc.status, {"error": exc.message})
        except Exception as exc:
          self._send_json(HTTPStatus.INTERNAL_SERVER_ERROR, {"error": f"Unhandled error: {exc}"})
        return

      self._send_text(HTTPStatus.NOT_FOUND, "not found")
    finally:
      self._flush_request_state()

  def do_POST(self) -> None:
    try:
      path, _query = self._parse_path()
      if not path.startswith("/pixel-stack/identity/api/"):
        self._send_text(HTTPStatus.NOT_FOUND, "not found")
        return
      if not self._require_session(is_api=True):
        return
      if path == "/pixel-stack/identity/api/v1/identities" and not self._require_same_origin():
        return
      try:
        self._handle_api_post(path)
      except IdentityWebError as exc:
        self._send_json(exc.status, {"error": exc.message})
      except Exception as exc:
        self._send_json(HTTPStatus.INTERNAL_SERVER_ERROR, {"error": f"Unhandled error: {exc}"})
    finally:
      self._flush_request_state()

  def do_DELETE(self) -> None:
    try:
      path, _query = self._parse_path()
      if not path.startswith("/pixel-stack/identity/api/"):
        self._send_text(HTTPStatus.NOT_FOUND, "not found")
        return
      if not self._require_session(is_api=True):
        return
      if not self._require_same_origin():
        return
      try:
        self._handle_api_delete(path)
      except IdentityWebError as exc:
        self._send_json(exc.status, {"error": exc.message})
      except Exception as exc:
        self._send_json(HTTPStatus.INTERNAL_SERVER_ERROR, {"error": f"Unhandled error: {exc}"})
    finally:
      self._flush_request_state()


def build_parser() -> argparse.ArgumentParser:
  parser = argparse.ArgumentParser(description="Serve encrypted DNS identity control plane web/API endpoints.")
  parser.add_argument("--host", default=os.environ.get("ADGUARDHOME_DOH_IDENTITY_WEB_HOST", "127.0.0.1"))
  parser.add_argument(
    "--port",
    type=int,
    default=int(os.environ.get("ADGUARDHOME_DOH_IDENTITY_WEB_PORT", "8097")),
  )
  parser.add_argument(
    "--adguard-web-port",
    type=int,
    default=int(os.environ.get("PIHOLE_WEB_PORT", os.environ.get("ADGUARDHOME_WEB_PORT", "8080"))),
  )
  parser.add_argument(
    "--identityctl",
    default=os.environ.get("ADGUARDHOME_DOH_IDENTITYCTL", "/usr/local/bin/adguardhome-doh-identityctl"),
  )
  parser.add_argument(
    "--skip-session-check",
    action="store_true",
    default=os.environ.get("ADGUARDHOME_DOH_IDENTITY_WEB_SKIP_SESSION_CHECK", "0").strip().lower() in (
      "1",
      "true",
      "yes",
      "on",
    ),
  )
  return parser


def main(argv: list[str]) -> int:
  args = build_parser().parse_args(argv)
  if args.port < 1 or args.port > 65535:
    print(f"invalid --port: {args.port}", file=sys.stderr)
    return 2
  if args.adguard_web_port < 1 or args.adguard_web_port > 65535:
    print(f"invalid --adguard-web-port: {args.adguard_web_port}", file=sys.stderr)
    return 2
  if not os.path.exists(args.identityctl):
    print(f"identityctl entrypoint missing: {args.identityctl}", file=sys.stderr)
    return 1
  if not os.access(args.identityctl, os.X_OK):
    print(f"identityctl entrypoint is not executable: {args.identityctl}", file=sys.stderr)
    return 1

  config = ServerConfig(
    host=args.host,
    port=args.port,
    identityctl=args.identityctl,
    adguard_web_port=args.adguard_web_port,
    skip_session_check=bool(args.skip_session_check),
  )

  server = IdentityWebServer((config.host, config.port), IdentityWebHandler, config)
  print(
    f"identity-web listening on {config.host}:{config.port} (adguard-web-port={config.adguard_web_port}, identityctl={config.identityctl})"
  )
  try:
    server.serve_forever(poll_interval=0.5)
  except KeyboardInterrupt:
    pass
  finally:
    server.server_close()
  return 0


if __name__ == "__main__":
  raise SystemExit(main(sys.argv[1:]))
