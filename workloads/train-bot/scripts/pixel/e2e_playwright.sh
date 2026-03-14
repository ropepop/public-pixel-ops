#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "$SCRIPT_DIR/common.sh"

log "Using playwright skill for Telegram Web smoke flow"

if ! command -v npx >/dev/null 2>&1; then
  log "npx is required for playwright-cli wrapper"
  exit 1
fi

PWCLI="${PWCLI:-$HOME/.codex/skills/playwright/scripts/playwright_cli.sh}"
if [[ ! -x "$PWCLI" ]]; then
  log "Playwright wrapper not found: $PWCLI"
  exit 1
fi

ensure_output_dirs
out_dir="${PLAYWRIGHT_SMOKE_OUT_DIR:-$REPO_ROOT/output/playwright/pixel-bot-smoke}"
mkdir -p "$out_dir"
rm -rf "$out_dir/.playwright-cli"
rm -f "$out_dir/smoke-console.log" "$out_dir/smoke-network.log" "$out_dir/e2e-evidence.txt"

export PLAYWRIGHT_CLI_SESSION="ttb"
profile_dir="${PLAYWRIGHT_PROFILE_DIR:-$HOME/.cache/playwright-cli/telegram-web}"
if [[ ! -d "$profile_dir" ]]; then
  log "Playwright profile dir not found: $profile_dir"
  log "Log in to Telegram Web once with this profile before running smoke."
  exit 1
fi
if [[ -z "$(find "$profile_dir" -mindepth 1 -print -quit 2>/dev/null)" ]]; then
  log "Playwright profile dir is empty: $profile_dir"
  log "Log in to Telegram Web once with this profile before running smoke."
  exit 1
fi

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

chat_url="${PLAYWRIGHT_CHAT_URL:-https://web.telegram.org/a/#8792187636}"
mobile_view="${PLAYWRIGHT_MOBILE_VIEW:-0}"
mobile_cfg="$out_dir/playwright-cli.mobile.json"

if [[ "$mobile_view" == "1" ]]; then
  cat >"$mobile_cfg" <<'JSON'
{
  "browser": {
    "contextOptions": {
      "viewport": {
        "width": 390,
        "height": 844
      }
    }
  }
}
JSON
  log "Playwright smoke running in mobile view (390x844)"
fi

open_chat_browser() {
  if [[ "$mobile_view" == "1" ]]; then
    run_pw open "$chat_url" --headed --profile "$profile_dir" --config "$mobile_cfg"
    run_pw resize 390 844
    return
  fi
  run_pw open "$chat_url" --headed --profile "$profile_dir"
}

# Playwright persistent contexts fail when Chrome is already using the profile.
if pgrep -f "Google Chrome.*${profile_dir}" >/dev/null 2>&1; then
  pkill -f "Google Chrome.*${profile_dir}" || true
  sleep 1
fi

open_chat_browser
run_pw snapshot

output_has() {
  local pattern="$1"
  printf '%s' "$RUN_PW_LAST_OUTPUT" | grep -Eq "$pattern"
}

js_open_bot_chat="$(cat <<'JS'
async (page) => {
  const botId = '8792187636';
  const botHandle = '@vivi_kontrole_bot';
  if ((page.url() || '').includes(`#${botId}`)) {
    return 'bot-chat-already';
  }
  const href = page.locator(`a[href="#${botId}"]`).first();
  if (await href.count() && await href.isVisible().catch(() => false)) {
    await href.click({ force: true }).catch(() => {});
    await page.waitForTimeout(500);
    if ((page.url() || '').includes(`#${botId}`)) {
      return 'bot-chat-by-href';
    }
  }
  const byName = page.locator('a, div, span').filter({ hasText: /Report Bot/i }).first();
  if (await byName.count() && await byName.isVisible().catch(() => false)) {
    await byName.click({ force: true }).catch(() => {});
    await page.waitForTimeout(500);
    if ((page.url() || '').includes(`#${botId}`)) {
      return 'bot-chat-by-name';
    }
  }
  const search = page.locator(
    'input[type="text"], input[placeholder*="Search"], [contenteditable="true"][data-placeholder*="Search"]'
  ).first();
  if (await search.count() && await search.isVisible().catch(() => false)) {
    await search.click({ force: true }).catch(() => {});
    await search.fill(botHandle).catch(() => {});
    await page.waitForTimeout(350);
    await search.press('Enter').catch(() => {});
    await page.waitForTimeout(600);
    if ((page.url() || '').includes(`#${botId}`)) {
      return 'bot-chat-by-search';
    }
  }
  await page.goto(`https://web.telegram.org/a/#${botId}`, { waitUntil: 'domcontentloaded' }).catch(() => {});
  await page.waitForTimeout(650);
  return (page.url() || '').includes(`#${botId}`) ? 'bot-chat-by-goto' : `bot-chat-missing:${page.url() || ''}`;
}
JS
)"

js_keyboard_state="$(cat <<'JS'
async (page) => {
  const count = async (label) => page.locator('button', { hasText: label }).count();
  const visible = async (label) => {
    const b = page.locator('button', { hasText: label }).first();
    return (await b.count()) && await b.isVisible().catch(() => false);
  };
  const checkinCount = (await count('🚆 Check in')) + (await count('🚆 Piesakies'));
  const reportCount = (await count('📣 Report')) + (await count('📣 Ziņot'));
  const checkinVisible = (await visible('🚆 Check in')) || (await visible('🚆 Piesakies'));
  const reportVisible = (await visible('📣 Report')) || (await visible('📣 Ziņot'));
  const showKeyboard = page.getByRole('button', { name: /Show bot keyboard|Parādīt bota tastatūru/i }).last();
  const hideKeyboard = page.getByRole('button', { name: /Hide bot keyboard|Paslēpt bota tastatūru/i }).last();
  const showVisible = (await showKeyboard.count()) && await showKeyboard.isVisible().catch(() => false);
  const hideVisible = (await hideKeyboard.count()) && await hideKeyboard.isVisible().catch(() => false);
  return JSON.stringify({ checkinCount, reportCount, checkinVisible, reportVisible, showVisible, hideVisible });
}
JS
)"

js_toggle_keyboard="$(cat <<'JS'
async (page) => {
  const labels = ['Show bot keyboard', 'Hide bot keyboard', 'Parādīt bota tastatūru', 'Paslēpt bota tastatūru'];
  for (const label of labels) {
    const b = page.getByRole('button', { name: label }).last();
    if (await b.count() && await b.isVisible().catch(() => false)) {
      await b.hover({ force: true }).catch(() => {});
      await page.waitForTimeout(150);
      await b.click({ force: true }).catch(() => {});
      await page.waitForTimeout(450);
      return `toggled:${label}`;
    }
  }
  return 'toggle-missing';
}
JS
)"

js_start_and_agree="$(cat <<'JS'
async (page) => {
  const actions = [];
  const clickIfVisible = async (locator, label) => {
    if (await locator.count() && await locator.isVisible().catch(() => false)) {
      await locator.hover({ force: true }).catch(() => {});
      await page.waitForTimeout(100);
      await locator.click({ force: true }).catch(() => {});
      await page.waitForTimeout(350);
      actions.push(label);
    }
  };
  await clickIfVisible(page.getByRole('button', { name: /^(Start|START|Sākt)$/i }).last(), 'start');
  await clickIfVisible(page.getByRole('button', { name: /^(START|Start)$/i }).last(), 'command-start');
  await clickIfVisible(page.getByRole('button', { name: /\/start/i }).last(), '/start');
  await clickIfVisible(page.getByRole('button', { name: /Agree|Piekrītu/i }).last(), 'agree');
  return actions.join(',') || 'none';
}
JS
)"

js_click_checkin="$(cat <<'JS'
async (page) => {
  const labels = ['🚆 Check in', '🚆 Piesakies'];
  for (const label of labels) {
    const b = page.locator('button', { hasText: label }).last();
    if (await b.count() && await b.isVisible().catch(() => false)) {
      await b.click({ force: true }).catch(() => {});
      await page.waitForTimeout(500);
      return `clicked:${label}`;
    }
  }
  return 'checkin-missing';
}
JS
)"

js_check_checkin_entry="$(cat <<'JS'
async (page) => {
  const visible = async (label) => {
    const b = page.locator('button', { hasText: label }).last();
    return (await b.count()) && await b.isVisible().catch(() => false);
  };
  const textVisible = async (pattern) => {
    const node = page.locator('div, span, p').filter({ hasText: pattern }).last();
    return (await node.count()) && await node.isVisible().catch(() => false);
  };
  const stationOptionVisible = (await visible('Type station name')) || (await visible('Ieraksti stacijas nosaukumu'));
  const timeVisible = (await visible('Choose by time')) || (await visible('Izvēlēties pēc laika'));
  const stationPromptVisible = (await textVisible(/Send the first few letters of your boarding station\./i))
    || (await textVisible(/Nosūti savas iekāpšanas stacijas pirmos burtus\./i));
  return `stationOptionVisible=${stationOptionVisible ? 1 : 0};timeVisible=${timeVisible ? 1 : 0};stationPromptVisible=${stationPromptVisible ? 1 : 0}`;
}
JS
)"

js_click_by_time="$(cat <<'JS'
async (page) => {
  const labels = ['Choose by time', 'Izvēlēties pēc laika'];
  for (const label of labels) {
    const b = page.locator('button', { hasText: label }).last();
    if (await b.count() && await b.isVisible().catch(() => false)) {
      await b.click({ force: true }).catch(() => {});
      await page.waitForTimeout(450);
      return `clicked:${label}`;
    }
  }
  return 'bytime-missing';
}
JS
)"

js_click_time_window="$(cat <<'JS'
async (page) => {
  const labels = ['Now', 'Next hour', 'Later today', 'Tagad', 'Nākamā stunda', 'Vēlāk šodien'];
  for (const label of labels) {
    const b = page.locator('button', { hasText: label }).last();
    if (await b.count() && await b.isVisible().catch(() => false)) {
      await b.click({ force: true }).catch(() => {});
      await page.waitForTimeout(400);
      return `clicked:${label}`;
    }
  }
  return 'time-window-missing';
}
JS
)"

run_flow_once() {
  local i
  local menu_ready=0
  local report_ready=0
  local entry_ready=0

  for i in 1 2 3; do
    run_pw run-code "$js_open_bot_chat"
    if output_has 'bot-chat-(already|by-href|by-name|by-search|by-goto)'; then
      break
    fi
    run_pw snapshot
  done
  if ! output_has 'bot-chat-(already|by-href|by-name|by-search|by-goto)'; then
    log "E2E failed: Telegram did not navigate to bot chat"
    return 1
  fi

  for i in 1 2 3 4 5; do
    run_pw run-code "$js_keyboard_state"
    if output_has 'checkinCount[^0-9]*[1-9]'; then
      menu_ready=1
    fi
    if output_has 'reportCount[^0-9]*[1-9]'; then
      report_ready=1
    fi
    run_pw run-code "$js_check_checkin_entry"
    if output_has 'timeVisible=1' && ( output_has 'stationOptionVisible=1' || output_has 'stationPromptVisible=1' ); then
      entry_ready=1
    fi
    if [[ "$report_ready" == "1" && ( "$menu_ready" == "1" || "$entry_ready" == "1" ) ]]; then
      break
    fi
    run_pw run-code "$js_toggle_keyboard" || true
    run_pw run-code "$js_start_and_agree" || true
    run_pw run-code "$js_open_bot_chat" || true
    run_pw snapshot
  done

  if [[ "$menu_ready" != "1" && "$entry_ready" != "1" ]]; then
    log "E2E failed: Main bot controls or active check-in entry were not visible after recovery"
    return 1
  fi
  if [[ "$report_ready" != "1" ]]; then
    log "E2E failed: Report command missing in bot keyboard"
    return 1
  fi

  if [[ "$entry_ready" != "1" ]]; then
    run_pw run-code "$js_click_checkin"
    if ! output_has 'clicked:🚆 Check in|clicked:🚆 Piesakies'; then
      run_pw run-code "$js_toggle_keyboard" || true
      run_pw run-code "$js_click_checkin" || true
    fi
    if ! output_has 'clicked:🚆 Check in|clicked:🚆 Piesakies'; then
      log "E2E failed: Check-in button not clickable"
      return 1
    fi

    run_pw run-code "$js_check_checkin_entry"
  fi

  if ! output_has 'timeVisible=1'; then
    log "E2E failed: guided check-in flow missing time entry"
    return 1
  fi
  if ! output_has 'stationOptionVisible=1|stationPromptVisible=1'; then
    log "E2E failed: guided check-in flow missing station entry or active station prompt"
    return 1
  fi

  run_pw run-code "$js_click_by_time"
  if ! output_has 'clicked:Choose by time|clicked:Izvēlēties pēc laika'; then
    log "E2E failed: By-time command not clickable"
    return 1
  fi

  run_pw run-code "$js_click_time_window" || true
  run_pw snapshot

  return 0
}

attempt=1
max_attempts=3
while true; do
  if run_flow_once; then
    break
  fi
  if (( attempt >= max_attempts )); then
    popd >/dev/null
    exit 1
  fi
  log "Playwright smoke attempt ${attempt}/${max_attempts} failed; restarting browser session"
  run_pw close >/dev/null 2>&1 || true
  open_chat_browser
  run_pw snapshot
  attempt=$((attempt + 1))
done

run_pw snapshot
run_pw screenshot
run_pw console warning > smoke-console.log || true
run_pw network > smoke-network.log || true
run_pw close >/dev/null 2>&1 || true

latest_snapshot="$(ls -1t .playwright-cli/page-*.yml 2>/dev/null | head -n 1 || true)"
all_snapshots=(.playwright-cli/page-*.yml)
if [[ -z "$latest_snapshot" ]]; then
  log "E2E failed: no Playwright snapshot generated"
  popd >/dev/null
  exit 1
fi

latest_console="$(ls -1t .playwright-cli/console-*.log 2>/dev/null | head -n 1 || true)"
if [[ -z "$latest_console" ]]; then
  log "E2E failed: no Playwright console log generated"
  popd >/dev/null
  exit 1
fi

{
  echo "snapshot=$latest_snapshot"
  echo "console=$latest_console"
  echo "required_markers=ok(runtime_verified)"
} > e2e-evidence.txt

popd >/dev/null

log "Playwright smoke completed; artifacts in $out_dir"
