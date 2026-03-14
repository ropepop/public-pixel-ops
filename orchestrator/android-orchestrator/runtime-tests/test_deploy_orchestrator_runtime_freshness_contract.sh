#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
DEPLOY_SCRIPT="${REPO_ROOT}/scripts/android/deploy_orchestrator_apk.sh"

if ! rg -Fq 'Runtime asset freshness after action:' "${DEPLOY_SCRIPT}"; then
  echo "FAIL: deploy_orchestrator_apk.sh missing post-action runtime freshness reporting" >&2
  exit 1
fi

if ! rg -Fq 'WARN: runtime asset precheck stale' "${DEPLOY_SCRIPT}"; then
  echo "FAIL: deploy_orchestrator_apk.sh missing phase-specific precheck stale wording" >&2
  exit 1
fi

if rg -Fq 'bootstrap|health|export_bundle' "${DEPLOY_SCRIPT}"; then
  echo "FAIL: deploy_orchestrator_apk.sh still includes bootstrap in pre-action stale advisory scope" >&2
  exit 1
fi

if ! rg -Fq 'post_action_runtime_freshness_scope' "${DEPLOY_SCRIPT}"; then
  echo "FAIL: deploy_orchestrator_apk.sh missing post-action runtime freshness scope gate" >&2
  exit 1
fi

echo "PASS: deploy orchestrator runtime freshness contract is present"
