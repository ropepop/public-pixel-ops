#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ORCHESTRATOR_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
WORKSPACE_ROOT="$(cd "${ORCHESTRATOR_ROOT}/.." && pwd)"
# shellcheck source=../../../tools/pixel/transport.sh
source "${WORKSPACE_ROOT}/tools/pixel/transport.sh"

BUILD_SCRIPT="${ORCHESTRATOR_ROOT}/scripts/android/build_orchestrator_apk.sh"
DEPLOY_SCRIPT="${ORCHESTRATOR_ROOT}/scripts/android/deploy_orchestrator_apk.sh"
PACKAGE_COMPONENT_RELEASE_SCRIPT="${ORCHESTRATOR_ROOT}/scripts/android/package_component_release.sh"
PACKAGE_RUNTIME_SCRIPT="${ORCHESTRATOR_ROOT}/scripts/android/package_runtime_bundle.sh"
RUNTIME_FRESHNESS_SCRIPT="${ORCHESTRATOR_ROOT}/scripts/android/runtime_asset_freshness.sh"

TRAIN_DEPLOY_SCRIPT="${WORKSPACE_ROOT}/workloads/train-bot/scripts/pixel/redeploy_release.sh"
TRAIN_RELEASE_CHECK_SCRIPT="${WORKSPACE_ROOT}/workloads/train-bot/scripts/pixel/release_check.sh"
SATIKSME_DEPLOY_SCRIPT="${WORKSPACE_ROOT}/workloads/satiksme-bot/scripts/pixel/redeploy_release.sh"
SATIKSME_RELEASE_CHECK_SCRIPT="${WORKSPACE_ROOT}/workloads/satiksme-bot/scripts/pixel/release_check.sh"
SITE_DEPLOY_SCRIPT="${WORKSPACE_ROOT}/workloads/site-notifications/scripts/pixel/redeploy_release.sh"
SITE_RELEASE_CHECK_SCRIPT="${WORKSPACE_ROOT}/workloads/site-notifications/scripts/pixel/release_check.sh"

DEFAULT_CONFIG_FILE="${ORCHESTRATOR_ROOT}/configs/orchestrator-config-v1.example.json"
DEFAULT_TRAIN_BOT_ENV_FILE="${WORKSPACE_ROOT}/workloads/train-bot/.env"
DEFAULT_SATIKSME_BOT_ENV_FILE="${WORKSPACE_ROOT}/workloads/satiksme-bot/.env"
DEFAULT_SITE_NOTIFIER_ENV_FILE="${WORKSPACE_ROOT}/workloads/site-notifications/.env"
DEFAULT_DDNS_TOKEN_FILE="${WORKSPACE_ROOT}/infra/pihole/secrets/cloudflare-token"

ORCHESTRATOR_CONFIG_FILE="${ORCHESTRATOR_CONFIG_FILE:-${DEFAULT_CONFIG_FILE}}"
TRAIN_BOT_ENV_FILE="${TRAIN_BOT_ENV_FILE:-${DEFAULT_TRAIN_BOT_ENV_FILE}}"
SATIKSME_BOT_ENV_FILE="${SATIKSME_BOT_ENV_FILE:-${DEFAULT_SATIKSME_BOT_ENV_FILE}}"
SITE_NOTIFIER_ENV_FILE="${SITE_NOTIFIER_ENV_FILE:-${DEFAULT_SITE_NOTIFIER_ENV_FILE}}"
DDNS_TOKEN_FILE="${DDNS_TOKEN_FILE:-${DEFAULT_DDNS_TOKEN_FILE}}"
SSH_PUBLIC_KEY_FILE="${SSH_PUBLIC_KEY_FILE:-}"
SSH_PASSWORD_HASH_FILE="${SSH_PASSWORD_HASH_FILE:-}"
ADMIN_PASSWORD_FILE="${ADMIN_PASSWORD_FILE:-}"
ACME_TOKEN_FILE="${ACME_TOKEN_FILE:-}"
VPN_AUTH_KEY_FILE="${VPN_AUTH_KEY_FILE:-}"
IPINFO_LITE_TOKEN_FILE="${IPINFO_LITE_TOKEN_FILE:-}"

ADB_SERIAL=""
SCOPE="full"
MODE="auto"
SKIP_BUILD=0
DESTRUCTIVE_E2E=0

PIXEL_RUN_ID="${PIXEL_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$RANDOM}"
export PIXEL_TRANSPORT ADB_SERIAL PIXEL_SSH_HOST PIXEL_SSH_PORT PIXEL_RUN_ID
REPORT_DIR="${WORKSPACE_ROOT}/output/pixel/redeploy/${PIXEL_RUN_ID}"
REPORT_LOG="${REPORT_DIR}/redeploy.log"
SUMMARY_JSON="${REPORT_DIR}/summary.json"
PLAN_TEXT="${REPORT_DIR}/plan.txt"
ROOTED_FRESHNESS_PRE_REPORT="${REPORT_DIR}/rooted-freshness.pre.txt"
ROOTED_FRESHNESS_POST_REPORT="${REPORT_DIR}/rooted-freshness.post.txt"

RUNTIME_BUNDLE_DIR=""
RUNTIME_MANIFEST_PATH=""
TRAIN_BOT_RELEASE_DIR=""
SATIKSME_BOT_RELEASE_DIR=""
SITE_NOTIFIER_RELEASE_DIR=""
TRAIN_BOT_BUNDLE_PATH=""
SATIKSME_BOT_BUNDLE_PATH=""
SITE_NOTIFIER_BUNDLE_PATH=""
TRAIN_BOT_DEPLOY_RESULT_SOURCE="none"
SATIKSME_BOT_DEPLOY_RESULT_SOURCE="none"
SITE_NOTIFIER_DEPLOY_RESULT_SOURCE="none"
TRAIN_BOT_LIVE_RELEASE_PATH=""
SATIKSME_BOT_LIVE_RELEASE_PATH=""
SITE_NOTIFIER_LIVE_RELEASE_PATH=""
TRAIN_BOT_RECOVERY_COMMAND=""
SATIKSME_BOT_RECOVERY_COMMAND=""
SITE_NOTIFIER_RECOVERY_COMMAND=""
LAST_DEPLOY_RESULT_SOURCE="none"

BOOTSTRAP_NEEDED=0
ROOTED_STALE=0
CONFIG_CHANGED=0
DDNS_TOKEN_CHANGED=0
PLATFORM_ARTIFACTS_CHANGED=0
DNS_REFRESH_NEEDED=0
REMOTE_RECOVERY_TRIGGERED=0
RUN_STATUS="failed"
PREFLIGHT_ROOTED_FRESHNESS="unknown"
FINAL_ROOTED_FRESHNESS="unknown"
FINAL_ROOTED_FRESHNESS_REPORT=""
LIVE_DNS_RUNTIME_CONVERGED=0
PLATFORM_MUTATION_PERFORMED=0
ROOTED_CONVERGENCE_REQUIRED=0
APK_INSTALL_PENDING=0

declare -a ACTIONS_EXECUTED=()
declare -a VALIDATION_RESULTS=()

usage() {
  cat <<USAGE
Usage: $(basename "$0") [options]

Options:
  --device SERIAL             adb serial to target
  --transport MODE            transport to use (adb|ssh|auto)
  --ssh-host IP               Tailscale or SSH host/IP
  --ssh-port PORT             SSH port (default: 2222)
  --scope full|platform|dns|train_bot|satiksme_bot|site_notifier
                              deployment scope (default: full)
  --mode auto|force-bootstrap|force-refresh|validate-only
                              deployment mode (default: auto)
  --skip-build                skip orchestrator APK build
  --destructive-e2e           run destructive restart/kill-recovery checks after standard validation
  -h, --help                  show help
USAGE
}

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "$*"
}

record_action() {
  ACTIONS_EXECUTED+=("$1")
}

record_validation() {
  VALIDATION_RESULTS+=("$1=$2")
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "Missing required command: $1" >&2
    exit 1
  }
}

sha256_file() {
  local path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${path}" | awk '{print $1}'
  else
    shasum -a 256 "${path}" | awk '{print $1}'
  fi
}

while (( $# > 0 )); do
  if pixel_transport_parse_arg "$1" "${2:-}"; then
    shift "${PIXEL_TRANSPORT_PARSE_CONSUMED}"
    continue
  fi

  case "$1" in
    --scope)
      shift
      SCOPE="${1:-}"
      ;;
    --mode)
      shift
      MODE="${1:-}"
      ;;
    --skip-build)
      SKIP_BUILD=1
      ;;
    --destructive-e2e)
      DESTRUCTIVE_E2E=1
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

case "${SCOPE}" in
  full|platform|dns|train_bot|satiksme_bot|site_notifier) ;;
  *)
    echo "Unsupported --scope: ${SCOPE}" >&2
    exit 2
    ;;
esac

case "${MODE}" in
  auto|force-bootstrap|force-refresh|validate-only) ;;
  *)
    echo "Unsupported --mode: ${MODE}" >&2
    exit 2
    ;;
esac

mkdir -p "${REPORT_DIR}"
exec > >(tee "${REPORT_LOG}") 2>&1

require_cmd python3
require_cmd curl
require_cmd bash

pixel_transport_require_device >/dev/null
adb_cmd=(pixel_transport_adb_compat)
"${adb_cmd[@]}" get-state >/dev/null

remote_sha256_file() {
  local remote_path="$1"
  pixel_transport_remote_sha256_file "${remote_path}"
}

remote_file_exists() {
  local remote_path="$1"
  pixel_transport_remote_file_exists "${remote_path}"
}

load_remote_json_file() {
  local remote_path="$1"
  if ! remote_file_exists "${remote_path}"; then
    return 1
  fi
  pixel_transport_remote_cat "${remote_path}"
}

pull_remote_file() {
  local remote_path="$1"
  local local_path="$2"
  pixel_transport_pull "${remote_path}" "${local_path}"
}

component_runtime_name() {
  case "${1}" in
    train_bot) printf 'train-bot\n' ;;
    satiksme_bot) printf 'satiksme-bot\n' ;;
    site_notifier) printf 'site-notifications\n' ;;
    *) printf '%s\n' "${1}" ;;
  esac
}

component_expected_release_id() {
  local manifest_path="$1/release-manifest.json"
  python3 - "${manifest_path}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as fh:
    payload = json.load(fh)
print((payload.get("releaseId") or "").strip())
PY
}

component_live_release_path() {
  local runtime_name=""
  runtime_name="$(component_runtime_name "${1}")"
  "${adb_cmd[@]}" shell "su -c 'readlink /data/local/pixel-stack/apps/${runtime_name}/current 2>/dev/null || readlink /data/local/pixel-stack/apps/${runtime_name}.current 2>/dev/null || true'" | tr -d '\r'
}

action_result_remote_path() {
  local action="$1"
  local component="$2"
  local component_key="${component:-all}"
  printf '/data/local/pixel-stack/run/orchestrator-action-results/%s--%s--%s.json\n' "${PIXEL_RUN_ID}" "${action}" "${component_key}"
}

json_field() {
  local payload="$1"
  local field_name="$2"
  JSON_PAYLOAD="${payload}" python3 - "${field_name}" <<'PY'
import json
import os
import sys
field = sys.argv[1]
payload = json.loads(os.environ["JSON_PAYLOAD"])
value = payload.get(field, "")
if isinstance(value, bool):
    print("true" if value else "false")
elif value is None:
    print("")
else:
    print(value)
PY
}

scope_includes_platform() {
  [[ "${SCOPE}" == "full" || "${SCOPE}" == "platform" || "${SCOPE}" == "dns" ]]
}

scope_includes_train() {
  [[ "${SCOPE}" == "full" || "${SCOPE}" == "train_bot" ]]
}

scope_includes_satiksme() {
  [[ "${SCOPE}" == "full" || "${SCOPE}" == "satiksme_bot" ]]
}

scope_includes_site() {
  [[ "${SCOPE}" == "full" || "${SCOPE}" == "site_notifier" ]]
}

add_optional_arg() {
  local ref_name="$1"
  local flag="$2"
  local path="$3"
  [[ -n "${path}" && -f "${path}" ]] || return 0
  # macOS still ships bash 3.2, so build the target array without namerefs.
  eval "${ref_name}+=(\"\${flag}\" \"\${path}\")"
}

deploy_base_args() {
  local -a args=()
  pixel_transport_append_cli_args args
  if (( SKIP_BUILD == 1 || APK_INSTALL_PENDING == 0 )); then
    args+=(--skip-build)
  fi
  printf '%s\n' "${args[@]}"
}

runtime_freshness_args() {
  local args=()
  pixel_transport_append_cli_args args
  printf '%s\n' "${args[@]}"
}

transport_cli_args_string() {
  local args=()
  pixel_transport_append_cli_args args
  if (( ${#args[@]} == 0 )); then
    return 0
  fi
  printf '%q ' "${args[@]}"
}

selected_target_label() {
  if [[ "$(pixel_transport_selected)" == "adb" ]]; then
    printf '%s\n' "${ADB_SERIAL}"
  else
    printf '%s:%s\n' "${PIXEL_SSH_HOST}" "${PIXEL_SSH_PORT}"
  fi
}

run_deploy() {
  local -a cmd=("${DEPLOY_SCRIPT}")
  local line=""
  local capture_file=""
  local rc=0
  while IFS= read -r line; do
    [[ -n "${line}" ]] && cmd+=("${line}")
  done < <(deploy_base_args)
  cmd+=("$@")
  log "Running orchestrator deploy: ${cmd[*]}"
  capture_file="$(mktemp "${REPORT_DIR}/run-deploy.XXXXXX")"
  set +e
  "${cmd[@]}" 2>&1 | tee "${capture_file}"
  rc=${PIPESTATUS[0]}
  set -e
  if (( rc == 0 && SKIP_BUILD == 0 && APK_INSTALL_PENDING == 1 )); then
    APK_INSTALL_PENDING=0
  fi
  LAST_DEPLOY_RESULT_SOURCE="$(sed -n 's/^Action result source: //p' "${capture_file}" | tail -n1)"
  if [[ -z "${LAST_DEPLOY_RESULT_SOURCE}" ]]; then
    LAST_DEPLOY_RESULT_SOURCE="none"
  fi
  return "${rc}"
}

build_orchestrator_if_needed() {
  if (( SKIP_BUILD == 1 )); then
    log "Skipping orchestrator APK build"
    return 0
  fi
  record_action "build_orchestrator_apk"
  "${BUILD_SCRIPT}"
  APK_INSTALL_PENDING=1
}

package_train_release() {
  local output=""
  local -a cmd=("${TRAIN_DEPLOY_SCRIPT}" --skip-build --package-only)
  local line=""
  while IFS= read -r line; do
    [[ -n "${line}" ]] && cmd+=("${line}")
  done < <(deploy_base_args)
  output="$("${cmd[@]}" 2>&1)"
  printf '%s\n' "${output}"
  TRAIN_BOT_RELEASE_DIR="$(printf '%s\n' "${output}" | grep -Eo '/[^[:space:]]+/\.artifacts/component-releases/train_bot-[^[:space:]]+' | tail -n 1)"
  if [[ -z "${TRAIN_BOT_RELEASE_DIR}" ]]; then
    TRAIN_BOT_RELEASE_DIR="$(printf '%s\n' "${output}" | awk -F= '/^TRAIN_BOT_RELEASE_DIR=/{print $2}' | tail -n 1)"
  fi
  [[ -n "${TRAIN_BOT_RELEASE_DIR}" && -d "${TRAIN_BOT_RELEASE_DIR}" ]] || {
    echo "Failed to resolve Train Bot release dir from package-only output" >&2
    exit 1
  }
  TRAIN_BOT_BUNDLE_PATH="$(find "${TRAIN_BOT_RELEASE_DIR}/artifacts" -maxdepth 1 -type f -name 'train-bot-bundle*.tar' | sort | tail -n 1)"
  [[ -n "${TRAIN_BOT_BUNDLE_PATH}" && -f "${TRAIN_BOT_BUNDLE_PATH}" ]] || {
    echo "Train Bot release dir is missing a train-bot bundle: ${TRAIN_BOT_RELEASE_DIR}" >&2
    exit 1
  }
}

package_satiksme_release() {
  local output=""
  local -a cmd=("${SATIKSME_DEPLOY_SCRIPT}" --skip-build --package-only)
  local line=""
  while IFS= read -r line; do
    [[ -n "${line}" ]] && cmd+=("${line}")
  done < <(deploy_base_args)
  output="$("${cmd[@]}" 2>&1)"
  printf '%s\n' "${output}"
  SATIKSME_BOT_RELEASE_DIR="$(printf '%s\n' "${output}" | grep -Eo '/[^[:space:]]+/\.artifacts/component-releases/satiksme_bot-[^[:space:]]+' | tail -n 1)"
  if [[ -z "${SATIKSME_BOT_RELEASE_DIR}" ]]; then
    SATIKSME_BOT_RELEASE_DIR="$(printf '%s\n' "${output}" | awk -F= '/^SATIKSME_BOT_RELEASE_DIR=/{print $2}' | tail -n 1)"
  fi
  [[ -n "${SATIKSME_BOT_RELEASE_DIR}" && -d "${SATIKSME_BOT_RELEASE_DIR}" ]] || {
    echo "Failed to resolve Satiksme Bot release dir from package-only output" >&2
    exit 1
  }
  SATIKSME_BOT_BUNDLE_PATH="$(find "${SATIKSME_BOT_RELEASE_DIR}/artifacts" -maxdepth 1 -type f -name 'satiksme-bot-bundle*.tar' | sort | tail -n 1)"
  [[ -n "${SATIKSME_BOT_BUNDLE_PATH}" && -f "${SATIKSME_BOT_BUNDLE_PATH}" ]] || {
    echo "Satiksme Bot release dir is missing a satiksme-bot bundle: ${SATIKSME_BOT_RELEASE_DIR}" >&2
    exit 1
  }
}

package_site_release() {
  local output=""
  local -a cmd=("${SITE_DEPLOY_SCRIPT}" --skip-build --package-only)
  local line=""
  while IFS= read -r line; do
    [[ -n "${line}" ]] && cmd+=("${line}")
  done < <(deploy_base_args)
  output="$("${cmd[@]}" 2>&1)"
  printf '%s\n' "${output}"
  SITE_NOTIFIER_RELEASE_DIR="$(printf '%s\n' "${output}" | grep -Eo '/[^[:space:]]+/\.artifacts/component-releases/site_notifier-[^[:space:]]+' | tail -n 1)"
  if [[ -z "${SITE_NOTIFIER_RELEASE_DIR}" ]]; then
    SITE_NOTIFIER_RELEASE_DIR="$(printf '%s\n' "${output}" | awk -F= '/^SITE_NOTIFIER_RELEASE_DIR=/{print $2}' | tail -n 1)"
  fi
  if [[ -z "${SITE_NOTIFIER_RELEASE_DIR}" ]]; then
    SITE_NOTIFIER_RELEASE_DIR="$(printf '%s\n' "${output}" | tail -n 1 | tr -d '\r')"
  fi
  [[ -n "${SITE_NOTIFIER_RELEASE_DIR}" && -d "${SITE_NOTIFIER_RELEASE_DIR}" ]] || {
    echo "Failed to resolve Site Notifier release dir from package-only output" >&2
    exit 1
  }
  SITE_NOTIFIER_BUNDLE_PATH="$(find "${SITE_NOTIFIER_RELEASE_DIR}/artifacts" -maxdepth 1 -type f -name 'site-notifier-bundle*.tar' | sort | tail -n 1)"
  [[ -n "${SITE_NOTIFIER_BUNDLE_PATH}" && -f "${SITE_NOTIFIER_BUNDLE_PATH}" ]] || {
    echo "Site Notifier release dir is missing a site-notifier bundle: ${SITE_NOTIFIER_RELEASE_DIR}" >&2
    exit 1
  }
}

package_runtime_bundle() {
  local manifest_version="pixel-redeploy-${PIXEL_RUN_ID}"
  local artifact_dir="${WORKSPACE_ROOT}/.artifacts/runtime-local/${manifest_version}"
  local -a cmd=("${PACKAGE_RUNTIME_SCRIPT}" --manifest-version "${manifest_version}" --out-dir "${artifact_dir}")
  if scope_includes_train; then
    [[ -n "${TRAIN_BOT_BUNDLE_PATH}" ]] && cmd+=(--train-bot-bundle "${TRAIN_BOT_BUNDLE_PATH}")
  fi
  if scope_includes_satiksme; then
    [[ -n "${SATIKSME_BOT_BUNDLE_PATH}" ]] && cmd+=(--satiksme-bot-bundle "${SATIKSME_BOT_BUNDLE_PATH}")
  fi
  if scope_includes_site; then
    [[ -n "${SITE_NOTIFIER_BUNDLE_PATH}" ]] && cmd+=(--site-notifier-bundle "${SITE_NOTIFIER_BUNDLE_PATH}")
  fi
  if ! scope_includes_train && ! scope_includes_satiksme && ! scope_includes_site; then
    cmd+=(--platform-only)
  fi
  record_action "package_runtime_bundle"
  "${cmd[@]}"
  RUNTIME_BUNDLE_DIR="${artifact_dir}"
  RUNTIME_MANIFEST_PATH="${artifact_dir}/runtime-manifest.json"
}

compare_platform_manifest_subset() {
  local local_manifest="$1"
  local remote_manifest="$2"
  python3 - "${local_manifest}" "${remote_manifest}" <<'PY'
import json
import sys

ids = {"adguardhome-rootfs", "dropbear-bundle", "tailscale-bundle"}
with open(sys.argv[1], "r", encoding="utf-8") as fh:
    local_manifest = json.load(fh)
with open(sys.argv[2], "r", encoding="utf-8") as fh:
    remote_manifest = json.load(fh)

def subset(payload):
    result = {}
    for artifact in payload.get("artifacts") or []:
        artifact_id = (artifact.get("id") or "").strip()
        if artifact_id in ids:
            result[artifact_id] = artifact.get("sha256")
    return result

print("match" if subset(local_manifest) == subset(remote_manifest) else "mismatch")
PY
}

run_rooted_freshness_check() {
  local output_path="$1"
  local freshness_output=""
  local freshness_rc=0
  local -a cmd=("${RUNTIME_FRESHNESS_SCRIPT}")
  local line=""

  while IFS= read -r line; do
    [[ -n "${line}" ]] && cmd+=("${line}")
  done < <(runtime_freshness_args)
  cmd+=(--scope rooted)
  freshness_output="$("${cmd[@]}" 2>&1)" || freshness_rc=$?
  printf '%s\n' "${freshness_output}" > "${output_path}"
  case "${freshness_rc}" in
    0)
      return 0
      ;;
    3)
      return 3
      ;;
    *)
      echo "runtime_asset_freshness.sh failed while checking rooted assets:" >&2
      printf '%s\n' "${freshness_output}" >&2
      exit 1
      ;;
  esac
}

preflight_rooted_freshness() {
  local rc=0
  run_rooted_freshness_check "${ROOTED_FRESHNESS_PRE_REPORT}" || rc=$?
  case "${rc}" in
    0)
      PREFLIGHT_ROOTED_FRESHNESS="fresh"
      ROOTED_STALE=0
      ;;
    3)
      PREFLIGHT_ROOTED_FRESHNESS="stale"
      ROOTED_STALE=1
      BOOTSTRAP_NEEDED=1
      DNS_REFRESH_NEEDED=1
      ROOTED_CONVERGENCE_REQUIRED=1
      ;;
  esac
}

post_deploy_rooted_freshness() {
  local rc=0
  run_rooted_freshness_check "${ROOTED_FRESHNESS_POST_REPORT}" || rc=$?
  FINAL_ROOTED_FRESHNESS_REPORT="${ROOTED_FRESHNESS_POST_REPORT}"
  LIVE_DNS_RUNTIME_CONVERGED=0
  if scope_includes_platform && verify_live_dns_runtime; then
    LIVE_DNS_RUNTIME_CONVERGED=1
  fi
  case "${rc}" in
    0)
      FINAL_ROOTED_FRESHNESS="fresh"
      ;;
    3)
      FINAL_ROOTED_FRESHNESS="stale"
      echo "Rooted runtime freshness is still stale after platform mutation; see ${ROOTED_FRESHNESS_POST_REPORT}" >&2
      exit 1
      ;;
  esac
}

compute_platform_bootstrap_need() {
  local remote_manifest_tmp=""
  local compare_result=""

  BOOTSTRAP_NEEDED=0
  ROOTED_STALE=0
  CONFIG_CHANGED=0
  DDNS_TOKEN_CHANGED=0
  PLATFORM_ARTIFACTS_CHANGED=0
  DNS_REFRESH_NEEDED=0
  ROOTED_CONVERGENCE_REQUIRED=0

  if ! remote_file_exists "/data/local/pixel-stack/conf/runtime/runtime-manifest.json"; then
    BOOTSTRAP_NEEDED=1
  fi

  if [[ -f "${ORCHESTRATOR_CONFIG_FILE}" ]]; then
    local_sha="$(sha256_file "${ORCHESTRATOR_CONFIG_FILE}")"
    remote_sha="$(remote_sha256_file "/data/local/pixel-stack/conf/orchestrator-config-v1.json")"
    if [[ "${remote_sha}" != "${local_sha}" ]]; then
      CONFIG_CHANGED=1
      BOOTSTRAP_NEEDED=1
    fi
  fi

  if [[ -f "${DDNS_TOKEN_FILE}" ]]; then
    local_sha="$(sha256_file "${DDNS_TOKEN_FILE}")"
    remote_sha="$(remote_sha256_file "/data/local/pixel-stack/conf/ddns/cloudflare-token")"
    if [[ "${remote_sha}" != "${local_sha}" ]]; then
      DDNS_TOKEN_CHANGED=1
      BOOTSTRAP_NEEDED=1
    fi
  fi

  preflight_rooted_freshness

  if remote_file_exists "/data/local/pixel-stack/conf/runtime/runtime-manifest.json"; then
    remote_manifest_tmp="$(mktemp)"
    pull_remote_file "/data/local/pixel-stack/conf/runtime/runtime-manifest.json" "${remote_manifest_tmp}"
    compare_result="$(compare_platform_manifest_subset "${RUNTIME_MANIFEST_PATH}" "${remote_manifest_tmp}")"
    rm -f "${remote_manifest_tmp}"
    if [[ "${compare_result}" != "match" ]]; then
      PLATFORM_ARTIFACTS_CHANGED=1
      BOOTSTRAP_NEEDED=1
    fi
  fi
}

emit_plan() {
  {
    printf 'transport=%s\n' "$(pixel_transport_selected)"
    printf 'target=%s\n' "$(selected_target_label)"
    printf 'scope=%s\n' "${SCOPE}"
    printf 'mode=%s\n' "${MODE}"
    printf 'runtime_bundle_dir=%s\n' "${RUNTIME_BUNDLE_DIR:-none}"
    printf 'train_release_dir=%s\n' "${TRAIN_BOT_RELEASE_DIR:-none}"
    printf 'satiksme_release_dir=%s\n' "${SATIKSME_BOT_RELEASE_DIR:-none}"
    printf 'site_release_dir=%s\n' "${SITE_NOTIFIER_RELEASE_DIR:-none}"
    printf 'bootstrap_needed=%s\n' "${BOOTSTRAP_NEEDED}"
    printf 'rooted_stale=%s\n' "${ROOTED_STALE}"
    printf 'config_changed=%s\n' "${CONFIG_CHANGED}"
    printf 'ddns_token_changed=%s\n' "${DDNS_TOKEN_CHANGED}"
    printf 'platform_artifacts_changed=%s\n' "${PLATFORM_ARTIFACTS_CHANGED}"
  } > "${PLAN_TEXT}"

  log "Deploy plan"
  sed 's/^/  /' "${PLAN_TEXT}"
}

verify_live_dns_runtime() {
  local expected_hash=""
  local live_hash=""
  expected_hash="$(sha256_file "${ORCHESTRATOR_ROOT}/android-orchestrator/app/src/main/assets/runtime/templates/rooted/adguardhome-start")"
  live_hash="$(remote_sha256_file "/data/local/pixel-stack/chroots/adguardhome/usr/local/bin/adguardhome-start")"
  [[ "${live_hash}" == "${expected_hash}" ]] || return 1
  "${adb_cmd[@]}" shell "su -c 'ss -ltn 2>/dev/null | grep -Eq \"[.:]53[[:space:]]\" && ss -ltn 2>/dev/null | grep -Eq \"127\\.0\\.0\\.1:8080[[:space:]]\" && chroot /data/local/pixel-stack/chroots/adguardhome /usr/local/bin/adguardhome-start --remote-healthcheck >/dev/null 2>&1'" >/dev/null 2>&1
}

restart_dns_if_needed() {
  if ! verify_live_dns_runtime || (( ROOTED_CONVERGENCE_REQUIRED == 1 )) || [[ "${MODE}" == "force-refresh" ]]; then
    log "Refreshing DNS runtime to converge staged rooted assets with the live process"
    record_action "restart_component_dns"
    run_deploy --action restart_component --component dns
    PLATFORM_MUTATION_PERFORMED=1
    ROOTED_CONVERGENCE_REQUIRED=1
    if ! verify_live_dns_runtime; then
      echo "DNS runtime did not converge after restart_component dns" >&2
      exit 1
    fi
  fi
}

run_platform_bootstrap() {
  local -a cmd=(--runtime-bundle-dir "${RUNTIME_BUNDLE_DIR}" --action bootstrap)
  add_optional_arg cmd --config-file "${ORCHESTRATOR_CONFIG_FILE}"
  add_optional_arg cmd --train-bot-env-file "${TRAIN_BOT_ENV_FILE}"
  add_optional_arg cmd --satiksme-bot-env-file "${SATIKSME_BOT_ENV_FILE}"
  add_optional_arg cmd --site-notifier-env-file "${SITE_NOTIFIER_ENV_FILE}"
  add_optional_arg cmd --ddns-token-file "${DDNS_TOKEN_FILE}"
  add_optional_arg cmd --ssh-public-key "${SSH_PUBLIC_KEY_FILE}"
  add_optional_arg cmd --ssh-password-hash-file "${SSH_PASSWORD_HASH_FILE}"
  add_optional_arg cmd --admin-password-file "${ADMIN_PASSWORD_FILE}"
  add_optional_arg cmd --acme-token-file "${ACME_TOKEN_FILE}"
  add_optional_arg cmd --vpn-auth-key-file "${VPN_AUTH_KEY_FILE}"
  add_optional_arg cmd --ipinfo-lite-token-file "${IPINFO_LITE_TOKEN_FILE}"
  record_action "bootstrap"
  run_deploy "${cmd[@]}"
}

run_component_redeploy() {
  local component="$1"
  local release_dir="$2"
  local env_flag="$3"
  local env_file="$4"
  local -a cmd=(--component-release-dir "${release_dir}" --action redeploy_component --component "${component}")
  local expected_release_id=""
  local live_release_path=""
  local recovery_command=""
  local action_result_json=""
  local action_result_path=""
  local rc=0
  add_optional_arg cmd "${env_flag}" "${env_file}"
  record_action "redeploy_component_${component}"
  expected_release_id="$(component_expected_release_id "${release_dir}")"
  recovery_command="${WORKSPACE_ROOT}/tools/pixel/redeploy.sh $(transport_cli_args_string)--scope ${component} --mode auto"
  if run_deploy "${cmd[@]}"; then
    rc=0
  else
    rc=$?
  fi

  action_result_path="$(action_result_remote_path "redeploy_component" "${component}")"
  if action_result_json="$(load_remote_json_file "${action_result_path}")"; then
    LAST_DEPLOY_RESULT_SOURCE="artifact"
    if [[ "$(json_field "${action_result_json}" "success")" != "true" ]]; then
      rc=1
    fi
  elif [[ "${LAST_DEPLOY_RESULT_SOURCE}" == "none" ]]; then
    live_release_path="$(component_live_release_path "${component}")"
    if [[ -n "${expected_release_id}" && "${live_release_path}" == *"${expected_release_id}"* ]]; then
      LAST_DEPLOY_RESULT_SOURCE="verification-fallback"
    else
      LAST_DEPLOY_RESULT_SOURCE="log"
    fi
  fi

  live_release_path="${live_release_path:-$(component_live_release_path "${component}")}"
  case "${component}" in
    train_bot)
      TRAIN_BOT_DEPLOY_RESULT_SOURCE="${LAST_DEPLOY_RESULT_SOURCE}"
      TRAIN_BOT_LIVE_RELEASE_PATH="${live_release_path}"
      TRAIN_BOT_RECOVERY_COMMAND="${recovery_command}"
      ;;
    satiksme_bot)
      SATIKSME_BOT_DEPLOY_RESULT_SOURCE="${LAST_DEPLOY_RESULT_SOURCE}"
      SATIKSME_BOT_LIVE_RELEASE_PATH="${live_release_path}"
      SATIKSME_BOT_RECOVERY_COMMAND="${recovery_command}"
      ;;
    site_notifier)
      SITE_NOTIFIER_DEPLOY_RESULT_SOURCE="${LAST_DEPLOY_RESULT_SOURCE}"
      SITE_NOTIFIER_LIVE_RELEASE_PATH="${live_release_path}"
      SITE_NOTIFIER_RECOVERY_COMMAND="${recovery_command}"
      ;;
  esac

  {
    printf 'action=redeploy_component\n'
    printf 'component=%s\n' "${component}"
    printf 'releaseDir=%s\n' "${release_dir}"
    printf 'releaseId=%s\n' "${expected_release_id}"
    printf 'resultSource=%s\n' "${LAST_DEPLOY_RESULT_SOURCE}"
    printf 'liveReleasePath=%s\n' "${live_release_path}"
    printf 'recoveryCommand=%s\n' "${recovery_command}"
  } > "${REPORT_DIR}/${component}-redeploy-summary.txt"

  if (( rc != 0 )); then
    echo "${component} redeploy failed; see ${REPORT_DIR}/${component}-redeploy-summary.txt" >&2
    exit "${rc}"
  fi
}

run_deploy_health_check() {
  local name="$1"
  shift
  if "$@"; then
    record_validation "${name}" "pass"
    return 0
  fi
  record_validation "${name}" "fail"
  return 1
}

http_code() {
  local url="$1"
  curl -ksS -o /dev/null -w '%{http_code}' --max-time 15 "${url}" 2>/dev/null || true
}

http_code_reachable_non_route_miss() {
  local code="$1"
  case "${code}" in
    2??|3??|400|401|403|405) return 0 ;;
    *) return 1 ;;
  esac
}

validate_dns_contract() {
  local remote_host=""
  local remote_token=""
  local admin_code=""
  local bare_code=""
  local tokenized_code=""

  remote_host="$(python3 - "${ORCHESTRATOR_CONFIG_FILE}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as fh:
    payload = json.load(fh)
print((payload.get("remote", {}).get("hostname") or "dns.example.com").strip())
PY
)"
  remote_token="$(python3 - "${ORCHESTRATOR_CONFIG_FILE}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as fh:
    payload = json.load(fh)
print((payload.get("remote", {}).get("dohPathToken") or "").strip())
PY
)"

  admin_code="$(http_code "https://${remote_host}/admin/")"
  bare_code="$(http_code "https://${remote_host}/dns-query")"
  tokenized_code="skipped"
  if [[ -n "${remote_token}" ]]; then
    tokenized_code="$(http_code "https://${remote_host}/${remote_token}/dns-query")"
  fi

  {
    printf 'admin=%s\n' "${admin_code}"
    printf 'bare=%s\n' "${bare_code}"
    printf 'tokenized=%s\n' "${tokenized_code}"
  } > "${REPORT_DIR}/dns-contract.txt"

  [[ "${bare_code}" == "404" ]] || return 1
  case "${admin_code}" in
    200|302|401) ;;
    *) return 1 ;;
  esac
  http_code_reachable_non_route_miss "${tokenized_code}"
}

maybe_recover_remote_frontend() {
  local dns_ok=0

  if run_deploy_health_check "dns_health" run_deploy --action health_component --component dns; then
    dns_ok=1
  else
    echo "DNS health validation failed" >&2
    exit 1
  fi

  if (( dns_ok == 1 )) &&
    ! run_deploy_health_check "remote_health" run_deploy --action health_component --component remote; then
    log "DNS is healthy but remote is not; retrying remote frontend recovery once"
    record_action "restart_component_remote"
    REMOTE_RECOVERY_TRIGGERED=1
    run_deploy --action restart_component --component remote
    if ! run_deploy_health_check "remote_health_after_recovery" run_deploy --action health_component --component remote; then
      echo "Remote frontend recovery did not restore component health" >&2
      exit 1
    fi
  fi
}

write_summary_json() {
  local actions_joined=""
  local validations_joined=""
  actions_joined="$(printf '%s\x1f' "${ACTIONS_EXECUTED[@]:-}")"
  validations_joined="$(printf '%s\x1f' "${VALIDATION_RESULTS[@]:-}")"

  export SUMMARY_DEVICE="$(selected_target_label)"
  export SUMMARY_SCOPE="${SCOPE}"
  export SUMMARY_MODE="${MODE}"
  export SUMMARY_STATUS="${RUN_STATUS}"
  export SUMMARY_REPORT_DIR="${REPORT_DIR}"
  export SUMMARY_RUNTIME_BUNDLE_DIR="${RUNTIME_BUNDLE_DIR}"
  export SUMMARY_RUNTIME_MANIFEST_PATH="${RUNTIME_MANIFEST_PATH}"
  export SUMMARY_TRAIN_RELEASE_DIR="${TRAIN_BOT_RELEASE_DIR}"
  export SUMMARY_SATIKSME_RELEASE_DIR="${SATIKSME_BOT_RELEASE_DIR}"
  export SUMMARY_SITE_RELEASE_DIR="${SITE_NOTIFIER_RELEASE_DIR}"
  export SUMMARY_BOOTSTRAP_NEEDED="${BOOTSTRAP_NEEDED}"
  export SUMMARY_ROOTED_STALE="${ROOTED_STALE}"
  export SUMMARY_PREFLIGHT_ROOTED_FRESHNESS="${PREFLIGHT_ROOTED_FRESHNESS}"
  export SUMMARY_CONFIG_CHANGED="${CONFIG_CHANGED}"
  export SUMMARY_DDNS_TOKEN_CHANGED="${DDNS_TOKEN_CHANGED}"
  export SUMMARY_PLATFORM_ARTIFACTS_CHANGED="${PLATFORM_ARTIFACTS_CHANGED}"
  export SUMMARY_DNS_REFRESH_NEEDED="${DNS_REFRESH_NEEDED}"
  export SUMMARY_REMOTE_RECOVERY_TRIGGERED="${REMOTE_RECOVERY_TRIGGERED}"
  export SUMMARY_FINAL_ROOTED_FRESHNESS="${FINAL_ROOTED_FRESHNESS}"
  export SUMMARY_FINAL_ROOTED_FRESHNESS_REPORT="${FINAL_ROOTED_FRESHNESS_REPORT}"
  export SUMMARY_LIVE_DNS_RUNTIME_CONVERGED="${LIVE_DNS_RUNTIME_CONVERGED}"
  export SUMMARY_ACTIONS="${actions_joined}"
  export SUMMARY_VALIDATIONS="${validations_joined}"
  export SUMMARY_TRAIN_BOT_RESULT_SOURCE="${TRAIN_BOT_DEPLOY_RESULT_SOURCE}"
  export SUMMARY_SATIKSME_BOT_RESULT_SOURCE="${SATIKSME_BOT_DEPLOY_RESULT_SOURCE}"
  export SUMMARY_SITE_NOTIFIER_RESULT_SOURCE="${SITE_NOTIFIER_DEPLOY_RESULT_SOURCE}"
  export SUMMARY_TRAIN_BOT_LIVE_RELEASE_PATH="${TRAIN_BOT_LIVE_RELEASE_PATH}"
  export SUMMARY_SATIKSME_BOT_LIVE_RELEASE_PATH="${SATIKSME_BOT_LIVE_RELEASE_PATH}"
  export SUMMARY_SITE_NOTIFIER_LIVE_RELEASE_PATH="${SITE_NOTIFIER_LIVE_RELEASE_PATH}"
  export SUMMARY_TRAIN_BOT_RECOVERY_COMMAND="${TRAIN_BOT_RECOVERY_COMMAND}"
  export SUMMARY_SATIKSME_BOT_RECOVERY_COMMAND="${SATIKSME_BOT_RECOVERY_COMMAND}"
  export SUMMARY_SITE_NOTIFIER_RECOVERY_COMMAND="${SITE_NOTIFIER_RECOVERY_COMMAND}"
  export SUMMARY_JSON_PATH="${SUMMARY_JSON}"

  python3 - <<'PY'
import json
import os
from pathlib import Path

def split_field(name):
    raw = os.environ.get(name, "")
    if not raw:
        return []
    return [item for item in raw.split("\x1f") if item]

validations = {}
for item in split_field("SUMMARY_VALIDATIONS"):
    if "=" in item:
        key, value = item.split("=", 1)
        validations[key] = value

payload = {
    "device": os.environ["SUMMARY_DEVICE"],
    "scope": os.environ["SUMMARY_SCOPE"],
    "mode": os.environ["SUMMARY_MODE"],
    "status": os.environ["SUMMARY_STATUS"],
    "reportDir": os.environ["SUMMARY_REPORT_DIR"],
    "runtimeBundleDir": os.environ.get("SUMMARY_RUNTIME_BUNDLE_DIR") or None,
    "runtimeManifestPath": os.environ.get("SUMMARY_RUNTIME_MANIFEST_PATH") or None,
    "trainBotReleaseDir": os.environ.get("SUMMARY_TRAIN_RELEASE_DIR") or None,
    "satiksmeBotReleaseDir": os.environ.get("SUMMARY_SATIKSME_RELEASE_DIR") or None,
    "siteNotifierReleaseDir": os.environ.get("SUMMARY_SITE_RELEASE_DIR") or None,
    "preflight": {
        "rootedFreshness": os.environ.get("SUMMARY_PREFLIGHT_ROOTED_FRESHNESS") or "unknown",
    },
    "decisions": {
        "bootstrapNeeded": os.environ["SUMMARY_BOOTSTRAP_NEEDED"] == "1",
        "rootedStale": os.environ["SUMMARY_ROOTED_STALE"] == "1",
        "configChanged": os.environ["SUMMARY_CONFIG_CHANGED"] == "1",
        "ddnsTokenChanged": os.environ["SUMMARY_DDNS_TOKEN_CHANGED"] == "1",
        "platformArtifactsChanged": os.environ["SUMMARY_PLATFORM_ARTIFACTS_CHANGED"] == "1",
        "dnsRefreshNeeded": os.environ["SUMMARY_DNS_REFRESH_NEEDED"] == "1",
        "remoteRecoveryTriggered": os.environ["SUMMARY_REMOTE_RECOVERY_TRIGGERED"] == "1",
    },
    "finalState": {
        "rootedFreshness": os.environ.get("SUMMARY_FINAL_ROOTED_FRESHNESS") or "unknown",
        "liveDnsRuntimeConverged": os.environ.get("SUMMARY_LIVE_DNS_RUNTIME_CONVERGED") == "1",
        "rootedFreshnessReport": os.environ.get("SUMMARY_FINAL_ROOTED_FRESHNESS_REPORT") or None,
    },
    "actionsExecuted": split_field("SUMMARY_ACTIONS"),
    "validations": validations,
    "componentResults": {
        "trainBot": {
            "action": "redeploy_component",
            "releaseDir": os.environ.get("SUMMARY_TRAIN_RELEASE_DIR") or None,
            "resultSource": os.environ.get("SUMMARY_TRAIN_BOT_RESULT_SOURCE") or "none",
            "liveReleasePath": os.environ.get("SUMMARY_TRAIN_BOT_LIVE_RELEASE_PATH") or None,
            "recoveryCommand": os.environ.get("SUMMARY_TRAIN_BOT_RECOVERY_COMMAND") or None,
        },
        "satiksmeBot": {
            "action": "redeploy_component",
            "releaseDir": os.environ.get("SUMMARY_SATIKSME_RELEASE_DIR") or None,
            "resultSource": os.environ.get("SUMMARY_SATIKSME_BOT_RESULT_SOURCE") or "none",
            "liveReleasePath": os.environ.get("SUMMARY_SATIKSME_BOT_LIVE_RELEASE_PATH") or None,
            "recoveryCommand": os.environ.get("SUMMARY_SATIKSME_BOT_RECOVERY_COMMAND") or None,
        },
        "siteNotifier": {
            "action": "redeploy_component",
            "releaseDir": os.environ.get("SUMMARY_SITE_RELEASE_DIR") or None,
            "resultSource": os.environ.get("SUMMARY_SITE_NOTIFIER_RESULT_SOURCE") or "none",
            "liveReleasePath": os.environ.get("SUMMARY_SITE_NOTIFIER_LIVE_RELEASE_PATH") or None,
            "recoveryCommand": os.environ.get("SUMMARY_SITE_NOTIFIER_RECOVERY_COMMAND") or None,
        },
    },
}

Path(os.environ["SUMMARY_JSON_PATH"]).write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")
PY
}

finalize() {
  local rc=$?
  if (( rc != 0 )) && [[ "${RUN_STATUS}" != "success" ]]; then
    RUN_STATUS="failed"
  fi
  write_summary_json || true
  if (( rc == 0 )); then
    log "Summary: ${SUMMARY_JSON}"
  fi
  return "${rc}"
}

trap finalize EXIT

main() {
  log "Using transport: $(pixel_transport_selected) ($(selected_target_label))"
  log "PIXEL_RUN_ID=${PIXEL_RUN_ID}"

  if [[ "${MODE}" != "validate-only" ]]; then
    build_orchestrator_if_needed

    if scope_includes_train; then
      record_action "package_train_bot_release"
      package_train_release
    fi

    if scope_includes_satiksme; then
      record_action "package_satiksme_bot_release"
      package_satiksme_release
    fi

    if scope_includes_site; then
      record_action "package_site_notifier_release"
      package_site_release
    fi

    if scope_includes_platform; then
      package_runtime_bundle
      compute_platform_bootstrap_need
    fi

    case "${MODE}" in
      force-bootstrap)
        BOOTSTRAP_NEEDED=1
        DNS_REFRESH_NEEDED=1
        ;;
      force-refresh)
        if scope_includes_platform && (( ROOTED_STALE == 1 || PLATFORM_ARTIFACTS_CHANGED == 1 )); then
          BOOTSTRAP_NEEDED=1
          ROOTED_CONVERGENCE_REQUIRED=1
        else
          BOOTSTRAP_NEEDED=0
        fi
        DNS_REFRESH_NEEDED=1
        ;;
    esac
  else
    BOOTSTRAP_NEEDED=0
    DNS_REFRESH_NEEDED=0
    if scope_includes_platform; then
      preflight_rooted_freshness
      FINAL_ROOTED_FRESHNESS="${PREFLIGHT_ROOTED_FRESHNESS}"
      FINAL_ROOTED_FRESHNESS_REPORT="${ROOTED_FRESHNESS_PRE_REPORT}"
      if verify_live_dns_runtime; then
        LIVE_DNS_RUNTIME_CONVERGED=1
      fi
    fi
  fi

  emit_plan

  if [[ "${MODE}" != "validate-only" ]]; then
    if scope_includes_platform && (( BOOTSTRAP_NEEDED == 1 )); then
      run_platform_bootstrap
      DNS_REFRESH_NEEDED=1
      PLATFORM_MUTATION_PERFORMED=1
      ROOTED_CONVERGENCE_REQUIRED=1
    fi

    if scope_includes_platform && (( DNS_REFRESH_NEEDED == 1 )); then
      restart_dns_if_needed
    fi

    if scope_includes_train; then
      run_component_redeploy "train_bot" "${TRAIN_BOT_RELEASE_DIR}" --train-bot-env-file "${TRAIN_BOT_ENV_FILE}"
    fi

    if scope_includes_satiksme; then
      run_component_redeploy "satiksme_bot" "${SATIKSME_BOT_RELEASE_DIR}" --satiksme-bot-env-file "${SATIKSME_BOT_ENV_FILE}"
    fi

    if scope_includes_site; then
      run_component_redeploy "site_notifier" "${SITE_NOTIFIER_RELEASE_DIR}" --site-notifier-env-file "${SITE_NOTIFIER_ENV_FILE}"
    fi
  fi

  if ! run_deploy_health_check "orchestrator_health" run_deploy --action health; then
    echo "Orchestrator health validation failed" >&2
    exit 1
  fi

  if scope_includes_platform; then
    maybe_recover_remote_frontend
    if (( REMOTE_RECOVERY_TRIGGERED == 1 )); then
      PLATFORM_MUTATION_PERFORMED=1
      ROOTED_CONVERGENCE_REQUIRED=1
    fi
    if validate_dns_contract; then
      record_validation "dns_contract" "pass"
    else
      record_validation "dns_contract" "fail"
      echo "DNS contract validation failed; see ${REPORT_DIR}/dns-contract.txt" >&2
      exit 1
    fi
  fi

  if scope_includes_platform; then
    if (( ROOTED_CONVERGENCE_REQUIRED == 1 )); then
      post_deploy_rooted_freshness
    elif [[ "${MODE}" == "validate-only" ]]; then
      :
    elif [[ "${FINAL_ROOTED_FRESHNESS}" == "unknown" ]]; then
      FINAL_ROOTED_FRESHNESS="${PREFLIGHT_ROOTED_FRESHNESS}"
      FINAL_ROOTED_FRESHNESS_REPORT="${ROOTED_FRESHNESS_PRE_REPORT}"
      if verify_live_dns_runtime; then
        LIVE_DNS_RUNTIME_CONVERGED=1
      else
        LIVE_DNS_RUNTIME_CONVERGED=0
      fi
    fi
  fi

  if scope_includes_train; then
    record_action "validate_train_bot"
    train_validate_cmd=("${TRAIN_DEPLOY_SCRIPT}" --skip-build --validate-only)
    while IFS= read -r line; do
      [[ -n "${line}" ]] && train_validate_cmd+=("${line}")
    done < <(deploy_base_args)
    "${train_validate_cmd[@]}"
    record_validation "train_bot_validation" "pass"
  fi

  if scope_includes_satiksme; then
    record_action "validate_satiksme_bot"
    satiksme_validate_cmd=("${SATIKSME_DEPLOY_SCRIPT}" --skip-build --validate-only)
    while IFS= read -r line; do
      [[ -n "${line}" ]] && satiksme_validate_cmd+=("${line}")
    done < <(deploy_base_args)
    "${satiksme_validate_cmd[@]}"
    record_validation "satiksme_bot_validation" "pass"
  fi

  if scope_includes_site; then
    record_action "validate_site_notifier"
    site_validate_cmd=("${SITE_DEPLOY_SCRIPT}" --skip-build --validate-only)
    while IFS= read -r line; do
      [[ -n "${line}" ]] && site_validate_cmd+=("${line}")
    done < <(deploy_base_args)
    "${site_validate_cmd[@]}"
    record_validation "site_notifier_validation" "pass"
  fi

  if (( DESTRUCTIVE_E2E == 1 )) && [[ "${MODE}" != "validate-only" ]]; then
    if scope_includes_platform || scope_includes_train || scope_includes_satiksme; then
      record_action "destructive_restart_isolation"
      "${WORKSPACE_ROOT}/workloads/train-bot/scripts/pixel/restart_isolation_acceptance.sh"
      record_validation "destructive_restart_isolation" "pass"
    fi
  fi

  RUN_STATUS="success"
}

main "$@"
