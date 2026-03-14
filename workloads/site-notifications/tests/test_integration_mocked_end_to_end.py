import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs

from app import NotifierService
from config import Config
from gribu_auth import GribuAuthenticator
from gribu_client import GribuClient
from state_store import StateStore
from telegram_control import TelegramClient, TelegramCommandCallbacks, TelegramController


class MockState:
    def __init__(self):
        self.updates = [
            {"update_id": 1, "message": {"chat": {"id": 42}, "text": "/on"}},
        ]
        self.sent_messages = []
        self.unread = 2
        self.message_rows = [
            ("/lv/messages/1", "Anna", "See you tonight"),
            ("/lv/messages/2", "Bruno", "Running 10 minutes late"),
        ]
        self.chat_rows = [
            ("/en/chat?conversation=1", 1, "Anna", "See you tonight"),
            ("/en/chat?conversation=2", 1, "Bruno", "Running 10 minutes late"),
        ]
        self.login_posts = []


def _json_response(handler: BaseHTTPRequestHandler, payload: dict):
    body = json.dumps(payload).encode("utf-8")
    handler.send_response(200)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


def _html_response(handler: BaseHTTPRequestHandler, html: str):
    body = html.encode("utf-8")
    handler.send_response(200)
    handler.send_header("Content-Type", "text/html; charset=utf-8")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


def _message_list_html(count: int, rows: list[tuple[str, str, str]]) -> str:
    items = "".join(
        (
            "<li class='messages-list-item' data-message-id='{idx}'>"
            "<a class='messages-list-item__link' href='{href}'>"
            "<span class='messages-list-item__title'>{title}</span>"
            "<span class='messages-list-item__preview'>{preview}</span>"
            "</a>"
            "</li>"
        ).format(idx=index, href=href, title=title, preview=preview)
        for index, (href, title, preview) in enumerate(rows, start=1)
    )
    return (
        "<html><body>"
        f"<div data-header-notification-count data-count=\"{count}\"></div>"
        "<ul class='messages-list'>"
        f"{items}"
        "</ul>"
        "</body></html>"
    )


def _cats_contact_list_html(count: int, rows: list[tuple[str, int, str, str]]) -> str:
    items = "".join(
        (
            "<li class='chat-contact-list-item' data-conversation-id='conv-{idx}'>"
            "<a class='chat-contact-list-item__link' href='{href}'>"
            "<span class='chat-contact-list-item__title'>{title}</span>"
            "{badge}"
            "<span class='chat-contact-list-item__preview'>{preview}</span>"
            "</a>"
            "</li>"
        ).format(
            idx=index,
            href=href,
            title=title,
            preview=preview,
            badge=(
                ""
                if unread_count <= 0
                else (
                    "<span class='chat-contact-list-item__badge'>"
                    f"<span class='online-counter__count'>{unread_count}</span>"
                    "</span>"
                )
            ),
        )
        for index, (href, unread_count, title, preview) in enumerate(rows, start=1)
    )
    return (
        "<html><body>"
        "<li class='header__top-left-group-item header__top-left-group-item_chat'>"
        "<a href='/cats' class='header__bottom-row-link'>"
        "<span class='header__counter-inner'>"
        "<span class='online-counter online-counter_header'>"
        f"<sup class='online-counter__count'>{count}</sup>"
        "</span>"
        "</span>"
        "</a>"
        "</li>"
        "<ul class='chat-contact-list'>"
        f"{items}"
        "</ul>"
        "</body></html>"
    )


def _make_handler(mock_state: MockState):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, _format, *_args):
            return

        def do_POST(self):  # noqa: N802
            content_length = int(self.headers.get("Content-Length", "0"))
            raw = self.rfile.read(content_length).decode("utf-8") if content_length else "{}"
            if self.path == "/pieslegties":
                payload = parse_qs(raw, keep_blank_values=True)
                mock_state.login_posts.append(payload)
                if payload.get("login[_token]", [""])[0] == "token123":
                    self.send_response(302)
                    self.send_header("Location", "/lv/messages")
                    self.send_header("Set-Cookie", "DATED=abc; Path=/")
                    self.send_header("Set-Cookie", "DATINGSES=def; Path=/")
                    self.end_headers()
                    return
                _html_response(self, "<form><input name='login[_token]' value='token123'></form>")
                return

            payload = json.loads(raw)
            if self.path.endswith("/botTOKEN/getUpdates"):
                offset = payload.get("offset")
                if offset is None:
                    updates = mock_state.updates
                else:
                    updates = [u for u in mock_state.updates if u["update_id"] >= offset]
                _json_response(self, {"ok": True, "result": updates})
                return
            if self.path.endswith("/botTOKEN/sendMessage"):
                mock_state.sent_messages.append(payload)
                _json_response(self, {"ok": True, "result": {"message_id": len(mock_state.sent_messages)}})
                return
            self.send_response(404)
            self.end_headers()

        def do_GET(self):  # noqa: N802
            if self.path == "/lv/messages":
                html = _message_list_html(mock_state.unread, mock_state.message_rows)
                _html_response(self, html)
                return
            if self.path == "/cats":
                html = _cats_contact_list_html(mock_state.unread, mock_state.chat_rows)
                _html_response(self, html)
                return
            if self.path == "/pieslegties":
                html = (
                    "<html><body><form>"
                    '<input type="hidden" name="login[_token]" value="token123">'
                    '<input type="text" name="login[email]">'
                    '<input type="password" name="login[password]">'
                    "</form></body></html>"
                )
                _html_response(self, html)
                return
            self.send_response(404)
            self.end_headers()

    return Handler


def _make_config(tmp_path: Path, base_url: str) -> Config:
    return Config(
        telegram_bot_token="TOKEN",
        telegram_chat_id=42,
        gribu_base_url=base_url,
        gribu_check_url="/lv/messages",
        gribu_check_url_candidates=("/lv/messages",),
        gribu_preview_url="/cats",
        gribu_preview_url_candidates=("/cats",),
        gribu_login_id="demo@example.com",
        gribu_login_password="secret",
        gribu_login_path="/pieslegties",
        gribu_cookie_header="",
        check_interval_sec=60,
        check_interval_fast_sec=20,
        check_interval_idle_sec=60,
        check_interval_error_backoff_max_sec=180,
        state_file=(tmp_path / "state.json"),
        http_timeout_sec=5,
        error_alert_cooldown_sec=1800,
        telegram_api_base_url=base_url,
        telegram_nav_buttons_enabled=True,
        env_file_path=(tmp_path / ".env"),
        daemon_lock_file=(tmp_path / "daemon.lock"),
        watchdog_check_sec=10,
        watchdog_stale_sec=120,
        supervisor_restart_base_sec=2,
        supervisor_restart_max_sec=30,
        parse_low_confidence_delta_limit=20,
        route_discovery_ttl_sec=21600,
        preview_route_discovery_ttl_sec=21600,
        parse_min_confidence_baseline=0.8,
        parse_min_confidence_update=0.7,
        parse_min_confidence_route_selection=0.7,
    )


def test_end_to_end_with_mocked_http(tmp_path: Path):
    mock_state = MockState()
    server = ThreadingHTTPServer(("127.0.0.1", 0), _make_handler(mock_state))
    host, port = server.server_address
    base_url = f"http://{host}:{port}"
    server_thread = threading.Thread(target=server.serve_forever, daemon=True)
    server_thread.start()

    try:
        config = _make_config(tmp_path, base_url)
        store = StateStore(config.state_file)
        gribu = GribuClient(config.gribu_base_url, config.gribu_cookie_header, timeout_sec=5)
        auth = GribuAuthenticator(
            base_url=config.gribu_base_url,
            login_path=config.gribu_login_path,
            session=gribu.session,
            timeout_sec=5,
        )
        telegram = TelegramClient(
            token=config.telegram_bot_token,
            timeout_sec=5,
            api_base_url=config.telegram_api_base_url,
        )
        service = NotifierService(config, store, gribu, auth, telegram)
        assert service.startup_authenticate() is True
        assert "GRIBU_COOKIE_HEADER=DATED=abc; DATINGSES=def" in config.env_file_path.read_text(
            encoding="utf-8"
        )
        callbacks = TelegramCommandCallbacks(
            on_on=service.command_on,
            on_off=service.command_off,
            on_status=service.command_status,
            on_debug=service.command_debug,
            on_checknow=service.command_checknow,
            on_reauth=service.command_reauth,
            on_help=service.command_help,
        )
        controller = TelegramController(
            client=telegram,
            state_store=store,
            authorized_chat_id=config.telegram_chat_id,
            callbacks=callbacks,
        )

        controller.poll_once(timeout=0)
        assert store.load()["enabled"] is True

        first = service.run_check(force=False)
        assert first == "baseline_set:2"

        mock_state.unread = 5
        mock_state.message_rows = [
            ("/lv/messages/2", "Bruno", "Running 10 minutes late"),
            ("/lv/messages/3", "Cara", "Fresh hello"),
            ("/lv/messages/4", "Dmitry", "Another unread note"),
            ("/lv/messages/5", "Eva", "See you soon"),
        ]
        mock_state.chat_rows = [
            ("/en/chat?conversation=2", 1, "Bruno", "Running 10 minutes late"),
            ("/en/chat?conversation=3", 1, "Cara", "Fresh hello"),
            ("/en/chat?conversation=4", 1, "Dmitry", "Another unread note"),
            ("/en/chat?conversation=5", 1, "Eva", "See you soon"),
        ]
        second = service.run_check(force=False)
        assert second == "notified:2->5"

        sent_texts = [payload["text"] for payload in mock_state.sent_messages]
        assert any("Checks enabled" in text for text in sent_texts)
        assert any("Unread increased: 2 -> 5" in text for text in sent_texts)
        assert any("New messages:" in text for text in sent_texts)
        assert any("Cara: Fresh hello" in text for text in sent_texts)
        assert any("reply_markup" in payload for payload in mock_state.sent_messages)
        assert mock_state.sent_messages[0]["link_preview_options"] == {"is_disabled": True}
        assert any(
            payload.get("link_preview_options") == {"is_disabled": False}
            and "Unread increased: 2 -> 5" in payload.get("text", "")
            for payload in mock_state.sent_messages
        )
        assert len(mock_state.login_posts) == 1
    finally:
        server.shutdown()
        server.server_close()
