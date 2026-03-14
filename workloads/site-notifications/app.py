from __future__ import annotations

import argparse
import json
import logging
import os
import queue
import re
import signal
import socket
import threading
import time
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Callable
from urllib.parse import urlparse

from config import Config, ConfigError, load_config
from env_store import EnvStoreError, upsert_env_value
from gribu_auth import GribuAuthError, GribuAuthenticator
from gribu_client import GribuClient, GribuClientError, GribuResponse, looks_like_session_expired
from process_lock import ProcessLock, ProcessLockHeldError
from state_store import StateStore
from telegram_control import (
    TelegramApiError,
    TelegramClient,
    TelegramCommandCallbacks,
    TelegramController,
    build_navigation_reply_markup,
)
from unread_parser import (
    UnreadParseError,
    UnreadPreview,
    extract_cats_unread_previews,
    parse_unread_count,
)

LOG = logging.getLogger("gribu_notifier")
ERROR_ALERT_THRESHOLD = 3
DAEMON_EXIT_LOCK_HELD = 10
DAEMON_EXIT_ORCHESTRATOR_OWNERSHIP_REQUIRED = 11
DAEMON_EXIT_THREAD_DIED = 20
DAEMON_EXIT_HEARTBEAT_STALE = 21
WORKER_HEARTBEAT_COMPONENTS = ("telegram", "scheduler", "command_worker")
HEARTBEAT_COMPONENTS = ("daemon",) + WORKER_HEARTBEAT_COMPONENTS
SUPERVISOR_STABLE_WINDOW_SEC = 600
RECOVERABLE_WORKER_EXIT_CODES = {DAEMON_EXIT_THREAD_DIED, DAEMON_EXIT_HEARTBEAT_STALE}
ASYNC_COMMAND_CHECKNOW = "checknow"
ASYNC_COMMAND_REAUTH = "reauth"
COMMAND_LATENCY_WARN_MS = 3000
TELEGRAM_HEALTH_LOG_INTERVAL_SEC = 300
_DNS_FALLBACK_INSTALLED = False
ALERT_PREVIEW_CHAR_LIMIT = 200
ALERT_PREVIEW_LINE_LIMIT = 5
PREVIEW_STATE_ITEM_LIMIT = 5
_CATS_PREVIEW_URL = "/cats"
_CHAT_APP_ID_PATTERN = re.compile(r"""id\s*=\s*["']chat-app["']""", flags=re.IGNORECASE)
_CHAT_APP_ATTR_PATTERN = r"""{attr}\s*=\s*["']([^"']*)["']"""


def utc_now_iso() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat()


def parse_iso(ts: str | None) -> datetime | None:
    if not ts:
        return None
    try:
        return datetime.fromisoformat(ts)
    except ValueError:
        return None


@dataclass(frozen=True)
class PreviewSyncResult:
    route: str | None
    route_result: str
    unread_count: int | None
    previews: tuple[Any, ...] = ()
    count_consistent: bool = False
    preview_source: str = "none"
    preview_item_count: int = 0
    error_message: str | None = None


@dataclass(frozen=True)
class PreviewContentResult:
    previews: tuple[UnreadPreview, ...]
    preview_items: list[dict[str, Any]]
    source: str
    error_message: str | None = None


def _preview_fingerprints_from_state(state: dict[str, Any]) -> list[str]:
    raw = state.get("last_unread_message_fingerprints")
    if not isinstance(raw, list):
        return []
    fingerprints: list[str] = []
    seen: set[str] = set()
    for item in raw:
        value = str(item or "").strip()
        if not value or value in seen:
            continue
        seen.add(value)
        fingerprints.append(value)
    return fingerprints


def _truncate_preview_text(text: str, limit: int = ALERT_PREVIEW_CHAR_LIMIT) -> str:
    normalized = " ".join(str(text or "").split()).strip()
    if len(normalized) <= limit:
        return normalized
    clipped = normalized[: max(0, limit - 3)].rstrip()
    return f"{clipped}..."


def _preview_items_from_parsed(
    previews: tuple[Any, ...],
    *,
    limit: int = PREVIEW_STATE_ITEM_LIMIT,
) -> list[dict[str, Any]]:
    items: list[dict[str, Any]] = []
    seen: set[str] = set()
    for preview in previews:
        fingerprint = str(getattr(preview, "fingerprint", "") or "").strip()
        text = str(getattr(preview, "text", "") or "").strip()
        href_raw = getattr(preview, "href", None)
        href = str(href_raw).strip() if href_raw is not None else None
        if not fingerprint or not text or fingerprint in seen:
            continue
        seen.add(fingerprint)
        items.append(
            {
                "fingerprint": fingerprint,
                "text": text,
                "href": href or None,
            }
        )
        if len(items) >= limit:
            break
    return items


def _latest_preview_text(preview_items: list[dict[str, Any]]) -> str | None:
    if not preview_items:
        return None
    text = str(preview_items[0].get("text") or "").strip()
    return text or None


def _extract_chat_app_attr(html: str, attr: str) -> str | None:
    match = re.search(
        _CHAT_APP_ATTR_PATTERN.format(attr=re.escape(attr)),
        html,
        flags=re.IGNORECASE,
    )
    if not match:
        return None
    value = str(match.group(1) or "").strip()
    return value or None


def _chat_page_locale(preview_url: str, html: str) -> str:
    path = urlparse(preview_url).path.lower()
    path_match = re.match(r"^/([a-z]{2})(?:/|$)", path)
    if path_match:
        return path_match.group(1)

    lang_match = re.search(r"""<html[^>]+lang\s*=\s*["']([a-z]{2})""", html, flags=re.IGNORECASE)
    if lang_match:
        return lang_match.group(1).lower()

    current_locale = _extract_chat_app_attr(html, "data-current-locale")
    if current_locale:
        for token in current_locale.split(","):
            locale = token.strip().lower()
            if re.fullmatch(r"[a-z]{2}", locale):
                return locale

    return "en"


def _build_chat_api_previews(
    preview_url: str,
    conversations: Any,
    *,
    limit: int = PREVIEW_STATE_ITEM_LIMIT,
) -> tuple[UnreadPreview, ...]:
    if not isinstance(conversations, list):
        return ()

    previews: list[UnreadPreview] = []
    seen: set[str] = set()

    for conversation in conversations:
        if not isinstance(conversation, dict):
            continue
        if conversation.get("isRoom") or conversation.get("isShoutbox") or conversation.get("isAdmin"):
            continue

        last_message = conversation.get("lastMessage")
        if not isinstance(last_message, dict):
            continue

        body = str(last_message.get("text") or "").strip()
        if not body:
            continue

        title = str(conversation.get("name") or conversation.get("username") or "").strip()
        combined = f"{title}: {body}" if title and title.lower() not in body.lower() else body
        fingerprint = ":".join(
            part
            for part in (
                str(conversation.get("conversationId") or "").strip(),
                str(last_message.get("id") or "").strip(),
            )
            if part
        )
        if not fingerprint:
            fingerprint = str(last_message.get("createdAt") or "").strip()
        if not fingerprint or fingerprint in seen:
            continue

        seen.add(fingerprint)
        previews.append(
            UnreadPreview(
                fingerprint=fingerprint,
                text=combined,
                href=preview_url,
            )
        )
        if len(previews) >= limit:
            break

    return tuple(previews)


def _select_new_message_preview_lines(
    previous_fingerprints: list[str],
    current_previews: tuple[Any, ...],
    delta: int,
) -> list[str]:
    if delta <= 0:
        return []

    seen = set(previous_fingerprints)
    selected: list[str] = []
    for preview in current_previews:
        fingerprint = str(getattr(preview, "fingerprint", "") or "").strip()
        text = str(getattr(preview, "text", "") or "").strip()
        if not fingerprint or not text or fingerprint in seen:
            continue
        seen.add(fingerprint)
        selected.append(text)
        if len(selected) >= delta:
            break
    return selected


def _build_unread_increase_notification(
    previous_unread: int,
    current_unread: int,
    preview_lines: list[str],
) -> str:
    lines = [
        "New gribu.lv messages.",
        f"Unread increased: {previous_unread} -> {current_unread}",
    ]

    if preview_lines:
        lines.append("New messages:")
        visible_preview_lines = preview_lines[:ALERT_PREVIEW_LINE_LIMIT]
        lines.extend(f"- {_truncate_preview_text(line)}" for line in visible_preview_lines)
        overflow = len(preview_lines) - len(visible_preview_lines)
        if overflow > 0:
            lines.append(f"...and {overflow} more")

    return "\n".join(lines)


def _cleared_preview_state_patch(now_iso: str, *, route_result: str) -> dict[str, Any]:
    return {
        "last_unread_previews": [],
        "last_latest_unread_preview_text": None,
        "last_preview_fetch_ts": now_iso,
        "last_preview_route": None,
        "last_preview_route_result": route_result,
        "last_preview_unread_count": 0,
        "last_preview_error_message": None,
        "last_preview_source": "none",
        "last_preview_item_count": 0,
        "last_unread_message_fingerprints": [],
    }


def detect_selinux_context() -> str | None:
    try:
        raw = Path("/proc/self/attr/current").read_text(encoding="utf-8")
    except OSError:
        return None
    return raw.strip() or None


def build_runtime_context_warning(
    selinux_context: str | None,
    runtime_context_policy: str | None = None,
) -> str | None:
    policy = (runtime_context_policy or os.getenv("RUNTIME_CONTEXT_POLICY", "")).strip().lower()
    if policy == "orchestrator_root":
        return None
    if not selinux_context:
        return None
    if "magisk" in selinux_context.lower():
        return (
            "Daemon is running under magisk/root context. "
            "Start it via orchestrator component 'site_notifier' instead of launching it directly under su."
        )
    return None


def is_orchestrator_owned_runtime(
    *,
    runtime_context_policy: str | None = None,
    runtime_root: str = "/data/local/pixel-stack/apps/site-notifications",
    cwd: Path | None = None,
) -> tuple[bool, str]:
    policy_raw = runtime_context_policy if runtime_context_policy is not None else os.getenv(
        "RUNTIME_CONTEXT_POLICY", ""
    )
    policy = policy_raw.strip().lower()
    if policy == "orchestrator_root":
        return True, "orchestrator_runtime_confirmed"

    current_cwd = (cwd or Path.cwd()).resolve()
    in_runtime_root = str(current_cwd).startswith(runtime_root)
    reason = (
        "Refusing daemon startup: RUNTIME_CONTEXT_POLICY must be 'orchestrator_root' "
        f"(got: {policy_raw!r})."
    )
    if in_runtime_root:
        reason += (
            f" Current working directory is under runtime root ({runtime_root}) "
            "but orchestrator ownership policy is missing."
        )
    else:
        reason += (
            f" Current working directory ({current_cwd}) is not under expected runtime root "
            f"({runtime_root})."
        )
    reason += (
        " Use orchestrator component 'site_notifier'; "
        "manual daemon launch is unsupported."
    )
    return False, reason


def _heartbeat_key(component: str) -> str:
    return f"{component}_last_heartbeat_ts"


def _heartbeat_age_seconds(now_dt: datetime, heartbeat_iso: str | None) -> float | None:
    heartbeat_dt = parse_iso(heartbeat_iso)
    if heartbeat_dt is None:
        return None
    age = (now_dt - heartbeat_dt).total_seconds()
    return max(0.0, age)


def _format_time_delta(seconds: float) -> str:
    seconds_int = max(0, int(seconds))
    minutes, secs = divmod(seconds_int, 60)
    hours, minutes = divmod(minutes, 60)
    if hours > 0:
        return f"{hours}h {minutes}m"
    if minutes > 0:
        return f"{minutes}m {secs}s"
    return f"{secs}s"


def _format_age(now_dt: datetime, ts: str | None) -> str:
    value = parse_iso(ts)
    if value is None:
        return "never"
    age_sec = (now_dt - value).total_seconds()
    if age_sec < 0:
        return "just now"
    return f"{_format_time_delta(age_sec)} ago"


def _format_eta(now_dt: datetime, ts: str | None) -> str:
    value = parse_iso(ts)
    if value is None:
        return "unknown"
    delta = (value - now_dt).total_seconds()
    if delta <= 0:
        return "due now"
    return f"in {_format_time_delta(delta)}"


def _extract_telegram_error_code(error: Exception) -> int | None:
    value = getattr(error, "error_code", None)
    if isinstance(value, int):
        return value
    status = getattr(error, "status_code", None)
    if isinstance(status, int):
        return status
    return None


def _parse_static_dns_fallbacks(raw: str) -> dict[str, list[str]]:
    mapping: dict[str, list[str]] = {}
    for item in raw.split(";"):
        token = item.strip()
        if not token or "=" not in token:
            continue
        host, ips_raw = token.split("=", 1)
        host = host.strip().lower()
        if not host:
            continue
        ips = [ip.strip() for ip in ips_raw.split(",") if ip.strip()]
        if ips:
            mapping[host] = ips
    return mapping


def install_static_dns_fallbacks() -> None:
    global _DNS_FALLBACK_INSTALLED
    if _DNS_FALLBACK_INSTALLED:
        return

    raw = os.getenv(
        "SITE_NOTIFIER_STATIC_DNS_FALLBACKS",
        "api.telegram.org=149.154.166.110,149.154.167.220,149.154.167.99,149.154.175.50,91.108.56.170,91.108.4.4",
    )
    mapping = _parse_static_dns_fallbacks(raw)
    if not mapping:
        _DNS_FALLBACK_INSTALLED = True
        return

    original_getaddrinfo = socket.getaddrinfo

    def _patched_getaddrinfo(host, *args, **kwargs):
        try:
            return original_getaddrinfo(host, *args, **kwargs)
        except socket.gaierror:
            lookup_host = host.decode("utf-8", errors="ignore") if isinstance(host, bytes) else str(host)
            fallback_ips = mapping.get(lookup_host.lower())
            if not fallback_ips:
                raise
            collected = []
            for ip in fallback_ips:
                try:
                    result = original_getaddrinfo(ip, *args, **kwargs)
                except socket.gaierror:
                    continue
                LOG.warning("DNS fallback used host=%s ip=%s", lookup_host, ip)
                collected.extend(result)
            if collected:
                return collected
            raise

    socket.getaddrinfo = _patched_getaddrinfo
    _DNS_FALLBACK_INSTALLED = True


def compute_check_interval_sec(config: Config, state: dict) -> int:
    enabled = bool(state.get("enabled"))
    paused_reason = str(state.get("paused_reason") or "none")
    if not enabled or paused_reason == "session_expired":
        return config.check_interval_idle_sec

    consecutive_errors = max(0, int(state.get("consecutive_errors", 0)))
    if consecutive_errors <= 0:
        return config.check_interval_fast_sec

    step = config.check_interval_fast_sec * (2 ** min(consecutive_errors - 1, 10))
    return min(config.check_interval_error_backoff_max_sec, step)


def _next_due_iso(interval_sec: int) -> str:
    return (datetime.now(timezone.utc) + timedelta(seconds=interval_sec)).replace(microsecond=0).isoformat()


def evaluate_watchdog(
    *,
    now_dt: datetime,
    stale_sec: int,
    thread_alive: dict[str, bool],
    heartbeats: dict[str, str | None],
) -> tuple[int | None, str | None]:
    for component in WORKER_HEARTBEAT_COMPONENTS:
        if not thread_alive.get(component, False):
            return DAEMON_EXIT_THREAD_DIED, f"{component}_thread_dead"

    for component in WORKER_HEARTBEAT_COMPONENTS:
        heartbeat_key = _heartbeat_key(component)
        age = _heartbeat_age_seconds(now_dt, heartbeats.get(heartbeat_key))
        if age is None:
            return DAEMON_EXIT_HEARTBEAT_STALE, f"{component}_heartbeat_missing"
        if age > stale_sec:
            return DAEMON_EXIT_HEARTBEAT_STALE, f"{component}_heartbeat_stale:{int(age)}s"

    return None, None


def evaluate_health_state(
    *,
    state: dict,
    now_dt: datetime,
    stale_sec: int,
) -> tuple[bool, str]:
    started_dt = parse_iso(state.get("daemon_started_ts"))
    if started_dt is None:
        return False, "daemon_not_initialized"

    for component in HEARTBEAT_COMPONENTS:
        heartbeat_key = _heartbeat_key(component)
        age = _heartbeat_age_seconds(now_dt, state.get(heartbeat_key))
        if age is None:
            return False, f"{heartbeat_key}_missing_or_invalid"
        if age > stale_sec:
            return False, f"{component}_heartbeat_stale:{int(age)}s"
    return True, "ok"


class HeartbeatTracker:
    def __init__(self, state_store: StateStore):
        self.state_store = state_store
        self._lock = threading.Lock()
        self._heartbeats: dict[str, str | None] = {
            _heartbeat_key(component): None for component in HEARTBEAT_COMPONENTS
        }

    def initialize(self, now_iso: str) -> None:
        with self._lock:
            for component in HEARTBEAT_COMPONENTS:
                self._heartbeats[_heartbeat_key(component)] = now_iso
            snapshot = dict(self._heartbeats)
        self.state_store.patch(snapshot)

    def mark(self, component: str, now_iso: str | None = None) -> str:
        if component not in HEARTBEAT_COMPONENTS:
            raise ValueError(f"Unknown heartbeat component: {component}")
        ts = now_iso or utc_now_iso()
        key = _heartbeat_key(component)
        with self._lock:
            self._heartbeats[key] = ts
        self.state_store.patch({key: ts})
        return ts

    def snapshot(self) -> dict[str, str | None]:
        with self._lock:
            return dict(self._heartbeats)


class NotifierService:
    def __init__(
        self,
        config: Config,
        state_store: StateStore,
        gribu_client: GribuClient,
        gribu_authenticator: GribuAuthenticator,
        telegram_client: TelegramClient,
        on_checks_enabled: Callable[[], None] | None = None,
        navigation_reply_markup: dict[str, Any] | None = None,
    ):
        self.config = config
        self.state_store = state_store
        self.gribu_client = gribu_client
        self.gribu_authenticator = gribu_authenticator
        self.telegram_client = telegram_client
        self._check_lock = threading.Lock()
        self._on_checks_enabled = on_checks_enabled
        self._navigation_reply_markup = navigation_reply_markup

    def is_enabled(self) -> bool:
        state = self.state_store.load()
        return bool(state.get("enabled"))

    def _send_telegram(
        self,
        text: str,
        *,
        link_preview_enabled: bool = False,
        open_url: str | None = None,
        remember_notification: bool = False,
    ) -> bool:
        started = time.monotonic()
        try:
            self.telegram_client.send_message(
                chat_id=self.config.telegram_chat_id,
                text=text,
                reply_markup=self._navigation_reply_markup,
                link_preview_enabled=link_preview_enabled,
            )
            latency_ms = int((time.monotonic() - started) * 1000)
            patch = {
                "telegram_send_latency_ms": max(0, latency_ms),
                "last_telegram_api_error_code": None,
            }
            if remember_notification:
                patch["last_notification_open_url"] = str(open_url or "").strip() or None
                patch["last_notification_had_link_preview_requested"] = bool(link_preview_enabled)
            self.state_store.patch(patch)
            return True
        except TelegramApiError as exc:
            LOG.error("Failed to send Telegram message: %s", exc)
            latency_ms = int((time.monotonic() - started) * 1000)
            patch = {
                "telegram_send_latency_ms": max(0, latency_ms),
                "last_telegram_api_error_code": _extract_telegram_error_code(exc),
            }
            if remember_notification:
                patch["last_notification_open_url"] = str(open_url or "").strip() or None
                patch["last_notification_had_link_preview_requested"] = bool(link_preview_enabled)
            self.state_store.patch(patch)
            return False

    def _status_mode(self, state: dict) -> str:
        if not state.get("enabled"):
            return "off"
        paused_reason = str(state.get("paused_reason") or "none")
        if paused_reason != "none":
            return f"paused ({paused_reason})"
        return "active"

    def _status_health(self, state: dict) -> str:
        problems: list[str] = []
        if int(state.get("consecutive_errors", 0)) > 0:
            problems.append(f"errors={state.get('consecutive_errors')}")
        last_watchdog_reason = state.get("last_watchdog_reason")
        if last_watchdog_reason not in (None, "none"):
            problems.append(f"watchdog={last_watchdog_reason}")
        if state.get("paused_reason") == "session_expired":
            problems.append("session_expired")
        if state.get("runtime_context_warning"):
            problems.append("runtime_context")
        if problems:
            return "degraded (" + ", ".join(problems) + ")"
        return "healthy"

    def _status_action(self, state: dict) -> str:
        if state.get("paused_reason") == "session_expired":
            return "Action: tap Reauth (or send /reauth)."
        if not state.get("enabled"):
            return "Action: tap Enable (or send /on)."
        if state.get("runtime_context_warning"):
            return (
                "Action: run under orchestrator "
                "(component=site_notifier); manual daemon launch is unsupported."
            )
        if int(state.get("consecutive_errors", 0)) > 0:
            return "Action: tap Check now (or send /checknow)."
        return "Action: monitoring normally."

    def _format_status(self, state: dict) -> str:
        now_dt = datetime.now(timezone.utc)
        current_interval = state.get("current_check_interval_sec") or self.config.check_interval_idle_sec
        resolved_route = state.get("resolved_check_url") or "unresolved"
        fetch_ms = state.get("fetch_duration_ms")
        parse_ms = state.get("parse_duration_ms")
        timing_summary = f"fetch={fetch_ms}ms parse={parse_ms}ms"
        return (
            "gribu notifier status:\n"
            f"- mode: {self._status_mode(state)}\n"
            f"- health: {self._status_health(state)}\n"
            f"- route: {resolved_route}\n"
            f"- unread baseline: {state.get('last_unread')}\n"
            f"- last success: {_format_age(now_dt, state.get('last_success_ts'))}\n"
            f"- cadence: every {current_interval}s, next check {_format_eta(now_dt, state.get('next_check_due_ts'))}\n"
            f"- timings: {timing_summary}\n"
            f"- last result: {state.get('last_check_result')}\n"
            f"- {self._status_action(state)}"
        )

    def _format_debug_status(self, state: dict) -> str:
        return (
            "gribu checker debug status:\n"
            f"- enabled: {state.get('enabled')}\n"
            f"- paused_reason: {state.get('paused_reason')}\n"
            f"- last_unread: {state.get('last_unread')}\n"
            f"- last_check_ts: {state.get('last_check_ts')}\n"
            f"- last_success_ts: {state.get('last_success_ts')}\n"
            f"- last_check_result: {state.get('last_check_result')}\n"
            f"- last_error_message: {state.get('last_error_message')}\n"
            f"- last_parse_source: {state.get('last_parse_source')}\n"
            f"- last_parse_confidence: {state.get('last_parse_confidence')}\n"
            f"- low_confidence_streak: {state.get('low_confidence_streak')}\n"
            f"- consecutive_errors: {state.get('consecutive_errors')}\n"
            f"- daemon_started_ts: {state.get('daemon_started_ts')}\n"
            f"- runtime_selinux_context: {state.get('runtime_selinux_context')}\n"
            f"- runtime_context_warning: {state.get('runtime_context_warning')}\n"
            f"- daemon_last_heartbeat_ts: {state.get('daemon_last_heartbeat_ts')}\n"
            f"- telegram_last_heartbeat_ts: {state.get('telegram_last_heartbeat_ts')}\n"
            f"- scheduler_last_heartbeat_ts: {state.get('scheduler_last_heartbeat_ts')}\n"
            f"- command_worker_last_heartbeat_ts: {state.get('command_worker_last_heartbeat_ts')}\n"
            f"- current_check_interval_sec: {state.get('current_check_interval_sec')}\n"
            f"- next_check_due_ts: {state.get('next_check_due_ts')}\n"
            f"- resolved_check_url: {state.get('resolved_check_url')}\n"
            f"- route_discovery_last_ts: {state.get('route_discovery_last_ts')}\n"
            f"- route_discovery_last_result: {state.get('route_discovery_last_result')}\n"
            f"- route_discovery_last_candidates: {state.get('route_discovery_last_candidates')}\n"
            f"- resolved_preview_url: {state.get('resolved_preview_url')}\n"
            f"- preview_route_discovery_last_ts: {state.get('preview_route_discovery_last_ts')}\n"
            f"- preview_route_discovery_last_result: {state.get('preview_route_discovery_last_result')}\n"
            f"- preview_route_discovery_last_candidates: {state.get('preview_route_discovery_last_candidates')}\n"
            f"- last_preview_fetch_ts: {state.get('last_preview_fetch_ts')}\n"
            f"- last_preview_route: {state.get('last_preview_route')}\n"
            f"- last_preview_route_result: {state.get('last_preview_route_result')}\n"
            f"- last_preview_unread_count: {state.get('last_preview_unread_count')}\n"
            f"- last_preview_error_message: {state.get('last_preview_error_message')}\n"
            f"- last_preview_source: {state.get('last_preview_source')}\n"
            f"- last_preview_item_count: {state.get('last_preview_item_count')}\n"
            f"- last_latest_unread_preview_text: {state.get('last_latest_unread_preview_text')}\n"
            f"- last_unread_previews: {state.get('last_unread_previews')}\n"
            f"- fetch_duration_ms: {state.get('fetch_duration_ms')}\n"
            f"- parse_duration_ms: {state.get('parse_duration_ms')}\n"
            f"- check_duration_ms: {state.get('check_duration_ms')}\n"
            f"- last_notification_sent_ts: {state.get('last_notification_sent_ts')}\n"
            f"- last_notification_open_url: {state.get('last_notification_open_url')}\n"
            f"- last_notification_had_link_preview_requested: {state.get('last_notification_had_link_preview_requested')}\n"
            f"- last_command_latency_ms: {state.get('last_command_latency_ms')}\n"
            f"- command_queue_depth: {state.get('command_queue_depth')}\n"
            f"- command_latency_histogram_ms: {state.get('command_latency_histogram_ms')}\n"
            f"- telegram_getupdates_latency_ms: {state.get('telegram_getupdates_latency_ms')}\n"
            f"- telegram_send_latency_ms: {state.get('telegram_send_latency_ms')}\n"
            f"- last_watchdog_reason: {state.get('last_watchdog_reason')}\n"
            f"- daemon_restart_count: {state.get('daemon_restart_count')}\n"
            f"- last_restart_ts: {state.get('last_restart_ts')}\n"
            f"- telegram_poll_error_count: {state.get('telegram_poll_error_count')}\n"
            f"- last_telegram_api_error_code: {state.get('last_telegram_api_error_code')}"
        )

    def _preview_status_detail(self, state: dict) -> str:
        result = str(state.get("last_preview_route_result") or "not_attempted")
        current_unread = state.get("last_unread")
        preview_unread = state.get("last_preview_unread_count")
        if result == "count_mismatch":
            return f"count_mismatch (preview={preview_unread}, count={current_unread})"
        if result == "ok_no_unread":
            return "ok_no_unread"
        if result == "request_error":
            return f"request_error ({state.get('last_preview_error_message')})"
        if result == "parse_error":
            return f"parse_error ({state.get('last_preview_error_message')})"
        if result == "session_expired":
            return "session_expired"
        if result == "no_previews":
            return "no_previews"
        if result == "ok":
            return "ok"
        return result

    def _manual_check_missing_preview_reason(self, state: dict, preview_route: str) -> str:
        result = str(state.get("last_preview_route_result") or "not_attempted")
        if result == "ok_no_unread":
            return "unread count is 0"
        if result == "no_previews":
            return f"no unread contact previews found on {preview_route}"
        if result == "count_mismatch":
            preview_unread = state.get("last_preview_unread_count")
            count_unread = state.get("last_unread")
            return f"preview unread count {preview_unread} does not match count route {count_unread}"
        if result in {"request_error", "parse_error"}:
            fallback = f"preview fetch from {preview_route} failed"
            return str(state.get("last_preview_error_message") or fallback)
        if result == "session_expired":
            return f"{preview_route} looked like a login page"
        return "preview not available"

    def _format_manual_check_result(self, result: str, state: dict) -> str:
        preview_text = str(state.get("last_latest_unread_preview_text") or "").strip()
        preview_route = (
            state.get("last_preview_route")
            or state.get("resolved_preview_url")
            or _CATS_PREVIEW_URL
        )
        lines = [
            f"Manual check result: {result}",
            f"Unread: {state.get('last_unread')}",
            f"Count route: {state.get('resolved_check_url') or self.config.gribu_check_url}",
            f"Preview route: {preview_route}",
            f"Preview status: {self._preview_status_detail(state)}",
        ]
        if preview_text:
            lines.append(f"Last unread message: {_truncate_preview_text(preview_text)}")
        else:
            lines.append(
                "Last unread message: unavailable "
                f"({self._manual_check_missing_preview_reason(state, str(preview_route))})"
            )
        return "\n".join(lines)

    def command_on(self) -> str:
        previous = self.state_store.load()
        state = self.state_store.patch(
            {
                "enabled": True,
                "paused_reason": "none",
            }
        )
        if not bool(previous.get("enabled")) and self._on_checks_enabled is not None:
            self._on_checks_enabled()
        return (
            "Checks enabled."
            f"\nFast cadence: {self.config.check_interval_fast_sec}s (idle {self.config.check_interval_idle_sec}s)."
            f"\nCurrent unread baseline: {state.get('last_unread')}"
        )

    def command_off(self) -> str:
        self.state_store.patch(
            {
                "enabled": False,
                "paused_reason": "manual_off",
            }
        )
        return "Checks disabled."

    def command_status(self) -> str:
        state = self.state_store.load()
        return self._format_status(state)

    def command_debug(self) -> str:
        state = self.state_store.load()
        return self._format_debug_status(state)

    def command_checknow(self) -> str:
        result = self.run_check(force=True)
        state = self.state_store.load()
        return self._format_manual_check_result(result, state)

    def command_reauth(self) -> str:
        with self._check_lock:
            ok, error_message = self._try_reauthenticate()
            if not ok:
                self.state_store.patch({
                    "last_error_message": error_message,
                    "last_check_result": "reauth_failed",
                })
                return f"Reauth failed: {error_message}"

            state = self.state_store.load()
            if state.get("paused_reason") == "session_expired":
                self.state_store.patch(
                    {
                        "enabled": True,
                        "paused_reason": "none",
                        "last_error_message": None,
                        "last_check_result": "reauth_resumed",
                    }
                )
                return "Reauth successful. Checks resumed."

            self.state_store.patch({"last_error_message": None, "last_check_result": "reauth_ok"})
            return "Reauth successful."

    def command_help(self) -> str:
        return (
            "Quick actions: Enable, Pause, Status, Check now, Reauth, Help.\n"
            "Commands:\n"
            "/on - enable checks\n"
            "/off - disable checks\n"
            "/status - compact status summary\n"
            "/debug - full technical diagnostics\n"
            "/checknow - run one check now\n"
            "/reauth - force login and refresh session cookie\n"
            "/help - show this help"
        )

    def _persist_cookie_header(self) -> None:
        cookie_header = self.gribu_client.export_cookie_header().strip()
        if not cookie_header:
            raise GribuAuthError("Authentication succeeded but no session cookies were captured")
        upsert_env_value(self.config.env_file_path, "GRIBU_COOKIE_HEADER", cookie_header)
        self.gribu_client.cookie_header = cookie_header

    def _authenticate_and_persist_cookie(self) -> None:
        self.gribu_authenticator.authenticate(
            login_id=self.config.gribu_login_id,
            login_password=self.config.gribu_login_password,
        )
        self._persist_cookie_header()

    def startup_authenticate(self) -> bool:
        try:
            self._authenticate_and_persist_cookie()
            return True
        except (GribuAuthError, EnvStoreError) as exc:
            LOG.warning("Initial authentication failed: %s", exc)
            return False

    def _try_reauthenticate(self) -> tuple[bool, str | None]:
        try:
            self._authenticate_and_persist_cookie()
            return True, None
        except (GribuAuthError, EnvStoreError) as exc:
            return False, str(exc)

    def _should_send_error_alert(self, state: dict, now_dt: datetime) -> bool:
        consecutive = int(state.get("consecutive_errors", 0))
        if consecutive < ERROR_ALERT_THRESHOLD:
            return False
        last_alert_dt = parse_iso(state.get("last_error_alert_ts"))
        if last_alert_dt is None:
            return True
        elapsed = (now_dt - last_alert_dt).total_seconds()
        return elapsed >= self.config.error_alert_cooldown_sec

    def _handle_transient_error(
        self,
        state: dict,
        now_iso: str,
        error_message: str,
        *,
        parse_source: str | None = None,
        parse_confidence: float | None = None,
        fetch_duration_ms: int | None = None,
        parse_duration_ms: int | None = None,
    ) -> str:
        now_dt = parse_iso(now_iso) or datetime.now(timezone.utc)
        next_errors = int(state.get("consecutive_errors", 0)) + 1
        result = f"error: {error_message}"
        patch = {
            "last_check_ts": now_iso,
            "consecutive_errors": next_errors,
            "last_check_result": result,
            "last_error_message": error_message,
            "last_parse_source": parse_source,
            "last_parse_confidence": parse_confidence,
            "low_confidence_streak": 0,
            "fetch_duration_ms": fetch_duration_ms,
            "parse_duration_ms": parse_duration_ms,
        }
        temp_state = dict(state)
        temp_state.update(patch)
        if self._should_send_error_alert(temp_state, now_dt):
            self._send_telegram(
                "gribu checker warning: consecutive errors detected.\n"
                f"errors: {next_errors}\n"
                f"last error: {error_message}"
            )
            patch["last_error_alert_ts"] = now_iso
        self.state_store.patch(patch)
        return result

    def _handle_session_expired(
        self,
        state: dict,
        now_iso: str,
        reauth_error: str | None = None,
        *,
        fetch_duration_ms: int | None = None,
        parse_duration_ms: int | None = None,
    ) -> str:
        already_paused = (
            state.get("enabled") is False and state.get("paused_reason") == "session_expired"
        )
        error_text = reauth_error or "session appears expired"
        patch = {
            "enabled": False,
            "paused_reason": "session_expired",
            "last_check_ts": now_iso,
            "consecutive_errors": int(state.get("consecutive_errors", 0)) + 1,
            "last_check_result": "session_expired",
            "last_error_message": error_text,
            "last_parse_source": None,
            "last_parse_confidence": None,
            "low_confidence_streak": 0,
            "fetch_duration_ms": fetch_duration_ms,
            "parse_duration_ms": parse_duration_ms,
        }
        self.state_store.patch(patch)
        if not already_paused:
            message = (
                "gribu session appears expired and automatic reauth failed. Checks are paused.\n"
                "Send /reauth to retry."
            )
            if reauth_error:
                message += f"\nlast error: {reauth_error}"
            self._send_telegram(message)
        return "session_expired"

    def _check_url_candidates(self) -> tuple[str, ...]:
        if self.config.gribu_check_url_candidates:
            return self.config.gribu_check_url_candidates
        return (self.config.gribu_check_url,)

    def _preview_url_candidates(self) -> tuple[str, ...]:
        if self.config.gribu_preview_url_candidates:
            return self.config.gribu_preview_url_candidates
        return (self.config.gribu_preview_url,)

    def _should_resolve_check_route(self, state: dict, now_dt: datetime) -> bool:
        if not state.get("resolved_check_url"):
            return True

        last_discovery_dt = parse_iso(state.get("route_discovery_last_ts"))
        if last_discovery_dt is None:
            return True

        age_sec = (now_dt - last_discovery_dt).total_seconds()
        if age_sec >= self.config.route_discovery_ttl_sec:
            return True

        return int(state.get("low_confidence_streak", 0)) >= 2

    def _should_resolve_preview_route(self, state: dict, now_dt: datetime) -> bool:
        if not state.get("resolved_preview_url"):
            return True

        last_discovery_dt = parse_iso(state.get("preview_route_discovery_last_ts"))
        if last_discovery_dt is None:
            return True

        age_sec = (now_dt - last_discovery_dt).total_seconds()
        if age_sec >= self.config.preview_route_discovery_ttl_sec:
            return True

        last_result = str(state.get("last_preview_route_result") or "")
        return last_result in {"no_previews", "request_error", "parse_error", "session_expired", "count_mismatch"}

    def _probe_check_url(self, check_url: str) -> dict[str, Any]:
        fetch_started = time.monotonic()
        try:
            response = self.gribu_client.fetch_check_page(check_url)
        except GribuClientError as exc:
            fetch_duration_ms = int((time.monotonic() - fetch_started) * 1000)
            return {
                "check_url": check_url,
                "status": "request_error",
                "error": str(exc),
                "fetch_duration_ms": max(0, fetch_duration_ms),
                "parse_duration_ms": None,
            }

        fetch_duration_ms = int((time.monotonic() - fetch_started) * 1000)
        probe: dict[str, Any] = {
            "check_url": check_url,
            "status": "unknown",
            "status_code": response.status_code,
            "fetch_duration_ms": max(0, fetch_duration_ms),
            "parse_duration_ms": None,
        }

        if response.status_code >= 400:
            probe["status"] = f"http_{response.status_code}"
            return probe
        if looks_like_session_expired(response):
            probe["status"] = "session_expired"
            return probe

        parse_started = time.monotonic()
        try:
            parsed = parse_unread_count(response.text)
        except UnreadParseError as exc:
            parse_duration_ms = int((time.monotonic() - parse_started) * 1000)
            probe.update(
                {
                    "status": "parse_error",
                    "error": str(exc),
                    "parse_duration_ms": max(0, parse_duration_ms),
                }
            )
            return probe

        parse_duration_ms = int((time.monotonic() - parse_started) * 1000)
        probe.update(
            {
                "status": "ok",
                "parse_source": parsed.source,
                "parse_confidence": parsed.confidence,
                "unread_count": parsed.unread_count,
                "parse_duration_ms": max(0, parse_duration_ms),
            }
        )
        return probe

    def _fetch_chat_api_previews(
        self,
        preview_url: str,
        page_html: str,
    ) -> tuple[tuple[UnreadPreview, ...], str | None]:
        if not _CHAT_APP_ID_PATTERN.search(page_html):
            return (), None
        if not _extract_chat_app_attr(page_html, "data-user-id"):
            return (), None

        locale = _chat_page_locale(preview_url, page_html)
        api_path = f"/api/{locale}/conversations/1"
        try:
            response = self.gribu_client.post_form(api_path, data={"page": "1"})
        except GribuClientError as exc:
            return (), f"Chat preview API request failed: {exc}"

        if response.status_code >= 400:
            return (), f"Chat preview API returned HTTP {response.status_code}"

        try:
            payload = json.loads(response.text)
        except json.JSONDecodeError:
            return (), "Chat preview API returned invalid JSON"

        if isinstance(payload, dict) and payload.get("status") == "error":
            message = str(payload.get("message") or "unknown error").strip() or "unknown error"
            return (), f"Chat preview API returned error: {message}"

        conversations = payload.get("conversations") if isinstance(payload, dict) else None
        return _build_chat_api_previews(preview_url, conversations), None

    def _extract_preview_content(
        self,
        *,
        preview_url: str,
        page_html: str,
        unread_count: int,
    ) -> PreviewContentResult:
        selector_previews = extract_cats_unread_previews(page_html)
        selector_items = _preview_items_from_parsed(selector_previews)
        if selector_items:
            return PreviewContentResult(
                previews=selector_previews,
                preview_items=selector_items,
                source="contact_selector",
            )

        if unread_count <= 0:
            return PreviewContentResult(
                previews=(),
                preview_items=[],
                source="none",
            )

        api_previews, api_error = self._fetch_chat_api_previews(preview_url, page_html)
        api_items = _preview_items_from_parsed(api_previews)
        if api_items:
            return PreviewContentResult(
                previews=api_previews,
                preview_items=api_items,
                source="chat_api",
            )

        return PreviewContentResult(
            previews=(),
            preview_items=[],
            source="none",
            error_message=api_error,
        )

    def _probe_preview_url(self, preview_url: str) -> dict[str, Any]:
        fetch_started = time.monotonic()
        try:
            response = self.gribu_client.fetch_check_page(preview_url)
        except GribuClientError as exc:
            fetch_duration_ms = int((time.monotonic() - fetch_started) * 1000)
            return {
                "preview_url": preview_url,
                "status": "request_error",
                "error": str(exc),
                "fetch_duration_ms": max(0, fetch_duration_ms),
                "parse_duration_ms": None,
            }

        fetch_duration_ms = int((time.monotonic() - fetch_started) * 1000)
        probe: dict[str, Any] = {
            "preview_url": preview_url,
            "status": "unknown",
            "status_code": response.status_code,
            "fetch_duration_ms": max(0, fetch_duration_ms),
            "parse_duration_ms": None,
        }

        if response.status_code >= 400:
            probe["status"] = f"http_{response.status_code}"
            return probe
        if looks_like_session_expired(response):
            probe["status"] = "session_expired"
            return probe

        parse_started = time.monotonic()
        try:
            parsed = parse_unread_count(response.text)
        except UnreadParseError as exc:
            parse_duration_ms = int((time.monotonic() - parse_started) * 1000)
            probe.update(
                {
                    "status": "parse_error",
                    "error": str(exc),
                    "parse_duration_ms": max(0, parse_duration_ms),
                }
            )
            return probe

        parse_duration_ms = int((time.monotonic() - parse_started) * 1000)
        preview_content = self._extract_preview_content(
            preview_url=preview_url,
            page_html=response.text,
            unread_count=parsed.unread_count,
        )
        probe.update(
            {
                "status": (
                    "request_error"
                    if preview_content.error_message
                    else ("ok" if preview_content.preview_items else "no_previews")
                ),
                "parse_source": parsed.source,
                "parse_confidence": parsed.confidence,
                "unread_count": parsed.unread_count,
                "preview_count": len(preview_content.preview_items),
                "preview_source": preview_content.source,
                "parse_duration_ms": max(0, parse_duration_ms),
            }
        )
        if preview_content.error_message:
            probe["error"] = preview_content.error_message
        return probe

    def _resolve_check_route(self, now_iso: str) -> tuple[str | None, dict[str, Any]]:
        candidate_results: list[dict[str, Any]] = []
        best_probe: dict[str, Any] | None = None
        best_score: tuple[float, int, float] | None = None

        for candidate in self._check_url_candidates():
            probe = self._probe_check_url(candidate)
            candidate_results.append(probe)
            if probe.get("status") != "ok":
                continue

            confidence = float(probe.get("parse_confidence", 0.0))
            if confidence < self.config.parse_min_confidence_route_selection:
                continue

            total_duration_ms = float(
                (probe.get("fetch_duration_ms") or 0) + (probe.get("parse_duration_ms") or 0)
            )
            primary_preference = 1 if candidate == self.config.gribu_check_url else 0
            score = (confidence, primary_preference, -total_duration_ms)
            if best_score is None or score > best_score:
                best_score = score
                best_probe = probe

        patch: dict[str, Any] = {
            "route_discovery_last_ts": now_iso,
            "route_discovery_last_candidates": candidate_results,
        }
        if best_probe is None:
            patch["route_discovery_last_result"] = "no_candidate_above_threshold"
            return None, patch

        selected_url = str(best_probe["check_url"])
        patch["resolved_check_url"] = selected_url
        patch["route_discovery_last_result"] = (
            f"selected:{selected_url}:"
            f"{best_probe.get('parse_source')}:{float(best_probe.get('parse_confidence', 0.0)):.2f}"
        )
        patch["low_confidence_streak"] = 0
        return selected_url, patch

    def _resolve_preview_route(self, now_iso: str) -> tuple[str | None, dict[str, Any]]:
        candidate_results: list[dict[str, Any]] = []
        best_probe: dict[str, Any] | None = None
        best_score: tuple[int, int, int, float] | None = None

        for candidate in self._preview_url_candidates():
            probe = self._probe_preview_url(candidate)
            candidate_results.append(probe)
            if probe.get("status") != "ok":
                continue

            preview_count = int(probe.get("preview_count") or 0)
            if preview_count <= 0:
                continue

            source_preference = 1 if probe.get("preview_source") == "contact_selector" else 0
            primary_preference = 1 if candidate == self.config.gribu_preview_url else 0
            total_duration_ms = float(
                (probe.get("fetch_duration_ms") or 0) + (probe.get("parse_duration_ms") or 0)
            )
            score = (source_preference, primary_preference, preview_count, -total_duration_ms)
            if best_score is None or score > best_score:
                best_score = score
                best_probe = probe

        patch: dict[str, Any] = {
            "preview_route_discovery_last_ts": now_iso,
            "preview_route_discovery_last_candidates": candidate_results,
        }
        if best_probe is None:
            patch["preview_route_discovery_last_result"] = "no_preview_candidate"
            return None, patch

        selected_url = str(best_probe["preview_url"])
        patch["resolved_preview_url"] = selected_url
        patch["preview_route_discovery_last_result"] = (
            f"selected:{selected_url}:"
            f"{best_probe.get('preview_source')}:{int(best_probe.get('preview_count') or 0)}"
        )
        return selected_url, patch

    def _sync_preview_state(
        self,
        *,
        state: dict[str, Any],
        now_iso: str,
        now_dt: datetime,
        current_unread: int,
        force: bool,
        count_route: str,
        count_response: GribuResponse,
        count_parsed: Any,
    ) -> PreviewSyncResult:
        route_patch: dict[str, Any] = {}
        if self._should_resolve_preview_route(state, now_dt):
            _selected_preview_url, route_patch = self._resolve_preview_route(now_iso)
            state = self.state_store.patch(route_patch)

        preview_url = str(
            state.get("resolved_preview_url")
            or self.config.gribu_preview_url
            or _CATS_PREVIEW_URL
        ).strip() or _CATS_PREVIEW_URL

        if current_unread <= 0 and not force:
            patch = _cleared_preview_state_patch(now_iso, route_result="cleared_unread_zero")
            patch.update(route_patch)
            patch["last_preview_route"] = preview_url
            patch["last_preview_unread_count"] = 0
            self.state_store.patch(patch)
            return PreviewSyncResult(
                route=preview_url,
                route_result="cleared_unread_zero",
                unread_count=0,
                previews=(),
                count_consistent=True,
            )

        if preview_url == count_route:
            response = count_response
            parsed = count_parsed
        else:
            try:
                response = self.gribu_client.fetch_check_page(preview_url)
            except GribuClientError as exc:
                error_message = str(exc)
                patch = {
                    **route_patch,
                    "last_preview_fetch_ts": now_iso,
                    "last_preview_route": preview_url,
                    "last_preview_route_result": "request_error",
                    "last_preview_unread_count": None,
                    "last_preview_error_message": error_message,
                    "last_preview_source": "none",
                    "last_preview_item_count": 0,
                }
                self.state_store.patch(patch)
                return PreviewSyncResult(
                    route=preview_url,
                    route_result="request_error",
                    unread_count=None,
                    previews=(),
                    count_consistent=False,
                    preview_source="none",
                    preview_item_count=0,
                    error_message=error_message,
                )

        if looks_like_session_expired(response):
            error_message = f"{preview_url} looked like a login page"
            patch = {
                **route_patch,
                "last_preview_fetch_ts": now_iso,
                "last_preview_route": preview_url,
                "last_preview_route_result": "session_expired",
                "last_preview_unread_count": None,
                "last_preview_error_message": error_message,
                "last_preview_source": "none",
                "last_preview_item_count": 0,
            }
            self.state_store.patch(patch)
            return PreviewSyncResult(
                route=preview_url,
                route_result="session_expired",
                unread_count=None,
                previews=(),
                count_consistent=False,
                preview_source="none",
                preview_item_count=0,
                error_message=error_message,
            )

        if preview_url != count_route:
            try:
                parsed = parse_unread_count(response.text)
            except UnreadParseError as exc:
                error_message = str(exc)
                patch = {
                    **route_patch,
                    "last_preview_fetch_ts": now_iso,
                    "last_preview_route": preview_url,
                    "last_preview_route_result": "parse_error",
                    "last_preview_unread_count": None,
                    "last_preview_error_message": error_message,
                    "last_preview_source": "none",
                    "last_preview_item_count": 0,
                }
                self.state_store.patch(patch)
                return PreviewSyncResult(
                    route=preview_url,
                    route_result="parse_error",
                    unread_count=None,
                    previews=(),
                    count_consistent=False,
                    preview_source="none",
                    preview_item_count=0,
                    error_message=error_message,
                )

        preview_content = self._extract_preview_content(
            preview_url=preview_url,
            page_html=response.text,
            unread_count=parsed.unread_count,
        )
        previews = preview_content.previews
        preview_items = preview_content.preview_items
        count_consistent = parsed.unread_count == current_unread

        if parsed.unread_count <= 0 and current_unread <= 0:
            patch = _cleared_preview_state_patch(now_iso, route_result="ok_no_unread")
            patch.update(
                {
                    **route_patch,
                    "last_preview_route": preview_url,
                    "last_preview_unread_count": parsed.unread_count,
                }
            )
            self.state_store.patch(patch)
            return PreviewSyncResult(
                route=preview_url,
                route_result="ok_no_unread",
                unread_count=parsed.unread_count,
                previews=previews,
                count_consistent=True,
                preview_source="none",
                preview_item_count=0,
            )

        route_result = "ok"
        error_message: str | None = None
        if preview_items and not count_consistent:
            route_result = "count_mismatch"
            error_message = (
                "Preview route unread count "
                f"{parsed.unread_count} does not match count route {current_unread}"
            )
        elif preview_content.error_message:
            route_result = "request_error"
            error_message = preview_content.error_message
        elif not preview_items:
            route_result = "no_previews"
            error_message = f"No unread contact previews found on {preview_url}"

        patch: dict[str, Any] = {
            **route_patch,
            "last_preview_fetch_ts": now_iso,
            "last_preview_route": preview_url,
            "last_preview_route_result": route_result,
            "last_preview_unread_count": parsed.unread_count,
            "last_preview_error_message": error_message,
            "last_preview_source": preview_content.source,
            "last_preview_item_count": len(preview_items),
        }
        if preview_items:
            patch["last_unread_previews"] = preview_items
            patch["last_latest_unread_preview_text"] = _latest_preview_text(preview_items)
            if count_consistent:
                patch["last_unread_message_fingerprints"] = [
                    str(item.get("fingerprint") or "").strip()
                    for item in preview_items
                    if str(item.get("fingerprint") or "").strip()
                ]

        self.state_store.patch(patch)
        return PreviewSyncResult(
            route=preview_url,
            route_result=route_result,
            unread_count=parsed.unread_count,
            previews=previews,
            count_consistent=count_consistent,
            preview_source=preview_content.source,
            preview_item_count=len(preview_items),
            error_message=error_message,
        )

    def run_check(self, force: bool = False) -> str:
        started = time.monotonic()
        try:
            with self._check_lock:
                state = self.state_store.load()
                if not force and not state.get("enabled"):
                    self.state_store.patch(
                        {
                            "last_check_result": "skipped_disabled",
                            "last_error_message": None,
                            "last_parse_source": None,
                            "last_parse_confidence": None,
                        }
                    )
                    return "skipped_disabled"

                now_iso = utc_now_iso()
                now_dt = parse_iso(now_iso) or datetime.now(timezone.utc)

                if self._should_resolve_check_route(state, now_dt):
                    selected_url, route_patch = self._resolve_check_route(now_iso)
                    state = self.state_store.patch(route_patch)
                    if selected_url is None and not state.get("resolved_check_url"):
                        return self._handle_transient_error(
                            state,
                            now_iso,
                            "route_resolution_failed",
                        )

                check_url = str(state.get("resolved_check_url") or self.config.gribu_check_url)
                response = None
                fetch_duration_ms: int | None = None
                parse_duration_ms: int | None = None

                fetch_started = time.monotonic()
                try:
                    response = self.gribu_client.fetch_check_page(check_url)
                    fetch_duration_ms = int((time.monotonic() - fetch_started) * 1000)
                except GribuClientError as exc:
                    fetch_duration_ms = int((time.monotonic() - fetch_started) * 1000)
                    return self._handle_transient_error(
                        state,
                        now_iso,
                        str(exc),
                        fetch_duration_ms=max(0, fetch_duration_ms),
                    )

                if looks_like_session_expired(response):
                    reauth_ok, reauth_error = self._try_reauthenticate()
                    if not reauth_ok:
                        return self._handle_session_expired(
                            state,
                            now_iso,
                            reauth_error,
                            fetch_duration_ms=max(0, fetch_duration_ms),
                        )
                    fetch_started = time.monotonic()
                    try:
                        response = self.gribu_client.fetch_check_page(check_url)
                        fetch_duration_ms = int((time.monotonic() - fetch_started) * 1000)
                    except GribuClientError as exc:
                        fetch_duration_ms = int((time.monotonic() - fetch_started) * 1000)
                        return self._handle_transient_error(
                            state,
                            now_iso,
                            str(exc),
                            fetch_duration_ms=max(0, fetch_duration_ms),
                        )
                    if looks_like_session_expired(response):
                        return self._handle_session_expired(
                            state,
                            now_iso,
                            "Session still appears expired after reauth",
                            fetch_duration_ms=max(0, fetch_duration_ms),
                        )

                parse_started = time.monotonic()
                try:
                    parsed = parse_unread_count(response.text)
                    parse_duration_ms = int((time.monotonic() - parse_started) * 1000)
                except UnreadParseError as exc:
                    parse_duration_ms = int((time.monotonic() - parse_started) * 1000)
                    if looks_like_session_expired(response):
                        return self._handle_session_expired(
                            state,
                            now_iso,
                            str(exc),
                            fetch_duration_ms=max(0, fetch_duration_ms or 0),
                            parse_duration_ms=max(0, parse_duration_ms),
                        )
                    return self._handle_transient_error(
                        state,
                        now_iso,
                        str(exc),
                        fetch_duration_ms=max(0, fetch_duration_ms or 0),
                        parse_duration_ms=max(0, parse_duration_ms),
                    )

                previous_unread = state.get("last_unread")
                current_unread = parsed.unread_count
                low_confidence_streak = int(state.get("low_confidence_streak", 0))
                parse_patch = {
                    "last_check_ts": now_iso,
                    "last_parse_source": parsed.source,
                    "last_parse_confidence": parsed.confidence,
                    "fetch_duration_ms": max(0, fetch_duration_ms or 0),
                    "parse_duration_ms": max(0, parse_duration_ms or 0),
                }

                if previous_unread is None and parsed.confidence < self.config.parse_min_confidence_baseline:
                    result = "low_confidence_baseline_rejected"
                    self.state_store.patch(
                        {
                            **parse_patch,
                            "last_check_result": result,
                            "last_error_message": (
                                "Baseline update rejected due to low-confidence parse "
                                f"(source={parsed.source}, confidence={parsed.confidence:.2f})"
                            ),
                            "low_confidence_streak": low_confidence_streak + 1,
                        }
                    )
                    return result

                if previous_unread is not None and parsed.confidence < self.config.parse_min_confidence_update:
                    result = "low_confidence_update_rejected"
                    self.state_store.patch(
                        {
                            **parse_patch,
                            "last_check_result": result,
                            "last_error_message": (
                                "Unread update rejected due to low-confidence parse "
                                f"(source={parsed.source}, confidence={parsed.confidence:.2f})"
                            ),
                            "low_confidence_streak": low_confidence_streak + 1,
                        }
                    )
                    return result

                if previous_unread is not None and parsed.confidence < 0.5:
                    delta = abs(current_unread - int(previous_unread))
                    if delta > self.config.parse_low_confidence_delta_limit:
                        return self._handle_transient_error(
                            state,
                            now_iso,
                            (
                                "Low-confidence parse jump rejected "
                                f"({previous_unread}->{current_unread}, "
                                f"source={parsed.source}, confidence={parsed.confidence:.2f})"
                            ),
                            parse_source=parsed.source,
                            parse_confidence=parsed.confidence,
                            fetch_duration_ms=max(0, fetch_duration_ms or 0),
                            parse_duration_ms=max(0, parse_duration_ms or 0),
                        )

                preview_sync = self._sync_preview_state(
                    state=state,
                    now_iso=now_iso,
                    now_dt=now_dt,
                    current_unread=current_unread,
                    force=force,
                    count_route=check_url,
                    count_response=response,
                    count_parsed=parsed,
                )

                if previous_unread is None:
                    result = f"baseline_set:{current_unread}"
                    alert_preview_lines: list[str] = []
                elif current_unread > int(previous_unread):
                    result = f"notified:{previous_unread}->{current_unread}"
                    if preview_sync.count_consistent:
                        alert_preview_lines = _select_new_message_preview_lines(
                            _preview_fingerprints_from_state(state),
                            preview_sync.previews,
                            current_unread - int(previous_unread),
                        )
                    else:
                        alert_preview_lines = []
                else:
                    result = f"no_change:{previous_unread}->{current_unread}"
                    alert_preview_lines = []

                self.state_store.patch(
                    {
                        **parse_patch,
                        "last_success_ts": now_iso,
                        "consecutive_errors": 0,
                        "last_unread": current_unread,
                        "last_check_result": result,
                        "last_error_message": None,
                        "low_confidence_streak": 0,
                    }
                )

                if previous_unread is not None and current_unread > int(previous_unread):
                    if self._send_telegram(
                        _build_unread_increase_notification(
                            int(previous_unread),
                            current_unread,
                            alert_preview_lines,
                        ),
                        link_preview_enabled=False,
                        open_url=None,
                        remember_notification=True,
                    ):
                        self.state_store.patch({"last_notification_sent_ts": utc_now_iso()})

                return result
        finally:
            duration_ms = int((time.monotonic() - started) * 1000)
            self.state_store.patch({"check_duration_ms": max(0, duration_ms)})


def build_service(
    config: Config,
    on_checks_enabled: Callable[[], None] | None = None,
) -> NotifierService:
    state_store = StateStore(config.state_file)
    state_store.load()
    gribu_client = GribuClient(
        base_url=config.gribu_base_url,
        cookie_header=config.gribu_cookie_header,
        timeout_sec=config.http_timeout_sec,
    )
    gribu_authenticator = GribuAuthenticator(
        base_url=config.gribu_base_url,
        login_path=config.gribu_login_path,
        session=gribu_client.session,
        timeout_sec=config.http_timeout_sec,
    )
    telegram_client = TelegramClient(
        token=config.telegram_bot_token,
        timeout_sec=config.http_timeout_sec,
        api_base_url=config.telegram_api_base_url,
    )
    navigation_markup = (
        build_navigation_reply_markup() if config.telegram_nav_buttons_enabled else None
    )
    service = NotifierService(
        config=config,
        state_store=state_store,
        gribu_client=gribu_client,
        gribu_authenticator=gribu_authenticator,
        telegram_client=telegram_client,
        on_checks_enabled=on_checks_enabled,
        navigation_reply_markup=navigation_markup,
    )
    service.startup_authenticate()
    return service


def _increment_telegram_poll_error(state_store: StateStore, error: Exception) -> None:
    def _mutator(state: dict) -> dict:
        state["telegram_poll_error_count"] = int(state.get("telegram_poll_error_count", 0)) + 1
        state["last_error_message"] = f"telegram_poll_error: {error}"
        state["last_telegram_api_error_code"] = _extract_telegram_error_code(error)
        return state

    state_store.mutate(_mutator)


def _record_scheduler_timing(state_store: StateStore, config: Config) -> int:
    state = state_store.load()
    interval_sec = compute_check_interval_sec(config, state)
    state_store.patch(
        {
            "current_check_interval_sec": interval_sec,
            "next_check_due_ts": _next_due_iso(interval_sec),
        }
    )
    return interval_sec


def _run_adaptive_scheduler_loop(
    *,
    stop_event: threading.Event,
    force_check_event: threading.Event,
    service: NotifierService,
    state_store: StateStore,
    config: Config,
    on_iteration: Callable[[], None] | None = None,
) -> None:
    next_run = time.monotonic()
    last_heartbeat_monotonic = next_run

    while not stop_event.is_set():
        now = time.monotonic()
        if on_iteration is not None and now - last_heartbeat_monotonic >= 1.0:
            on_iteration()
            last_heartbeat_monotonic = now

        force = force_check_event.is_set()
        if not force and now < next_run:
            sleep_for = min(0.25, max(0.0, next_run - now))
            time.sleep(sleep_for)
            continue

        if force:
            force_check_event.clear()
            state = state_store.load()
            if not state.get("enabled"):
                force = False

        started = time.monotonic()
        try:
            service.run_check(force=force)
        except Exception:
            LOG.exception("Unhandled exception in scheduler job")

        duration_ms = int((time.monotonic() - started) * 1000)
        interval_sec = _record_scheduler_timing(state_store, config)
        next_run = time.monotonic() + interval_sec
        state_store.patch({"check_duration_ms": max(0, duration_ms)})
        if on_iteration is not None:
            on_iteration()
            last_heartbeat_monotonic = time.monotonic()


def _log_slow_command_if_needed(command_name: str, latency_ms: int) -> None:
    if latency_ms > COMMAND_LATENCY_WARN_MS:
        LOG.warning(
            "Command latency is high (command=%s latency_ms=%s threshold_ms=%s)",
            command_name,
            latency_ms,
            COMMAND_LATENCY_WARN_MS,
        )


def _record_command_latency_histogram(
    state_store: StateStore,
    *,
    command_name: str,
    latency_ms: int,
    queue_depth: int,
) -> None:
    def _mutator(state: dict) -> dict:
        histogram = state.get("command_latency_histogram_ms")
        if not isinstance(histogram, dict):
            histogram = {}

        command_hist = histogram.get(command_name)
        if not isinstance(command_hist, dict):
            command_hist = {"le_250": 0, "le_1000": 0, "le_3000": 0, "gt_3000": 0}

        if latency_ms <= 250:
            bucket = "le_250"
        elif latency_ms <= 1000:
            bucket = "le_1000"
        elif latency_ms <= 3000:
            bucket = "le_3000"
        else:
            bucket = "gt_3000"

        command_hist[bucket] = int(command_hist.get(bucket, 0)) + 1
        histogram[command_name] = command_hist
        state["command_latency_histogram_ms"] = histogram
        state["command_queue_depth"] = max(0, queue_depth)
        return state

    state_store.mutate(_mutator)


def _run_async_command_worker(
    *,
    stop_event: threading.Event,
    command_queue: "queue.Queue[tuple[str, Callable[[], str]]]",
    in_flight_commands: set[str],
    in_flight_lock: threading.Lock,
    service: NotifierService,
    state_store: StateStore,
    on_iteration: Callable[[], None] | None = None,
) -> None:
    last_heartbeat_monotonic = time.monotonic()
    while not stop_event.is_set():
        now = time.monotonic()
        if on_iteration is not None and now - last_heartbeat_monotonic >= 1.0:
            on_iteration()
            last_heartbeat_monotonic = now

        try:
            command_name, handler = command_queue.get(timeout=0.25)
        except queue.Empty:
            continue

        started = time.monotonic()
        try:
            response = handler()
        except Exception:
            LOG.exception("Unhandled exception in async command worker (%s)", command_name)
            response = f"{command_name} failed due to an unexpected error."

        latency_ms = int((time.monotonic() - started) * 1000)
        queue_depth = command_queue.qsize()
        state_store.patch(
            {
                "last_command_latency_ms": max(0, latency_ms),
                "command_queue_depth": max(0, queue_depth),
            }
        )
        _record_command_latency_histogram(
            state_store,
            command_name=command_name,
            latency_ms=max(0, latency_ms),
            queue_depth=queue_depth,
        )
        _log_slow_command_if_needed(command_name, latency_ms)
        service._send_telegram(response)

        with in_flight_lock:
            in_flight_commands.discard(command_name)
        command_queue.task_done()
        state_store.patch({"command_queue_depth": max(0, command_queue.qsize())})

        if on_iteration is not None:
            on_iteration()
            last_heartbeat_monotonic = time.monotonic()


def _run_daemon_worker(config: Config, lock: ProcessLock, supervisor_stop_event: threading.Event) -> int:
    del lock  # Lock ownership is managed by the supervisor.
    stop_event = threading.Event()
    force_check_event = threading.Event()
    command_queue: queue.Queue[tuple[str, Callable[[], str]]] = queue.Queue()
    in_flight_commands: set[str] = set()
    in_flight_lock = threading.Lock()
    telegram_thread: threading.Thread | None = None
    scheduler_thread: threading.Thread | None = None
    command_worker_thread: threading.Thread | None = None
    last_health_log_monotonic = 0.0

    try:
        def _request_immediate_check() -> None:
            force_check_event.set()

        service = build_service(config, on_checks_enabled=_request_immediate_check)
        state_store = service.state_store
        heartbeat_tracker = HeartbeatTracker(state_store)
        now_iso = utc_now_iso()
        runtime_selinux_context = detect_selinux_context()
        runtime_context_warning = build_runtime_context_warning(runtime_selinux_context)
        state_store.patch(
            {
                "daemon_started_ts": now_iso,
                "last_watchdog_reason": "none",
                "runtime_selinux_context": runtime_selinux_context,
                "runtime_context_warning": runtime_context_warning,
                "command_queue_depth": 0,
            }
        )
        if runtime_context_warning:
            LOG.warning(runtime_context_warning)
        heartbeat_tracker.initialize(now_iso)
        _record_scheduler_timing(state_store, config)

        def _enqueue_async_command(
            command_name: str,
            handler: Callable[[], str],
            started_message: str,
            in_progress_message: str,
        ) -> str:
            with in_flight_lock:
                if command_name in in_flight_commands:
                    return in_progress_message
                in_flight_commands.add(command_name)
            command_queue.put((command_name, handler))
            state_store.patch({"command_queue_depth": max(0, command_queue.qsize())})
            return started_message

        def _queue_checknow() -> str:
            return _enqueue_async_command(
                ASYNC_COMMAND_CHECKNOW,
                service.command_checknow,
                "Manual check started. I will send the result shortly.",
                "Manual check is already in progress.",
            )

        def _queue_reauth() -> str:
            return _enqueue_async_command(
                ASYNC_COMMAND_REAUTH,
                service.command_reauth,
                "Reauth started. I will send the result shortly.",
                "Reauth is already in progress.",
            )

        callbacks = TelegramCommandCallbacks(
            on_on=service.command_on,
            on_off=service.command_off,
            on_status=service.command_status,
            on_debug=service.command_debug,
            on_checknow=_queue_checknow,
            on_reauth=_queue_reauth,
            on_help=service.command_help,
        )
        controller = TelegramController(
            client=service.telegram_client,
            state_store=state_store,
            authorized_chat_id=config.telegram_chat_id,
            callbacks=callbacks,
            navigation_buttons_enabled=config.telegram_nav_buttons_enabled,
        )

        def _mark_telegram_heartbeat() -> None:
            heartbeat_tracker.mark("telegram")

        def _mark_scheduler_heartbeat() -> None:
            heartbeat_tracker.mark("scheduler")

        def _mark_command_worker_heartbeat() -> None:
            heartbeat_tracker.mark("command_worker")

        def _on_telegram_poll_error(exc: Exception) -> None:
            _increment_telegram_poll_error(state_store, exc)

        telegram_thread = threading.Thread(
            target=controller.run_forever,
            kwargs={
                "stop_event": stop_event,
                "on_iteration": _mark_telegram_heartbeat,
                "on_poll_error": _on_telegram_poll_error,
            },
            daemon=True,
            name="telegram-controller",
        )
        scheduler_thread = threading.Thread(
            target=_run_adaptive_scheduler_loop,
            kwargs={
                "stop_event": stop_event,
                "force_check_event": force_check_event,
                "service": service,
                "state_store": state_store,
                "config": config,
                "on_iteration": _mark_scheduler_heartbeat,
            },
            daemon=True,
            name="check-scheduler",
        )
        command_worker_thread = threading.Thread(
            target=_run_async_command_worker,
            kwargs={
                "stop_event": stop_event,
                "command_queue": command_queue,
                "in_flight_commands": in_flight_commands,
                "in_flight_lock": in_flight_lock,
                "service": service,
                "state_store": state_store,
                "on_iteration": _mark_command_worker_heartbeat,
            },
            daemon=True,
            name="command-worker",
        )
        telegram_thread.start()
        scheduler_thread.start()
        command_worker_thread.start()
        LOG.info("Daemon worker started. Waiting for Telegram commands.")

        while not supervisor_stop_event.is_set():
            time.sleep(config.watchdog_check_sec)
            now_iso = heartbeat_tracker.mark("daemon")
            now_dt = parse_iso(now_iso) or datetime.now(timezone.utc)
            thread_alive = {
                "telegram": telegram_thread.is_alive(),
                "scheduler": scheduler_thread.is_alive(),
                "command_worker": command_worker_thread.is_alive(),
            }
            exit_code, reason = evaluate_watchdog(
                now_dt=now_dt,
                stale_sec=config.watchdog_stale_sec,
                thread_alive=thread_alive,
                heartbeats=heartbeat_tracker.snapshot(),
            )
            if exit_code is not None:
                state_store.patch({"last_watchdog_reason": reason})
                LOG.error("Watchdog requested restart: %s", reason)
                stop_event.set()
                return exit_code

            now_monotonic = time.monotonic()
            if now_monotonic - last_health_log_monotonic >= TELEGRAM_HEALTH_LOG_INTERVAL_SEC:
                state = state_store.load()
                LOG.info(
                    "telegram_health_snapshot thread_alive=%s poll_errors=%s offset=%s getupdates_ms=%s "
                    "send_ms=%s cmd_latency_ms=%s queue_depth=%s last_error=%s",
                    thread_alive,
                    state.get("telegram_poll_error_count"),
                    state.get("telegram_update_offset"),
                    state.get("telegram_getupdates_latency_ms"),
                    state.get("telegram_send_latency_ms"),
                    state.get("last_command_latency_ms"),
                    state.get("command_queue_depth"),
                    state.get("last_error_message"),
                )
                last_health_log_monotonic = now_monotonic

        return 0
    finally:
        stop_event.set()
        if telegram_thread is not None:
            telegram_thread.join(timeout=2.0)
        if scheduler_thread is not None:
            scheduler_thread.join(timeout=2.0)
        if command_worker_thread is not None:
            command_worker_thread.join(timeout=2.0)


def _sleep_with_stop(stop_event: threading.Event, sleep_sec: float) -> None:
    remaining = max(0.0, sleep_sec)
    while remaining > 0 and not stop_event.is_set():
        step = min(0.25, remaining)
        time.sleep(step)
        remaining -= step


def run_daemon(config: Config) -> None:
    policy = os.getenv("RUNTIME_CONTEXT_POLICY", "")
    LOG.info(
        "Daemon startup preflight policy=%r cwd=%s expected_runtime_root=%s",
        policy,
        Path.cwd().resolve(),
        "/data/local/pixel-stack/apps/site-notifications",
    )
    is_owned, ownership_reason = is_orchestrator_owned_runtime()
    if not is_owned:
        LOG.error(ownership_reason)
        raise SystemExit(DAEMON_EXIT_ORCHESTRATOR_OWNERSHIP_REQUIRED)

    lock = ProcessLock(config.daemon_lock_file)
    try:
        lock.acquire()
    except ProcessLockHeldError:
        LOG.warning("Daemon already running (lock held): %s", config.daemon_lock_file)
        raise SystemExit(DAEMON_EXIT_LOCK_HELD)

    supervisor_stop_event = threading.Event()

    def _stop_handler(_signum, _frame):
        supervisor_stop_event.set()

    signal.signal(signal.SIGINT, _stop_handler)
    signal.signal(signal.SIGTERM, _stop_handler)

    restart_attempt = 0
    state_store = StateStore(config.state_file)

    try:
        while not supervisor_stop_event.is_set():
            started_monotonic = time.monotonic()
            try:
                worker_exit_code = _run_daemon_worker(config, lock, supervisor_stop_event)
            except Exception:
                LOG.exception("Unhandled exception in daemon worker")
                worker_exit_code = DAEMON_EXIT_THREAD_DIED
                state_store.patch({"last_watchdog_reason": "worker_exception"})

            run_duration = time.monotonic() - started_monotonic

            if supervisor_stop_event.is_set():
                return

            if worker_exit_code in RECOVERABLE_WORKER_EXIT_CODES:
                if run_duration >= SUPERVISOR_STABLE_WINDOW_SEC:
                    restart_attempt = 0

                sleep_sec = min(
                    config.supervisor_restart_max_sec,
                    config.supervisor_restart_base_sec * (2 ** restart_attempt),
                )
                restart_attempt = min(restart_attempt + 1, 60)
                restart_ts = utc_now_iso()

                def _restart_mutator(state: dict) -> dict:
                    state["daemon_restart_count"] = int(state.get("daemon_restart_count", 0)) + 1
                    state["last_restart_ts"] = restart_ts
                    return state

                state_store.mutate(_restart_mutator)
                LOG.warning(
                    "Recoverable worker exit (%s). Restarting in %.1f seconds.",
                    worker_exit_code,
                    sleep_sec,
                )
                _sleep_with_stop(supervisor_stop_event, sleep_sec)
                continue

            if worker_exit_code != 0:
                raise SystemExit(worker_exit_code)

            return
    finally:
        lock.release()


def run_check_once(config: Config) -> None:
    service = build_service(config)
    result = service.run_check(force=True)
    print(result)


def show_local_status(config: Config) -> None:
    state_store = StateStore(config.state_file)
    state = state_store.load()
    print(json.dumps(state, indent=2, sort_keys=True))


def run_healthcheck(config: Config) -> int:
    state_store = StateStore(config.state_file)
    state = state_store.load()
    now_dt = datetime.now(timezone.utc)
    healthy, reason = evaluate_health_state(
        state=state,
        now_dt=now_dt,
        stale_sec=config.watchdog_stale_sec,
    )
    if healthy:
        print("healthy: daemon heartbeat is fresh")
        return 0
    print(f"unhealthy: {reason}")
    return 1


def run_diag_telegram(config: Config) -> int:
    state_store = StateStore(config.state_file)
    state = state_store.load()
    client = TelegramClient(
        token=config.telegram_bot_token,
        timeout_sec=config.http_timeout_sec,
        api_base_url=config.telegram_api_base_url,
    )

    report: dict[str, Any] = {
        "timestamp_utc": utc_now_iso(),
        "api_base_url": config.telegram_api_base_url,
        "chat_id_configured": bool(config.telegram_chat_id),
        "state": {
            "telegram_last_heartbeat_ts": state.get("telegram_last_heartbeat_ts"),
            "command_worker_last_heartbeat_ts": state.get("command_worker_last_heartbeat_ts"),
            "telegram_poll_error_count": state.get("telegram_poll_error_count"),
            "last_error_message": state.get("last_error_message"),
            "last_command_latency_ms": state.get("last_command_latency_ms"),
            "telegram_update_offset": state.get("telegram_update_offset"),
            "telegram_getupdates_latency_ms": state.get("telegram_getupdates_latency_ms"),
            "telegram_send_latency_ms": state.get("telegram_send_latency_ms"),
            "last_telegram_api_error_code": state.get("last_telegram_api_error_code"),
        },
    }

    try:
        me = client.get_me()
        report["get_me"] = {"ok": True, "id": me.get("id"), "username": me.get("username")}
    except TelegramApiError as exc:
        report["get_me"] = {
            "ok": False,
            "error": str(exc),
            "status_code": exc.status_code,
            "error_code": exc.error_code,
            "method": exc.method,
        }

    try:
        webhook = client.get_webhook_info()
        report["get_webhook_info"] = {
            "ok": True,
            "url": webhook.get("url"),
            "pending_update_count": webhook.get("pending_update_count"),
            "last_error_date": webhook.get("last_error_date"),
            "last_error_message": webhook.get("last_error_message"),
            "max_connections": webhook.get("max_connections"),
        }
    except TelegramApiError as exc:
        report["get_webhook_info"] = {
            "ok": False,
            "error": str(exc),
            "status_code": exc.status_code,
            "error_code": exc.error_code,
            "method": exc.method,
        }

    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="gribu.lv Telegram notifier")
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser("daemon", help="Run Telegram listener + adaptive scheduler")
    subparsers.add_parser("check-once", help="Run one immediate check")
    subparsers.add_parser("status-local", help="Print current local state JSON")
    subparsers.add_parser("healthcheck", help="Exit 0 when daemon heartbeats are healthy")
    subparsers.add_parser("diag-telegram", help="Print Telegram transport and heartbeat diagnostics")
    return parser.parse_args()


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    install_static_dns_fallbacks()
    args = parse_args()
    try:
        config = load_config(".env")
    except ConfigError as exc:
        raise SystemExit(f"Config error: {exc}")

    if args.command == "daemon":
        run_daemon(config)
        return
    if args.command == "check-once":
        run_check_once(config)
        return
    if args.command == "status-local":
        show_local_status(config)
        return
    if args.command == "healthcheck":
        raise SystemExit(run_healthcheck(config))
    if args.command == "diag-telegram":
        raise SystemExit(run_diag_telegram(config))
    raise SystemExit(f"Unknown command: {args.command}")


if __name__ == "__main__":
    main()
