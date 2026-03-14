#!/usr/bin/env python3
"""Idempotent Pi-hole payload importer for AdGuard Home."""

from __future__ import annotations

import argparse
import json
import pathlib
import ssl
import sys
import urllib.error
import urllib.parse
import urllib.request
from http.cookiejar import CookieJar
from typing import Any


class ApiClient:
    def __init__(
        self,
        base_url: str,
        username: str,
        password: str,
        timeout: float = 10.0,
        insecure_tls: bool = False,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        cookie_jar = CookieJar()
        handlers: list[Any] = [urllib.request.HTTPCookieProcessor(cookie_jar)]
        if insecure_tls:
            tls_context = ssl.create_default_context()
            tls_context.check_hostname = False
            tls_context.verify_mode = ssl.CERT_NONE
            handlers.append(urllib.request.HTTPSHandler(context=tls_context))
        self.opener = urllib.request.build_opener(*handlers)
        self.username = username
        self.password = password

    def request(self, method: str, path: str, payload: Any | None = None) -> tuple[int, str]:
        url = f"{self.base_url}{path}"
        data: bytes | None = None
        headers = {"Accept": "application/json"}
        if payload is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(url=url, method=method, data=data, headers=headers)
        try:
            with self.opener.open(req, timeout=self.timeout) as resp:
                body = resp.read().decode("utf-8", errors="replace")
                return int(getattr(resp, "status", 200)), body
        except urllib.error.HTTPError as exc:
            body = exc.read().decode("utf-8", errors="replace")
            return int(exc.code), body

    def login(self) -> None:
        code, body = self.request(
            "POST",
            "/control/login",
            {"name": self.username, "password": self.password},
        )
        if code != 200:
            raise RuntimeError(f"login failed: status={code} body={body[:200]}")



def normalize_payload(raw: dict[str, Any]) -> dict[str, Any]:
    adlists: list[str] = []
    for entry in raw.get("adlists", []):
        if isinstance(entry, str):
            if entry.strip():
                adlists.append(entry.strip())
            continue
        if isinstance(entry, dict):
            url = str(entry.get("url", "")).strip()
            enabled = int(entry.get("enabled", 1) or 0)
            if url and enabled:
                adlists.append(url)

    domains = raw.get("domain_rules", {}) or {}
    if not isinstance(domains, dict):
        domains = {}

    return {
        "adlists": sorted(set(adlists)),
        "allow_exact": sorted(set(map(str, domains.get("allow_exact", []) or []))),
        "block_exact": sorted(set(map(str, domains.get("block_exact", []) or []))),
        "allow_regex": sorted(set(map(str, domains.get("allow_regex", []) or []))),
        "block_regex": sorted(set(map(str, domains.get("block_regex", []) or []))),
        "upstreams": list(raw.get("upstreams", []) or []),
    }



def build_filter_rules(payload: dict[str, Any]) -> list[str]:
    rules: list[str] = []

    for domain in payload["allow_exact"]:
        d = domain.strip().lower()
        if d:
            rules.append(f"@@||{d}^")

    for domain in payload["block_exact"]:
        d = domain.strip().lower()
        if d:
            rules.append(f"||{d}^")

    for regex in payload["allow_regex"]:
        r = regex.strip()
        if r:
            rules.append(f"@@/{r}/")

    for regex in payload["block_regex"]:
        r = regex.strip()
        if r:
            rules.append(f"/{r}/")

    deduped: list[str] = []
    seen: set[str] = set()
    for rule in rules:
        if rule not in seen:
            seen.add(rule)
            deduped.append(rule)
    return deduped



def import_adlists(client: ApiClient, adlists: list[str]) -> dict[str, int]:
    added = 0
    skipped = 0
    failed = 0

    for url in adlists:
        code, body = client.request(
            "POST",
            "/control/filtering/add_url",
            {"name": url, "url": url, "whitelist": False},
        )
        if code in (200, 201):
            added += 1
            continue
        body_lower = body.lower()
        if code in (400, 409) and ("already exists" in body_lower or "duplicate" in body_lower):
            skipped += 1
            continue
        failed += 1

    return {"added": added, "skipped": skipped, "failed": failed}



def import_rules(client: ApiClient, rules: list[str]) -> dict[str, int]:
    if not rules:
        return {"applied": 0, "failed": 0}

    rules_text = "\n".join(rules)
    code, body = client.request("POST", "/control/filtering/set_rules", {"rules": rules_text})
    if code != 200:
        raise RuntimeError(f"set_rules failed: status={code} body={body[:200]}")
    return {"applied": len(rules), "failed": 0}



def apply_dns_upstreams(client: ApiClient, upstreams: list[str]) -> dict[str, Any]:
    cleaned = [u.strip() for u in upstreams if isinstance(u, str) and u.strip()]
    if not cleaned:
        cleaned = ["https://1.1.1.1/dns-query", "https://1.0.0.1/dns-query"]

    code, body = client.request(
        "POST",
        "/control/dns_config",
        {
            "upstream_dns": cleaned,
            "bootstrap_dns": ["1.1.1.1", "1.0.0.1"],
        },
    )
    if code != 200:
        raise RuntimeError(f"dns_config failed: status={code} body={body[:200]}")
    return {"upstreams": cleaned, "status": "applied"}



def load_password(path: pathlib.Path) -> str:
    if not path.exists():
        raise RuntimeError(f"password file not found: {path}")
    password = path.read_text(encoding="utf-8", errors="ignore").strip()
    if not password:
        raise RuntimeError(f"password file is empty: {path}")
    return password



def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Import Pi-hole migration payload into AdGuard Home")
    p.add_argument("--base-url", required=True, help="AdGuard Home base URL (e.g. http://127.0.0.1:8080)")
    p.add_argument("--username", required=True, help="AdGuard Home admin username")
    p.add_argument("--password-file", required=True, help="Path to admin password file")
    p.add_argument("--payload-file", required=True, help="Path to exported migration payload JSON")
    p.add_argument("--report-file", required=True, help="Path to write JSON migration report")
    p.add_argument(
        "--insecure-tls",
        action="store_true",
        help="Disable TLS certificate verification for local migration calls",
    )
    return p.parse_args()



def main() -> int:
    args = parse_args()
    payload_path = pathlib.Path(args.payload_file)
    report_path = pathlib.Path(args.report_file)

    report: dict[str, Any] = {
        "source_payload": str(payload_path),
        "target": args.base_url,
        "status": "failed",
        "error": None,
        "adlists": {},
        "rules": {},
        "dns": {},
    }

    try:
        payload_raw = json.loads(payload_path.read_text(encoding="utf-8"))
        if not isinstance(payload_raw, dict):
            raise RuntimeError("payload root must be an object")

        payload = normalize_payload(payload_raw)
        password = load_password(pathlib.Path(args.password_file))

        client = ApiClient(
            base_url=args.base_url,
            username=args.username,
            password=password,
            insecure_tls=args.insecure_tls,
        )
        client.login()

        report["adlists"] = import_adlists(client, payload["adlists"])
        rules = build_filter_rules(payload)
        report["rules"] = import_rules(client, rules)
        report["dns"] = apply_dns_upstreams(client, payload.get("upstreams", []))
        report["status"] = "ok"
    except Exception as exc:  # noqa: BLE001
        report["error"] = str(exc)

    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(json.dumps(report, indent=2, sort_keys=True), encoding="utf-8")

    if report["status"] != "ok":
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
