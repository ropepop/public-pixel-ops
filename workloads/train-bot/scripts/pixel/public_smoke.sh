#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "$SCRIPT_DIR/common.sh"

ensure_output_dirs

for cmd in curl npx python3; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    log "Missing required command: $cmd"
    exit 1
  fi
done

PWCLI="${PWCLI:-$HOME/.codex/skills/playwright/scripts/playwright_cli.sh}"
if [[ ! -x "$PWCLI" ]]; then
  log "Playwright wrapper not found: $PWCLI"
  exit 1
fi

if [[ -f "$REPO_ROOT/.env" ]]; then
  set -a
  # shellcheck source=/dev/null
  . "$REPO_ROOT/.env"
  set +a
fi

public_base_url="${TRAIN_WEB_PUBLIC_BASE_URL:-https://train-bot.example.com}"
out_dir="${PLAYWRIGHT_SMOKE_OUT_DIR:-$REPO_ROOT/output/playwright/pixel-public-smoke}"

train_id="$(
  python3 - "${public_base_url}" <<'PY'
import json
import sys
import time
import urllib.parse
import urllib.request
import urllib.error

base_url = sys.argv[1].rstrip("/")
headers = {"User-Agent": "Mozilla/5.0 (pixel-public-smoke)"}

def fetch_json(url: str):
    last_error = None
    for _ in range(10):
        req = urllib.request.Request(url, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=20) as response:
                return json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            last_error = exc
            if exc.code not in (502, 503, 504, 520, 522, 524, 530):
                raise
        except Exception as exc:
            last_error = exc
        time.sleep(2)
    raise last_error

dashboard = fetch_json(f"{base_url}/api/v1/public/dashboard")
for item in dashboard.get("trains", []):
    train = item.get("train") or {}
    train_id = (train.get("id") or "").strip()
    if not train_id:
        continue
    stops_url = f"{base_url}/api/v1/public/trains/{urllib.parse.quote(train_id, safe='')}/stops"
    stops_payload = fetch_json(stops_url)
    stops = stops_payload.get("stops") or []
    if any(stop.get("latitude") is not None and stop.get("longitude") is not None for stop in stops):
        print(train_id)
        raise SystemExit(0)
raise SystemExit("no public train with mapped stops found")
PY
)"

station_probe="$(
  python3 - "${public_base_url}" <<'PY'
import json
import sys
import time
import urllib.parse
import urllib.request
import urllib.error

base_url = sys.argv[1].rstrip("/")
headers = {"User-Agent": "Mozilla/5.0 (pixel-public-smoke)"}
fold_table = str.maketrans({
    "ā": "a",
    "č": "c",
    "ē": "e",
    "ģ": "g",
    "ī": "i",
    "ķ": "k",
    "ļ": "l",
    "ņ": "n",
    "š": "s",
    "ū": "u",
    "ž": "z",
})

def normalize(value: str) -> str:
    return " ".join(value.strip().lower().translate(fold_table).replace("-", " ").split())

def fetch_json(url: str):
    last_error = None
    for _ in range(10):
        req = urllib.request.Request(url, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=20) as response:
                return json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            last_error = exc
            if exc.code not in (502, 503, 504, 520, 522, 524, 530):
                raise
        except Exception as exc:
            last_error = exc
        time.sleep(2)
    raise last_error

stations = fetch_json(f"{base_url}/api/v1/public/stations").get("stations", [])
target = None
for station in stations:
    station_id = (station.get("id") or "").strip()
    station_name = (station.get("name") or "").strip()
    normalized_name = normalize(station_name)
    if not station_id or not normalized_name:
        continue
    raw_name = " ".join(station_name.strip().lower().replace("-", " ").split())
    if normalized_name != raw_name:
        target = {
            "id": station_id,
            "name": station_name,
            "query": normalized_name,
        }
        break

if target is None:
    raise SystemExit("no accent-bearing public station found for plain-latin search verification")

query_url = f"{base_url}/api/v1/public/stations?q={urllib.parse.quote(target['query'], safe='')}"
matches = fetch_json(query_url).get("stations", [])
if not any((station.get("id") or "").strip() == target["id"] for station in matches):
    raise SystemExit(
        f"plain-latin station query {target['query']!r} did not return {target['name']!r} ({target['id']})"
    )

print(f"{target['id']}\t{target['name']}\t{target['query']}")
PY
)"

IFS=$'\t' read -r station_probe_id station_probe_name station_probe_query <<<"$station_probe"
log "Verified plain-latin public station search: query=${station_probe_query} station=${station_probe_name} (${station_probe_id})"

mkdir -p "$out_dir"
rm -rf "$out_dir/.playwright-cli"
rm -f "$out_dir/public-smoke-console.log" "$out_dir/public-smoke-network.log"

export PLAYWRIGHT_CLI_SESSION="ttb-public-smoke"

pushd "$out_dir" >/dev/null

run_pw() {
  local output
  output="$("$PWCLI" "$@" 2>&1)"
  RUN_PW_LAST_OUTPUT="$output"
  printf '%s\n' "$output"
  if printf '%s' "$output" | grep -q '^### Error'; then
    return 1
  fi
}

output_has() {
  local pattern="$1"
  printf '%s' "$RUN_PW_LAST_OUTPUT" | grep -Eq "$pattern"
}

js_has_stops_map_cta="$(cat <<'JS'
async (page) => {
  const want = /Stops map|Pieturu karte/i;
  for (let i = 0; i < 20; i++) {
    const links = page.locator('a, button').filter({ hasText: want });
    const count = await links.count();
    if (count > 0) {
      return `cta:${count}`;
    }
    await page.waitForTimeout(500);
  }
  return 'cta:0';
}
JS
)"

js_has_public_network_map_button="$(cat <<'JS'
async (page) => {
  const findCount = async () => page.evaluate(() => {
    const want = /^(Map|Karte)$/i;
    return Array.from(document.querySelectorAll('a, button')).filter((node) => {
      const text = (node.textContent || '').trim();
      if (!want.test(text)) {
        return false;
      }
      if (node.tagName !== 'A') {
        return false;
      }
      const href = node.getAttribute('href') || '';
      if (!href) {
        return false;
      }
      const path = new URL(href, window.location.href).pathname;
      return /\/map$/.test(path) && !/\/t\/.+\/map$/.test(path);
    }).length;
  });

  for (let i = 0; i < 20; i++) {
    const count = await findCount();
    if (count > 0) {
      return `mapbutton:${count}`;
    }
    await page.waitForTimeout(500);
  }
  return `mapbutton:${await findCount()}`;
}
JS
)"

js_station_root_has_no_inline_map="$(cat <<'JS'
async (page) => JSON.stringify(await page.evaluate(() => ({
  legacyPanelCount: document.querySelectorAll('#public-stations-map-panel').length,
  standalonePanelCount: document.querySelectorAll('#public-network-map-panel').length,
  inlineMapCount: document.querySelectorAll('.train-map').length,
})));
JS
)"

js_public_network_map_ready="$(cat <<'JS'
async (page) => {
  for (let i = 0; i < 24; i++) {
    const mapCount = await page.locator('.train-map').count();
    const sightingsCount = await page.locator('#public-network-map-sightings-card').count();
    if (mapCount > 0 && sightingsCount > 0) {
      return `map=${mapCount};sightings=${sightingsCount}`;
    }
    await page.waitForTimeout(500);
  }
  return `map=${await page.locator('.train-map').count()};sightings=${await page.locator('#public-network-map-sightings-card').count()}`;
}
JS
)"

js_public_map_ready="$(cat <<'JS'
async (page) => {
  for (let i = 0; i < 24; i++) {
    const mapCount = await page.locator('.train-map').count();
    const stopCount = await page.locator('.stop-row').count();
    if (mapCount > 0 && stopCount > 0) {
      return `map=${mapCount};stops=${stopCount}`;
    }
    await page.waitForTimeout(500);
  }
  return `map=${await page.locator('.train-map').count()};stops=${await page.locator('.stop-row').count()}`;
}
JS
)"

run_pw open "${public_base_url%/}"
run_pw run-code "$js_has_public_network_map_button"
if ! output_has 'mapbutton:[1-9]'; then
  log "Public smoke failed: station search view did not render a top-bar Map button"
  exit 1
fi
run_pw run-code "$js_station_root_has_no_inline_map"
if ! output_has '"legacyPanelCount":0' || ! output_has '"standalonePanelCount":0' || ! output_has '"inlineMapCount":0'; then
  log "Public smoke failed: station search view still renders an inline map"
  exit 1
fi

run_pw open "${public_base_url%/}/departures"
run_pw run-code "$js_has_public_network_map_button"
if ! output_has 'mapbutton:[1-9]'; then
  log "Public smoke failed: departures view did not render a top-bar Map button"
  exit 1
fi

run_pw open "${public_base_url%/}/t/${train_id}"
run_pw run-code "$js_has_stops_map_cta"
if ! output_has 'cta:[1-9]'; then
  log "Public smoke failed: train detail view did not render a Stops map CTA"
  exit 1
fi

run_pw open "${public_base_url%/}/map"
run_pw run-code "$js_public_network_map_ready"
if ! output_has 'map=[1-9][0-9]*' || ! output_has 'sightings=[1-9][0-9]*'; then
  log "Public smoke failed: standalone network map page did not render both the map container and sightings card"
  exit 1
fi

run_pw open "${public_base_url%/}/t/${train_id}/map"
run_pw run-code "$js_public_map_ready"
if ! output_has 'map=[1-9][0-9]*' || ! output_has 'stops=[1-9][0-9]*'; then
  log "Public smoke failed: map page did not render both the map container and stop rows"
  exit 1
fi

run_pw snapshot
run_pw screenshot
run_pw console warning > public-smoke-console.log || true
run_pw network > public-smoke-network.log || true
run_pw close >/dev/null 2>&1 || true

popd >/dev/null

log "Public smoke completed for train ${train_id}; artifacts in $out_dir"
