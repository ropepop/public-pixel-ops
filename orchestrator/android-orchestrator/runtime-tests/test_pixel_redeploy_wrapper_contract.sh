#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
WORKSPACE_ROOT="$(cd "${REPO_ROOT}/.." && pwd)"
TOOL_WRAPPER="${WORKSPACE_ROOT}/tools/pixel/redeploy.sh"
SOURCE_SCRIPT="${REPO_ROOT}/scripts/android/pixel_redeploy.sh"
TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "${TMP_ROOT}"' EXIT

if [[ ! -f "${TOOL_WRAPPER}" ]]; then
  echo "FAIL: missing tools/pixel/redeploy.sh" >&2
  exit 1
fi

if [[ ! -f "${SOURCE_SCRIPT}" ]]; then
  echo "FAIL: missing orchestrator/scripts/android/pixel_redeploy.sh" >&2
  exit 1
fi

if ! rg -Fq '/orchestrator/scripts/android/pixel_redeploy.sh' "${TOOL_WRAPPER}"; then
  echo "FAIL: tools/pixel/redeploy.sh no longer delegates to pixel_redeploy.sh" >&2
  exit 1
fi

for required in '--scope' '--mode' '--skip-build' '--destructive-e2e'; do
  if ! rg -Fq -- "${required}" "${SOURCE_SCRIPT}"; then
    echo "FAIL: pixel_redeploy.sh missing ${required} flag" >&2
    exit 1
  fi
done

if ! rg -Fq 'package_runtime_bundle.sh' "${SOURCE_SCRIPT}"; then
  echo "FAIL: pixel_redeploy.sh no longer packages runtime bundles" >&2
  exit 1
fi

if ! rg -Fq -- '--platform-only' "${SOURCE_SCRIPT}"; then
  echo "FAIL: pixel_redeploy.sh missing platform-only runtime bundle packaging path" >&2
  exit 1
fi

if ! rg -Fq -- '--package-only' "${SOURCE_SCRIPT}"; then
  echo "FAIL: pixel_redeploy.sh no longer reuses workload package-only entrypoints" >&2
  exit 1
fi

if ! rg -Fq -- '--validate-only' "${SOURCE_SCRIPT}"; then
  echo "FAIL: pixel_redeploy.sh no longer reuses workload validate-only entrypoints" >&2
  exit 1
fi

if ! rg -Fq 'output/pixel/redeploy/' "${SOURCE_SCRIPT}"; then
  echo "FAIL: pixel_redeploy.sh missing canonical report directory" >&2
  exit 1
fi

if ! rg -Fq 'summary.json' "${SOURCE_SCRIPT}"; then
  echo "FAIL: pixel_redeploy.sh missing machine-readable summary output" >&2
  exit 1
fi

if ! rg -Fq 'runtime_freshness_args' "${SOURCE_SCRIPT}"; then
  echo "FAIL: pixel_redeploy.sh missing dedicated runtime freshness argument builder" >&2
  exit 1
fi

for required in 'rooted-freshness.pre.txt' 'rooted-freshness.post.txt' 'preflight' 'finalState' 'liveDnsRuntimeConverged' 'rootedFreshnessReport'; do
  if ! rg -Fq -- "${required}" "${SOURCE_SCRIPT}"; then
    echo "FAIL: pixel_redeploy.sh missing ${required} reporting contract" >&2
    exit 1
  fi
done

if ! rg -Fq 'validate_dns_contract' "${SOURCE_SCRIPT}"; then
  echo "FAIL: pixel_redeploy.sh missing DNS contract validation" >&2
  exit 1
fi

WORKSPACE_FIXTURE="${TMP_ROOT}/workspace"
ORCHESTRATOR_FIXTURE="${WORKSPACE_FIXTURE}/orchestrator"
STATE_DIR="${TMP_ROOT}/state"
BIN_DIR="${TMP_ROOT}/bin"
RELEASE_DIR="${WORKSPACE_FIXTURE}/workloads/train-bot/.artifacts/component-releases/train_bot-20260309T105826Z-332599446cd3"

mkdir -p \
  "${ORCHESTRATOR_FIXTURE}/scripts/android" \
  "${WORKSPACE_FIXTURE}/tools/pixel" \
  "${WORKSPACE_FIXTURE}/workloads/train-bot/scripts/pixel" \
  "${WORKSPACE_FIXTURE}/workloads/site-notifications/scripts/pixel" \
  "${WORKSPACE_FIXTURE}/output/pixel" \
  "${RELEASE_DIR}/artifacts" \
  "${BIN_DIR}" \
  "${STATE_DIR}"

cp "${SOURCE_SCRIPT}" "${ORCHESTRATOR_FIXTURE}/scripts/android/pixel_redeploy.sh"
chmod +x "${ORCHESTRATOR_FIXTURE}/scripts/android/pixel_redeploy.sh"
cp "${WORKSPACE_ROOT}/tools/pixel/transport.sh" "${WORKSPACE_FIXTURE}/tools/pixel/transport.sh"

cat > "${ORCHESTRATOR_FIXTURE}/scripts/android/build_orchestrator_apk.sh" <<'EOF_BUILD'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$(dirname "${FAKE_STATE_DIR}/apk-builds.log")"
printf 'build\n' >> "${FAKE_STATE_DIR}/apk-builds.log"
EOF_BUILD
chmod +x "${ORCHESTRATOR_FIXTURE}/scripts/android/build_orchestrator_apk.sh"

cat > "${ORCHESTRATOR_FIXTURE}/scripts/android/deploy_orchestrator_apk.sh" <<'EOF_DEPLOY'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >> "${FAKE_STATE_DIR}/${FAKE_LOG_PREFIX}-deploy-invocations.log"

action=""
component=""
while (( $# > 0 )); do
  case "$1" in
    --action)
      shift
      action="${1:-}"
      ;;
    --component)
      shift
      component="${1:-}"
      ;;
  esac
  shift || true
done

if [[ "${action}" == "redeploy_component" && "${component}" == "train_bot" ]]; then
  cat > "${FAKE_STATE_DIR}/remote-action-result.json" <<'EOF_JSON'
{"pixelRunId":"test-run-id","action":"redeploy_component","component":"train_bot","success":true,"message":"artifact success"}
EOF_JSON
fi

printf 'Action result source: artifact\n'
EOF_DEPLOY
chmod +x "${ORCHESTRATOR_FIXTURE}/scripts/android/deploy_orchestrator_apk.sh"

cat > "${WORKSPACE_FIXTURE}/workloads/train-bot/scripts/pixel/redeploy_release.sh" <<EOF_TRAIN
#!/usr/bin/env bash
set -euo pipefail

mode=""
while (( \$# > 0 )); do
  case "\$1" in
    --package-only)
      mode="package"
      ;;
    --validate-only)
      mode="validate"
      ;;
  esac
  shift || true
done

case "\${mode}" in
  package)
    printf 'TRAIN_BOT_RELEASE_DIR=%s\n' "${RELEASE_DIR}"
    ;;
  validate)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
EOF_TRAIN
chmod +x "${WORKSPACE_FIXTURE}/workloads/train-bot/scripts/pixel/redeploy_release.sh"

cat > "${WORKSPACE_FIXTURE}/workloads/site-notifications/scripts/pixel/redeploy_release.sh" <<'EOF_SITE'
#!/usr/bin/env bash
set -euo pipefail
exit 0
EOF_SITE
chmod +x "${WORKSPACE_FIXTURE}/workloads/site-notifications/scripts/pixel/redeploy_release.sh"

touch "${WORKSPACE_FIXTURE}/workloads/train-bot/scripts/pixel/release_check.sh"
touch "${WORKSPACE_FIXTURE}/workloads/site-notifications/scripts/pixel/release_check.sh"

cat > "${RELEASE_DIR}/release-manifest.json" <<'EOF_MANIFEST'
{"releaseId":"train_bot-20260309T105826Z-332599446cd3"}
EOF_MANIFEST
printf 'bundle' > "${RELEASE_DIR}/artifacts/train-bot-bundle.tar"

cat > "${BIN_DIR}/adb" <<'EOF_ADB'
#!/usr/bin/env bash
set -euo pipefail

state_dir="${FAKE_STATE_DIR:?}"

if [[ "${1:-}" == "-s" ]]; then
  shift 2
fi

cmd="${1:-}"
shift || true

case "${cmd}" in
  devices)
    printf 'List of devices attached\nfake-device\tdevice\n'
    ;;
  get-state)
    printf 'device\n'
    ;;
  shell)
    shell_cmd="$*"
    if [[ "${shell_cmd}" == *"/orchestrator-action-results/"* ]]; then
      if [[ "${shell_cmd}" == *"test -f "* ]]; then
        [[ -f "${state_dir}/remote-action-result.json" ]]
      elif [[ "${shell_cmd}" == *"cat "* || "${shell_cmd}" == *'"cat"'* ]]; then
        cat "${state_dir}/remote-action-result.json"
      fi
    elif [[ "${shell_cmd}" == *"readlink /data/local/pixel-stack/apps/train-bot/current"* ]]; then
        printf '/data/local/pixel-stack/apps/train-bot/releases/train_bot-20260309T105826Z-332599446cd3\n'
    fi
    ;;
  *)
    ;;
esac
EOF_ADB
chmod +x "${BIN_DIR}/adb"

run_wrapper() {
  local log_prefix="$1"
  shift
  PATH="${BIN_DIR}:${PATH}" \
  FAKE_STATE_DIR="${STATE_DIR}" \
  FAKE_LOG_PREFIX="${log_prefix}" \
  PIXEL_RUN_ID="test-run-id" \
  "${ORCHESTRATOR_FIXTURE}/scripts/android/pixel_redeploy.sh" \
  --device fake-device \
  --scope train_bot \
  --mode auto \
  "$@"
}

read_lines_into_array() {
  local path="$1"
  local target_name="$2"
  local line=""
  eval "${target_name}=()"
  while IFS= read -r line; do
    eval "${target_name}+=(\"\${line}\")"
  done < "${path}"
}

auto_log="${TMP_ROOT}/auto.log"
if ! run_wrapper auto >"${auto_log}" 2>&1; then
  echo "FAIL: pixel_redeploy.sh should succeed in the auto build/install fixture" >&2
  cat "${auto_log}" >&2
  exit 1
fi

read_lines_into_array "${STATE_DIR}/auto-deploy-invocations.log" auto_invocations
if [[ "${#auto_invocations[@]}" -lt 2 ]]; then
  echo "FAIL: expected multiple orchestrator deploy invocations in auto mode" >&2
  cat "${auto_log}" >&2
  exit 1
fi

if [[ "${auto_invocations[0]}" == *"--skip-build"* ]]; then
  echo "FAIL: first deploy invocation should install the freshly built APK before action dispatch" >&2
  printf '%s\n' "${auto_invocations[@]}" >&2
  exit 1
fi

if [[ "${auto_invocations[1]}" != *"--skip-build"* ]]; then
  echo "FAIL: subsequent deploy invocation should reuse the freshly installed APK" >&2
  printf '%s\n' "${auto_invocations[@]}" >&2
  exit 1
fi

if [[ "$(grep -c '^build$' "${STATE_DIR}/apk-builds.log")" != "1" ]]; then
  echo "FAIL: auto mode should build the orchestrator APK exactly once" >&2
  cat "${STATE_DIR}/apk-builds.log" >&2
  exit 1
fi

rm -f "${STATE_DIR}/remote-action-result.json"

skip_log="${TMP_ROOT}/skip.log"
if ! run_wrapper skip --skip-build >"${skip_log}" 2>&1; then
  echo "FAIL: pixel_redeploy.sh should succeed in skip-build mode" >&2
  cat "${skip_log}" >&2
  exit 1
fi

read_lines_into_array "${STATE_DIR}/skip-deploy-invocations.log" skip_invocations
if [[ "${#skip_invocations[@]}" -lt 2 ]]; then
  echo "FAIL: expected multiple orchestrator deploy invocations in skip-build mode" >&2
  cat "${skip_log}" >&2
  exit 1
fi

for invocation in "${skip_invocations[@]}"; do
  if [[ "${invocation}" != *"--skip-build"* ]]; then
    echo "FAIL: skip-build mode should preserve --skip-build on every deploy invocation" >&2
    printf '%s\n' "${skip_invocations[@]}" >&2
    exit 1
  fi
done

echo "PASS: pixel redeploy wrapper installs a fresh APK once, then reuses it for later deploy actions"
