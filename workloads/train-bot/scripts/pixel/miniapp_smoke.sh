#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "$SCRIPT_DIR/common.sh"

ensure_output_dirs

for cmd in npx python3; do
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

profile_dir="${PLAYWRIGHT_PROFILE_DIR:-$HOME/.cache/playwright-cli/telegram-web}"
if [[ ! -d "$profile_dir" || -z "$(find "$profile_dir" -mindepth 1 -print -quit 2>/dev/null)" ]]; then
  log "Playwright profile dir missing or empty: $profile_dir"
  exit 1
fi

out_dir="${PLAYWRIGHT_SMOKE_OUT_DIR:-$REPO_ROOT/output/playwright/pixel-miniapp-smoke}"
mkdir -p "$out_dir"
rm -rf "$out_dir/.playwright-cli"
rm -f "$out_dir/miniapp-smoke-console.log" "$out_dir/miniapp-smoke-network.log"

export PLAYWRIGHT_CLI_SESSION="ttb-miniapp-smoke"

pushd "$out_dir" >/dev/null

run_pw() {
  local output
  output="$("$PWCLI" "$@" 2>&1)"
  RUN_PW_LAST_OUTPUT="$output"
  RUN_PW_LAST_RESULT="$(printf '%s\n' "$output" | awk '
    /^### Result$/ { capture = 1; next }
    capture && /^### / { exit }
    capture { print }
  ')"
  printf '%s\n' "$output"
  if printf '%s' "$output" | grep -q '^### Error'; then
    return 1
  fi
}

output_has() {
  local pattern="$1"
  local haystack="${RUN_PW_LAST_RESULT:-$RUN_PW_LAST_OUTPUT}"
  printf '%s' "$haystack" | grep -Eq "$pattern"
}

chat_url="${PLAYWRIGHT_CHAT_URL:-https://web.telegram.org/a/#8792187636}"

open_chat_browser() {
  run_pw open "$chat_url" --headed --profile "$profile_dir"
}

close_chat_browser() {
  run_pw close >/dev/null 2>&1 || true
}

output_value() {
  local key="$1"
  local haystack="${RUN_PW_LAST_RESULT:-$RUN_PW_LAST_OUTPUT}"
  printf '%s\n' "$haystack" | sed -n "s/.*${key}=\\([^;[:space:]]*\\).*/\\1/p" | tail -n 1
}

log_direct_ride_debug() {
  local action_kind action_train action_station observed_train observed_station fetch_status
  action_kind="$(output_value 'directRideActionKind')"
  action_train="$(output_value 'directRideActionTrainId')"
  action_station="$(output_value 'directRideActionStationId')"
  observed_train="$(output_value 'directRideObservedTrainId')"
  observed_station="$(output_value 'directRideObservedStationId')"
  fetch_status="$(output_value 'directRideCurrentRideFetchStatus')"
  log "Mini app smoke direct-action debug: kind=${action_kind:-missing} selectedTrain=${action_train:-missing} selectedStation=${action_station:-missing} observedTrain=${observed_train:-missing} observedStation=${observed_station:-missing} currentRideFetchStatus=${fetch_status:-missing}"
}

if pgrep -f "Google Chrome.*${profile_dir}" >/dev/null 2>&1; then
  pkill -f "Google Chrome.*${profile_dir}" || true
  sleep 1
fi

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
  const byName = page.locator('a, div, span').filter({ hasText: /Vivi kontrole bot|Report Bot/i }).first();
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
  await page.waitForTimeout(900);
  return (page.url() || '').includes(`#${botId}`) ? 'bot-chat-by-goto' : `bot-chat-missing:${page.url() || ''}`;
}
JS
)"

js_open_miniapp_frame="$(cat <<'JS'
async (page) => {
  const sleep = (ms) => page.waitForTimeout(ms);
  const findAppFrame = () =>
    page.frames().find((frame) => /train-bot\.jolkins\.id\.lv/.test(frame.url() || "") || /tgWebAppData=/.test(frame.url() || ""));

  const clickOpenApp = async () => {
    const exact = page.locator('button, a, div[role="button"]').filter({
      hasText: /^📱\s*(Atvērt lietotni|Open app)$/i,
    }).last();
    if (await exact.count() && await exact.isVisible().catch(() => false)) {
      await exact.click({ force: true }).catch(() => {});
      return 'launcher-exact';
    }

    const fallback = page.locator('button, a, div[role="button"]').filter({
      hasText: /Atvērt lietotni|Open app|Atvērt mini lietotni|Mini App/i,
    }).last();
    if (await fallback.count() && await fallback.isVisible().catch(() => false)) {
      await fallback.click({ force: true }).catch(() => {});
      return 'launcher-fallback';
    }
    return 'launcher-missing';
  };

  let frame = findAppFrame();
  if (frame) {
    return `frameReady=1;frameMode=existing;frameUrl=${encodeURIComponent(frame.url() || '')}`;
  }

  let lastLauncher = 'launcher-missing';
  for (let attempt = 0; attempt < 8; attempt++) {
    lastLauncher = await clickOpenApp();
    await sleep(1200);
    frame = findAppFrame();
    if (frame) {
      return `frameReady=1;frameMode=opened;frameLauncher=${lastLauncher};frameUrl=${encodeURIComponent(frame.url() || '')}`;
    }
    await sleep(800);
  }
  return `frameReady=0;frameLauncher=${lastLauncher}`;
}
JS
)"

js_open_miniapp_and_verify="$(cat <<'JS'
async (page) => {
  const sleep = (ms) => page.waitForTimeout(ms);
  const findAppFrame = () =>
    page.frames().find((frame) => /train-bot\.jolkins\.id\.lv/.test(frame.url() || "") || /tgWebAppData=/.test(frame.url() || ""));

  let frame = findAppFrame();
  if (!frame) {
    return "frameReady=0";
  }

  const waitForVisible = async (frameRef, selector, loops = 24) => {
    for (let i = 0; i < loops; i++) {
      if (await frameRef.locator(selector).count()) {
        return true;
      }
      await sleep(500);
    }
    return false;
  };

  const waitForCondition = async (fn, loops = 24, delayMs = 500) => {
    for (let i = 0; i < loops; i++) {
      if (await fn()) {
        return true;
      }
      await sleep(delayMs);
    }
    return false;
  };

  const mapTab = frame.locator('button').filter({ hasText: /^Map$|^Karte$/i }).first();
  const dashboardTab = frame.locator('button').filter({ hasText: /^Dashboard$|^Panelis$/i }).first();
  const settingsTab = frame.locator('button').filter({ hasText: /^Settings$|^Iestatījumi$/i }).first();
  let mapTabVisible = false;
  for (let i = 0; i < 20; i++) {
    if (await mapTab.count() && await mapTab.isVisible().catch(() => false)) {
      mapTabVisible = true;
      break;
    }
    await sleep(500);
  }
  if (!mapTabVisible) {
    return "frameReady=1;mapTabVisible=0";
  }

  if (await dashboardTab.count() && await dashboardTab.isVisible().catch(() => false)) {
    await dashboardTab.click({ force: true }).catch(() => {});
    await sleep(600);
  }

  const waitForVisibleLocator = async (locator, loops = 20) => {
    for (let i = 0; i < loops; i++) {
      if (await locator.count() && await locator.isVisible().catch(() => false)) {
        return true;
      }
      await sleep(500);
    }
    return false;
  };

  const waitForAnyVisibleAction = async (action, loops = 24) => {
    const selector = `[data-action='${action}']`;
    for (let i = 0; i < loops; i++) {
      const locator = frame.locator(selector);
      const count = await locator.count();
      for (let index = 0; index < count; index++) {
        const candidate = locator.nth(index);
        if (await candidate.isVisible().catch(() => false)) {
          return true;
        }
      }
      await sleep(500);
    }
    return false;
  };

  const countVisible = async (locator) => {
    const count = await locator.count();
    let visibleCount = 0;
    for (let i = 0; i < count; i++) {
      const candidate = locator.nth(i);
      if (await candidate.isVisible().catch(() => false)) {
        visibleCount++;
      }
    }
    return visibleCount;
  };

  const visibleLocatorAt = async (locator, visibleIndex = 0) => {
    const count = await locator.count();
    let seen = 0;
    for (let i = 0; i < count; i++) {
      const candidate = locator.nth(i);
      if (!await candidate.isVisible().catch(() => false)) {
        continue;
      }
      if (seen === visibleIndex) {
        return candidate;
      }
      seen++;
    }
    return null;
  };

  const stationContextElements = (trainId) => {
    const selectorValue = JSON.stringify(String(trainId || ""));
    const toggle = frame.locator(`.station-departure-card [data-action='toggle-station-context'][data-train-id=${selectorValue}]`).first();
    const card = toggle.locator("xpath=ancestor::article[contains(@class, 'station-departure-card')]").first();
    return { toggle, card };
  };

  const clickFirstVisibleAction = async (action, loops = 24) => {
    const selector = `[data-action='${action}']`;
    for (let i = 0; i < loops; i++) {
      const locator = frame.locator(selector);
      const count = await locator.count();
      for (let index = 0; index < count; index++) {
        const candidate = locator.nth(index);
        if (await candidate.isVisible().catch(() => false)) {
          await candidate.scrollIntoViewIfNeeded().catch(() => {});
          await candidate.click({ force: true }).catch(() => {});
          return true;
        }
      }
      await sleep(500);
    }
    return false;
  };

  const activateLocator = async (locator, mode = 'auto') => {
    if (!locator) {
      return false;
    }
    await locator.scrollIntoViewIfNeeded().catch(() => {});
    if (mode === 'dom') {
      await locator.evaluate((el) => el.click()).catch(() => {});
      return true;
    }
    try {
      await locator.click({ force: true });
      return true;
    } catch (_) {
      await locator.evaluate((el) => el.click()).catch(() => {});
      return true;
    }
  };

  const clearExistingRide = async () => {
    for (let attempt = 0; attempt < 3; attempt++) {
      const checkoutButton = await visibleLocatorAt(frame.locator("[data-action='checkout']"), 0);
      if (!checkoutButton) {
        return true;
      }
      await checkoutButton.scrollIntoViewIfNeeded().catch(() => {});
      await checkoutButton.click({ force: true }).catch(() => {});
      const cleared = await waitForCondition(async () => {
        const nextCheckout = await visibleLocatorAt(frame.locator("[data-action='checkout']"), 0);
        return !nextCheckout;
      }, 16, 400);
      if (cleared) {
        await sleep(600);
        return true;
      }
    }
    return false;
  };

  const switchLanguageToLv = async () => {
    if (!await settingsTab.count() || !await settingsTab.isVisible().catch(() => false)) {
      return false;
    }
    await settingsTab.click({ force: true }).catch(() => {});
    await sleep(800);
    const languageSelect = frame.locator('#settings-language').first();
    if (!await waitForVisible(frame, '#settings-language', 12)) {
      return false;
    }
    const currentValue = await languageSelect.inputValue().catch(() => '');
    if (currentValue !== 'LV') {
      await languageSelect.selectOption('LV').catch(() => {});
      const saveButton = frame.locator('#save-settings').first();
      if (await saveButton.count() && await saveButton.isVisible().catch(() => false)) {
        await saveButton.click({ force: true }).catch(() => {});
      }
      await sleep(1400);
    }
    const lvDashboardTab = frame.locator('button').filter({ hasText: /^Panelis$/i }).first();
    const lvSightingsTab = frame.locator('button').filter({ hasText: /^Novērojumi$/i }).first();
    const languageSwitched = await waitForCondition(
      async () => (await lvDashboardTab.count()) > 0 && (await lvSightingsTab.count()) > 0,
      16,
      400,
    );
    if (languageSwitched && await lvDashboardTab.isVisible().catch(() => false)) {
      await lvDashboardTab.click({ force: true }).catch(() => {});
      await sleep(600);
    }
    return languageSwitched;
  };

  const stationQuery = frame.locator('#station-query');
  let stationQueryTexts = [];

  const discoverStationQueries = async () => {
    const discovered = await frame.evaluate(async () => {
      const basePath = (window.TRAIN_APP_CONFIG && window.TRAIN_APP_CONFIG.basePath) || '';
      const windows = ['now', 'next_hour', 'today'];
      const picks = [];
      for (const windowName of windows) {
        try {
          const response = await fetch(`${basePath}/api/v1/windows/${windowName}`, {
            credentials: 'include',
            headers: { 'Content-Type': 'application/json' },
          });
          if (!response.ok) {
            continue;
          }
          const payload = await response.json();
          const trains = Array.isArray(payload && payload.trains) ? payload.trains : [];
          for (const item of trains) {
            const train = item && item.train ? item.train : null;
            const fromStation = String((train && train.fromStation) || '').trim();
            if (!fromStation) {
              continue;
            }
            picks.push(fromStation.slice(0, 4));
            if (picks.length >= 6) {
              return picks;
            }
          }
        } catch (_) {
          // Ignore discovery errors and fall back to static queries.
        }
      }
      return picks;
    }).catch(() => []);

    const fallback = ['Rig', 'Jel', 'Aiz', 'Zil', 'Ata'];
    const merged = [...discovered, ...fallback]
      .map((item) => String(item || '').trim())
      .filter(Boolean);
    return [...new Set(merged)];
  };

  const waitForStationDepartureCards = async () => {
    return waitForCondition(async () => {
      const selectorToggle = frame.locator("[data-action='toggle-checkin-dropdown']");
      const registerButton = frame.locator("[data-action='selected-checkin']");
      const mapButton = frame.locator("[data-action='selected-checkin-map']");
      return (await countVisible(selectorToggle)) > 0
        && (await countVisible(registerButton)) > 0
        && (await countVisible(mapButton)) > 0;
    }, 24, 400);
  };

  const ensureStationDepartures = async () => {
    if (!await waitForVisible(frame, '#station-query')) {
      return false;
    }
    const searchButton = frame.locator('#station-search');
    if (!await waitForVisibleLocator(searchButton, 8)) {
      return false;
    }
    const stationMatches = frame.locator("[data-action='station-departures']");
    if (!stationQueryTexts.length) {
      stationQueryTexts = await discoverStationQueries();
    }

    for (const queryText of stationQueryTexts) {
      await stationQuery.fill(queryText).catch(() => {});
      await searchButton.click({ force: true }).catch(() => {});
      const haveMatches = await waitForCondition(async () => (await countVisible(stationMatches)) > 0, 16, 400);
      if (!haveMatches) {
        continue;
      }
      const visibleMatchCount = Math.min(await countVisible(stationMatches), 4);
      for (let matchIndex = 0; matchIndex < visibleMatchCount; matchIndex++) {
        const stationMatch = await visibleLocatorAt(stationMatches, matchIndex);
        if (!stationMatch) {
          continue;
        }
        await stationMatch.scrollIntoViewIfNeeded().catch(() => {});
        await stationMatch.click({ force: true }).catch(() => {});
        const departuresReady = await waitForStationDepartureCards();
        if (departuresReady) {
          await sleep(400);
          return true;
        }
        if (await dashboardTab.count() && await dashboardTab.isVisible().catch(() => false)) {
          await dashboardTab.click({ force: true }).catch(() => {});
          await sleep(500);
        }
      }
    }
    return false;
  };

  const readMapState = async () => frame.evaluate(() => {
    const mapEl = document.querySelector('.train-map.leaflet-container, .leaflet-container.train-map, .leaflet-container');
    if (!mapEl) {
      return null;
    }
    mapEl.setAttribute('data-smoke-map-persist', '1');
    const pane = mapEl.querySelector('.leaflet-map-pane');
    const tile = mapEl.querySelector('.leaflet-tile');
    const src = tile ? (tile.getAttribute('src') || '') : '';
    const zoomMatch = src.match(/\/(\d+)\/\d+\/\d+(?:\.[a-z]+)?(?:\?|$)/i);
    return {
      marked: true,
      transform: pane ? (pane.style.transform || '') : '',
      zoom: zoomMatch ? zoomMatch[1] : '',
    };
  }).catch(() => null);

  const languageLv = await switchLanguageToLv();
  await clearExistingRide();

  let mapLoaded = false;
  let stationDeparturesLoaded = await ensureStationDepartures();
  let selectorButtonVisible = false;
  let selectorDropdownVisible = false;
  let selectorSelectedOptionVisible = false;
  let selectorSelectionChanged = false;
  let selectorOptionCount = 0;
  let selectorOriginalTrainId = '';
  let selectorOriginalStationId = '';
  let registerMetricsVisible = false;
  let registerMetricsMatch = false;
  let sightingsShortcutVisible = false;
  let dashboardStationContextToggleVisible = false;
  let dashboardStationContextExpanded = false;
  let dashboardStationContextCollapsible = false;
  let dashboardStationContextSingleOpen = false;
  let reportingSelectionVisible = false;
  let reportingSelectionOpenedSightings = false;
  let stationMapActionVisible = false;
  let stopsMapVisible = false;
  let directRideActionVisible = false;
  let directRideActionSucceeded = false;
  let directRideActionToast = false;
  let directRideActionKind = 'missing';
  let directRideActionStateConfirmed = false;
  let directRideActionTrainMatched = false;
  let directRideActionStationMatched = false;
  let directRideActionTrainId = '';
  let directRideActionStationId = '';
  let directRideObservedTrainId = '';
  let directRideObservedStationId = '';
  let directRideCurrentRideFetchStatus = 'unknown';

  const fetchCurrentRide = async () => {
    return frame.evaluate(async () => {
      const basePath = (window.TRAIN_APP_CONFIG && window.TRAIN_APP_CONFIG.basePath) || '';
      const response = await fetch(`${basePath}/api/v1/checkins/current?smokeTs=${Date.now()}`, {
        credentials: 'include',
        headers: {
          'Content-Type': 'application/json',
          'Cache-Control': 'no-cache',
          'Pragma': 'no-cache',
        },
        cache: 'no-store',
      });
      if (!response.ok) {
        return { ok: false, status: response.status };
      }
      return { ok: true, payload: await response.json() };
    }).catch(() => ({ ok: false, status: 0 }));
  };

  if (stationDeparturesLoaded) {
    const selectorToggle = await visibleLocatorAt(frame.locator("[data-action='toggle-checkin-dropdown']"), 0);
    const registerButton = await visibleLocatorAt(frame.locator("[data-action='selected-checkin']"), 0);
    const mapButton = await visibleLocatorAt(frame.locator("[data-action='selected-checkin-map']"), 0);
    const sightingShortcut = await visibleLocatorAt(frame.locator("[data-action='tab-sightings']"), 0);

    selectorButtonVisible = Boolean(selectorToggle);
    directRideActionVisible = Boolean(registerButton);
    stationMapActionVisible = Boolean(mapButton);
    if (sightingShortcut) {
      sightingsShortcutVisible = !await sightingShortcut.getAttribute('disabled').catch(() => '');
    }

    if (registerButton) {
      directRideActionTrainId = await registerButton.getAttribute('data-train-id').catch(() => '') || '';
      directRideActionStationId = await registerButton.getAttribute('data-station-id').catch(() => '') || '';
      directRideActionKind = await registerButton.getAttribute('data-action').catch(() => 'missing') || 'missing';
      selectorOriginalTrainId = directRideActionTrainId;
      selectorOriginalStationId = directRideActionStationId;
    }

    const metrics = frame.locator('.checkin-register-metric');
    if ((await countVisible(metrics)) >= 2) {
      const leftMetric = await visibleLocatorAt(metrics, 0);
      const rightMetric = await visibleLocatorAt(metrics, 1);
      const leftText = leftMetric ? (await leftMetric.textContent().catch(() => '') || '').trim() : '';
      const rightText = rightMetric ? (await rightMetric.textContent().catch(() => '') || '').trim() : '';
      registerMetricsVisible = true;
      registerMetricsMatch = Boolean(leftText) && leftText === rightText;
    }

    if (selectorToggle) {
      await activateLocator(selectorToggle, 'dom');
      selectorDropdownVisible = await waitForCondition(
        async () => (await countVisible(frame.locator('.checkin-dropdown-option'))) > 0,
        12,
        300,
      );
      if (selectorDropdownVisible) {
        selectorOptionCount = await countVisible(frame.locator('.checkin-dropdown-option'));
        selectorSelectedOptionVisible = (await countVisible(frame.locator('#selected-checkin-option'))) > 0;
        if (selectorOptionCount > 1 && directRideActionTrainId) {
          const options = frame.locator('.checkin-dropdown-option');
          const optionCount = await options.count();
          let alternativeOption = null;
          for (let index = 0; index < optionCount; index++) {
            const option = options.nth(index);
            if (!await option.isVisible().catch(() => false)) {
              continue;
            }
            const optionTrainId = await option.getAttribute('data-train-id').catch(() => '') || '';
            if (optionTrainId && optionTrainId !== directRideActionTrainId) {
              alternativeOption = option;
              break;
            }
          }
          if (alternativeOption) {
            await activateLocator(alternativeOption, 'dom');
            selectorSelectionChanged = await waitForCondition(async () => {
              const nextRegister = await visibleLocatorAt(frame.locator("[data-action='selected-checkin']"), 0);
              if (!nextRegister) {
                return false;
              }
              const nextTrainId = await nextRegister.getAttribute('data-train-id').catch(() => '') || '';
              if (!nextTrainId || nextTrainId === directRideActionTrainId) {
                return false;
              }
              directRideActionTrainId = nextTrainId;
              directRideActionStationId = await nextRegister.getAttribute('data-station-id').catch(() => '') || directRideActionStationId;
              return true;
            }, 12, 300);
            if (selectorSelectionChanged && selectorOriginalTrainId) {
              const reopenToggle = await visibleLocatorAt(frame.locator("[data-action='toggle-checkin-dropdown']"), 0);
              if (reopenToggle) {
                await activateLocator(reopenToggle, 'dom');
                const originalOptionVisible = await waitForCondition(async () => {
                  const originalOption = frame.locator(`.checkin-dropdown-option[data-train-id=${JSON.stringify(selectorOriginalTrainId)}]`).first();
                  return (await originalOption.count()) > 0 && await originalOption.isVisible().catch(() => false);
                }, 12, 300);
                if (originalOptionVisible) {
                  const originalOption = frame.locator(`.checkin-dropdown-option[data-train-id=${JSON.stringify(selectorOriginalTrainId)}]`).first();
                  await activateLocator(originalOption, 'dom');
                  await waitForCondition(async () => {
                    const restoredRegister = await visibleLocatorAt(frame.locator("[data-action='selected-checkin']"), 0);
                    if (!restoredRegister) {
                      return false;
                    }
                    const restoredTrainId = await restoredRegister.getAttribute('data-train-id').catch(() => '') || '';
                    if (restoredTrainId !== selectorOriginalTrainId) {
                      return false;
                    }
                    directRideActionTrainId = restoredTrainId;
                    directRideActionStationId = await restoredRegister.getAttribute('data-station-id').catch(() => '') || selectorOriginalStationId;
                    return true;
                  }, 12, 300);
                }
              }
            }
          } else {
            selectorSelectionChanged = true;
            await activateLocator(selectorToggle, 'dom');
          }
        } else {
          selectorSelectionChanged = true;
          await activateLocator(selectorToggle, 'dom');
        }
      }
    }
  }

  if (stationDeparturesLoaded) {
    const rideActionButton = await visibleLocatorAt(frame.locator("[data-action='selected-checkin']"), 0);
    directRideActionVisible = Boolean(rideActionButton);
    if (rideActionButton) {
      directRideActionTrainId = await rideActionButton.getAttribute('data-train-id').catch(() => '') || '';
      directRideActionStationId = await rideActionButton.getAttribute('data-station-id').catch(() => '') || '';
      directRideActionKind = await rideActionButton.getAttribute('data-action').catch(() => 'missing') || 'missing';
      for (let attempt = 0; attempt < 3 && !directRideActionSucceeded; attempt++) {
        const currentRideAction = await visibleLocatorAt(frame.locator("[data-action='selected-checkin']"), 0);
        if (!currentRideAction) {
          break;
        }
        await currentRideAction.scrollIntoViewIfNeeded().catch(() => {});
        await currentRideAction.click({ force: true }).catch(() => {});
        directRideActionSucceeded = await waitForCondition(async () => {
          const currentRide = await fetchCurrentRide();
          directRideCurrentRideFetchStatus = currentRide && Object.prototype.hasOwnProperty.call(currentRide, 'status')
            ? String(currentRide.status)
            : (currentRide && currentRide.ok ? '200' : '0');
          const currentRidePayload = currentRide && currentRide.ok ? currentRide.payload : null;
          const currentRideState = currentRidePayload && currentRidePayload.currentRide ? currentRidePayload.currentRide : null;
          const currentRideTrainId = currentRideState && currentRideState.checkIn ? currentRideState.checkIn.trainInstanceId || '' : '';
          const currentRideStationId = currentRideState ? currentRideState.boardingStationId || '' : '';
          directRideObservedTrainId = currentRideTrainId;
          directRideObservedStationId = currentRideStationId;
          directRideActionTrainMatched = Boolean(currentRideTrainId) && currentRideTrainId === directRideActionTrainId;
          directRideActionStationMatched = !directRideActionStationId || currentRideStationId === directRideActionStationId;
          directRideActionStateConfirmed = directRideActionTrainMatched && directRideActionStationMatched;
          return directRideActionStateConfirmed;
        }, 28, 500);
      }
      const toast = frame.locator('.toast.success, .toast').first();
      directRideActionToast = await toast.count() > 0 && await toast.isVisible().catch(() => false);
      if (await dashboardTab.count() && await dashboardTab.isVisible().catch(() => false)) {
        await dashboardTab.click({ force: true }).catch(() => {});
        await sleep(600);
      }
      stationDeparturesLoaded = await ensureStationDepartures();
    }
  }

  if (stationDeparturesLoaded) {
    const stationMapAction = await visibleLocatorAt(frame.locator("[data-action='selected-checkin-map'], [data-action='open-map']"), 0);
    stationMapActionVisible = Boolean(stationMapAction);
    if (stationMapActionVisible) {
      for (let attempt = 0; attempt < 3 && !mapLoaded; attempt++) {
        await stationMapAction.scrollIntoViewIfNeeded().catch(() => {});
        await stationMapAction.evaluate((el) => el.click()).catch(() => {});
        stopsMapVisible = true;
        for (let i = 0; i < 20; i++) {
          const mapCount = await frame.locator('.train-map').count();
          const stopCount = await frame.locator('.stop-row').count();
          if (mapCount > 0 && stopCount > 0) {
            mapLoaded = true;
            break;
          }
          await sleep(500);
        }
        if (!mapLoaded && attempt < 2 && await dashboardTab.count() && await dashboardTab.isVisible().catch(() => false)) {
          await dashboardTab.click({ force: true }).catch(() => {});
          await sleep(600);
          await ensureStationDepartures();
        }
      }
    }
  }

  if (await dashboardTab.count() && await dashboardTab.isVisible().catch(() => false)) {
    await dashboardTab.click({ force: true }).catch(() => {});
  }

  if (stationDeparturesLoaded && await ensureStationDepartures()) {
    const dashboardCards = frame.locator('.station-departure-card');
    const dashboardStationToggles = frame.locator(".station-departure-card [data-action='toggle-station-context']");
    dashboardStationContextToggleVisible = await countVisible(dashboardStationToggles) > 0;
    if (dashboardStationContextToggleVisible) {
      const firstCard = await visibleLocatorAt(dashboardCards, 0);
      const firstVisibleToggle = firstCard
        ? await visibleLocatorAt(firstCard.locator("[data-action='toggle-station-context']"), 0)
        : null;
      const firstTrainId = firstVisibleToggle
        ? await firstVisibleToggle.getAttribute('data-train-id').catch(() => '')
        : '';
      const firstContext = firstTrainId ? stationContextElements(firstTrainId) : null;
      if (firstVisibleToggle && firstContext) {
        await activateLocator(firstVisibleToggle, 'dom');
        dashboardStationContextExpanded = await waitForCondition(async () => {
          const expanded = await firstContext.toggle.getAttribute('aria-expanded').catch(() => 'false');
          return expanded === 'true' && (await firstContext.card.locator('.station-context').count()) > 0;
        }, 12, 300);
        if (dashboardStationContextExpanded) {
          const reportingButtonByText = firstContext.card.locator('.station-context button').filter({ hasText: /^Izmantot paziņošanai$/i }).first();
          const reportingButtonByAction = firstContext.card.locator(".station-context [data-action='open-sightings-train']").first();
          reportingSelectionVisible =
            ((await reportingButtonByText.count()) > 0 && await reportingButtonByText.isVisible().catch(() => false))
            || ((await reportingButtonByAction.count()) > 0 && await reportingButtonByAction.isVisible().catch(() => false));
        }
        await activateLocator(firstContext.toggle, 'dom');
        dashboardStationContextCollapsible = await waitForCondition(async () => {
          const expanded = await firstContext.toggle.getAttribute('aria-expanded').catch(() => 'true');
          return expanded === 'false' && (await firstContext.card.locator('.station-context').count()) === 0;
        }, 12, 300);
      }
      const secondCard = await visibleLocatorAt(dashboardCards, 1);
      const secondVisibleToggle = secondCard
        ? await visibleLocatorAt(secondCard.locator("[data-action='toggle-station-context']"), 0)
        : null;
      const secondTrainId = secondVisibleToggle
        ? await secondVisibleToggle.getAttribute('data-train-id').catch(() => '')
        : '';
      const secondContext = secondTrainId ? stationContextElements(secondTrainId) : null;
      if (firstContext && secondContext) {
        await activateLocator(firstContext.toggle, 'dom');
        await waitForCondition(async () => (await firstContext.toggle.getAttribute('aria-expanded').catch(() => 'false')) === 'true', 12, 300);
        await activateLocator(secondContext.toggle, 'dom');
        dashboardStationContextSingleOpen = await waitForCondition(async () => {
          const expandedCount = await frame.locator(".station-departure-card [data-action='toggle-station-context'][aria-expanded='true']").count();
          const secondExpanded = await secondContext.toggle.getAttribute('aria-expanded').catch(() => 'false');
          return expandedCount === 1 && secondExpanded === 'true';
        }, 12, 300);
      } else {
        dashboardStationContextSingleOpen = dashboardStationContextCollapsible;
      }
    }
  }

  let stationSightingVisible = false;
  let sightingSubmitWashed = false;
  let blockedSightingToast = false;
  let sightingDepartureSelected = false;
  let sightingSubmitArmed = false;
  let submitAfterSelectionFeedback = false;
  let sightingsStationContextExpanded = false;
  let sightingsStationContextCollapsible = false;
  let sightingsStationContextSingleOpen = false;
  let sightingsTabActive = false;
  if (await ensureStationDepartures()) {
    const sightingCta = frame.locator("[data-action='tab-sightings']").first();
    if (await sightingCta.count() && await sightingCta.isVisible().catch(() => false)) {
      await activateLocator(sightingCta, 'dom');
      await sleep(1000);
      const submitVisible = await waitForVisible(frame, '#station-sighting-submit');
      stationSightingVisible = submitVisible;
      const activeSightingsTab = frame.locator('button.active').filter({ hasText: /^Sightings$|^Novērojumi$/i }).first();
      sightingsTabActive = await activeSightingsTab.count() > 0 && await activeSightingsTab.isVisible().catch(() => false);
      if (submitVisible) {
        const submitButton = frame.locator('#station-sighting-submit').first();
        sightingSubmitWashed = await submitButton.evaluate((el) => el.classList.contains('washed-success')).catch(() => false);

        await submitButton.click({ force: true }).catch(() => {});
        await sleep(600);
        const blockedToast = frame.locator('.toast.info').first();
        blockedSightingToast = await blockedToast.count() > 0 && await blockedToast.isVisible().catch(() => false);

        const sightingCards = frame.locator('.station-departure-card');
        const firstSightingCard = await visibleLocatorAt(sightingCards, 0);
        const firstSightingToggle = firstSightingCard
          ? await visibleLocatorAt(firstSightingCard.locator("[data-action='toggle-station-context']"), 0)
          : null;
        if (firstSightingToggle) {
          await activateLocator(firstSightingToggle, 'dom');
          sightingsStationContextExpanded = await waitForCondition(async () => {
            const expanded = await firstSightingToggle.getAttribute('aria-expanded').catch(() => 'false');
            return expanded === 'true' && (await firstSightingCard.locator('.station-context').count()) > 0;
          }, 12, 300);
          if (sightingsStationContextExpanded) {
            await activateLocator(firstSightingToggle, 'dom');
            sightingsStationContextCollapsible = await waitForCondition(async () => {
              const expanded = await firstSightingToggle.getAttribute('aria-expanded').catch(() => 'true');
              return expanded === 'false' && (await firstSightingCard.locator('.station-context').count()) === 0;
            }, 12, 300);
          }
        }
        const secondSightingCard = await visibleLocatorAt(sightingCards, 1);
        const secondSightingToggle = secondSightingCard
          ? await visibleLocatorAt(secondSightingCard.locator("[data-action='toggle-station-context']"), 0)
          : null;
        if (firstSightingToggle && secondSightingToggle) {
          await activateLocator(firstSightingToggle, 'dom');
          await waitForCondition(async () => (await firstSightingToggle.getAttribute('aria-expanded').catch(() => 'false')) === 'true', 12, 300);
          await activateLocator(secondSightingToggle, 'dom');
          sightingsStationContextSingleOpen = await waitForCondition(async () => {
            const expandedCount = await frame.locator(".station-departure-card [data-action='toggle-station-context'][aria-expanded='true']").count();
            const secondExpanded = await secondSightingToggle.getAttribute('aria-expanded').catch(() => 'false');
            return expandedCount === 1 && secondExpanded === 'true';
          }, 12, 300);
        } else {
          sightingsStationContextSingleOpen = sightingsStationContextCollapsible;
        }

        sightingDepartureSelected = (await countVisible(frame.locator('.selected-train-card'))) > 0;
        const selectDeparture = await visibleLocatorAt(frame.locator("[data-action='select-sighting-train']"), 0);
        if (selectDeparture) {
          for (let attempt = 0; attempt < 3 && !sightingDepartureSelected; attempt++) {
            await selectDeparture.scrollIntoViewIfNeeded().catch(() => {});
            await selectDeparture.evaluate((el) => el.click()).catch(() => {});
            await sleep(600);
            sightingDepartureSelected = (await countVisible(frame.locator('.selected-train-card'))) > 0;
          }
          sightingSubmitArmed = await submitButton.evaluate((el) => !el.classList.contains('washed-success')).catch(() => false);

          if (sightingSubmitArmed) {
            await submitButton.click({ force: true }).catch(() => {});
            await sleep(1500);
            const feedbackToast = frame.locator('.toast').first();
            if (await feedbackToast.count() && await feedbackToast.isVisible().catch(() => false)) {
              const feedbackText = await feedbackToast.textContent().catch(() => '');
              submitAfterSelectionFeedback = Boolean(feedbackText) && !/Select a departure|Izvēlieties atiešanu/i.test(feedbackText || '');
            }
          }
        }
      }
    }
  }

  if (await dashboardTab.count() && await dashboardTab.isVisible().catch(() => false)) {
    await dashboardTab.click({ force: true }).catch(() => {});
    await sleep(500);
  }
  if (await ensureStationDepartures() && dashboardStationContextToggleVisible) {
    const dashboardCards = frame.locator('.station-departure-card');
    const firstCard = await visibleLocatorAt(dashboardCards, 0);
    const firstToggle = firstCard
      ? await visibleLocatorAt(firstCard.locator("[data-action='toggle-station-context']"), 0)
      : null;
    if (firstToggle) {
      await activateLocator(firstToggle, 'dom');
      const reopenedDashboardContext = await waitForCondition(async () => (await firstCard.locator('.station-context').count()) > 0, 12, 300);
      dashboardStationContextExpanded = dashboardStationContextExpanded || reopenedDashboardContext;
      const reportingButtonByText = firstCard.locator('.station-context button').filter({ hasText: /^Izmantot paziņošanai$/i }).first();
      const reportingButton = (await reportingButtonByText.count())
        ? reportingButtonByText
        : firstCard.locator(".station-context [data-action='open-sightings-train']").first();
      reportingSelectionVisible = reportingSelectionVisible
        || ((await reportingButton.count()) > 0 && await reportingButton.isVisible().catch(() => false));
      if (reopenedDashboardContext) {
        await activateLocator(firstToggle, 'dom');
        dashboardStationContextCollapsible = dashboardStationContextCollapsible || await waitForCondition(
          async () => (await firstCard.locator('.station-context').count()) === 0,
          12,
          300,
        );
        await activateLocator(firstToggle, 'dom');
        dashboardStationContextExpanded = dashboardStationContextExpanded || await waitForCondition(
          async () => (await firstCard.locator('.station-context').count()) > 0,
          12,
          300,
        );
      }
      if (await reportingButton.count() && await reportingButton.isVisible().catch(() => false)) {
        const reportingTrainId = await reportingButton.getAttribute('data-train-id').catch(() => '') || '';
        const selectedReportingCard = reportingTrainId
          ? frame.locator(`.selected-train-card [data-action='toggle-station-context'][data-train-id=${JSON.stringify(reportingTrainId)}]`).first()
          : null;
        for (let attempt = 0; attempt < 3 && !reportingSelectionOpenedSightings; attempt++) {
          await activateLocator(reportingButton, 'dom');
          await sleep(1200);
          reportingSelectionOpenedSightings = await waitForCondition(async () => {
            const activeSightingsTab = frame.locator('button.active').filter({ hasText: /^Sightings$|^Novērojumi$/i }).first();
            const sightingsTabReady = ((await activeSightingsTab.count()) > 0 && await activeSightingsTab.isVisible().catch(() => false))
              && await waitForVisible(frame, '#station-sighting-submit', 6);
            if (!sightingsTabReady) {
              return false;
            }
            if (!selectedReportingCard) {
              return true;
            }
            return (await selectedReportingCard.count()) > 0 && await selectedReportingCard.isVisible().catch(() => false);
          }, 12, 300);
          if (!reportingSelectionOpenedSightings && attempt < 2) {
            if (await dashboardTab.count() && await dashboardTab.isVisible().catch(() => false)) {
              await dashboardTab.click({ force: true }).catch(() => {});
              await sleep(500);
            }
            if (await ensureStationDepartures()) {
              const retryFirstCard = await visibleLocatorAt(frame.locator('.station-departure-card'), 0);
              const retryFirstToggle = retryFirstCard
                ? await visibleLocatorAt(retryFirstCard.locator("[data-action='toggle-station-context']"), 0)
                : null;
              if (retryFirstToggle) {
                await activateLocator(retryFirstToggle, 'dom');
                await waitForCondition(async () => (await retryFirstCard.locator('.station-context').count()) > 0, 12, 300);
              }
            }
          }
        }
        if (reportingSelectionOpenedSightings) {
          sightingDepartureSelected = sightingDepartureSelected || (await countVisible(frame.locator('.selected-train-card'))) > 0;
          const submitButton = frame.locator('#station-sighting-submit').first();
          if (await submitButton.count() && await submitButton.isVisible().catch(() => false)) {
            sightingSubmitArmed = sightingSubmitArmed || await submitButton.evaluate((el) => !el.classList.contains('washed-success')).catch(() => false);
            if (sightingSubmitArmed && !submitAfterSelectionFeedback) {
              await submitButton.click({ force: true }).catch(() => {});
              await sleep(1500);
              const feedbackToast = frame.locator('.toast').first();
              if (await feedbackToast.count() && await feedbackToast.isVisible().catch(() => false)) {
                const feedbackText = await feedbackToast.textContent().catch(() => '');
                submitAfterSelectionFeedback = Boolean(feedbackText) && !/Select a departure|Izvēlieties atiešanu/i.test(feedbackText || '');
              }
            }
          }
        }
      }
    }
  }

  let stopContextAvailable = false;
  let stopContextExpanded = false;
  let stopContextCollapsible = false;
  let stopContextSingleOpen = false;
  let stopContextOutsideClosed = false;
  let mapPersisted = false;
  let mapZoomStable = false;
  let mapTransformStable = false;
  let mapPopupOpened = false;
  let mapPopupSingleOpen = false;
  let mapPopupOutsideClosed = false;
  let mapPopupRestoredAfterRefresh = false;
  if (await dashboardTab.count() && await dashboardTab.isVisible().catch(() => false)) {
    await dashboardTab.click({ force: true }).catch(() => {});
    await sleep(400);
  }
  if (await ensureStationDepartures()) {
    const stationMapButton = await visibleLocatorAt(frame.locator("[data-action='selected-checkin-map'], .station-departure-card [data-action='open-map']"), 0);
    if (stationMapButton) {
      stationMapActionVisible = true;
      await stationMapButton.scrollIntoViewIfNeeded().catch(() => {});
      await stationMapButton.evaluate((el) => el.click()).catch(() => {});
      stopsMapVisible = true;
      for (let i = 0; i < 30; i++) {
        if (await frame.locator('.stop-row').count()) {
          break;
        }
        await sleep(500);
      }
      mapLoaded = mapLoaded || ((await frame.locator('.train-map').count()) > 0 && (await frame.locator('.stop-row').count()) > 0);
      const stopToggles = frame.locator("[data-action='toggle-stop-context']");
      const stopRows = frame.locator('.stop-row');
      const firstStopRow = await visibleLocatorAt(stopRows, 0);
      const firstStopToggle = firstStopRow
        ? await visibleLocatorAt(firstStopRow.locator("[data-action='toggle-stop-context']"), 0)
        : null;
      stopContextAvailable = (await countVisible(stopToggles)) > 0 && Boolean(firstStopToggle);
      if (stopContextAvailable) {
        await activateLocator(firstStopToggle, 'dom');
        stopContextExpanded = await waitForCondition(async () => {
          const expanded = await firstStopToggle.getAttribute('aria-expanded').catch(() => 'false');
          return expanded === 'true' && (await firstStopRow.locator('.stop-context').count()) > 0;
        }, 12, 300);
        if (stopContextExpanded) {
          await activateLocator(firstStopToggle, 'dom');
          stopContextCollapsible = await waitForCondition(async () => {
            const expanded = await firstStopToggle.getAttribute('aria-expanded').catch(() => 'true');
            return expanded === 'false' && (await firstStopRow.locator('.stop-context').count()) === 0;
          }, 12, 300);
        }
      }
      const secondStopRow = await visibleLocatorAt(stopRows, 1);
      const secondStopToggle = secondStopRow
        ? await visibleLocatorAt(secondStopRow.locator("[data-action='toggle-stop-context']"), 0)
        : null;
      if (firstStopToggle && secondStopToggle) {
        await activateLocator(firstStopToggle, 'dom');
        await waitForCondition(async () => (await firstStopToggle.getAttribute('aria-expanded').catch(() => 'false')) === 'true', 12, 300);
        await activateLocator(secondStopToggle, 'dom');
        stopContextSingleOpen = await waitForCondition(async () => {
          const expandedCount = await frame.locator("[data-action='toggle-stop-context'][aria-expanded='true']").count();
          const secondExpanded = await secondStopToggle.getAttribute('aria-expanded').catch(() => 'false');
          return expandedCount === 1 && secondExpanded === 'true';
        }, 12, 300);
      } else {
        stopContextSingleOpen = stopContextCollapsible;
      }

      const mapRoot = frame.locator('.train-map').first();
      const zoomIn = frame.locator('.leaflet-control-zoom-in').first();
      if (await mapRoot.count() && await zoomIn.count() && await zoomIn.isVisible().catch(() => false)) {
        const box = await mapRoot.boundingBox().catch(() => null);
        const compactMarkers = frame.locator('.train-map .map-station-marker, .train-map .map-train-marker');
        const popupCards = frame.locator('.leaflet-popup .map-popup-card');
        const popupTitle = async () => {
          const titleNode = await visibleLocatorAt(frame.locator('.leaflet-popup .map-popup-heading strong'), 0);
          return titleNode ? ((await titleNode.textContent().catch(() => '')) || '').trim() : '';
        };
        const firstCompactMarker = await visibleLocatorAt(compactMarkers, 0);
        const secondCompactMarker = await visibleLocatorAt(compactMarkers, 1);

        if (firstCompactMarker) {
          await activateLocator(firstCompactMarker, 'dom');
          mapPopupOpened = await waitForCondition(async () => (await countVisible(popupCards)) === 1, 12, 300);
          const firstPopupTitle = mapPopupOpened ? await popupTitle() : '';
          if (mapPopupOpened && secondCompactMarker) {
            await activateLocator(secondCompactMarker, 'dom');
            mapPopupSingleOpen = await waitForCondition(async () => {
              const openCount = await countVisible(popupCards);
              const secondTitle = await popupTitle();
              return openCount === 1 && Boolean(secondTitle) && secondTitle !== firstPopupTitle;
            }, 12, 300);
          } else {
            mapPopupSingleOpen = mapPopupOpened;
          }
          if (box) {
            await page.mouse.click(box.x + 18, box.y + box.height - 18);
            mapPopupOutsideClosed = await waitForCondition(async () => (await countVisible(popupCards)) === 0, 12, 300);
          }
        }

        if (firstStopToggle && box) {
          await activateLocator(firstStopToggle, 'dom');
          await waitForCondition(async () => (await firstStopToggle.getAttribute('aria-expanded').catch(() => 'false')) === 'true', 12, 300);
          await page.mouse.click(box.x + 18, box.y + box.height - 18);
          stopContextOutsideClosed = await waitForCondition(async () => {
            const expandedCount = await frame.locator("[data-action='toggle-stop-context'][aria-expanded='true']").count();
            return expandedCount === 0 && (await frame.locator('.stop-context').count()) === 0;
          }, 12, 300);
        }

        if (box) {
          await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
          await page.mouse.down();
          await page.mouse.move(box.x + box.width / 2 + 46, box.y + box.height / 2 + 14, { steps: 12 });
          await page.mouse.up();
          await sleep(800);
        }
        await zoomIn.click({ force: true }).catch(() => {});
        await sleep(800);
        await sleep(1200);
        let popupTitleBeforeRefresh = '';
        if (firstCompactMarker) {
          await activateLocator(firstCompactMarker, 'dom');
          const popupRestoredReady = await waitForCondition(async () => (await countVisible(popupCards)) === 1, 12, 300);
          popupTitleBeforeRefresh = popupRestoredReady ? await popupTitle() : '';
        }
        const beforeRefresh = await readMapState();
        await sleep(17000);
        const afterRefresh = await frame.evaluate(() => {
          const mapEl = document.querySelector('[data-smoke-map-persist="1"]');
          if (!mapEl) {
            return null;
          }
          const pane = mapEl.querySelector('.leaflet-map-pane');
          const tile = mapEl.querySelector('.leaflet-tile');
          const src = tile ? (tile.getAttribute('src') || '') : '';
          const zoomMatch = src.match(/\/(\d+)\/\d+\/\d+(?:\.[a-z]+)?(?:\?|$)/i);
          const popupTitleNode = document.querySelector('.leaflet-popup .map-popup-heading strong');
          const popupCardCount = document.querySelectorAll('.leaflet-popup .map-popup-card').length;
          return {
            transform: pane ? (pane.style.transform || '') : '',
            zoom: zoomMatch ? zoomMatch[1] : '',
            popupTitle: popupTitleNode ? (popupTitleNode.textContent || '').trim() : '',
            popupCardCount: popupCardCount,
          };
        }).catch(() => null);
        mapPersisted = Boolean(beforeRefresh && afterRefresh);
        mapZoomStable = Boolean(beforeRefresh && afterRefresh && beforeRefresh.zoom && beforeRefresh.zoom === afterRefresh.zoom);
        mapTransformStable = Boolean(beforeRefresh && afterRefresh && beforeRefresh.transform && beforeRefresh.transform === afterRefresh.transform);
        mapPopupRestoredAfterRefresh = Boolean(
          popupTitleBeforeRefresh
            && afterRefresh
            && afterRefresh.popupCardCount === 1
            && afterRefresh.popupTitle === popupTitleBeforeRefresh
        );
      }
    } else {
      stopContextSingleOpen = stopContextCollapsible;
    }
  }

  return `frameReady=1;languageLv=${languageLv ? 1 : 0};mapTabVisible=${mapTabVisible ? 1 : 0};stationDeparturesLoaded=${stationDeparturesLoaded ? 1 : 0};selectorButtonVisible=${selectorButtonVisible ? 1 : 0};selectorDropdownVisible=${selectorDropdownVisible ? 1 : 0};selectorSelectedOptionVisible=${selectorSelectedOptionVisible ? 1 : 0};selectorSelectionChanged=${selectorSelectionChanged ? 1 : 0};selectorOptionCount=${selectorOptionCount};registerMetricsVisible=${registerMetricsVisible ? 1 : 0};registerMetricsMatch=${registerMetricsMatch ? 1 : 0};sightingsShortcutVisible=${sightingsShortcutVisible ? 1 : 0};directRideActionVisible=${directRideActionVisible ? 1 : 0};directRideActionSucceeded=${directRideActionSucceeded ? 1 : 0};directRideActionStateConfirmed=${directRideActionStateConfirmed ? 1 : 0};directRideActionTrainMatched=${directRideActionTrainMatched ? 1 : 0};directRideActionStationMatched=${directRideActionStationMatched ? 1 : 0};directRideActionToast=${directRideActionToast ? 1 : 0};directRideActionKind=${directRideActionKind};directRideActionTrainId=${encodeURIComponent(directRideActionTrainId)};directRideActionStationId=${encodeURIComponent(directRideActionStationId)};directRideObservedTrainId=${encodeURIComponent(directRideObservedTrainId)};directRideObservedStationId=${encodeURIComponent(directRideObservedStationId)};directRideCurrentRideFetchStatus=${encodeURIComponent(directRideCurrentRideFetchStatus)};stationMapActionVisible=${stationMapActionVisible ? 1 : 0};stopsMapVisible=${stopsMapVisible ? 1 : 0};mapLoaded=${mapLoaded ? 1 : 0};dashboardStationContextToggleVisible=${dashboardStationContextToggleVisible ? 1 : 0};dashboardStationContextExpanded=${dashboardStationContextExpanded ? 1 : 0};dashboardStationContextCollapsible=${dashboardStationContextCollapsible ? 1 : 0};dashboardStationContextSingleOpen=${dashboardStationContextSingleOpen ? 1 : 0};reportingSelectionVisible=${reportingSelectionVisible ? 1 : 0};reportingSelectionOpenedSightings=${reportingSelectionOpenedSightings ? 1 : 0};sightingsTabActive=${sightingsTabActive ? 1 : 0};stationSightingVisible=${stationSightingVisible ? 1 : 0};sightingSubmitWashed=${sightingSubmitWashed ? 1 : 0};blockedSightingToast=${blockedSightingToast ? 1 : 0};sightingsStationContextExpanded=${sightingsStationContextExpanded ? 1 : 0};sightingsStationContextCollapsible=${sightingsStationContextCollapsible ? 1 : 0};sightingsStationContextSingleOpen=${sightingsStationContextSingleOpen ? 1 : 0};sightingDepartureSelected=${sightingDepartureSelected ? 1 : 0};sightingSubmitArmed=${sightingSubmitArmed ? 1 : 0};submitAfterSelectionFeedback=${submitAfterSelectionFeedback ? 1 : 0};stopContextAvailable=${stopContextAvailable ? 1 : 0};stopContextExpanded=${stopContextExpanded ? 1 : 0};stopContextCollapsible=${stopContextCollapsible ? 1 : 0};stopContextSingleOpen=${stopContextSingleOpen ? 1 : 0};stopContextOutsideClosed=${stopContextOutsideClosed ? 1 : 0};mapPersisted=${mapPersisted ? 1 : 0};mapZoomStable=${mapZoomStable ? 1 : 0};mapTransformStable=${mapTransformStable ? 1 : 0};mapPopupOpened=${mapPopupOpened ? 1 : 0};mapPopupSingleOpen=${mapPopupSingleOpen ? 1 : 0};mapPopupOutsideClosed=${mapPopupOutsideClosed ? 1 : 0};mapPopupRestoredAfterRefresh=${mapPopupRestoredAfterRefresh ? 1 : 0}`;
}
JS
)"

open_miniapp_frame_with_retry() {
  local attempt max_attempts=3
  for attempt in $(seq 1 "$max_attempts"); do
    open_chat_browser
    run_pw run-code "$js_open_bot_chat"
    if ! output_has 'bot-chat-(already|by-goto|by-href|by-name|by-search)'; then
      log "Mini app smoke failed: Telegram did not navigate to the bot chat"
      exit 1
    fi

    run_pw run-code "$js_open_miniapp_frame"
    if output_has 'frameReady=1'; then
      return 0
    fi

    if [[ "$attempt" -lt "$max_attempts" ]]; then
      log "Mini app smoke bootstrap retry ${attempt}/${max_attempts}: mini app frame was not ready"
      close_chat_browser
      sleep 2
    fi
  done
  return 1
}

verify_miniapp_with_retry() {
  local attempt max_attempts=2
  for attempt in $(seq 1 "$max_attempts"); do
    run_pw run-code "$js_open_miniapp_and_verify"
    if output_has 'frameReady=1' && output_has 'languageLv=1'; then
      return 0
    fi
    if [[ "$attempt" -lt "$max_attempts" ]] && (output_has 'frameReady=0' || output_has 'languageLv=0'); then
      log "Mini app smoke bootstrap retry ${attempt}/${max_attempts}: mini app bootstrap did not settle"
      close_chat_browser
      sleep 2
      if ! open_miniapp_frame_with_retry; then
        return 1
      fi
      continue
    fi
    return 1
  done
  return 1
}

if ! open_miniapp_frame_with_retry; then
  log "Mini app smoke failed: Telegram mini app frame did not open"
  exit 1
fi

if ! verify_miniapp_with_retry; then
  if output_has 'directRideActionSucceeded=0' || output_has 'directRideActionStateConfirmed=0' || output_has 'directRideActionTrainMatched=0' || output_has 'directRideActionStationMatched=0'; then
    log_direct_ride_debug
  fi
fi
if ! output_has 'frameReady=1'; then
  log "Mini app smoke failed: Telegram mini app frame did not open"
  exit 1
fi
if ! output_has 'languageLv=1'; then
  log "Mini app smoke failed: mini app language could not be switched to LV"
  exit 1
fi
if ! output_has 'mapTabVisible=1'; then
  log "Mini app smoke failed: mini app did not render the Map tab"
  exit 1
fi
if ! output_has 'stationDeparturesLoaded=1'; then
  log "Mini app smoke failed: station search did not load station departures"
  exit 1
fi
if ! output_has 'selectorButtonVisible=1'; then
  log "Mini app smoke failed: the single check-in selector button did not render"
  exit 1
fi
if ! output_has 'selectorDropdownVisible=1'; then
  log "Mini app smoke failed: the single check-in selector did not open its dropdown"
  exit 1
fi
if ! output_has 'selectorSelectedOptionVisible=1'; then
  log "Mini app smoke failed: the selector dropdown did not mark the current departure"
  exit 1
fi
if ! output_has 'selectorOptionCount=[1-9]'; then
  log "Mini app smoke failed: the selector dropdown did not expose any departures"
  exit 1
fi
if ! output_has 'selectorSelectionChanged=1'; then
  log "Mini app smoke failed: the selector dropdown did not support changing the chosen departure"
  exit 1
fi
if ! output_has 'registerMetricsVisible=1'; then
  log "Mini app smoke failed: the register button did not render both train number side metrics"
  exit 1
fi
if ! output_has 'registerMetricsMatch=1'; then
  log "Mini app smoke failed: the register button side metrics did not match"
  exit 1
fi
if ! output_has 'sightingsShortcutVisible=1'; then
  log "Mini app smoke failed: the selected station did not unlock the sightings shortcut"
  exit 1
fi
if ! output_has 'directRideActionVisible=1'; then
  log "Mini app smoke failed: the single register button did not render"
  exit 1
fi
if ! output_has 'directRideActionKind=selected-checkin'; then
  log "Mini app smoke failed: the dashboard register action was not bound to the new selected-checkin control"
  exit 1
fi
if ! output_has 'directRideActionSucceeded=1'; then
  log "Mini app smoke failed: the selected register action did not confirm the authenticated current ride state"
  exit 1
fi
if ! output_has 'directRideActionStateConfirmed=1'; then
  log "Mini app smoke failed: the selected register action did not persist an authenticated ride state"
  exit 1
fi
if ! output_has 'directRideActionTrainMatched=1'; then
  log "Mini app smoke failed: the selected register action did not bind the chosen train to the current ride"
  exit 1
fi
if ! output_has 'directRideActionStationMatched=1'; then
  log "Mini app smoke failed: the selected register action did not preserve the selected boarding station"
  exit 1
fi
if ! output_has 'stationMapActionVisible=1'; then
  log "Mini app smoke failed: the single selector flow did not expose a visible Stops map action"
  exit 1
fi
if ! output_has 'stopsMapVisible=1'; then
  log "Mini app smoke failed: mini app did not render a Stops map action"
  exit 1
fi
if ! output_has 'mapLoaded=1'; then
  log "Mini app smoke failed: mini app map did not load a mapped train"
  exit 1
fi
if ! output_has 'mapPersisted=1'; then
  log "Mini app smoke failed: map state was not readable before and after refresh"
  exit 1
fi
if ! output_has 'mapZoomStable=1'; then
  log "Mini app smoke failed: map zoom changed after refresh"
  exit 1
fi
if ! output_has 'mapTransformStable=1'; then
  log "Mini app smoke failed: map position changed after refresh"
  exit 1
fi
if ! output_has 'mapPopupOpened=1'; then
  log "Mini app smoke failed: compact map markers did not open a detail popup"
  exit 1
fi
if ! output_has 'mapPopupSingleOpen=1'; then
  log "Mini app smoke failed: map allowed more than one popup to stay open"
  exit 1
fi
if ! output_has 'mapPopupOutsideClosed=1'; then
  log "Mini app smoke failed: map popup did not close when clicking outside it"
  exit 1
fi
if ! output_has 'mapPopupRestoredAfterRefresh=1'; then
  log "Mini app smoke failed: open map popup did not survive a live refresh"
  exit 1
fi
if ! output_has 'stopContextAvailable=1'; then
  log "Mini app smoke failed: stop rows did not expose detail toggles"
  exit 1
fi
if ! output_has 'stopContextSingleOpen=1'; then
  log "Mini app smoke failed: multiple stop detail panels could stay open"
  exit 1
fi
if ! output_has 'stopContextOutsideClosed=1'; then
  log "Mini app smoke failed: stop detail panel did not close on outside click"
  exit 1
fi
if ! output_has 'sightingsTabActive=1'; then
  log "Mini app smoke failed: the sightings shortcut did not open the Sightings tab"
  exit 1
fi
if ! output_has 'stationSightingVisible=1'; then
  log "Mini app smoke failed: station sighting form did not appear in the Sightings tab"
  exit 1
fi

run_pw snapshot
run_pw screenshot
run_pw console warning > miniapp-smoke-console.log || true
run_pw network > miniapp-smoke-network.log || true
run_pw close >/dev/null 2>&1 || true

popd >/dev/null

log "Mini app smoke completed; artifacts in $out_dir"
