#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
IDENTITY_HELPER="${REPO_ROOT}/android-orchestrator/app/src/main/assets/runtime/templates/rooted/adguardhome-doh-identities.py"
WEB_HELPER="${REPO_ROOT}/android-orchestrator/app/src/main/assets/runtime/templates/rooted/adguardhome-doh-identity-web.py"

for cmd in python3 jq curl; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "FAIL: ${cmd} is required" >&2
    exit 1
  fi
done
if ! command -v node >/dev/null 2>&1; then
  echo "FAIL: node is required" >&2
  exit 1
fi

if [[ ! -f "${IDENTITY_HELPER}" ]]; then
  echo "FAIL: identity helper script missing: ${IDENTITY_HELPER}" >&2
  exit 1
fi
if [[ ! -f "${WEB_HELPER}" ]]; then
  echo "FAIL: web helper script missing: ${WEB_HELPER}" >&2
  exit 1
fi

tmpdir="$(mktemp -d)"
sidecar_pid=""
trap 'if [[ -n "${sidecar_pid}" ]] && kill -0 "${sidecar_pid}" >/dev/null 2>&1; then kill "${sidecar_pid}" >/dev/null 2>&1 || true; wait "${sidecar_pid}" 2>/dev/null || true; fi; rm -rf "${tmpdir}"' EXIT

port="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"

identityctl_wrapper="${tmpdir}/identityctl"
identityctl_invocation_log="${tmpdir}/identityctl-invocations.log"
cat > "${identityctl_wrapper}" <<EOF_WRAPPER
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >> "${identityctl_invocation_log}"
case "\${1:-}" in
  list|usage|events)
    sleep 0.2
    ;;
esac
exec python3 "${IDENTITY_HELPER}" "\$@"
EOF_WRAPPER
chmod 0755 "${identityctl_wrapper}"

count_invocations() {
  local prefix="$1"
  awk -v prefix="${prefix}" 'index($0, prefix) == 1 { count += 1 } END { print count + 0 }' "${identityctl_invocation_log}"
}

run_parallel_get() {
  local url="$1"
  local concurrency="${2:-4}"
  python3 - <<'PY' "${url}" "${concurrency}"
import concurrent.futures
import sys
import urllib.request

url = sys.argv[1]
concurrency = int(sys.argv[2])

def fetch(_index: int) -> None:
  with urllib.request.urlopen(url, timeout=5) as response:
    response.read()

with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as executor:
  list(executor.map(fetch, range(concurrency)))
PY
}

export ADGUARDHOME_DOH_IDENTITIES_FILE="${tmpdir}/doh-identities.json"
export ADGUARDHOME_DOH_USAGE_EVENTS_FILE="${tmpdir}/state/doh-usage-events.jsonl"
export ADGUARDHOME_DOH_USAGE_CURSOR_FILE="${tmpdir}/state/doh-usage-cursor.json"
export ADGUARDHOME_DOH_ACCESS_LOG_FILE="${tmpdir}/remote-nginx-doh-access.log"
export ADGUARDHOME_DOT_ACCESS_LOG_FILE="${tmpdir}/remote-nginx-dot-access.log"
export ADGUARDHOME_DOH_USAGE_RETENTION_DAYS=30
export ADGUARDHOME_DOH_IDENTITYCTL_APPLY=0
export ADGUARDHOME_DOH_IDENTITY_WEB_QUERYLOG_JSON_FILE="${tmpdir}/querylog-fixture.json"
export ADGUARDHOME_DOH_IDENTITY_WEB_STATUS_JSON_FILE="${tmpdir}/status-fixture.json"
export ADGUARDHOME_DOH_IDENTITY_WEB_STATS_JSON_FILE="${tmpdir}/stats-fixture.json"
export ADGUARDHOME_DOH_IDENTITY_WEB_CLIENTS_JSON_FILE="${tmpdir}/clients-fixture.json"
export ADGUARDHOME_DOH_IDENTITY_WEB_CLIENTS_SEARCH_JSON_FILE="${tmpdir}/clients-search-fixture.json"
export ADGUARDHOME_DOH_IDENTITY_WEB_IPINFO_CACHE_FILE="${tmpdir}/ipinfo-cache.json"
export ADGUARDHOME_DOH_IDENTITY_WEB_RESTART_ENTRY="${tmpdir}/fake-restart-entry.sh"
export ADGUARDHOME_DOH_IDENTITY_WEB_RESTART_MODE="--remote-reload-frontend"
export ADGUARDHOME_REMOTE_DOT_IDENTITY_ENABLED=1
export ADGUARDHOME_REMOTE_DOT_IDENTITY_LABEL_LENGTH=20
export PIHOLE_REMOTE_DOT_HOSTNAME="dns.jolkins.id.lv"
export ADGUARDHOME_REMOTE_ROUTER_LAN_IP="192.168.31.1"
export PIHOLE_WEB_PORT=8080
reload_log="${tmpdir}/reload.log"

cache_epoch="$(date +%s)"
querylog_times="$(python3 - <<'PY'
from datetime import datetime, timedelta, timezone

base = datetime.now(timezone.utc).replace(microsecond=100000) - timedelta(minutes=8)
for offset_seconds in range(9):
    print((base + timedelta(seconds=offset_seconds)).isoformat().replace("+00:00", "Z"))
PY
)"
querylog_internal_doh_time="$(printf '%s\n' "${querylog_times}" | sed -n '1p')"
querylog_internal_plain_time="$(printf '%s\n' "${querylog_times}" | sed -n '2p')"
querylog_self_time="$(printf '%s\n' "${querylog_times}" | sed -n '3p')"
querylog_ipv6_time="$(printf '%s\n' "${querylog_times}" | sed -n '4p')"
querylog_public_time="$(printf '%s\n' "${querylog_times}" | sed -n '5p')"
querylog_service_time="$(printf '%s\n' "${querylog_times}" | sed -n '6p')"
querylog_device_time="$(printf '%s\n' "${querylog_times}" | sed -n '7p')"
querylog_router_time="$(printf '%s\n' "${querylog_times}" | sed -n '8p')"
querylog_dot_beta_time="$(printf '%s\n' "${querylog_times}" | sed -n '9p')"

cat > "${ADGUARDHOME_DOH_IDENTITY_WEB_RESTART_ENTRY}" <<EOF_RESTART
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >> "${reload_log}"
exit 0
EOF_RESTART
chmod 0755 "${ADGUARDHOME_DOH_IDENTITY_WEB_RESTART_ENTRY}"

cat > "${ADGUARDHOME_DOH_IDENTITY_WEB_QUERYLOG_JSON_FILE}" <<'EOF_QUERYLOG'
{
  "data": [
    {"time":"__QUERYLOG_INTERNAL_DOH_TIME__","client":"127.0.0.1","client_proto":"doh","elapsedMs":"5","status":"NOERROR","question":{"name":"example.com","type":"A"},"client_info":{"whois":{}}},
    {"time":"__QUERYLOG_INTERNAL_PLAIN_TIME__","client":"127.0.0.1","client_proto":"plain","elapsedMs":"2","status":"NOERROR","question":{"name":"example.com","type":"AAAA"},"client_info":{"whois":{}}},
    {"time":"__QUERYLOG_SELF_TIME__","client":"127.0.0.1","client_proto":"plain","elapsedMs":"3","status":"NOERROR","question":{"name":"self.example.net","type":"A"},"client_info":{"whois":{}}},
    {"time":"__QUERYLOG_IPV6_TIME__","client":"::1","client_proto":"doh","elapsedMs":"4","status":"NOERROR","question":{"name":"example.com","type":"AAAA"},"client_info":{"whois":{}}},
    {"time":"__QUERYLOG_PUBLIC_TIME__","client":"212.3.197.32","client_proto":"doh","elapsedMs":"12","status":"NOERROR","question":{"name":"public.example.net","type":"A"},"client_info":{"whois":{}}},
    {"time":"__QUERYLOG_SERVICE_TIME__","client":"212.3.197.32","client_proto":"doh","elapsedMs":"40","status":"NOERROR","question":{"name":"service.example.net","type":"A"},"client_info":{"whois":{}}},
    {"time":"__QUERYLOG_DEVICE_TIME__","client":"192.168.31.39","client_proto":"doh","elapsedMs":"8","status":"NOERROR","question":{"name":"example.com","type":"A"},"client_info":{"whois":{}}},
    {"time":"__QUERYLOG_ROUTER_TIME__","client":"192.168.31.1","client_proto":"doh","elapsedMs":"14","status":"NOERROR","question":{"name":"router.example","type":"A"},"client_info":{"whois":{}}},
    {"time":"__QUERYLOG_DOT_BETA_TIME__","client":"127.0.0.1","client_proto":"dot","elapsedMs":"11","status":"NOERROR","question":{"name":"beta-dot.example.net","type":"A"},"client_info":{"name":"Identity beta","disallowed_rule":"__BETA_DOT_LABEL__","whois":{}}}
  ]
}
EOF_QUERYLOG
python3 - <<'PY' \
  "${ADGUARDHOME_DOH_IDENTITY_WEB_QUERYLOG_JSON_FILE}" \
  "${querylog_internal_doh_time}" \
  "${querylog_internal_plain_time}" \
  "${querylog_self_time}" \
  "${querylog_ipv6_time}" \
  "${querylog_public_time}" \
  "${querylog_service_time}" \
  "${querylog_device_time}" \
  "${querylog_router_time}" \
  "${querylog_dot_beta_time}"
import pathlib
import sys
path = pathlib.Path(sys.argv[1])
path.write_text(
    path.read_text()
    .replace("__QUERYLOG_INTERNAL_DOH_TIME__", sys.argv[2])
    .replace("__QUERYLOG_INTERNAL_PLAIN_TIME__", sys.argv[3])
    .replace("__QUERYLOG_SELF_TIME__", sys.argv[4])
    .replace("__QUERYLOG_IPV6_TIME__", sys.argv[5])
    .replace("__QUERYLOG_PUBLIC_TIME__", sys.argv[6])
    .replace("__QUERYLOG_SERVICE_TIME__", sys.argv[7])
    .replace("__QUERYLOG_DEVICE_TIME__", sys.argv[8])
    .replace("__QUERYLOG_ROUTER_TIME__", sys.argv[9])
    .replace("__QUERYLOG_DOT_BETA_TIME__", sys.argv[10]),
    encoding="utf-8",
)
PY

cat > "${ADGUARDHOME_DOH_IDENTITY_WEB_STATUS_JSON_FILE}" <<'EOF_STATUS'
{
  "dns_addresses": ["127.0.0.1", "192.168.31.1", "192.168.31.25"]
}
EOF_STATUS

cat > "${ADGUARDHOME_DOH_IDENTITY_WEB_STATS_JSON_FILE}" <<'EOF_STATS'
{
  "top_clients": [{"127.0.0.1": 3}, {"212.3.197.32": 1}]
}
EOF_STATS

cat > "${ADGUARDHOME_DOH_IDENTITY_WEB_CLIENTS_JSON_FILE}" <<'EOF_CLIENTS'
{
  "auto_clients": [
    {"whois_info": {}, "ip": "212.3.197.32", "name": "", "source": "ARP"},
    {"whois_info": {}, "ip": "192.168.31.25", "name": "", "source": "ARP"}
  ],
  "clients": [],
  "supported_tags": []
}
EOF_CLIENTS

cat > "${ADGUARDHOME_DOH_IDENTITY_WEB_CLIENTS_SEARCH_JSON_FILE}" <<'EOF_CLIENT_SEARCH'
[
  {
    "212.3.197.32": {
      "disallowed": false,
      "whois_info": {},
      "name": "",
      "ids": ["212.3.197.32"],
      "filtering_enabled": false,
      "parental_enabled": false,
      "safebrowsing_enabled": false,
      "safesearch_enabled": false,
      "use_global_blocked_services": false,
      "use_global_settings": false,
      "ignore_querylog": null,
      "ignore_statistics": null,
      "upstreams_cache_size": 0,
      "upstreams_cache_enabled": null
    }
  }
]
EOF_CLIENT_SEARCH

cat > "${ADGUARDHOME_DOH_IDENTITY_WEB_IPINFO_CACHE_FILE}" <<EOF_IPINFO
{"212.3.197.32":{"cachedAtEpochSeconds":${cache_epoch},"whois_info":{"country":"LV","orgname":"Operator Example"}}}
EOF_IPINFO

python3 "${WEB_HELPER}" \
  --host 127.0.0.1 \
  --port "${port}" \
  --identityctl "${identityctl_wrapper}" \
  --adguard-web-port 8080 \
  --skip-session-check \
  >"${tmpdir}/sidecar.log" 2>&1 &
sidecar_pid="$!"

for _ in $(seq 1 40); do
  if curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/inject.js" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done

if ! curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/inject.js" >/dev/null 2>&1; then
  echo "FAIL: sidecar did not start" >&2
  exit 1
fi
if ! curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/bootstrap.js" >/dev/null 2>&1; then
  echo "FAIL: bootstrap injector endpoint did not return 200" >&2
  exit 1
fi
identity_html="${tmpdir}/identity.html"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity" > "${identity_html}"
if ! rg -Fq '.table-scroll { overflow-x: auto; -webkit-overflow-scrolling: touch; }' "${identity_html}"; then
  echo "FAIL: identity page should make wide tables horizontally scrollable on narrow screens" >&2
  exit 1
fi
if ! rg -Fq 'class="table-scroll table-scroll--wide"' "${identity_html}"; then
  echo "FAIL: identity page should wrap the identities table in a wide scroll container" >&2
  exit 1
fi
if ! rg -Fq 'value="1000"' "${identity_html}"; then
  echo "FAIL: identity page should default the querylog summary limit to 1000 rows" >&2
  exit 1
fi
if ! rg -Fq 'await Promise.all([querylogPromise, refreshUsage()]);' "${identity_html}"; then
  echo "FAIL: identity page should overlap querylog and usage refresh work during load" >&2
  exit 1
fi
bootstrap_js="${tmpdir}/bootstrap.js"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/bootstrap.js" > "${bootstrap_js}"
if rg -Fq '/pixel-stack/identity/api/v1/adguard/stats' "${bootstrap_js}"; then
  echo "FAIL: bootstrap.js should not rewrite native /control/stats requests" >&2
  exit 1
fi
if rg -Fq '/pixel-stack/identity/api/v1/adguard/clients' "${bootstrap_js}"; then
  echo "FAIL: bootstrap.js should not rewrite native /control/clients requests" >&2
  exit 1
fi
if ! rg -Fq '/pixel-stack/identity/api/v1/adguard/querylog' "${bootstrap_js}"; then
  echo "FAIL: bootstrap.js should rewrite native /control/querylog requests on the logs route" >&2
  exit 1
fi
if ! rg -Fq 'pixelstack:native-querylog-updated' "${bootstrap_js}"; then
  echo "FAIL: bootstrap.js should continue publishing native querylog updates" >&2
  exit 1
fi
if ! rg -Fq 'pixelstack:native-dashboard-updated' "${bootstrap_js}"; then
  echo "FAIL: bootstrap.js should publish native dashboard refresh events" >&2
  exit 1
fi
inject_js="${tmpdir}/inject.js"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/inject.js" > "${inject_js}"
if rg -Fq 'MutationObserver' "${inject_js}"; then
  echo "FAIL: inject.js should no longer use MutationObserver-driven global sync" >&2
  exit 1
fi
if rg -Fq 'window=30d' "${inject_js}"; then
  echo "FAIL: inject.js should not prefetch 30d usage for querylog options" >&2
  exit 1
fi
if ! rg -Fq 'waitForElement' "${inject_js}"; then
  echo "FAIL: inject.js should use route-scoped waitForElement mounting" >&2
  exit 1
fi
if ! rg -Fq 'pixel-stack-summary-row' "${inject_js}"; then
  echo "FAIL: inject.js should add dashboard summary row marker classes" >&2
  exit 1
fi
if ! rg -Fq 'pixel-stack-summary-card--blocked' "${inject_js}"; then
  echo "FAIL: inject.js should add blocked summary card marker classes" >&2
  exit 1
fi
if ! rg -Fq '.pixel-stack-summary-card--blocked .card-wrap { position: relative; }' "${inject_js}"; then
  echo "FAIL: inject.js should anchor the blocked summary chart inside the card" >&2
  exit 1
fi
if ! rg -Fq '.pixel-stack-summary-card--blocked .card-body-stats { position: relative; z-index: 2; padding-bottom: 0.75rem; }' "${inject_js}"; then
  echo "FAIL: inject.js should keep blocked summary text above the full-height chart" >&2
  exit 1
fi
if ! rg -Fq '.pixel-stack-summary-card--blocked .card-chart-bg { position: absolute; inset: 0; height: 100%; min-height: 100%; z-index: 1; }' "${inject_js}"; then
  echo "FAIL: inject.js should render the blocked summary chart across the full card height" >&2
  exit 1
fi
if ! rg -Fq '.pixel-stack-summary-card--dns .card-wrap { position: relative; }' "${inject_js}"; then
  echo "FAIL: inject.js should anchor the DNS summary chart inside the card" >&2
  exit 1
fi
if ! rg -Fq '.pixel-stack-summary-card--dns .card-body-stats { position: relative; z-index: 2; padding-bottom: 0.75rem; }' "${inject_js}"; then
  echo "FAIL: inject.js should keep DNS summary text above the full-height chart" >&2
  exit 1
fi
if ! rg -Fq '.pixel-stack-summary-card--dns .card-chart-bg { position: absolute; inset: 0; height: 100%; min-height: 100%; z-index: 1; }' "${inject_js}"; then
  echo "FAIL: inject.js should render the DNS summary chart across the full card height" >&2
  exit 1
fi
if ! rg -Fq '.pixel-stack-summary-card--dns .card-title-stats a { text-shadow: 0 1px 2px rgba(255, 255, 255, 0.92), 0 0 0.8rem rgba(255, 255, 255, 0.72); }' "${inject_js}"; then
  echo "FAIL: inject.js should keep DNS summary text readable with the blocked-summary shadow treatment" >&2
  exit 1
fi
if ! rg -Fq 'text-shadow: 0 1px 2px rgba(255, 255, 255, 0.92), 0 0 0.8rem rgba(255, 255, 255, 0.72);' "${inject_js}"; then
  echo "FAIL: inject.js should keep blocked summary text readable with a shadow-only treatment" >&2
  exit 1
fi
if ! rg -Fq 'pixel-stack-summary-card--compact-quarter' "${inject_js}"; then
  echo "FAIL: inject.js should include quarter-height compact summary card classes" >&2
  exit 1
fi
if ! rg -Fq 'grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);' "${inject_js}"; then
  echo "FAIL: inject.js should include the desktop summary reflow grid rule" >&2
  exit 1
fi
if ! rg -Fq 'pixel-stack-dashboard-masonry-row' "${inject_js}"; then
  echo "FAIL: inject.js should include the desktop dashboard masonry row class" >&2
  exit 1
fi
if ! rg -Fq 'pixel-stack-dashboard-masonry-columns' "${inject_js}"; then
  echo "FAIL: inject.js should include the desktop dashboard masonry wrapper class" >&2
  exit 1
fi
if ! rg -Fq 'pixel-stack-dashboard-masonry-item' "${inject_js}"; then
  echo "FAIL: inject.js should include the desktop dashboard masonry item class" >&2
  exit 1
fi
if ! rg -Fq -- '--pixel-stack-dashboard-gap:' "${inject_js}"; then
  echo "FAIL: inject.js should define shared desktop dashboard spacing tokens" >&2
  exit 1
fi
if ! rg -Fq '.pixel-stack-dashboard-toolbar {' "${inject_js}"; then
  echo "FAIL: inject.js should include the desktop dashboard toolbar spacing class" >&2
  exit 1
fi
if ! rg -Fq 'gap: var(--pixel-stack-dashboard-gap);' "${inject_js}"; then
  echo "FAIL: inject.js should normalize desktop masonry wrapper spacing with gap" >&2
  exit 1
fi
if ! rg -Fq 'gap: var(--pixel-stack-dashboard-section-gap);' "${inject_js}"; then
  echo "FAIL: inject.js should normalize desktop stacked card spacing with gap" >&2
  exit 1
fi
if ! rg -Fq '.pixel-stack-dashboard-later-row {' "${inject_js}"; then
  echo "FAIL: inject.js should include the desktop later-section spacing class" >&2
  exit 1
fi
if ! rg -Fq 'matchMedia(DASHBOARD_DESKTOP_MEDIA_QUERY)' "${inject_js}"; then
  echo "FAIL: inject.js should include the desktop dashboard breakpoint listener" >&2
  exit 1
fi
node - <<'EOF_NODE' "${bootstrap_js}" "${inject_js}"
const fs = require("fs");
const vm = require("vm");

const bootstrapJsPath = process.argv[2];
const injectJsPath = process.argv[3];
const bootstrapJs = fs.readFileSync(bootstrapJsPath, "utf8");
const injectJs = fs.readFileSync(injectJsPath, "utf8");

let documentRef = null;

class Element {
  constructor(tagName, ownerDocument) {
    this.tagName = String(tagName || "div").toUpperCase();
    this.ownerDocument = ownerDocument;
    this.children = [];
    this.parentElement = null;
    this.attributes = new Map();
    this.listeners = new Map();
    this.id = "";
    this.className = "";
    this.dataset = {};
    this._textContent = "";
    this._innerHTML = "";
    this._mockRectWidth = 0;
    this._mockRectHeight = 0;
  }

  get isConnected() {
    let current = this;
    while (current) {
      if (current === this.ownerDocument.body || current === this.ownerDocument.head) {
        return true;
      }
      current = current.parentElement;
    }
    return false;
  }

  set textContent(value) {
    this._textContent = String(value ?? "");
  }

  get textContent() {
    if (this.children.length) {
      return this.children.map((child) => child.textContent).join("");
    }
    return this._textContent;
  }

  set innerHTML(value) {
    this._innerHTML = String(value ?? "");
    this._textContent = this._innerHTML.replace(/<[^>]+>/g, " ").replace(/\s+/g, " ").trim();
    this.children = [];
    if (this._innerHTML.includes("data-pixel-identities-body='1'") || this._innerHTML.includes('data-pixel-identities-body="1"')) {
      const header = new Element("div", this.ownerDocument);
      header.className = "card-header with-border";
      header.setMockRect(64);
      const headerInner = new Element("div", this.ownerDocument);
      headerInner.className = "card-inner";
      const title = new Element("div", this.ownerDocument);
      title.className = "card-title";
      title.textContent = "Top identities";
      headerInner.appendChild(title);
      const subtitle = new Element("div", this.ownerDocument);
      subtitle.className = "card-subtitle";
      subtitle.textContent = "for the last 24 hours";
      headerInner.appendChild(subtitle);
      header.appendChild(headerInner);
      const refreshButton = new Element("button", this.ownerDocument);
      refreshButton.setAttribute("data-pixel-refresh-identities", "1");
      header.appendChild(refreshButton);
      this.appendChild(header);
      const cardTable = new Element("div", this.ownerDocument);
      cardTable.className = "card-table";
      cardTable.setMockRect(148);
      const table = new Element("table", this.ownerDocument);
      cardTable.appendChild(table);
      const tbody = new Element("tbody", this.ownerDocument);
      tbody.setAttribute("data-pixel-identities-body", "1");
      table.appendChild(tbody);
      this.appendChild(cardTable);
    }
    if (this._innerHTML.includes("<select id=\"pixel-stack-querylog-identity\"")) {
      const select = new Element("select", this.ownerDocument);
      select.id = "pixel-stack-querylog-identity";
      this.appendChild(select);
    }
  }

  get innerHTML() {
    return this._innerHTML;
  }

  appendChild(child) {
    if (child.parentElement) {
      const siblings = child.parentElement.children;
      const index = siblings.indexOf(child);
      if (index >= 0) {
        siblings.splice(index, 1);
      }
    }
    child.parentElement = this;
    this.children.push(child);
    return child;
  }

  insertBefore(child, referenceNode) {
    if (!referenceNode || referenceNode.parentElement !== this) {
      return this.appendChild(child);
    }
    if (child.parentElement) {
      const siblings = child.parentElement.children;
      const index = siblings.indexOf(child);
      if (index >= 0) {
        siblings.splice(index, 1);
      }
    }
    const targetIndex = this.children.indexOf(referenceNode);
    child.parentElement = this;
    this.children.splice(targetIndex, 0, child);
    return child;
  }

  remove() {
    if (!this.parentElement) {
      return;
    }
    const siblings = this.parentElement.children;
    const index = siblings.indexOf(this);
    if (index >= 0) {
      siblings.splice(index, 1);
    }
    this.parentElement = null;
  }

  insertAdjacentElement(position, element) {
    if (position !== "afterend" || !this.parentElement) {
      return null;
    }
    if (element.parentElement) {
      const previousSiblings = element.parentElement.children;
      const previousIndex = previousSiblings.indexOf(element);
      if (previousIndex >= 0) {
        previousSiblings.splice(previousIndex, 1);
      }
    }
    const siblings = this.parentElement.children;
    const index = siblings.indexOf(this);
    element.parentElement = this.parentElement;
    siblings.splice(index + 1, 0, element);
    return element;
  }

  setAttribute(name, value) {
    const stringValue = String(value ?? "");
    this.attributes.set(name, stringValue);
    if (name === "id") {
      this.id = stringValue;
    }
    if (name === "class") {
      this.className = stringValue;
    }
  }

  getAttribute(name) {
    if (name === "id") {
      return this.id || null;
    }
    if (name === "class") {
      return this.className || null;
    }
    return this.attributes.has(name) ? this.attributes.get(name) : null;
  }

  setMockRect(height, width = this._mockRectWidth || 0) {
    this._mockRectHeight = Number(height) || 0;
    this._mockRectWidth = Number(width) || 0;
  }

  getBoundingClientRect() {
    const width = this._mockRectWidth || 0;
    if (this._mockRectHeight) {
      return {
        top: 0,
        right: width,
        bottom: this._mockRectHeight,
        left: 0,
        width,
        height: this._mockRectHeight,
      };
    }
    const childHeight = this.children.reduce((total, child) => (
      total + (Number(child.getBoundingClientRect().height) || 0)
    ), 0);
    return {
      top: 0,
      right: width,
      bottom: childHeight,
      left: 0,
      width,
      height: childHeight,
    };
  }

  addEventListener(type, handler) {
    this.listeners.set(type, handler);
  }

  dispatchEvent(type) {
    const handler = this.listeners.get(type);
    if (handler) {
      handler({ target: this });
    }
  }

  matches(selector) {
    if (!selector) {
      return false;
    }
    const tagClassMatch = selector.match(/^([a-z0-9_-]+)((?:\.[a-z0-9_-]+)+)$/i);
    if (tagClassMatch) {
      const classes = tagClassMatch[2].split(".").filter(Boolean);
      return (
        this.tagName.toLowerCase() === tagClassMatch[1].toLowerCase() &&
        classes.every((className) => this.className.split(/\s+/).filter(Boolean).includes(className))
      );
    }
    const multiClassMatch = selector.match(/^((?:\.[a-z0-9_-]+)+)$/i);
    if (multiClassMatch) {
      const classes = multiClassMatch[1].split(".").filter(Boolean);
      return classes.every((className) => this.className.split(/\s+/).filter(Boolean).includes(className));
    }
    if (selector.startsWith("#")) {
      return this.id === selector.slice(1);
    }
    if (selector.startsWith(".")) {
      return this.className.split(/\s+/).filter(Boolean).includes(selector.slice(1));
    }
    const attrMatch = selector.match(/^\[([^=\]]+)=['"]?([^'"\]]+)['"]?\]$/);
    if (attrMatch) {
      return this.getAttribute(attrMatch[1]) === attrMatch[2];
    }
    return this.tagName.toLowerCase() === selector.toLowerCase();
  }

  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }

  querySelectorAll(selector) {
    const results = [];
    const visit = (node) => {
      for (const child of node.children) {
        if (child.matches(selector)) {
          results.push(child);
        }
        visit(child);
      }
    };
    visit(this);
    return results;
  }
}

class Document {
  constructor() {
    this.head = new Element("head", this);
    this.body = new Element("body", this);
    this.listeners = new Map();
  }

  createElement(tagName) {
    return new Element(tagName, this);
  }

  getElementById(id) {
    const visit = (node) => {
      if (node.id === id) {
        return node;
      }
      for (const child of node.children) {
        const match = visit(child);
        if (match) {
          return match;
        }
      }
      return null;
    };
    return visit(this.head) || visit(this.body);
  }

  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }

  querySelectorAll(selector) {
    return [...this.head.querySelectorAll(selector), ...this.body.querySelectorAll(selector)];
  }

  addEventListener(type, handler) {
    this.listeners.set(type, handler);
  }
}

documentRef = new Document();

const fetchLog = [];
let usage24hCallCount = 0;
let usage24hResponseCounts = [7, 9];
let usage24hResponseDelaysMs = [0, 250];
let proxyQuerylogCallCount = 0;
const windowListeners = new Map();
const mediaQueries = new Map();
const noop = () => {};

class FakeResponse {
  constructor(url, payload, status = 200) {
    this.url = String(url || "");
    this.status = status;
    this.ok = status >= 200 && status < 300;
    this._payload = payload;
  }

  async json() {
    return JSON.parse(JSON.stringify(this._payload));
  }

  async text() {
    return JSON.stringify(this._payload);
  }

  clone() {
    return new FakeResponse(this.url, this._payload, this.status);
  }
}

class FakeRequest {
  constructor(url, init = {}) {
    this.url = String(url || "");
    this.method = init.method;
    this.headers = init.headers;
    this.body = init.body;
  }
}

function FakeXMLHttpRequest() {}
FakeXMLHttpRequest.prototype.open = noop;
FakeXMLHttpRequest.prototype.send = noop;
FakeXMLHttpRequest.prototype.addEventListener = noop;

const windowObject = {
  document: documentRef,
  location: {
    hash: "",
    search: "",
    pathname: "/",
    origin: "https://example.test",
  },
  history: {
    state: null,
    replaceState(state, _title, url) {
      this.state = state;
      const parsed = new URL(String(url || ""), windowObject.location.origin);
      windowObject.location.pathname = parsed.pathname;
      windowObject.location.search = parsed.search;
      windowObject.location.hash = parsed.hash;
    },
  },
  setTimeout,
  clearTimeout,
  addEventListener(type, handler) {
    windowListeners.set(type, handler);
  },
  dispatchEvent(event) {
    const eventType = typeof event === "string" ? event : event && event.type;
    const handler = windowListeners.get(eventType);
    if (handler) {
      handler(event);
    }
  },
  matchMedia(query) {
    const key = String(query || "");
    if (!mediaQueries.has(key)) {
      const listeners = new Set();
      const mediaQuery = {
        media: key,
        matches: true,
        addEventListener(type, handler) {
          if (type === "change") {
            listeners.add(handler);
          }
        },
        removeEventListener(type, handler) {
          if (type === "change") {
            listeners.delete(handler);
          }
        },
        addListener(handler) {
          listeners.add(handler);
        },
        removeListener(handler) {
          listeners.delete(handler);
        },
        dispatch(matches) {
          mediaQuery.matches = matches;
          const event = { type: "change", media: key, matches };
          listeners.forEach((handler) => handler(event));
        },
      };
      mediaQueries.set(key, mediaQuery);
    }
    return mediaQueries.get(key);
  },
  fetch: async (url, init) => {
    const resolvedUrl = typeof url === "string" ? url : String(url && url.url ? url.url : url);
    fetchLog.push({ method: String(init?.method || "GET").toUpperCase(), url: resolvedUrl });
    if (resolvedUrl.includes("/pixel-stack/identity/api/v1/usage?identity=all&window=24h")) {
      usage24hCallCount += 1;
      const responseIndex = Math.max(0, usage24hCallCount - 1);
      const requestCount = usage24hResponseCounts[Math.min(responseIndex, usage24hResponseCounts.length - 1)];
      const responseDelayMs = usage24hResponseDelaysMs[Math.min(responseIndex, usage24hResponseDelaysMs.length - 1)] || 0;
      return await new Promise((resolve) => {
        setTimeout(() => {
          resolve(new FakeResponse(resolvedUrl, {
              identities: [{ id: "alpha", requestCount }],
            }));
        }, responseDelayMs);
      });
    }
    if (resolvedUrl.includes("/pixel-stack/identity/api/v1/identities")) {
      return new FakeResponse(resolvedUrl, { identities: [] });
    }
    if (resolvedUrl.includes("/pixel-stack/identity/api/v1/adguard/querylog")) {
      proxyQuerylogCallCount += 1;
      const requestUrl = new URL(resolvedUrl, windowObject.location.origin);
      const requestIdentity = requestUrl.searchParams.get("identity") || "";
      const olderThan = requestUrl.searchParams.get("older_than") || "";
      const responseLabel = proxyQuerylogCallCount === 2 && requestIdentity === "alpha" ? "alpha-refresh" : requestIdentity;
      return await new Promise((resolve) => {
        setTimeout(() => {
          const data = olderThan ? [] : (
            requestIdentity
              ? [
                  {
                    pixelIdentityId: requestIdentity,
                    pixelIdentity: { label: responseLabel },
                    time: "2026-03-07T05:00:00.100000Z",
                  },
                ]
              : [
                  { time: "2026-03-07T05:00:00.100000Z" },
                  { time: "2026-03-07T04:59:57.100000Z" },
                ]
          );
          resolve(new FakeResponse(resolvedUrl, {
            data,
            oldest: data.length ? data[data.length - 1].time : olderThan,
          }));
        }, 250);
      });
    }
    if (resolvedUrl.includes("/control/querylog")) {
      return new FakeResponse(resolvedUrl, {
        data: [
          { question: { name: "unfiltered.example.net" } },
          { question: { name: "still-unfiltered.example.net" } },
        ],
        oldest: "2026-03-07T04:59:57.100000Z",
      });
    }
    return new FakeResponse(resolvedUrl, {});
  },
  XMLHttpRequest: FakeXMLHttpRequest,
  Request: FakeRequest,
  Map,
  URL,
  URLSearchParams,
  CustomEvent: function CustomEvent(type, init) {
    this.type = type;
    this.detail = init && init.detail;
  },
  console,
};

const context = {
  window: windowObject,
  document: documentRef,
  fetch: windowObject.fetch,
  XMLHttpRequest: FakeXMLHttpRequest,
  console,
  Map,
  URL,
  URLSearchParams,
  Request: FakeRequest,
  history: windowObject.history,
  CustomEvent: windowObject.CustomEvent,
  setTimeout,
  clearTimeout,
};
windowObject.window = windowObject;
windowObject.globalThis = windowObject;
context.globalThis = windowObject;

const dashboardRoot = documentRef.createElement("div");
documentRef.body.appendChild(dashboardRoot);
const setDashboardDesktopMatches = (matches) => {
  windowObject.matchMedia("(min-width: 992px)").dispatch(Boolean(matches));
};

const mountDashboardToolbarRow = () => {
  const row = documentRef.createElement("div");
  row.className = "row";
  const title = documentRef.createElement("h1");
  title.className = "page-title";
  title.textContent = "Dashboard";
  row.appendChild(title);
  const refresh = documentRef.createElement("button");
  refresh.className = "btn";
  refresh.textContent = "Refresh statistics";
  row.appendChild(refresh);
  dashboardRoot.appendChild(row);
  return row;
};

const summaryCardSpecs = [
  { variant: "dns", title: "DNS Queries", href: "#logs", value: "15,653", percent: "" },
  { variant: "blocked", title: "Blocked by Filters", href: "#logs?response_status=blocked", value: "3,150", percent: "20.12" },
  { variant: "safebrowsing", title: "Blocked malware/phishing", href: "#logs?response_status=blocked_safebrowsing", value: "0", percent: "0" },
  { variant: "adult", title: "Blocked adult websites", href: "#logs?response_status=blocked_parental", value: "0", percent: "0" },
];

const mountDashboardSummaryRow = () => {
  const row = documentRef.createElement("div");
  row.className = "row";
  summaryCardSpecs.forEach((spec) => {
    const column = documentRef.createElement("div");
    column.className = "col-sm-6 col-lg-3";
    const card = documentRef.createElement("div");
    card.className = "card card--full";
    const wrap = documentRef.createElement("div");
    wrap.className = "card-wrap";
    const body = documentRef.createElement("div");
    body.className = "card-body-stats";
    const value = documentRef.createElement("div");
    value.className = "card-value card-value-stats";
    value.textContent = spec.value;
    const title = documentRef.createElement("div");
    title.className = "card-title-stats";
    const link = documentRef.createElement("a");
    link.setAttribute("href", spec.href);
    link.textContent = spec.title;
    title.appendChild(link);
    body.appendChild(value);
    body.appendChild(title);
    wrap.appendChild(body);
    if (spec.percent) {
      const percent = documentRef.createElement("div");
      percent.className = "card-value card-value-percent";
      percent.textContent = spec.percent;
      wrap.appendChild(percent);
    }
    const chart = documentRef.createElement("div");
    chart.className = "card-chart-bg";
    wrap.appendChild(chart);
    card.appendChild(wrap);
    column.appendChild(card);
    row.appendChild(column);
  });
  dashboardRoot.appendChild(row);
  return row;
};

const dashboardCardSpecs = [
  { key: "general", title: "General statistics", height: 180 },
  { key: "topClients", title: "Top clients", height: 220 },
  { key: "queried", title: "Top queried domains", height: 170 },
  { key: "blocked", title: "Top blocked domains", height: 165 },
  { key: "upstreams", title: "Top upstreams", height: 140 },
  { key: "avg", title: "Average upstream response time", height: 130 },
];

let dashboardCardsRowRef = null;

const createDashboardCard = (spec) => {
  const card = documentRef.createElement("div");
  card.className = "card";
  card.setMockRect(spec.height);
  const title = documentRef.createElement("div");
  title.className = "card-title";
  title.textContent = spec.title;
  card.appendChild(title);
  return card;
};

const createDashboardCardColumn = (spec) => {
  const column = documentRef.createElement("div");
  column.className = "col-lg-6";
  column.appendChild(createDashboardCard(spec));
  return column;
};

const dashboardCardsRow = () => dashboardCardsRowRef;

const mountDashboardCardsRow = ({ includeTopClients = false } = {}) => {
  const row = documentRef.createElement("div");
  row.className = "row row-cards dashboard";
  dashboardCardSpecs.forEach((spec) => {
    if (!includeTopClients && spec.key === "topClients") {
      return;
    }
    row.appendChild(createDashboardCardColumn(spec));
  });
  dashboardRoot.appendChild(row);
  dashboardCardsRowRef = row;
  return row;
};

const mountDashboardLaterSectionRow = () => {
  const row = documentRef.createElement("div");
  row.className = "row";
  const column = documentRef.createElement("div");
  column.className = "col-lg-6";
  const card = documentRef.createElement("div");
  card.className = "card";
  card.setMockRect(150);
  const title = documentRef.createElement("div");
  title.className = "card-title";
  title.textContent = "Later dashboard section";
  card.appendChild(title);
  column.appendChild(card);
  row.appendChild(column);
  dashboardRoot.appendChild(row);
  return row;
};

const mountTopClientsCard = () => {
  const row = dashboardCardsRow() || mountDashboardCardsRow();
  const existing = Array.from(row.querySelectorAll(".card")).find((card) => {
    const title = card.querySelector(".card-title");
    return title && title.textContent === "Top clients";
  });
  if (existing) {
    return existing;
  }
  const topClientsSpec = dashboardCardSpecs.find((spec) => spec.key === "topClients");
  const column = createDashboardCardColumn(topClientsSpec);
  const referenceNode = row.children[1] || null;
  if (referenceNode) {
    row.insertBefore(column, referenceNode);
  } else {
    row.appendChild(column);
  }
  return column.querySelector(".card");
};

const expectDecoratedSummaryCards = (stage) => {
  const summaryRows = documentRef.querySelectorAll(".pixel-stack-summary-row");
  if (summaryRows.length !== 1) {
    fail(`dashboard summary row should be decorated during ${stage}`);
  }
  for (const variant of ["dns", "blocked", "safebrowsing", "adult"]) {
    if (documentRef.querySelectorAll(`.pixel-stack-summary-col--${variant}`).length !== 1) {
      fail(`dashboard summary column ${variant} should be decorated during ${stage}`);
    }
    if (documentRef.querySelectorAll(`.pixel-stack-summary-card--${variant}`).length !== 1) {
      fail(`dashboard summary card ${variant} should be decorated during ${stage}`);
    }
  }
  if (documentRef.querySelectorAll(".pixel-stack-summary-card--compact").length !== 3) {
    fail(`three compact dashboard summary cards should be decorated during ${stage}`);
  }
  if (documentRef.querySelectorAll(".pixel-stack-summary-card--compact-quarter").length !== 2) {
    fail(`two quarter-height dashboard summary cards should be decorated during ${stage}`);
  }
};

const expectDashboardDesktopSpacingClasses = (stage) => {
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-desktop-surface").length !== 1) {
    fail(`dashboard desktop surface class should be applied exactly once during ${stage}`);
  }
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-toolbar").length !== 1) {
    fail(`dashboard toolbar spacing class should be applied exactly once during ${stage}`);
  }
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-summary-section").length !== 1) {
    fail(`dashboard summary spacing class should be applied exactly once during ${stage}`);
  }
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-masonry-section").length !== 1) {
    fail(`dashboard masonry spacing class should be applied exactly once during ${stage}`);
  }
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-later-row").length !== 1) {
    fail(`dashboard later-section spacing class should be applied exactly once during ${stage}`);
  }
};

const expectDashboardDesktopSpacingRemoved = (stage) => {
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-desktop-surface").length !== 0) {
    fail(`dashboard desktop surface class should be removed during ${stage}`);
  }
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-toolbar").length !== 0) {
    fail(`dashboard toolbar spacing class should be removed during ${stage}`);
  }
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-summary-section").length !== 0) {
    fail(`dashboard summary spacing class should be removed during ${stage}`);
  }
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-masonry-section").length !== 0) {
    fail(`dashboard masonry spacing class should be removed during ${stage}`);
  }
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-later-row").length !== 0) {
    fail(`dashboard later-section spacing class should be removed during ${stage}`);
  }
};

const directDashboardTitles = () => (
  Array.from((dashboardCardsRow() && dashboardCardsRow().children) || [])
    .filter((node) => node.querySelector && node.querySelector(".card-title"))
    .map((node) => {
      const title = node.querySelector(".card-title");
      return title ? title.textContent : "";
    })
);

const masonryColumnTitles = () => (
  Array.from(documentRef.querySelectorAll(".pixel-stack-dashboard-masonry-col")).map((column) => (
    Array.from(column.querySelectorAll(".pixel-stack-dashboard-masonry-item")).map((item) => {
      const title = item.querySelector(".card-title");
      return title ? title.textContent : "";
    })
  ))
);

const expectDashboardMasonryLayout = (stage) => {
  const row = dashboardCardsRow();
  if (!row || !row.querySelector(".pixel-stack-dashboard-masonry-columns")) {
    fail(`dashboard masonry wrapper should be mounted during ${stage}`);
  }
  if (!documentRef.querySelectorAll(".pixel-stack-dashboard-masonry-row").length) {
    fail(`dashboard masonry row class should be applied during ${stage}`);
  }
  const columns = documentRef.querySelectorAll(".pixel-stack-dashboard-masonry-col");
  if (columns.length !== 2) {
    fail(`dashboard masonry should render exactly two columns during ${stage}`);
  }
  const items = documentRef.querySelectorAll(".pixel-stack-dashboard-masonry-item");
  if (items.length !== 6) {
    fail(`dashboard masonry should redistribute six dashboard items during ${stage}`);
  }
  const topClientsHost = Array.from(items).find((item) => item.textContent.includes("Top clients"));
  if (!topClientsHost || !topClientsHost.textContent.includes("Top identities")) {
    fail(`Top identities should stay grouped with Top clients during ${stage}`);
  }
  const titles = masonryColumnTitles();
  const expected = [
    ["General statistics", "Top queried domains", "Top blocked domains", "Average upstream response time"],
    ["Top clients", "Top upstreams"],
  ];
  if (JSON.stringify(titles) !== JSON.stringify(expected)) {
    fail(`dashboard masonry should balance cards deterministically during ${stage}; saw ${JSON.stringify(titles)}`);
  }
};

const expectNativeDashboardOrder = (stage) => {
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-masonry-columns").length !== 0) {
    fail(`dashboard masonry wrapper should be removed during ${stage}`);
  }
  if (documentRef.querySelectorAll(".pixel-stack-dashboard-masonry-item").length !== 0) {
    fail(`dashboard masonry item classes should be removed during ${stage}`);
  }
  const titles = directDashboardTitles();
  const expected = [
    "General statistics",
    "Top clients",
    "Top queried domains",
    "Top blocked domains",
    "Top upstreams",
    "Average upstream response time",
  ];
  if (JSON.stringify(titles) !== JSON.stringify(expected)) {
    fail(`dashboard cards should restore native order during ${stage}; saw ${JSON.stringify(titles)}`);
  }
  const topClientsHost = Array.from((dashboardCardsRow() && dashboardCardsRow().children) || []).find((node) => node.textContent.includes("Top clients"));
  if (!topClientsHost || !topClientsHost.textContent.includes("Top identities")) {
    fail(`Top identities should remain grouped with Top clients during ${stage}`);
  }
};

mountDashboardToolbarRow();
mountDashboardSummaryRow();
mountDashboardCardsRow();
mountDashboardLaterSectionRow();

setTimeout(() => {
  mountTopClientsCard();
}, 5000);

vm.createContext(context);
vm.runInContext(bootstrapJs, context);
context.fetch = windowObject.fetch;
vm.runInContext(injectJs, context);

const fail = (message) => {
  console.error(`FAIL: ${message}`);
  process.exit(1);
};

const setLocation = (target) => {
  const parsed = new URL(String(target), windowObject.location.origin);
  windowObject.location.pathname = parsed.pathname;
  windowObject.location.search = parsed.search;
  windowObject.location.hash = parsed.hash;
};

const navigateAnchor = (anchor) => {
  if (!anchor || typeof anchor.getAttribute !== "function") {
    fail("anchor navigation helper requires an element with an href attribute");
  }
  const href = anchor.getAttribute("href");
  if (!href) {
    fail("anchor navigation helper requires a non-empty href");
  }
  const previousPath = `${windowObject.location.pathname}${windowObject.location.search}`;
  const previousHash = windowObject.location.hash;
  setLocation(href);
  if (`${windowObject.location.pathname}${windowObject.location.search}` !== previousPath) {
    windowObject.dispatchEvent({ type: "popstate" });
  }
  if (windowObject.location.hash !== previousHash) {
    windowObject.dispatchEvent({ type: "hashchange" });
  }
};

const mountQuerylogDom = () => {
  const form = documentRef.createElement("form");
  form.className = "form-control--container";
  documentRef.body.appendChild(form);
  const row = documentRef.createElement("div");
  row.setAttribute("data-testid", "querylog_cell");
  const clientCell = documentRef.createElement("div");
  clientCell.className = "logs__cell--client";
  row.appendChild(clientCell);
  documentRef.body.appendChild(row);
  windowObject.__pixelStackAdguardIdentity.lastNativeQuerylogRequestUrl = "https://example.test/control/querylog?search=&response_status=all&older_than=&limit=20";
  return { form, row, clientCell };
};

const clearQuerylogDom = () => {
  documentRef.querySelectorAll("form.form-control--container").forEach((node) => node.remove());
  documentRef.querySelectorAll("[data-testid='querylog_cell']").forEach((node) => node.remove());
};

const resetQuerylogScenarioState = () => {
  fetchLog.length = 0;
  proxyQuerylogCallCount = 0;
  if (windowObject.__pixelStackAdguardIdentity) {
    windowObject.__pixelStackAdguardIdentity.querylogRows = [];
    windowObject.__pixelStackAdguardIdentity.querylogSessionKey = "";
    windowObject.__pixelStackAdguardIdentity.lastQuerylogPayload = null;
  }
};

const simulateNativeQuerylogFetch = async (requestUrl = "https://example.test/control/querylog?search=&response_status=all&older_than=&limit=20") => {
  const response = await windowObject.fetch(requestUrl);
  return await response.json();
};

setTimeout(() => {
  expectDecoratedSummaryCards("initial dashboard sync");
  expectDashboardMasonryLayout("initial dashboard sync");
  expectDashboardDesktopSpacingClasses("initial dashboard sync");
  const dashboardCard = documentRef.getElementById("pixel-stack-top-identities-card");
  const usage24hRequests = fetchLog.filter((entry) => entry.url.includes("window=24h"));
  const identitiesRequests = fetchLog.filter((entry) => entry.url.includes("/api/v1/identities"));
  const usage30dRequests = fetchLog.filter((entry) => entry.url.includes("window=30d"));
  if (!dashboardCard) {
    fail("inject.js should mount dashboard identities card after delayed Top clients hydration");
  }
  if (usage24hRequests.length !== 1) {
    fail(`inject.js should issue exactly one 24h usage request during delayed dashboard mount (saw ${usage24hRequests.length})`);
  }
  if (identitiesRequests.length !== 0) {
    fail(`inject.js should not request identities while mounting delayed dashboard card (saw ${identitiesRequests.length})`);
  }
  if (usage30dRequests.length !== 0) {
    fail(`inject.js should not request 30d usage while mounting delayed dashboard card (saw ${usage30dRequests.length})`);
  }
  setTimeout(() => {
    if (fetchLog.filter((entry) => entry.url.includes("window=24h")).length !== 1) {
      fail("inject.js should not duplicate the 24h usage request after delayed dashboard mount settles");
    }
    const staleDashboardCard = documentRef.getElementById("pixel-stack-top-identities-card");
    if (!staleDashboardCard || !staleDashboardCard.textContent.includes("alpha")) {
      fail("dashboard card should retain rendered identity content before native refresh simulation");
    }
    setDashboardDesktopMatches(false);
    setTimeout(() => {
      expectNativeDashboardOrder("mobile breakpoint restore");
      expectDashboardDesktopSpacingRemoved("mobile breakpoint restore");
      setDashboardDesktopMatches(true);
      setTimeout(() => {
        expectDashboardMasonryLayout("desktop breakpoint reapply");
        expectDashboardDesktopSpacingClasses("desktop breakpoint reapply");
        fetchLog.length = 0;
        dashboardRoot.children = [];
        dashboardCardsRowRef = null;
        setTimeout(() => {
          mountDashboardToolbarRow();
          mountDashboardSummaryRow();
          mountDashboardCardsRow({ includeTopClients: true });
          mountDashboardLaterSectionRow();
          windowObject.dispatchEvent(new windowObject.CustomEvent("pixelstack:native-dashboard-updated"));
          windowObject.dispatchEvent(new windowObject.CustomEvent("pixelstack:native-dashboard-updated"));
          windowObject.dispatchEvent(new windowObject.CustomEvent("pixelstack:native-dashboard-updated"));
          setTimeout(() => {
            expectDecoratedSummaryCards("dashboard refresh recovery");
            expectDashboardMasonryLayout("dashboard refresh recovery");
            expectDashboardDesktopSpacingClasses("dashboard refresh recovery");
            const rebuiltDashboardCard = documentRef.getElementById("pixel-stack-top-identities-card");
            if (!rebuiltDashboardCard) {
              fail("dashboard card should be rebuilt after native dashboard refresh");
            }
            if (!rebuiltDashboardCard.textContent.includes("alpha")) {
              fail("dashboard card should be immediately rehydrated from cached payload after native dashboard refresh");
            }
          }, 50);
          setTimeout(() => {
            const refreshedUsageRequests = fetchLog.filter((entry) => entry.url.includes("window=24h"));
            const rebuiltDashboardCard = documentRef.getElementById("pixel-stack-top-identities-card");
            if (refreshedUsageRequests.length !== 1) {
              fail(`dashboard refresh recovery should issue exactly one background 24h usage request (saw ${refreshedUsageRequests.length})`);
            }
            if (!rebuiltDashboardCard || !rebuiltDashboardCard.textContent.includes("9")) {
              fail("dashboard card should update in place after refreshed dashboard payload arrives");
            }
            void runDashboardInflightRefreshFlow();
          }, 500);
        }, 20);
      }, 50);
    }, 50);
  }, 1000);

  const runDashboardInflightRefreshFlow = async () => {
    fetchLog.length = 0;
    usage24hCallCount = 0;
    usage24hResponseCounts = [11, 13];
    usage24hResponseDelaysMs = [250, 0];
    dashboardRoot.children = [];
    dashboardCardsRowRef = null;
    mountDashboardToolbarRow();
    mountDashboardSummaryRow();
    mountDashboardCardsRow({ includeTopClients: true });
    mountDashboardLaterSectionRow();
    windowObject.dispatchEvent(new windowObject.CustomEvent("pixelstack:native-dashboard-updated"));
    setTimeout(() => {
      windowObject.dispatchEvent(new windowObject.CustomEvent("pixelstack:native-dashboard-updated"));
    }, 20);
    setTimeout(() => {
      const interimDashboardCard = documentRef.getElementById("pixel-stack-top-identities-card");
      if (!interimDashboardCard || !interimDashboardCard.textContent.includes("11")) {
        fail("dashboard card should resolve the in-flight initial usage payload before queued refresh applies");
      }
    }, 320);
    setTimeout(() => {
      const usageRequests = fetchLog.filter((entry) => entry.url.includes("window=24h"));
      const refreshedDashboardCard = documentRef.getElementById("pixel-stack-top-identities-card");
      if (usageRequests.length !== 2) {
        fail(`dashboard refresh queue should replay one deferred 24h usage request after an in-flight refresh (saw ${usageRequests.length})`);
      }
      if (!refreshedDashboardCard || !refreshedDashboardCard.textContent.includes("13")) {
        fail("dashboard card should update after a deferred refresh that was queued during an in-flight request");
      }
      usage24hResponseCounts = [7, 9];
      usage24hResponseDelaysMs = [0, 250];
      void runBootstrapUnfilteredFlow();
    }, 700);
  };

  const runBootstrapUnfilteredFlow = async () => {
    resetQuerylogScenarioState();
    setLocation("/#logs?response_status=all");
    const payload = await simulateNativeQuerylogFetch();
    const directQuerylogRequests = fetchLog.filter((entry) => entry.url.includes("/control/querylog"));
    const proxyQuerylogRequests = fetchLog.filter((entry) => entry.url.includes("/pixel-stack/identity/api/v1/adguard/querylog"));
    if (directQuerylogRequests.length !== 0) {
      fail("native querylog requests on the logs route should be rewritten away from /control/querylog");
    }
    if (proxyQuerylogRequests.length !== 1) {
      fail(`native querylog requests on the logs route should be rewritten to the proxied querylog endpoint (saw ${proxyQuerylogRequests.length})`);
    }
    if (!Array.isArray(payload.data) || payload.data.length !== 2) {
      fail("unfiltered native querylog fetch should preserve the unfiltered row set through the proxy");
    }
    void runBootstrapRewriteFlow();
  };

  const runBootstrapRewriteFlow = async () => {
    resetQuerylogScenarioState();
    setLocation("/#logs?response_status=all&pixel_identity=alpha");
    const payload = await simulateNativeQuerylogFetch();
    const directQuerylogRequests = fetchLog.filter((entry) => entry.url.includes("/control/querylog"));
    const proxyQuerylogRequests = fetchLog.filter((entry) => entry.url.includes("/pixel-stack/identity/api/v1/adguard/querylog"));
    if (directQuerylogRequests.length !== 0) {
      fail("native querylog requests with an identity filter should be rewritten away from /control/querylog");
    }
    if (proxyQuerylogRequests.length !== 1 || !proxyQuerylogRequests[0].url.includes("identity=alpha")) {
      fail("native querylog requests with an identity filter should be rewritten to the proxied querylog endpoint");
    }
    if (!Array.isArray(payload.data) || payload.data.length !== 1 || payload.data[0].pixelIdentityId !== "alpha") {
      fail("rewritten native querylog fetch should receive identity-filtered rows");
    }
    if (windowObject.__pixelStackAdguardIdentity.lastNativeQuerylogRequestUrl !== "https://example.test/control/querylog?search=&response_status=all&older_than=&limit=20") {
      fail("rewritten native querylog fetch should still publish the original querylog request URL");
    }
    runSettingsRouteFlow();
  };

  const runSettingsRouteFlow = () => {
    const pageHeader = documentRef.createElement("div");
    pageHeader.className = "page-header";
    documentRef.body.appendChild(pageHeader);
    setLocation("/#settings");
    windowObject.dispatchEvent({ type: "hashchange" });
    setTimeout(() => {
      const settingsButton = documentRef.getElementById("pixel-stack-doh-identities-btn");
      if (!settingsButton) {
        fail("settings route should mount the DNS identities button");
      }
      if (settingsButton.textContent !== "DNS identities") {
        fail(`settings route button should be renamed to DNS identities (saw ${settingsButton.textContent || "(empty)"})`);
      }
      const href = settingsButton.getAttribute("href") || "";
      if (href !== "/pixel-stack/identity?return=%2F%23settings") {
        fail(`settings route button should encode the return target instead of splitting it into the fragment (saw ${href || "(empty)"})`);
      }
      navigateAnchor(settingsButton);
      if (windowObject.location.pathname !== "/pixel-stack/identity") {
        fail(`settings route button should navigate to the identity page path (saw ${windowObject.location.pathname || "(empty)"})`);
      }
      if (windowObject.location.search !== "?return=%2F%23settings") {
        fail(`settings route button should preserve the encoded return target in search (saw ${windowObject.location.search || "(empty)"})`);
      }
      if (windowObject.location.hash !== "") {
        fail(`settings route button should not leak the settings hash into the identity page fragment (saw ${windowObject.location.hash || "(empty)"})`);
      }
      pageHeader.remove();
      runQuerylogFlow();
    }, 50);
  };

  const runQuerylogFlow = () => {
    resetQuerylogScenarioState();
    clearQuerylogDom();
    const dashboardCard = documentRef.getElementById("pixel-stack-top-identities-card");
    const dashboardLink = dashboardCard && dashboardCard.querySelector("a");
    if (!dashboardLink) {
      fail("dashboard identities card should render an identity link into query logs");
    }
    if (dashboardLink.getAttribute("href") !== "/?pixel_identity=alpha#logs?response_status=all&pixel_identity=alpha") {
      fail(`dashboard identity link should target the logs route with pixel_identity (saw ${dashboardLink.getAttribute("href") || "(empty)"})`);
    }
    navigateAnchor(dashboardLink);
    const { form, row, clientCell } = mountQuerylogDom();
    setTimeout(() => {
      if (clientCell.querySelector(".pixel-stack-identity-chip")) {
        fail("querylog identity chips should not block initial native row render");
      }
    }, 50);
    setTimeout(() => {
      const select = documentRef.getElementById("pixel-stack-querylog-identity");
      const chip = clientCell.querySelector(".pixel-stack-identity-chip");
      const identitiesRequests = fetchLog.filter((entry) => entry.url.includes("/pixel-stack/identity/api/v1/identities"));
      const proxyQuerylogRequests = fetchLog.filter((entry) => entry.url.includes("/pixel-stack/identity/api/v1/adguard/querylog"));
      const usage30dQuerylogRequests = fetchLog.filter((entry) => entry.url.includes("window=30d"));
      if (!select) {
        fail("querylog identity filter should mount independently of native querylog readiness");
      }
      if (!chip || chip.textContent !== "alpha") {
        fail("querylog identity chips should appear after background enrichment completes");
      }
      if (identitiesRequests.length !== 1) {
        fail(`querylog enhancement should issue exactly one identities request (saw ${identitiesRequests.length})`);
      }
      if (proxyQuerylogRequests.length !== 1) {
        fail(`querylog enhancement should issue exactly one proxied querylog request (saw ${proxyQuerylogRequests.length})`);
      }
      if (usage30dQuerylogRequests.length !== 0) {
        fail(`querylog enhancement should not request 30d usage (saw ${usage30dQuerylogRequests.length})`);
      }
      const selectedOption = select.children.find((child) => child.value === "alpha" && child.selected === true);
      if (!selectedOption) {
        fail("querylog identity filter should preserve selected pixel_identity hash value");
      }
      if (windowObject.location.search !== "?pixel_identity=alpha") {
        fail(`querylog identity filter should mirror hash selection into the real URL (saw ${windowObject.location.search || "(empty)"})`);
      }
      fetchLog.length = 0;
      form.remove();
      row.remove();
      const { clientCell: refreshedClientCell } = mountQuerylogDom();
      windowObject.dispatchEvent(new windowObject.CustomEvent("pixelstack:native-querylog-updated", {
        detail: {
          requestUrl: "https://example.test/control/querylog?search=&response_status=all&older_than=&limit=20",
        },
      }));
      setTimeout(() => {
        const refreshedSelect = documentRef.getElementById("pixel-stack-querylog-identity");
        const refreshedChip = refreshedClientCell.querySelector(".pixel-stack-identity-chip");
        const selectedRefreshedOption = refreshedSelect && refreshedSelect.children.find((child) => child.value === "alpha" && child.selected === true);
        if (!refreshedSelect) {
          fail("querylog filter should be rebuilt after native querylog refresh");
        }
        if (!selectedRefreshedOption) {
          fail("querylog refresh should preserve selected identity filter value");
        }
        if (!refreshedChip || refreshedChip.textContent !== "alpha") {
          fail("querylog chips should be restored immediately from cached enrichment after native refresh");
        }
      }, 50);
      setTimeout(() => {
        const proxyQuerylogRefreshRequests = fetchLog.filter((entry) => entry.url.includes("/pixel-stack/identity/api/v1/adguard/querylog"));
        const identitiesRefreshRequests = fetchLog.filter((entry) => entry.url.includes("/pixel-stack/identity/api/v1/identities"));
        const refreshedChip = refreshedClientCell.querySelector(".pixel-stack-identity-chip");
        if (proxyQuerylogRefreshRequests.length !== 1) {
          fail(`querylog refresh recovery should issue exactly one proxied querylog refresh request (saw ${proxyQuerylogRefreshRequests.length})`);
        }
        if (identitiesRefreshRequests.length > 1) {
          fail(`querylog refresh recovery should not storm identities requests (saw ${identitiesRefreshRequests.length})`);
        }
        if (!refreshedChip || refreshedChip.textContent !== "alpha-refresh") {
          fail("querylog chips should update in place after refreshed enrichment arrives");
        }
        runStandaloneUsageLinkFlow();
      }, 900);
    }, 800);
  };

  const runStandaloneUsageLinkFlow = () => {
    resetQuerylogScenarioState();
    clearQuerylogDom();
    setLocation("/pixel-stack/identity?return=%2F%23settings");
    const usageAnchor = documentRef.createElement("a");
    usageAnchor.setAttribute("href", "/?pixel_identity=alpha#logs?response_status=all&pixel_identity=alpha");
    navigateAnchor(usageAnchor);
    const { clientCell } = mountQuerylogDom();
    setTimeout(() => {
      const select = documentRef.getElementById("pixel-stack-querylog-identity");
      const chip = clientCell.querySelector(".pixel-stack-identity-chip");
      const proxyQuerylogRequests = fetchLog.filter((entry) => entry.url.includes("/pixel-stack/identity/api/v1/adguard/querylog"));
      const selectedOption = select && select.children.find((child) => child.value === "alpha" && child.selected === true);
      if (!select || !selectedOption) {
        fail("standalone usage link navigation should preserve the selected identity on first logs render");
      }
      if (windowObject.location.search !== "?pixel_identity=alpha") {
        fail(`standalone usage link should preserve pixel_identity in the real URL (saw ${windowObject.location.search || "(empty)"})`);
      }
      if (windowObject.location.hash !== "#logs?response_status=all&pixel_identity=alpha") {
        fail(`standalone usage link should preserve pixel_identity in the hash route (saw ${windowObject.location.hash || "(empty)"})`);
      }
      if (!chip || chip.textContent !== "alpha") {
        fail("standalone usage link should restore identity chips after the first enrichment pass");
      }
      if (proxyQuerylogRequests.length !== 1 || !proxyQuerylogRequests[0].url.includes("identity=alpha")) {
        fail("standalone usage link should trigger the proxied first querylog request with identity=alpha");
      }
      fetchLog.length = 0;
      clearQuerylogDom();
      runQuerylogUrlFlow();
    }, 800);
  };

  const runQuerylogUrlFlow = () => {
    resetQuerylogScenarioState();
    clearQuerylogDom();
    setLocation("/?pixel_identity=alpha#logs?response_status=all");
    const { form, row, clientCell } = mountQuerylogDom();
    windowObject.dispatchEvent({ type: "hashchange" });
    setTimeout(() => {
      const select = documentRef.getElementById("pixel-stack-querylog-identity");
      const chip = clientCell.querySelector(".pixel-stack-identity-chip");
      const proxyQuerylogRequests = fetchLog.filter((entry) => entry.url.includes("/pixel-stack/identity/api/v1/adguard/querylog"));
      if (!select) {
        fail("querylog identity filter should mount when identity only exists in the top-level URL query");
      }
      const selectedOption = select.children.find((child) => child.value === "alpha" && child.selected === true);
      if (!selectedOption) {
        fail("querylog identity filter should restore selection from the top-level URL query");
      }
      if (!chip || chip.textContent !== "alpha") {
        fail("querylog identity chips should use the top-level URL query fallback");
      }
      if (windowObject.location.hash !== "#logs?response_status=all") {
        fail(`querylog top-level URL fallback should not rewrite the hash on load (saw ${windowObject.location.hash || "(empty)"})`);
      }
      if (proxyQuerylogRequests.length !== 1 || !proxyQuerylogRequests[0].url.includes("identity=alpha")) {
        fail("querylog top-level URL fallback should send the resolved identity to the proxied querylog API");
      }
      fetchLog.length = 0;
      form.remove();
      row.remove();
      runQuerylogHashPrecedenceFlow();
    }, 800);
  };

  const runQuerylogHashPrecedenceFlow = () => {
    resetQuerylogScenarioState();
    clearQuerylogDom();
    setLocation("/?pixel_identity=beta#logs?response_status=all&pixel_identity=alpha");
    const { form, row, clientCell } = mountQuerylogDom();
    windowObject.dispatchEvent({ type: "hashchange" });
    setTimeout(() => {
      const select = documentRef.getElementById("pixel-stack-querylog-identity");
      const chip = clientCell.querySelector(".pixel-stack-identity-chip");
      const selectedOption = select && select.children.find((child) => child.value === "alpha" && child.selected === true);
      if (!select || !selectedOption) {
        fail("querylog identity resolution should prefer the hash value over the top-level URL query");
      }
      if (!chip || chip.textContent !== "alpha") {
        fail("querylog identity chips should prefer the hash value over the top-level URL query");
      }
      if (windowObject.location.search !== "?pixel_identity=alpha") {
        fail(`querylog hash precedence should sync the real URL to the hash-selected identity (saw ${windowObject.location.search || "(empty)"})`);
      }
      select.value = "";
      select.dispatchEvent("change");
      if (windowObject.location.hash !== "#logs?response_status=all") {
        fail(`clearing the querylog identity filter should remove pixel_identity from the hash (saw ${windowObject.location.hash || "(empty)"})`);
      }
      if (windowObject.location.search !== "") {
        fail(`clearing the querylog identity filter should remove pixel_identity from the real URL (saw ${windowObject.location.search || "(empty)"})`);
      }
      fetchLog.length = 0;
      form.remove();
      row.remove();
      runQuerylogUrlRefreshFlow();
    }, 800);
  };

  const runQuerylogUrlRefreshFlow = () => {
    resetQuerylogScenarioState();
    clearQuerylogDom();
    setLocation("/?pixel_identity=alpha#logs?response_status=all");
    mountQuerylogDom();
    windowObject.dispatchEvent({ type: "hashchange" });
    setTimeout(() => {
      fetchLog.length = 0;
      clearQuerylogDom();
      const { clientCell } = mountQuerylogDom();
      windowObject.dispatchEvent(new windowObject.CustomEvent("pixelstack:native-querylog-updated", {
        detail: {
          requestUrl: "https://example.test/control/querylog?search=&response_status=all&older_than=&limit=20",
        },
      }));
      setTimeout(() => {
        const select = documentRef.getElementById("pixel-stack-querylog-identity");
        const chip = clientCell.querySelector(".pixel-stack-identity-chip");
        const selectedOption = select && select.children.find((child) => child.value === "alpha" && child.selected === true);
        if (!select || !selectedOption) {
          fail("querylog refresh should preserve resolved identity when it came from the top-level URL query");
        }
        if (!chip || chip.textContent !== "alpha") {
          fail("querylog refresh should immediately restore chips from the top-level URL query identity");
        }
      }, 50);
      setTimeout(() => {
        const proxyQuerylogRefreshRequests = fetchLog.filter((entry) => entry.url.includes("/pixel-stack/identity/api/v1/adguard/querylog"));
        if (proxyQuerylogRefreshRequests.length !== 1 || !proxyQuerylogRefreshRequests[0].url.includes("identity=alpha")) {
          fail("querylog refresh should preserve the top-level URL query identity in the proxied refresh request");
        }
        process.exit(0);
      }, 900);
    }, 800);
  };
}, 7000);
EOF_NODE
if [[ "$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/pixel-stack/identity")" != "200" ]]; then
  echo "FAIL: identity web page endpoint did not return 200" >&2
  exit 1
fi
identity_html="${tmpdir}/identity.html"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity" > "${identity_html}"
if ! rg -Fq 'url.searchParams.set("pixel_identity", normalizedIdentityId);' "${identity_html}"; then
  echo "FAIL: standalone identity page should mirror pixel_identity into the real URL query when building logs links" >&2
  exit 1
fi
if ! rg -Fq 'url.hash = `#logs?${hashParams.toString()}`;' "${identity_html}"; then
  echo "FAIL: standalone identity page should preserve pixel_identity in the logs hash route when building links" >&2
  exit 1
fi

list_json="${tmpdir}/list-initial.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities" > "${list_json}"
if [[ "$(jq -r '.identities | length' "${list_json}")" != "0" ]]; then
  echo "FAIL: initial identities should be empty" >&2
  exit 1
fi

querylog_default_json="${tmpdir}/querylog-default.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/querylog/summary" > "${querylog_default_json}"
if [[ "$(jq -r '.querylog_view_mode' "${querylog_default_json}")" != "user_only" ]]; then
  echo "FAIL: querylog summary default view mode should be user_only" >&2
  exit 1
fi
if [[ "$(jq -r '.querylog_status' "${querylog_default_json}")" != "ok" ]]; then
  echo "FAIL: querylog summary default status should be ok" >&2
  exit 1
fi
if [[ "$(jq -r '.top_clients' "${querylog_default_json}")" == *"127.0.0.1:"* ]]; then
  echo "FAIL: default querylog top_clients should exclude loopback internal entries" >&2
  exit 1
fi
if [[ "$(jq -r '.top_clients' "${querylog_default_json}")" == *"::1:"* ]]; then
  echo "FAIL: default querylog top_clients should exclude IPv6 loopback internal entries" >&2
  exit 1
fi
if [[ "$(jq -r '.internal_total_count' "${querylog_default_json}")" != "5" ]]; then
  echo "FAIL: querylog summary internal_total_count mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.internal_doh_count' "${querylog_default_json}")" != "2" ]]; then
  echo "FAIL: querylog summary internal_doh_count mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.internal_probe_domain_counts' "${querylog_default_json}")" != "example.com:3" ]]; then
  echo "FAIL: querylog summary internal_probe_domain_counts mismatch" >&2
  exit 1
fi

querylog_all_json="${tmpdir}/querylog-all.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/querylog/summary?view=all&limit=5000" > "${querylog_all_json}"
if [[ "$(jq -r '.querylog_view_mode' "${querylog_all_json}")" != "all" ]]; then
  echo "FAIL: querylog summary view mode should be all when requested" >&2
  exit 1
fi
if [[ "$(jq -r '.top_clients' "${querylog_all_json}")" != *"127.0.0.1:doh:1"* ]]; then
  echo "FAIL: all-view querylog top_clients should include loopback internal DoH entries" >&2
  exit 1
fi
if [[ "$(jq -r '.total_doh_count' "${querylog_default_json}")" != "4" ]]; then
  echo "FAIL: querylog summary user_only total_doh_count mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.total_doh_count' "${querylog_all_json}")" != "6" ]]; then
  echo "FAIL: querylog summary all-view total_doh_count mismatch" >&2
  exit 1
fi

create_alpha_json="${tmpdir}/create-alpha.json"
curl -fsS \
  -H "Origin: http://127.0.0.1:${port}" \
  -H "X-Forwarded-Proto: http" \
  -H 'Content-Type: application/json' \
  -X POST \
  -d '{"id":"alpha"}' \
  "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities" > "${create_alpha_json}"
alpha_token="$(jq -r '.token' "${create_alpha_json}")"
alpha_dot_label="$(jq -r '.dotLabel' "${create_alpha_json}")"
alpha_dot_hostname="$(jq -r '.dotHostname' "${create_alpha_json}")"
if [[ ! "${alpha_token}" =~ ^[A-Za-z0-9._~-]{16,128}$ ]]; then
  echo "FAIL: create did not return valid generated token" >&2
  exit 1
fi
if [[ ! "${alpha_dot_label}" =~ ^[a-z0-9]{20}$ ]]; then
  echo "FAIL: create should return a 20-char DoT label when DoT identities are enabled" >&2
  exit 1
fi
if [[ "${alpha_dot_hostname}" != "${alpha_dot_label}.dns.jolkins.id.lv" ]]; then
  echo "FAIL: create should return the derived DoT hostname" >&2
  exit 1
fi
if [[ "$(jq -r '.applied' "${create_alpha_json}")" != "true" ]]; then
  echo "FAIL: create should report applied=true when runtime reload is scheduled" >&2
  exit 1
fi
if [[ "$(jq -r '.expiresEpochSeconds' "${create_alpha_json}")" != "null" ]]; then
  echo "FAIL: default create should return expiresEpochSeconds=null" >&2
  exit 1
fi

list_after_create="${tmpdir}/list-after-create.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities" > "${list_after_create}"
if [[ "$(jq -r '.primaryIdentityId' "${list_after_create}")" != "alpha" ]]; then
  echo "FAIL: primaryIdentityId should be alpha after first create" >&2
  exit 1
fi
if [[ "$(jq -r '.dotIdentityEnabled' "${list_after_create}")" != "true" ]]; then
  echo "FAIL: list endpoint should expose dotIdentityEnabled=true" >&2
  exit 1
fi
if [[ "$(jq -r '.dotHostnameBase' "${list_after_create}")" != "dns.jolkins.id.lv" ]]; then
  echo "FAIL: list endpoint should expose dotHostnameBase" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[0].token' "${list_after_create}")" != "${alpha_token}" ]]; then
  echo "FAIL: list endpoint token mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[0].tokenMasked' "${list_after_create}")" == "${alpha_token}" ]]; then
  echo "FAIL: tokenMasked should not expose full token" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[0].dotLabel' "${list_after_create}")" != "${alpha_dot_label}" ]]; then
  echo "FAIL: list endpoint dotLabel mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[0].dotHostname' "${list_after_create}")" != "${alpha_dot_hostname}" ]]; then
  echo "FAIL: list endpoint dotHostname mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[0].expiresEpochSeconds' "${list_after_create}")" != "null" ]]; then
  echo "FAIL: list endpoint should expose expiresEpochSeconds=null for default no-expiry identities" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[0].isExpired' "${list_after_create}")" != "false" ]]; then
  echo "FAIL: list endpoint should expose isExpired=false for active no-expiry identities" >&2
  exit 1
fi

duplicate_body="${tmpdir}/duplicate-body.json"
duplicate_code="$(curl -sS -o "${duplicate_body}" -w '%{http_code}' \
  -H "Origin: http://127.0.0.1:${port}" \
  -H "X-Forwarded-Proto: http" \
  -H 'Content-Type: application/json' \
  -X POST \
  -d '{"id":"alpha"}' \
  "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities")"
if [[ "${duplicate_code}" != "400" ]]; then
  echo "FAIL: duplicate create should return 400" >&2
  exit 1
fi
if ! jq -r '.error' "${duplicate_body}" | rg -Fq 'Identity already exists'; then
  echo "FAIL: duplicate create error message mismatch" >&2
  exit 1
fi

create_beta_json="${tmpdir}/create-beta.json"
curl -fsS \
  -H "Origin: http://127.0.0.1:${port}" \
  -H "X-Forwarded-Proto: http" \
  -H 'Content-Type: application/json' \
  -X POST \
  -d '{"id":"beta"}' \
  "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities" > "${create_beta_json}"
beta_token="$(jq -r '.token' "${create_beta_json}")"
beta_dot_label="$(jq -r '.dotLabel' "${create_beta_json}")"
if [[ ! "${beta_token}" =~ ^[A-Za-z0-9._~-]{16,128}$ ]]; then
  echo "FAIL: create beta did not return valid generated token" >&2
  exit 1
fi
if [[ ! "${beta_dot_label}" =~ ^[a-z0-9]{20}$ ]]; then
  echo "FAIL: create beta should return a 20-char DoT label when DoT identities are enabled" >&2
  exit 1
fi
python3 - <<'PY' "${ADGUARDHOME_DOH_IDENTITY_WEB_QUERYLOG_JSON_FILE}" "${beta_dot_label}"
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
path.write_text(
    path.read_text().replace("__BETA_DOT_LABEL__", sys.argv[2]),
    encoding="utf-8",
)
PY

python3 - <<'PY' "${ADGUARDHOME_DOT_ACCESS_LOG_FILE}" "${querylog_dot_beta_time}" "${beta_dot_label}"
from datetime import datetime, timedelta, timezone
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
row_time = datetime.fromisoformat(sys.argv[2].replace("Z", "+00:00")).astimezone(timezone.utc)
end_time = row_time + timedelta(seconds=1)
hostname = f"{sys.argv[3]}.dns.jolkins.id.lv"
path.write_text(
    f"{end_time.isoformat().replace('+00:00', 'Z')}\t{hostname}\t200\t2.000\t62.205.193.194\t{end_time.timestamp():.3f}\n",
    encoding="utf-8",
)
PY

iso_now="$(date -u +%Y-%m-%dT%H:%M:%S+00:00)"
cat > "${ADGUARDHOME_DOH_ACCESS_LOG_FILE}" <<EOF_LOG
${querylog_public_time}	/${alpha_token}/dns-query?dns=a	200	0.010	212.3.197.32	$(python3 - <<'PY'
from datetime import datetime, timezone
print(f"{datetime(2026, 3, 7, 5, 0, 0, 100000, tzinfo=timezone.utc).timestamp():.3f}")
PY
)
${querylog_service_time}	/${beta_token}/dns-query?dns=service	200	0.040	212.3.197.32	$(python3 - <<'PY'
from datetime import datetime, timezone
print(f"{datetime(2026, 3, 7, 5, 0, 0, 600000, tzinfo=timezone.utc).timestamp():.3f}")
PY
)
${iso_now}	/${alpha_token}/dns-query?dns=b	404	0.020
${iso_now}	/dns-query?dns=bare	404	0.030
EOF_LOG

usage_json="${tmpdir}/usage.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/usage?identity=all&window=7d" > "${usage_json}"
if [[ "$(jq -r '.totalRequests' "${usage_json}")" != "4" ]]; then
  echo "FAIL: usage totalRequests mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.totalRequestCount' "${usage_json}")" != "5" ]]; then
  echo "FAIL: usage totalRequestCount should include DoT querylog rows" >&2
  exit 1
fi
if [[ "$(jq -r '.dotTotalRequests' "${usage_json}")" != "1" ]]; then
  echo "FAIL: usage dotTotalRequests should count matched DoT rows" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[] | select(.id == "alpha") | .requestCount' "${usage_json}")" != "2" ]]; then
  echo "FAIL: usage alpha requestCount mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[] | select(.id == "beta") | .requestCount' "${usage_json}")" != "2" ]]; then
  echo "FAIL: usage beta requestCount should include DoT traffic" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[] | select(.id == "beta") | .dotRequestCount' "${usage_json}")" != "1" ]]; then
  echo "FAIL: usage beta dotRequestCount mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[] | select(.id == "__bare__") | .requestCount' "${usage_json}")" != "1" ]]; then
  echo "FAIL: usage __bare__ requestCount mismatch" >&2
  exit 1
fi

proxy_querylog_json="${tmpdir}/proxy-querylog.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/adguard/querylog?search=&response_status=all&older_than=&limit=20" > "${proxy_querylog_json}"
if [[ "$(count_invocations 'events --window 24h ')" != "1" ]]; then
  echo "FAIL: proxied querylog should narrow identity event lookups to a 24h window for recent rows" >&2
  exit 1
fi
if [[ "$(count_invocations 'events --window 30d ')" != "0" ]]; then
  echo "FAIL: proxied querylog should not use the 30d identity event window for recent rows" >&2
  exit 1
fi
if [[ "$(jq -r '.data[] | select(.question.name == "public.example.net") | .client_info.whois.orgname' "${proxy_querylog_json}")" != "Operator Example" ]]; then
  echo "FAIL: proxy querylog should fill missing public-IP orgname from cache" >&2
  exit 1
fi
if [[ "$(jq -r '.data[] | select(.question.name == "public.example.net") | .pixelIdentityId' "${proxy_querylog_json}")" != "alpha" ]]; then
  echo "FAIL: proxy querylog should correlate DoH rows back to identity events" >&2
  exit 1
fi
if [[ "$(jq -r '.data[] | select(.question.name == "service.example.net") | .pixelIdentityId' "${proxy_querylog_json}")" != "beta" ]]; then
  echo "FAIL: proxy querylog should keep non-phone/service traffic outside the alpha identity" >&2
  exit 1
fi
if [[ "$(jq -r '.data[] | select(.question.name == "beta-dot.example.net") | .pixelIdentityId' "${proxy_querylog_json}")" != "beta" ]]; then
  echo "FAIL: proxy querylog should map DoT rows directly from AdGuard client metadata" >&2
  exit 1
fi
if [[ "$(jq -r '.data[] | select(.question.name == "beta-dot.example.net") | .client' "${proxy_querylog_json}")" != "62.205.193.194" ]]; then
  echo "FAIL: proxy querylog should recover the origin client IP for DoT rows from the stream access log" >&2
  exit 1
fi
if [[ "$(jq -r '.data[] | select(.question.name == "self.example.net") | .client' "${proxy_querylog_json}")" != "192.168.31.25" ]]; then
  echo "FAIL: proxy querylog should remap non-probe loopback rows to device LAN IP" >&2
  exit 1
fi
if [[ "$(jq -r '.data[] | select(.question.name == "example.com" and .client_proto == "plain") | .client' "${proxy_querylog_json}")" != "127.0.0.1" ]]; then
  echo "FAIL: proxy querylog should keep probe loopback rows as localhost" >&2
  exit 1
fi

proxy_querylog_alpha_json="${tmpdir}/proxy-querylog-alpha.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/adguard/querylog?search=&response_status=all&older_than=&limit=20&identity=alpha" > "${proxy_querylog_alpha_json}"
if [[ "$(jq -r '.data | length' "${proxy_querylog_alpha_json}")" != "1" ]]; then
  echo "FAIL: identity-filtered proxy querylog should return only matching rows" >&2
  exit 1
fi
if [[ "$(jq -r '.data[0].question.name' "${proxy_querylog_alpha_json}")" != "public.example.net" ]]; then
  echo "FAIL: identity-filtered proxy querylog should keep the matching query row" >&2
  exit 1
fi
if jq -e '.data[] | select(.question.name == "service.example.net")' "${proxy_querylog_alpha_json}" >/dev/null 2>&1; then
  echo "FAIL: identity-filtered proxy querylog should exclude non-alpha rows" >&2
  exit 1
fi
if [[ "$(jq -r '.oldest' "${proxy_querylog_alpha_json}")" != "$(jq -r '.data[-1].time' "${proxy_querylog_alpha_json}")" ]]; then
  echo "FAIL: identity-filtered proxy querylog should expose native-compatible oldest cursor" >&2
  exit 1
fi
alpha_oldest="$(jq -r '.oldest' "${proxy_querylog_alpha_json}")"
alpha_oldest_encoded="$(python3 - <<'PY' "${alpha_oldest}"
import sys
import urllib.parse
print(urllib.parse.quote(sys.argv[1], safe=""))
PY
)"
proxy_querylog_alpha_next_json="${tmpdir}/proxy-querylog-alpha-next.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/adguard/querylog?search=&response_status=all&older_than=${alpha_oldest_encoded}&limit=20&identity=alpha" > "${proxy_querylog_alpha_next_json}"
if jq -e '.data[] | select(.question.name == "public.example.net")' "${proxy_querylog_alpha_next_json}" >/dev/null 2>&1; then
  echo "FAIL: identity-filtered proxy querylog should not repeat rows after paging with oldest" >&2
  exit 1
fi

proxy_querylog_beta_json="${tmpdir}/proxy-querylog-beta.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/adguard/querylog?search=&response_status=all&older_than=&limit=20&identity=beta" > "${proxy_querylog_beta_json}"
if [[ "$(jq -r '.data | length' "${proxy_querylog_beta_json}")" != "2" ]]; then
  echo "FAIL: identity-filtered proxy querylog should return only beta rows" >&2
  exit 1
fi
if ! jq -e '.data[] | select(.question.name == "service.example.net")' "${proxy_querylog_beta_json}" >/dev/null 2>&1; then
  echo "FAIL: identity-filtered proxy querylog should keep the DoH beta row" >&2
  exit 1
fi
if ! jq -e '.data[] | select(.question.name == "beta-dot.example.net")' "${proxy_querylog_beta_json}" >/dev/null 2>&1; then
  echo "FAIL: identity-filtered proxy querylog should keep the DoT beta row" >&2
  exit 1
fi
if [[ "$(jq -r '.data[] | select(.question.name == "beta-dot.example.net") | .pixelIdentityId' "${proxy_querylog_beta_json}")" != "beta" ]]; then
  echo "FAIL: beta identity-filtered proxy querylog should preserve direct DoT identity metadata" >&2
  exit 1
fi
if jq -e '.data[] | select(.question.name == "public.example.net")' "${proxy_querylog_beta_json}" >/dev/null 2>&1; then
  echo "FAIL: beta identity-filtered proxy querylog should exclude alpha rows" >&2
  exit 1
fi

proxy_stats_json="${tmpdir}/proxy-stats.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/adguard/stats" > "${proxy_stats_json}"
if [[ "$(jq -r '.top_clients | map(keys[]) | join(",")' "${proxy_stats_json}")" != *"192.168.31.25"* ]]; then
  echo "FAIL: proxy stats should expose remapped device LAN IP in top_clients" >&2
  exit 1
fi
if [[ "$(jq -r '.top_clients | map(keys[]) | join(",")' "${proxy_stats_json}")" != *"62.205.193.194"* ]]; then
  echo "FAIL: proxy stats should expose recovered DoT origin client IP in top_clients" >&2
  exit 1
fi
if [[ "$(jq -r '.top_clients | map(keys[]) | join(",")' "${proxy_stats_json}")" == *"127.0.0.1"* ]]; then
  echo "FAIL: proxy stats should exclude IPv4 loopback in top_clients" >&2
  exit 1
fi
if [[ "$(jq -r '.top_clients | map(keys[]) | join(",")' "${proxy_stats_json}")" == *"::1"* ]]; then
  echo "FAIL: proxy stats should exclude IPv6 loopback in top_clients" >&2
  exit 1
fi

revoke_beta_json="${tmpdir}/revoke-beta.json"
curl -fsS \
  -H "Origin: http://127.0.0.1:${port}" \
  -H "X-Forwarded-Proto: http" \
  -X DELETE \
  "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities/beta" > "${revoke_beta_json}"
if [[ "$(jq -r '.revoked' "${revoke_beta_json}")" != "beta" ]]; then
  echo "FAIL: revoke(beta) should report revoked=beta" >&2
  exit 1
fi
if [[ "$(jq -r '.remaining' "${revoke_beta_json}")" != "1" ]]; then
  echo "FAIL: revoke(beta) should leave alpha as the sole identity" >&2
  exit 1
fi

proxy_clients_json="${tmpdir}/proxy-clients.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/adguard/clients" > "${proxy_clients_json}"
if [[ "$(jq -r '.auto_clients[] | select(.ip == "212.3.197.32") | .whois_info.orgname' "${proxy_clients_json}")" != "Operator Example" ]]; then
  echo "FAIL: proxy clients should enrich public auto_clients whois metadata" >&2
  exit 1
fi

proxy_client_search_json="${tmpdir}/proxy-client-search.json"
curl -fsS \
  -H 'Content-Type: application/json' \
  -X POST \
  -d '{"clients":[{"id":"212.3.197.32"},{"id":"192.168.31.25"}]}' \
  "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/adguard/clients/search" > "${proxy_client_search_json}"
if [[ "$(jq -r '.[0]["212.3.197.32"].whois_info.orgname' "${proxy_client_search_json}")" != "Operator Example" ]]; then
  echo "FAIL: proxy clients/search should enrich missing public-IP whois metadata" >&2
  exit 1
fi
if [[ "$(jq -r '.[1]["192.168.31.25"].ids[0]' "${proxy_client_search_json}")" != "192.168.31.25" ]]; then
  echo "FAIL: proxy clients/search should synthesize missing LAN client entries" >&2
  exit 1
fi

sleep 3
: > "${identityctl_invocation_log}"

run_parallel_get "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities"
if [[ "$(count_invocations 'list ')" != "1" ]]; then
  echo "FAIL: concurrent identities GETs should collapse to one identityctl list invocation" >&2
  exit 1
fi

run_parallel_get "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/usage?identity=all&window=7d"
if [[ "$(count_invocations 'usage ')" != "1" ]]; then
  echo "FAIL: concurrent usage GETs should collapse to one identityctl usage invocation" >&2
  exit 1
fi

run_parallel_get "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/adguard/querylog?search=&response_status=all&older_than=&limit=20"
if [[ "$(count_invocations 'events ')" != "1" ]]; then
  echo "FAIL: concurrent proxied querylog GETs should collapse to one identityctl events invocation" >&2
  exit 1
fi

sleep 3
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities" >/dev/null
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/usage?identity=all&window=7d" >/dev/null
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/adguard/querylog?search=&response_status=all&older_than=&limit=20" >/dev/null
if [[ "$(count_invocations 'list ')" != "2" ]]; then
  echo "FAIL: identities GET cache should expire after burst TTL" >&2
  exit 1
fi
if [[ "$(count_invocations 'usage ')" != "2" ]]; then
  echo "FAIL: usage GET cache should expire after burst TTL" >&2
  exit 1
fi
if [[ "$(count_invocations 'events ')" != "2" ]]; then
  echo "FAIL: proxied querylog GET cache should expire after burst TTL" >&2
  exit 1
fi

revoke_last_body="${tmpdir}/revoke-last-body.json"
revoke_last_code="$(curl -sS -o "${revoke_last_body}" -w '%{http_code}' \
  -H "Origin: http://127.0.0.1:${port}" \
  -H "X-Forwarded-Proto: http" \
  -X DELETE \
  "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities/alpha")"
if [[ "${revoke_last_code}" != "400" ]]; then
  echo "FAIL: revoking last identity should return 400" >&2
  exit 1
fi
if ! jq -r '.error' "${revoke_last_body}" | rg -Fq 'Refusing to revoke the last identity'; then
  echo "FAIL: last-identity revoke error message mismatch" >&2
  exit 1
fi

future_expiry="$(( $(date +%s) + 7200 ))"
create_beta_json="${tmpdir}/create-beta.json"
curl -fsS \
  -H "Origin: http://127.0.0.1:${port}" \
  -H "X-Forwarded-Proto: http" \
  -H 'Content-Type: application/json' \
  -X POST \
  -d "{\"id\":\"beta\",\"expiresEpochSeconds\":${future_expiry}}" \
  "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities" > "${create_beta_json}"
if [[ "$(jq -r '.applied' "${create_beta_json}")" != "true" ]]; then
  echo "FAIL: create(beta) should report applied=true when runtime reload is scheduled" >&2
  exit 1
fi
if [[ "$(jq -r '.expiresEpochSeconds' "${create_beta_json}")" != "${future_expiry}" ]]; then
  echo "FAIL: create with expiresEpochSeconds should echo persisted expiry value" >&2
  exit 1
fi

create_gamma_json="${tmpdir}/create-gamma.json"
curl -fsS \
  -H "Origin: http://127.0.0.1:${port}" \
  -H "X-Forwarded-Proto: http" \
  -H 'Content-Type: application/json' \
  -X POST \
  -d '{"id":"gamma","expiresEpochSeconds":null}' \
  "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities" > "${create_gamma_json}"
if [[ "$(jq -r '.expiresEpochSeconds' "${create_gamma_json}")" != "null" ]]; then
  echo "FAIL: create with explicit null expiry should persist as no-expiry" >&2
  exit 1
fi

list_after_expiry_creates="${tmpdir}/list-after-expiry-creates.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities" > "${list_after_expiry_creates}"
if [[ "$(jq -r '.identities[] | select(.id == "beta") | .expiresEpochSeconds' "${list_after_expiry_creates}")" != "${future_expiry}" ]]; then
  echo "FAIL: list endpoint beta expiresEpochSeconds mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[] | select(.id == "beta") | .isExpired' "${list_after_expiry_creates}")" != "false" ]]; then
  echo "FAIL: beta should not be marked expired immediately after creation" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[] | select(.id == "gamma") | .expiresEpochSeconds' "${list_after_expiry_creates}")" != "null" ]]; then
  echo "FAIL: gamma should expose null expiresEpochSeconds in list response" >&2
  exit 1
fi

past_expiry_body="${tmpdir}/past-expiry-body.json"
past_expiry_code="$(curl -sS -o "${past_expiry_body}" -w '%{http_code}' \
  -H "Origin: http://127.0.0.1:${port}" \
  -H "X-Forwarded-Proto: http" \
  -H 'Content-Type: application/json' \
  -X POST \
  -d "{\"id\":\"stale\",\"expiresEpochSeconds\":$(( $(date +%s) - 10 ))}" \
  "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities")"
if [[ "${past_expiry_code}" != "400" ]]; then
  echo "FAIL: create with past expiresEpochSeconds should return 400" >&2
  exit 1
fi
if ! jq -r '.error' "${past_expiry_body}" | rg -Fq 'must be in the future'; then
  echo "FAIL: create with past expiry should return validation error" >&2
  exit 1
fi

revoke_alpha_json="${tmpdir}/revoke-alpha.json"
curl -fsS \
  -H "Origin: http://127.0.0.1:${port}" \
  -H "X-Forwarded-Proto: http" \
  -X DELETE \
  "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities/alpha" > "${revoke_alpha_json}"
if [[ "$(jq -r '.revoked' "${revoke_alpha_json}")" != "alpha" ]]; then
  echo "FAIL: revoke response revoked id mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.remaining' "${revoke_alpha_json}")" != "2" ]]; then
  echo "FAIL: revoke remaining count mismatch" >&2
  exit 1
fi
if [[ "$(jq -r '.applied' "${revoke_alpha_json}")" != "true" ]]; then
  echo "FAIL: revoke should report applied=true when runtime reload is scheduled" >&2
  exit 1
fi

revoke_gamma_json="${tmpdir}/revoke-gamma.json"
curl -fsS \
  -H "Origin: http://127.0.0.1:${port}" \
  -H "X-Forwarded-Proto: http" \
  -X DELETE \
  "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities/gamma" > "${revoke_gamma_json}"
if [[ "$(jq -r '.revoked' "${revoke_gamma_json}")" != "gamma" ]]; then
  echo "FAIL: revoke(gamma) response revoked id mismatch" >&2
  exit 1
fi

final_list_json="${tmpdir}/final-list.json"
curl -fsS "http://127.0.0.1:${port}/pixel-stack/identity/api/v1/identities" > "${final_list_json}"
if [[ "$(jq -r '.identities | length' "${final_list_json}")" != "1" ]]; then
  echo "FAIL: final identity count should be 1" >&2
  exit 1
fi
if [[ "$(jq -r '.identities[0].id' "${final_list_json}")" != "beta" ]]; then
  echo "FAIL: remaining identity should be beta" >&2
  exit 1
fi

sleep 2
reload_count="$(wc -l < "${reload_log}" | tr -d '[:space:]')"
if [[ -z "${reload_count}" || "${reload_count}" -lt 3 ]]; then
  echo "FAIL: expected runtime reload entrypoint to be invoked for create/revoke operations" >&2
  exit 1
fi

echo "PASS: DoH identity web sidecar list/create/revoke/usage contracts are correct"
