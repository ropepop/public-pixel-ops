#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
HELPER_SCRIPT="${REPO_ROOT}/orchestrator/scripts/android/runtime_asset_freshness.sh"

if [[ ! -f "${HELPER_SCRIPT}" ]]; then
  echo "FAIL: missing runtime asset freshness helper ${HELPER_SCRIPT}" >&2
  exit 1
fi

specs="$("${HELPER_SCRIPT}" --scope readiness --print-specs)"

if [[ -z "${specs}" ]]; then
  echo "FAIL: runtime asset freshness helper returned no readiness specs" >&2
  exit 1
fi

if ! printf '%s\n' "${specs}" | rg -Fq '/data/local/pixel-stack/templates/rooted/adguardhome-start'; then
  echo "FAIL: readiness scope is missing rooted runtime asset checks" >&2
  exit 1
fi

if ! printf '%s\n' "${specs}" | rg -Fq '/data/local/pixel-stack/templates/train/train-web-tunnel-service-loop.sh'; then
  echo "FAIL: readiness scope is missing train runtime template checks" >&2
  exit 1
fi

if ! printf '%s\n' "${specs}" | rg -Fq '/data/local/pixel-stack/bin/pixel-dns-start.sh'; then
  echo "FAIL: readiness scope is missing rooted entrypoint checks" >&2
  exit 1
fi

if ! printf '%s\n' "${specs}" | rg -Fq '/data/local/pixel-stack/bin/pixel-train-start.sh'; then
  echo "FAIL: readiness scope is missing train entrypoint checks" >&2
  exit 1
fi

if ! printf '%s\n' "${specs}" | rg -Fq '/data/local/pixel-stack/templates/satiksme/satiksme-service-loop.sh'; then
  echo "FAIL: readiness scope is missing satiksme runtime template checks" >&2
  exit 1
fi

if ! printf '%s\n' "${specs}" | rg -Fq '/data/local/pixel-stack/bin/pixel-satiksme-start.sh'; then
  echo "FAIL: readiness scope is missing satiksme entrypoint checks" >&2
  exit 1
fi

if ! printf '%s\n' "${specs}" | rg -Fq '/data/local/pixel-stack/bin/pixel-satiksme-health.sh'; then
  echo "FAIL: readiness scope is missing satiksme health entrypoint checks" >&2
  exit 1
fi

if printf '%s\n' "${specs}" | rg -Fq '__pycache__'; then
  echo "FAIL: runtime asset freshness helper should ignore __pycache__ artifacts" >&2
  exit 1
fi

if printf '%s\n' "${specs}" | rg -Fq '.pyc'; then
  echo "FAIL: runtime asset freshness helper should ignore Python bytecode artifacts" >&2
  exit 1
fi

echo "PASS: runtime asset freshness helper covers rooted, train, and satiksme readiness assets"
