#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SIM_CONFIG_ROOT="${REPO_ROOT}/infra/arbuzas/android-sim"
SIM_COMPOSE_FILE="${SIM_CONFIG_ROOT}/compose.yml"
REMOTE_SIM_ROOT="/etc/arbuzas/android-sim"
REMOTE_STATE_ROOT="/srv/arbuzas/android-sim"
REMOTE_COMPOSE_FILE="${REMOTE_SIM_ROOT}/compose.yml"
COMPOSE_PROJECT="arbuzas-android-sim"
ROOT_FALLBACK_IMAGE="${ROOT_FALLBACK_IMAGE:-busybox:1.36.1}"

ARBUZAS_HOST="${ARBUZAS_HOST:-arbuzas}"
ARBUZAS_USER="${ARBUZAS_USER:-${USER}}"
ARBUZAS_SSH_PORT="${ARBUZAS_SSH_PORT:-}"
ARBUZAS_ANDROID_SIM_VARIANT="${ARBUZAS_ANDROID_SIM_VARIANT:-google-apis}"
ARBUZAS_ANDROID_SIM_MEMORY="${ARBUZAS_ANDROID_SIM_MEMORY:-4096}"
ARBUZAS_ANDROID_SIM_CORES="${ARBUZAS_ANDROID_SIM_CORES:-2}"
ARBUZAS_ANDROID_SIM_CONSOLE_PORT="${ARBUZAS_ANDROID_SIM_CONSOLE_PORT:-15554}"
ARBUZAS_ANDROID_SIM_ADB_PORT="${ARBUZAS_ANDROID_SIM_ADB_PORT:-15555}"
ARBUZAS_ANDROID_SIM_TICKET_REMOTE_PORT="${ARBUZAS_ANDROID_SIM_TICKET_REMOTE_PORT:-19338}"
ARBUZAS_ANDROID_SIM_ACCRESCENT_APK_URL="${ARBUZAS_ANDROID_SIM_ACCRESCENT_APK_URL:-https://accrescent.app/accrescent.apk}"
ARBUZAS_ANDROID_SIM_AURORA_APK_URL="${ARBUZAS_ANDROID_SIM_AURORA_APK_URL:-https://f-droid.org/repo/com.aurora.store_71.apk}"
ARBUZAS_ANDROID_SIM_VIVI_PACKAGE="${ARBUZAS_ANDROID_SIM_VIVI_PACKAGE:-com.pv.vivi}"
ARBUZAS_ANDROID_SIM_FORCE_STORE_INSTALL="${ARBUZAS_ANDROID_SIM_FORCE_STORE_INSTALL:-0}"
ARBUZAS_ANDROID_SIM_DISPLAY_SIZE="${ARBUZAS_ANDROID_SIM_DISPLAY_SIZE:-540x960}"
ARBUZAS_ANDROID_SIM_DISPLAY_DENSITY="${ARBUZAS_ANDROID_SIM_DISPLAY_DENSITY:-220}"
ARBUZAS_ANDROID_SIM_DISPLAY_PROFILE="${ARBUZAS_ANDROID_SIM_DISPLAY_PROFILE:-aggressive-low-load}"
ARBUZAS_ANDROID_SIM_COMPARISON_CORES="${ARBUZAS_ANDROID_SIM_COMPARISON_CORES:-}"
ARBUZAS_ANDROID_SIM_MIN_DOCKER_FREE_KB="${ARBUZAS_ANDROID_SIM_MIN_DOCKER_FREE_KB:-12000000}"
ARBUZAS_ANDROID_SIM_RESOURCE_PROFILE="stable-6gb-total-4gb-guest-2core-noswap"

action=""
variant_arg_seen=0
LAST_VIVI_REPORT_DIR=""

usage() {
  cat <<USAGE
Usage: $(basename "$0") ACTION [options]

Actions:
  preflight   Check Arbuzas host readiness without starting the emulator
  launch      Stage the private simulator compose file and start one variant
  validate    Wait for Android, install Accrescent/Aurora, and try to open/source ViVi
  vivi-test   Run the no-login ViVi store UI and responsiveness trial, then stop the stack
  compare-cores
             Compatibility alias for the normal 4096 MB guest / 6 GB total / 2 core vivi-test
  benchmark   Run validate, sample load, and write an evidence report
  report      Write a current evidence report without changing simulator state
  reset-state Remove the persistent Android AVD state for one variant
  stop        Stop the private simulator stack

Options:
  --variant NAME     google-apis or playstore (default: google-apis)
  --comparison-cores N
                     Opt-in comparison run; only 2 is supported
  --force-store-install
                     Reinstall Accrescent/Aurora even when already present
  --ssh-host HOST    Arbuzas SSH host (default: arbuzas)
  --ssh-user USER    Arbuzas SSH user (default: current user)
  --ssh-port PORT    SSH port
  -h, --help         Show this help

The normal trial is intentionally fixed at 4096 MB Android guest RAM, 6 GB total
container memory, and 2 cores with swap disabled.
compare-cores and vivi-test --comparison-cores 2 are compatibility aliases.
USAGE
}

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "$*" >&2
}

die() {
  echo "android-sim: $*" >&2
  exit 1
}

remote_target() {
  printf '%s@%s' "${ARBUZAS_USER}" "${ARBUZAS_HOST}"
}

run_ssh() {
  local -a args=()
  if [[ -n "${ARBUZAS_SSH_PORT}" ]]; then
    args+=(-p "${ARBUZAS_SSH_PORT}")
  fi
  ssh ${args[@]+"${args[@]}"} "$@"
}

remote_shell() {
  local script="$1"
  {
    printf '%s\n' 'set -euo pipefail'
    printf '%s\n' "${script}"
  } | run_ssh "$(remote_target)" 'bash -s'
}

shell_quote() {
  printf '%q' "$1"
}

require_fixed_resources() {
  [[ "${ARBUZAS_ANDROID_SIM_MEMORY}" == "4096" ]] || die "ARBUZAS_ANDROID_SIM_MEMORY must stay 4096 for this trial"
  [[ "${ARBUZAS_ANDROID_SIM_CORES}" == "2" ]] || die "ARBUZAS_ANDROID_SIM_CORES must stay 2 for this trial"
}

variant_image() {
  case "$1" in
    google-apis) printf '%s\n' 'halimqarroum/docker-android:api-33' ;;
    playstore) printf '%s\n' 'halimqarroum/docker-android:api-33-playstore' ;;
    *) die "unsupported --variant ${1}; use google-apis or playstore" ;;
  esac
}

variant_dir() {
  case "$1" in
    google-apis) printf '%s\n' 'google-apis' ;;
    playstore) printf '%s\n' 'playstore' ;;
    *) die "unsupported --variant ${1}; use google-apis or playstore" ;;
  esac
}

remote_env_file() {
  printf '%s/%s.env\n' "${REMOTE_SIM_ROOT}" "$(variant_dir "${ARBUZAS_ANDROID_SIM_VARIANT}")"
}

remote_compose_prefix() {
  local env_file
  env_file="$(remote_env_file)"
  printf "docker compose --project-name %q --env-file %q -f %q" \
    "${COMPOSE_PROJECT}" "${env_file}" "${REMOTE_COMPOSE_FILE}"
}

parse_args() {
  if (( $# == 0 )); then
    usage >&2
    exit 2
  fi
  action="$1"
  shift

  while (( $# > 0 )); do
    case "$1" in
      --variant)
        shift
        ARBUZAS_ANDROID_SIM_VARIANT="${1:-}"
        variant_arg_seen=1
        ;;
      --ssh-host)
        shift
        ARBUZAS_HOST="${1:-}"
        ;;
      --ssh-user)
        shift
        ARBUZAS_USER="${1:-}"
        ;;
      --ssh-port)
        shift
        ARBUZAS_SSH_PORT="${1:-}"
        ;;
      --comparison-cores)
        shift
        ARBUZAS_ANDROID_SIM_COMPARISON_CORES="${1:-}"
        ;;
      --force-store-install)
        ARBUZAS_ANDROID_SIM_FORCE_STORE_INSTALL=1
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "unknown argument: $1"
        ;;
    esac
    shift
  done

  case "${action}" in
    preflight|launch|validate|vivi-test|compare-cores|benchmark|report|reset-state|stop) ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown action: ${action}"
      ;;
  esac

  if [[ "${action}" == "compare-cores" && -z "${ARBUZAS_ANDROID_SIM_COMPARISON_CORES}" ]]; then
    ARBUZAS_ANDROID_SIM_COMPARISON_CORES=2
  fi

  if [[ -n "${ARBUZAS_ANDROID_SIM_COMPARISON_CORES}" ]]; then
    [[ "${action}" == "vivi-test" || "${action}" == "compare-cores" ]] || die "--comparison-cores is only allowed for vivi-test or compare-cores"
    [[ "${ARBUZAS_ANDROID_SIM_COMPARISON_CORES}" == "2" ]] || die "only --comparison-cores 2 is supported"
    ARBUZAS_ANDROID_SIM_CORES=2
    ARBUZAS_ANDROID_SIM_RESOURCE_PROFILE="stable-6gb-total-4gb-guest-2core-noswap"
  fi

  require_fixed_resources
  variant_image "${ARBUZAS_ANDROID_SIM_VARIANT}" >/dev/null
}

resolve_remote_release_id_script() {
  cat <<'REMOTE'
release_id=""
if [[ -f /etc/arbuzas/current/release.env ]]; then
  release_id="$(awk -F= '$1=="ARBUZAS_RELEASE_ID" {print $2; exit}' /etc/arbuzas/current/release.env)"
fi
if [[ -z "${release_id}" ]]; then
  echo "missing /etc/arbuzas/current/release.env with ARBUZAS_RELEASE_ID" >&2
  exit 1
fi
printf '%s\n' "${release_id}"
REMOTE
}

prepare_remote_layout() {
  local compose_base64 image variant data_dir env_file release_script cpu_quota
  compose_base64="$(base64 < "${SIM_COMPOSE_FILE}" | tr -d '\n')"
  image="$(variant_image "${ARBUZAS_ANDROID_SIM_VARIANT}")"
  variant="$(variant_dir "${ARBUZAS_ANDROID_SIM_VARIANT}")"
  data_dir="${REMOTE_STATE_ROOT}/${variant}/avd"
  env_file="$(remote_env_file)"
  release_script="$(resolve_remote_release_id_script)"
  cpu_quota="${ARBUZAS_ANDROID_SIM_CORES}.0"

  log "Staging private Android simulator config on ${ARBUZAS_HOST}"
  remote_shell "
    mkdir -p '${REMOTE_SIM_ROOT}'
    printf '%s' '${compose_base64}' | base64 -d > '${REMOTE_COMPOSE_FILE}'

    state_mode=fresh
    if docker run --rm \
      -v /srv/arbuzas:/srv/arbuzas \
      '${ROOT_FALLBACK_IMAGE}' \
      sh -c '[ -d /srv/arbuzas/android-sim/${variant}/avd ] && find /srv/arbuzas/android-sim/${variant}/avd -mindepth 1 -print -quit 2>/dev/null | grep -q .' >/dev/null 2>&1; then
      state_mode=warm
    fi

    docker run --rm \
      -v /srv/arbuzas:/srv/arbuzas \
      '${ROOT_FALLBACK_IMAGE}' \
      sh -c 'mkdir -p /srv/arbuzas/android-sim/${variant}/avd /srv/arbuzas/android-sim/apks /srv/arbuzas/android-sim/evidence && chown -R 1000:1000 /srv/arbuzas/android-sim' >/dev/null

    release_id=\$(${release_script})
    cat > '${env_file}' <<EOF_ENV
ARBUZAS_ANDROID_SIM_IMAGE=${image}
ARBUZAS_ANDROID_SIM_RELEASE_ID=\${release_id}
ARBUZAS_ANDROID_SIM_DATA_DIR=${data_dir}
ARBUZAS_ANDROID_SIM_ROOT_DIR=${REMOTE_STATE_ROOT}
ARBUZAS_ANDROID_SIM_APK_DIR=${REMOTE_STATE_ROOT}/apks
ARBUZAS_ANDROID_SIM_MEMORY=${ARBUZAS_ANDROID_SIM_MEMORY}
ARBUZAS_ANDROID_SIM_MEMORY_LIMIT=6g
ARBUZAS_ANDROID_SIM_MEMORY_SWAP_LIMIT=6g
ARBUZAS_ANDROID_SIM_MEMORY_SWAPPINESS=0
ARBUZAS_ANDROID_SIM_CORES=${ARBUZAS_ANDROID_SIM_CORES}
ARBUZAS_ANDROID_SIM_CPUS=${cpu_quota}
ARBUZAS_ANDROID_SIM_RESOURCE_PROFILE=${ARBUZAS_ANDROID_SIM_RESOURCE_PROFILE}
ARBUZAS_ANDROID_SIM_STATE_MODE=\${state_mode}
ARBUZAS_ANDROID_SIM_DISPLAY_PROFILE=${ARBUZAS_ANDROID_SIM_DISPLAY_PROFILE}
ARBUZAS_ANDROID_SIM_DISPLAY_SIZE=${ARBUZAS_ANDROID_SIM_DISPLAY_SIZE}
ARBUZAS_ANDROID_SIM_DISPLAY_DENSITY=${ARBUZAS_ANDROID_SIM_DISPLAY_DENSITY}
ARBUZAS_ANDROID_SIM_FORCE_STORE_INSTALL=${ARBUZAS_ANDROID_SIM_FORCE_STORE_INSTALL}
ARBUZAS_ANDROID_SIM_CONSOLE_PORT=${ARBUZAS_ANDROID_SIM_CONSOLE_PORT}
ARBUZAS_ANDROID_SIM_ADB_PORT=${ARBUZAS_ANDROID_SIM_ADB_PORT}
ARBUZAS_ANDROID_SIM_TICKET_REMOTE_PORT=${ARBUZAS_ANDROID_SIM_TICKET_REMOTE_PORT}
ARBUZAS_TZ=Europe/Riga
EOF_ENV

    cat > '${REMOTE_STATE_ROOT}/restore-aggressive-packages.sh' <<'RESTORE'
#!/usr/bin/env bash
set -euo pipefail
container="${1:-arbuzas-android-sim-android_sim_ticket_bridge-1}"
adb_target="${2:-android_sim:5555}"
docker exec "${container}" sh -s -- "${adb_target}" <<'BRIDGE'
set -eu
adb_target="$1"
adb connect "${adb_target}" >/dev/null 2>&1 || true
packages='
com.google.android.gm
com.google.android.apps.maps
com.google.android.apps.photos
com.google.android.apps.youtube.music
com.google.android.apps.docs
com.google.android.googlequicksearchbox
com.google.android.apps.wellbeing
com.google.android.apps.wallpaper
com.google.android.apps.wallpaper.nexus
com.google.android.apps.customization.pixel
com.google.android.feedback
com.google.android.apps.restore
com.google.android.onetimeinitializer
com.google.android.partnersetup
com.google.android.projection.gearhead
com.google.android.tts
com.google.android.dialer
com.google.android.contacts
com.google.android.calendar
com.google.android.apps.messaging
com.google.android.deskclock
com.google.android.soundpicker
com.google.android.cellbroadcastreceiver
com.google.android.cellbroadcastservice
com.google.android.tag
com.google.android.printservice.recommendation
'
for package in ${packages}; do
  if adb -s "${adb_target}" shell pm path "${package}" >/dev/null 2>&1; then
    adb -s "${adb_target}" shell su 0 cmd package enable --user 0 "${package}" >/dev/null 2>&1 || true
    echo "package_restore package=${package}"
  fi
done
BRIDGE
RESTORE
    chmod 0755 '${REMOTE_STATE_ROOT}/restore-aggressive-packages.sh'
  "
}

preflight_remote() {
  local ports
  ports="${ARBUZAS_ANDROID_SIM_CONSOLE_PORT} ${ARBUZAS_ANDROID_SIM_ADB_PORT} ${ARBUZAS_ANDROID_SIM_TICKET_REMOTE_PORT}"
  log "Running Arbuzas Android simulator preflight"
  remote_shell "
    command -v docker >/dev/null 2>&1 || { echo 'docker is required' >&2; exit 1; }
    docker compose version >/dev/null 2>&1 || { echo 'docker compose is required' >&2; exit 1; }
    command -v curl >/dev/null 2>&1 || { echo 'curl is required' >&2; exit 1; }
    [[ -e /dev/kvm ]] || { echo '/dev/kvm is missing' >&2; exit 1; }
    [[ -r /etc/arbuzas/secrets/android-adb/adbkey ]] || { echo 'missing /etc/arbuzas/secrets/android-adb/adbkey' >&2; exit 1; }
    [[ -r /etc/arbuzas/secrets/android-adb/adbkey.pub ]] || { echo 'missing /etc/arbuzas/secrets/android-adb/adbkey.pub' >&2; exit 1; }
    release_id=\$($(resolve_remote_release_id_script))
    docker image inspect \"arbuzas/ticket-phone-bridge:\${release_id}\" >/dev/null
    docker image inspect \"arbuzas/ticket-remote:\${release_id}\" >/dev/null
    for port in ${ports}; do
      if ss -H -ltn 2>/dev/null | awk '{print \$4}' | grep -E '(^|:)'\${port}'$' >/dev/null; then
        echo \"port \${port} is already listening\" >&2
        exit 1
      fi
    done
    free_kb=\$(df -Pk /var/lib/docker 2>/dev/null | awk 'NR==2 {print \$4}')
    if [[ -z \"\${free_kb}\" || \"\${free_kb}\" -lt ${ARBUZAS_ANDROID_SIM_MIN_DOCKER_FREE_KB} ]]; then
      echo \"at least 12 GB free under Docker storage is required for one simulator variant\" >&2
      exit 1
    fi
    echo 'preflight ok'
    printf 'host: '; hostname
    printf 'kvm: '; ls -l /dev/kvm
    printf 'docker: '; docker --version
    printf 'compose: '; docker compose version
    printf 'memory: '; free -h | awk '/Mem:/ {print \$2 \" total, \" \$7 \" available\"}'
    printf 'disk: '; df -h /var/lib/docker | awk 'NR==2 {print \$4 \" free at \" \$6}'
  "
}

launch_remote() {
  local compose
  prepare_remote_layout
  compose="$(remote_compose_prefix)"
  log "Starting ${ARBUZAS_ANDROID_SIM_VARIANT} Android simulator on private Arbuzas ports"
  remote_shell "
    ${compose} pull android_sim
    ${compose} up -d android_sim android_sim_tuner android_sim_ticket_bridge ticket_remote_sim
    ${compose} ps
  "
}

adb_in_bridge() {
  local command="$1"
  local compose
  compose="$(remote_compose_prefix)"
  printf "%s exec -T android_sim_ticket_bridge sh -lc %q" "${compose}" "${command}"
}

wait_for_android_remote() {
  local adb_wait adb_boot adb_installer_probe adb_installer
  adb_wait="$(adb_in_bridge "adb connect android_sim:5555 >/dev/null 2>&1 || true; adb -s android_sim:5555 wait-for-device")"
  adb_boot="$(adb_in_bridge "adb -s android_sim:5555 shell getprop sys.boot_completed 2>/dev/null | tr -d '\r'")"
read -r -d '' adb_installer_probe <<'BRIDGE' || true
adb -s android_sim:5555 shell cmd package list packages android >/dev/null 2>&1 || exit 0
out="$(adb -s android_sim:5555 shell cmd package install-create -r -S 1 2>&1 | tr -d '\r' || true)"
session="$(printf '%s\n' "${out}" | sed -n 's/.*\[\([0-9][0-9]*\)\].*/\1/p')"
if printf '%s\n' "${out}" | grep -F 'Success:' >/dev/null 2>&1; then
  if [ -n "${session}" ]; then
    adb -s android_sim:5555 shell cmd package install-abandon "${session}" >/dev/null 2>&1 || true
  fi
  echo ready
else
  printf '%s\n' "${out}" >&2
fi
BRIDGE
  adb_installer="$(adb_in_bridge "${adb_installer_probe}")"
  log "Waiting for Android boot completion"
  remote_shell "
    start=\$(date +%s)
    timeout_at=\$((start + 360))
    ${adb_wait}
    while :; do
      booted=\$(${adb_boot} || true)
      if [[ \"\${booted}\" == '1' ]]; then
        now=\$(date +%s)
        echo \"android boot completed in \$((now - start))s\"
        break
      fi
      if [[ \$(date +%s) -ge \${timeout_at} ]]; then
        echo 'timed out waiting for Android boot completion' >&2
        exit 1
      fi
      sleep 5
    done
    installer_timeout_at=\$((\$(date +%s) + 180))
    while :; do
      installer_ready=\$(${adb_installer} || true)
      if [[ \"\${installer_ready}\" == 'ready' ]]; then
        now=\$(date +%s)
        echo \"android installer ready in \$((now - start))s\"
        break
      fi
      if [[ \$(date +%s) -ge \${installer_timeout_at} ]]; then
        echo 'Android installer never became ready' >&2
        exit 1
      fi
      sleep 5
    done
  "
}

apply_display_tuning_remote() {
  local tune_cmd bridge_script
  read -r -d '' bridge_script <<BRIDGE || true
set -e
adb connect android_sim:5555 >/dev/null 2>&1 || true

disable_android_swap() {
  for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    adb -s android_sim:5555 shell su 0 'swapoff /dev/block/zram0 2>/dev/null || swapoff -a 2>/dev/null || true; [ -e /sys/block/zram0/reset ] && echo 1 > /sys/block/zram0/reset 2>/dev/null || true; [ -e /sys/block/zram0/disksize ] && echo 0 > /sys/block/zram0/disksize 2>/dev/null || true' >/dev/null 2>&1 || true
    swap_total="\$(adb -s android_sim:5555 shell su 0 cat /proc/meminfo 2>/dev/null | tr -d '\r' | awk '/^SwapTotal:/ {print \$2; exit}')"
    if [ "\${swap_total:-}" = "0" ]; then
      echo "android_swap result=disabled swap_total_kb=\${swap_total}"
      return 0
    fi
    sleep 3
  done
  echo "Android simulator swap disable failed: SwapTotal=\${swap_total:-unknown} kB" >&2
  exit 1
}

disable_nonessential_package() {
  package="\$1"
  if ! adb -s android_sim:5555 shell pm path "\${package}" >/dev/null 2>&1; then
    return 0
  fi
  if adb -s android_sim:5555 shell su 0 cmd package disable-user --user 0 "\${package}" >/dev/null 2>&1; then
    echo "package_tune package=\${package} result=disabled-user"
  else
    adb -s android_sim:5555 shell am force-stop "\${package}" >/dev/null 2>&1 || true
    echo "package_tune package=\${package} result=force-stopped"
  fi
}

disable_nonessential_packages() {
  packages='
com.google.android.gm
com.google.android.apps.maps
com.google.android.apps.photos
com.google.android.apps.youtube.music
com.google.android.apps.docs
com.google.android.googlequicksearchbox
com.google.android.apps.wellbeing
com.google.android.apps.wallpaper
com.google.android.apps.wallpaper.nexus
com.google.android.apps.customization.pixel
com.google.android.feedback
com.google.android.apps.restore
com.google.android.onetimeinitializer
com.google.android.partnersetup
com.google.android.projection.gearhead
com.google.android.tts
com.google.android.dialer
com.google.android.contacts
com.google.android.calendar
com.google.android.apps.messaging
com.google.android.deskclock
com.google.android.soundpicker
com.google.android.cellbroadcastreceiver
com.google.android.cellbroadcastservice
com.google.android.tag
com.google.android.printservice.recommendation
'
  for package in \${packages}; do
    disable_nonessential_package "\${package}"
  done
}

disable_android_swap
actual_size=''
actual_density=''
for attempt in 1 2 3 4 5 6 7 8; do
  adb -s android_sim:5555 shell wm size '${ARBUZAS_ANDROID_SIM_DISPLAY_SIZE}' >/dev/null 2>&1 || true
  adb -s android_sim:5555 shell wm density '${ARBUZAS_ANDROID_SIM_DISPLAY_DENSITY}' >/dev/null 2>&1 || true
  actual_size="\$(adb -s android_sim:5555 shell wm size 2>/dev/null | tr -d '\r' || true)"
  actual_density="\$(adb -s android_sim:5555 shell wm density 2>/dev/null | tr -d '\r' || true)"
  if printf '%s\n' "\${actual_size}" | grep -F '${ARBUZAS_ANDROID_SIM_DISPLAY_SIZE}' >/dev/null 2>&1 &&
    printf '%s\n' "\${actual_density}" | grep -F '${ARBUZAS_ANDROID_SIM_DISPLAY_DENSITY}' >/dev/null 2>&1; then
    break
  fi
  sleep 5
done
adb -s android_sim:5555 shell settings put global window_animation_scale 0 >/dev/null 2>&1 || true
adb -s android_sim:5555 shell settings put global transition_animation_scale 0 >/dev/null 2>&1 || true
adb -s android_sim:5555 shell settings put global animator_duration_scale 0 >/dev/null 2>&1 || true
adb -s android_sim:5555 shell settings put global background_process_limit 2 >/dev/null 2>&1 || true
adb -s android_sim:5555 shell settings put global app_process_limit 2 >/dev/null 2>&1 || true
adb -s android_sim:5555 shell settings put global cached_apps_freezer enabled >/dev/null 2>&1 || true
adb -s android_sim:5555 shell settings put global wifi_scan_always_enabled 0 >/dev/null 2>&1 || true
adb -s android_sim:5555 shell settings put global ble_scan_always_enabled 0 >/dev/null 2>&1 || true
disable_nonessential_packages
disable_android_swap
echo 'display_profile=${ARBUZAS_ANDROID_SIM_DISPLAY_PROFILE}'
echo 'display_size_requested=${ARBUZAS_ANDROID_SIM_DISPLAY_SIZE}'
echo 'display_density_requested=${ARBUZAS_ANDROID_SIM_DISPLAY_DENSITY}'
echo 'avd_optimization background_process_limit=2 cached_apps_freezer=enabled scans=disabled'
printf '%s\n' "\${actual_size}"
printf '%s\n' "\${actual_density}"
BRIDGE
  tune_cmd="$(adb_in_bridge "${bridge_script}")"
  log "Applying aggressive no-swap AVD tuning"
  remote_shell "${tune_cmd}"
}

download_store_apks_remote() {
  log "Checking cached official store APKs on Arbuzas"
  remote_shell "
    mkdir -p '${REMOTE_STATE_ROOT}/apks'
    download_if_missing_or_stale() {
      label=\"\$1\"
      url=\"\$2\"
      apk=\"\$3\"
      if [[ -s \"\${apk}\" ]] && find \"\${apk}\" -mtime -7 -print -quit 2>/dev/null | grep -q .; then
        echo \"store_apk_cache label=\${label} result=hit path=\${apk}\"
        return 0
      fi
      echo \"store_apk_cache label=\${label} result=refresh path=\${apk}\"
      tmp=\"\${apk}.tmp\"
      curl -fL --retry 3 -o \"\${tmp}\" \"\${url}\"
      mv \"\${tmp}\" \"\${apk}\"
    }
    download_if_missing_or_stale Accrescent '$(shell_quote "${ARBUZAS_ANDROID_SIM_ACCRESCENT_APK_URL}")' '${REMOTE_STATE_ROOT}/apks/accrescent.apk'
    download_if_missing_or_stale Aurora '$(shell_quote "${ARBUZAS_ANDROID_SIM_AURORA_APK_URL}")' '${REMOTE_STATE_ROOT}/apks/aurora-store.apk'
  "
}

install_store_clients_remote() {
  local install_cmd
  install_cmd="$(adb_in_bridge "set -e
    adb connect android_sim:5555 >/dev/null 2>&1 || true
    install_apk() {
      label=\"\$1\"
      apk=\"\$2\"
      log_file=\"\$3\"
      for attempt in 1 2 3 4 5 6 7 8 9 10 11 12; do
        if adb -s android_sim:5555 install -r \"\${apk}\" >\"\${log_file}\" 2>&1; then
          cat \"\${log_file}\"
          return 0
        fi
        cat \"\${log_file}\" >&2
        if [ \"\${attempt}\" = '12' ]; then
          return 1
        fi
        echo \"\${label} install attempt \${attempt} failed; waiting for Android install services\" >&2
        sleep 10
      done
    }
    ensure_apk() {
      label=\"\$1\"
      package=\"\$2\"
      apk=\"\$3\"
      log_file=\"\$4\"
      if [ '${ARBUZAS_ANDROID_SIM_FORCE_STORE_INSTALL}' != '1' ] && adb -s android_sim:5555 shell pm path \"\${package}\" >/dev/null 2>&1; then
        echo \"store_client label=\${label} package=\${package} result=already-installed\"
        return 0
      fi
      echo \"store_client label=\${label} package=\${package} result=installing force=${ARBUZAS_ANDROID_SIM_FORCE_STORE_INSTALL}\"
      install_apk \"\${label}\" \"\${apk}\" \"\${log_file}\"
    }
    ensure_apk Accrescent app.accrescent.client /srv/android-sim/apks/accrescent.apk /tmp/accrescent-install.log
    ensure_apk Aurora com.aurora.store /srv/android-sim/apks/aurora-store.apk /tmp/aurora-install.log
    adb -s android_sim:5555 shell pm path app.accrescent.client >/dev/null
    adb -s android_sim:5555 shell pm path com.aurora.store >/dev/null
  ")"
  log "Installing Accrescent and Aurora Store in the emulator"
  remote_shell "${install_cmd}"
}

try_vivi_sources_remote() {
  local source_cmd
  source_cmd="$(adb_in_bridge "set -e
    target='${ARBUZAS_ANDROID_SIM_VIVI_PACKAGE}'
    adb connect android_sim:5555 >/dev/null 2>&1 || true
    if adb -s android_sim:5555 shell pm path \"\${target}\" >/dev/null 2>&1; then
      echo 'vivi already installed'
      exit 0
    fi
    echo 'opening Accrescent for ViVi'
    adb -s android_sim:5555 shell monkey -p app.accrescent.client 1 >/dev/null 2>&1 || true
    adb -s android_sim:5555 shell am start -a android.intent.action.VIEW -d 'https://accrescent.app/app/com.pv.vivi' >/dev/null 2>&1 || true
    sleep 12
    if adb -s android_sim:5555 shell pm path \"\${target}\" >/dev/null 2>&1; then
      echo 'vivi installed from Accrescent flow'
      exit 0
    fi
    echo 'opening Aurora Store for ViVi'
    adb -s android_sim:5555 shell monkey -p com.aurora.store 1 >/dev/null 2>&1 || true
    adb -s android_sim:5555 shell am start -a android.intent.action.VIEW -d 'market://details?id=com.pv.vivi' com.aurora.store >/dev/null 2>&1 || true
    sleep 12
    if adb -s android_sim:5555 shell pm path \"\${target}\" >/dev/null 2>&1; then
      echo 'vivi installed from Aurora flow'
      exit 0
    fi
    echo 'vivi_source_blocked: Accrescent and Aurora were opened, but ViVi is not installed without in-store user action'
    exit 20
  ")"
  log "Trying to source ViVi from Accrescent, then Aurora"
  set +e
  remote_shell "${source_cmd}"
  return_code=$?
  set -e
  if [[ "${return_code}" == "0" ]]; then
    return 0
  fi
  if [[ "${return_code}" == "20" ]]; then
    return 20
  fi
  return "${return_code}"
}

launch_vivi_if_installed_remote() {
  local launch_cmd
  launch_cmd="$(adb_in_bridge "set -e
    target='${ARBUZAS_ANDROID_SIM_VIVI_PACKAGE}'
    adb connect android_sim:5555 >/dev/null 2>&1 || true
    adb -s android_sim:5555 shell pm path \"\${target}\" >/dev/null 2>&1 || {
      echo 'vivi_open_blocked: package is not installed'
      exit 20
    }
    adb -s android_sim:5555 shell monkey -p \"\${target}\" 1 >/dev/null
    sleep 5
    adb -s android_sim:5555 shell dumpsys window windows 2>/dev/null | grep -F \"\${target}\" >/dev/null || {
      echo 'vivi_open_blocked: package installed but not visible in foreground'
      exit 21
    }
    echo 'vivi_open_ok'
  ")"
  log "Opening ViVi if it is installed"
  remote_shell "${launch_cmd}"
}

validate_ticket_remote_sim() {
  local compose
  compose="$(remote_compose_prefix)"
  log "Validating private ticket_remote_sim health"
  remote_shell "
    ${compose} ps
    for i in \$(seq 1 30); do
      if curl -fsS 'http://127.0.0.1:${ARBUZAS_ANDROID_SIM_TICKET_REMOTE_PORT}/api/v1/health' >/dev/null 2>&1; then
        curl -fsS 'http://127.0.0.1:${ARBUZAS_ANDROID_SIM_TICKET_REMOTE_PORT}/api/v1/health' | head -c 1200
        printf '\n'
        exit 0
      fi
      sleep 2
    done
    echo 'ticket_remote_sim health did not respond on localhost' >&2
    exit 1
  "
}

validate_remote() {
  wait_for_android_remote
  apply_display_tuning_remote
  download_store_apks_remote
  install_store_clients_remote
  local vivi_source_status=0
  try_vivi_sources_remote || vivi_source_status=$?
  if [[ "${vivi_source_status}" == "0" ]]; then
    launch_vivi_if_installed_remote
  elif [[ "${vivi_source_status}" == "20" ]]; then
    log "ViVi source step is blocked on in-store install action; continuing with private health checks"
  else
    return "${vivi_source_status}"
  fi
  validate_ticket_remote_sim
}

report_remote() {
  local compose report_dir report_file adb_props ticket_health
  compose="$(remote_compose_prefix)"
  report_dir="${REMOTE_STATE_ROOT}/evidence/$(date -u +%Y%m%dT%H%M%SZ)-$(variant_dir "${ARBUZAS_ANDROID_SIM_VARIANT}")"
  report_file="${report_dir}/summary.txt"
  adb_props="$(adb_in_bridge "adb connect android_sim:5555 >/dev/null 2>&1 || true
    printf 'boot_completed='; adb -s android_sim:5555 shell getprop sys.boot_completed 2>/dev/null | tr -d '\r' || true
    printf 'vivi_installed='; adb -s android_sim:5555 shell pm path '${ARBUZAS_ANDROID_SIM_VIVI_PACKAGE}' >/dev/null 2>&1 && echo yes || echo no
    printf 'accrescent_installed='; adb -s android_sim:5555 shell pm path app.accrescent.client >/dev/null 2>&1 && echo yes || echo no
    printf 'aurora_installed='; adb -s android_sim:5555 shell pm path com.aurora.store >/dev/null 2>&1 && echo yes || echo no
    printf 'foreground='; adb -s android_sim:5555 shell dumpsys window windows 2>/dev/null | awk -F= '/mCurrentFocus/ {print \$NF; exit}' | tr -d '\r' || true
    printf 'display_size='; adb -s android_sim:5555 shell wm size 2>/dev/null | tr -d '\r' || true
    printf 'display_density='; adb -s android_sim:5555 shell wm density 2>/dev/null | tr -d '\r' || true
    printf 'swap_total_kb='; adb -s android_sim:5555 shell su 0 cat /proc/meminfo 2>/dev/null | tr -d '\r' | awk '/^SwapTotal:/ {print \$2; exit}' || true
    printf 'cached_apps_freezer='; adb -s android_sim:5555 shell settings get global cached_apps_freezer 2>/dev/null | tr -d '\r' || true
    printf 'background_process_limit='; adb -s android_sim:5555 shell settings get global background_process_limit 2>/dev/null | tr -d '\r' || true
  ")"
  ticket_health="curl -fsS 'http://127.0.0.1:${ARBUZAS_ANDROID_SIM_TICKET_REMOTE_PORT}/api/v1/health' 2>/dev/null | head -c 4000 || true"

  log "Writing simulator evidence report on Arbuzas"
  remote_shell "
    mkdir -p '${report_dir}'
    if [[ -f '$(remote_env_file)' ]]; then
      set -a
      source '$(remote_env_file)'
      set +a
    fi
    {
      echo 'Arbuzas Android simulator report'
      echo 'variant: ${ARBUZAS_ANDROID_SIM_VARIANT}'
      echo \"target_resources: \${ARBUZAS_ANDROID_SIM_MEMORY:-4096} MB Android guest, 6 GB total container, \${ARBUZAS_ANDROID_SIM_CORES:-2} core(s), no swap\"
      echo \"resource_profile: \${ARBUZAS_ANDROID_SIM_RESOURCE_PROFILE:-stable-6gb-total-4gb-guest-2core-noswap}\"
      echo \"state_mode: \${ARBUZAS_ANDROID_SIM_STATE_MODE:-unknown}\"
      echo \"display_profile: \${ARBUZAS_ANDROID_SIM_DISPLAY_PROFILE:-unknown}\"
      echo \"display_size_requested: \${ARBUZAS_ANDROID_SIM_DISPLAY_SIZE:-unknown}\"
      echo \"display_density_requested: \${ARBUZAS_ANDROID_SIM_DISPLAY_DENSITY:-unknown}\"
      echo
      echo '[tuning_status]'
      cat '${REMOTE_STATE_ROOT}/status/tuning-status.env' 2>/dev/null || true
      echo
      echo '[host]'
      hostname
      uname -srmo
      free -h
      df -h / /var/lib/docker /srv 2>/dev/null || true
      for f in /sys/class/thermal/thermal_zone*/temp /sys/devices/platform/coretemp.0/hwmon/hwmon*/temp*_input; do
        [[ -r \"\$f\" ]] && printf '%s=%s\n' \"\$f\" \"\$(cat \"\$f\")\"
      done
      echo
      echo '[compose]'
      ${compose} ps || true
      echo
      echo '[docker-stats]'
      container_ids=\$(${compose} ps -q 2>/dev/null | tr '\n' ' ')
      if [[ -n \"\${container_ids}\" ]]; then
        docker stats --no-stream --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}\t{{.BlockIO}}' \${container_ids} 2>/dev/null || true
      fi
      echo
      echo '[android]'
      ${adb_props} || true
      echo
      echo '[ticket_remote_sim_health]'
      ${ticket_health}
    } | tee '${report_file}'
    echo '${report_file}'
  "
}

append_report_remote() {
  local report_file="$1"
  local text="$2"
  remote_shell "printf '%s\n' $(shell_quote "${text}") >> '${report_file}'"
}

measure_step_remote() {
  local report_file="$1"
  local label="$2"
  shift 2

  local start ended status
  start="$(date +%s)"
  set +e
  "$@"
  status=$?
  set -e
  ended="$(date +%s)"

  append_report_remote "${report_file}" "metric_${label}_seconds=$((ended - start)) status=${status}"
  return "${status}"
}

init_vivi_evidence_remote() {
  local variant report_dir report_file
  variant="$(variant_dir "${ARBUZAS_ANDROID_SIM_VARIANT}")"
  report_dir="${REMOTE_STATE_ROOT}/evidence/$(date -u +%Y%m%dT%H%M%SZ)-${variant}-${ARBUZAS_ANDROID_SIM_RESOURCE_PROFILE}-vivi-test"
  report_file="${report_dir}/summary.txt"

  remote_shell "
    mkdir -p '${report_dir}/screens'
    if [[ -f '$(remote_env_file)' ]]; then
      set -a
      source '$(remote_env_file)'
      set +a
    fi
    {
      echo 'ViVi emulator responsiveness test'
      echo 'variant: ${ARBUZAS_ANDROID_SIM_VARIANT}'
      echo \"target_resources: \${ARBUZAS_ANDROID_SIM_MEMORY:-4096} MB Android guest, 6 GB total container, \${ARBUZAS_ANDROID_SIM_CORES:-2} core(s), no swap\"
      echo \"resource_profile: \${ARBUZAS_ANDROID_SIM_RESOURCE_PROFILE:-${ARBUZAS_ANDROID_SIM_RESOURCE_PROFILE}}\"
      echo \"state_mode: \${ARBUZAS_ANDROID_SIM_STATE_MODE:-unknown}\"
      echo \"display_profile: \${ARBUZAS_ANDROID_SIM_DISPLAY_PROFILE:-${ARBUZAS_ANDROID_SIM_DISPLAY_PROFILE}}\"
      echo \"display_size_requested: \${ARBUZAS_ANDROID_SIM_DISPLAY_SIZE:-${ARBUZAS_ANDROID_SIM_DISPLAY_SIZE}}\"
      echo \"display_density_requested: \${ARBUZAS_ANDROID_SIM_DISPLAY_DENSITY:-${ARBUZAS_ANDROID_SIM_DISPLAY_DENSITY}}\"
      echo \"force_store_install: \${ARBUZAS_ANDROID_SIM_FORCE_STORE_INSTALL:-${ARBUZAS_ANDROID_SIM_FORCE_STORE_INSTALL}}\"
      echo 'store_apk_cache: refresh only when missing or older than 7 days'
      echo 'package: ${ARBUZAS_ANDROID_SIM_VIVI_PACKAGE}'
      echo 'source_policy: no login, official store UI only, no APK mirrors'
      echo 'created_at_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)'
    } > '${report_file}'
    printf '%s\n' '${report_dir}'
  "
}

capture_responsiveness_snapshot_remote() {
  local report_file="$1"
  local label="$2"
  local compose
  compose="$(remote_compose_prefix)"

  remote_shell "
    {
      echo
      echo '[snapshot:${label}]'
      date -Is
      echo
      echo '[host-memory]'
      free -h
      echo
      echo '[host-disk]'
      df -h / /var/lib/docker /srv 2>/dev/null || true
      echo
      echo '[store-apks]'
      ls -lh '${REMOTE_STATE_ROOT}/apks' 2>/dev/null || true
      echo
      echo '[host-temperature]'
      for f in /sys/class/thermal/thermal_zone*/temp /sys/devices/platform/coretemp.0/hwmon/hwmon*/temp*_input; do
        [[ -r \"\$f\" ]] && printf '%s=%s\n' \"\$f\" \"\$(cat \"\$f\")\"
      done
      echo
      echo '[compose]'
      ${compose} ps || true
      echo
      echo '[docker-stats]'
      container_ids=\$(${compose} ps -q 2>/dev/null | tr '\n' ' ')
      if [[ -n \"\${container_ids}\" ]]; then
        docker stats --no-stream --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}\t{{.BlockIO}}' \${container_ids} 2>/dev/null || true
      fi
      echo
      echo '[display]'
      ${compose} exec -T android_sim_ticket_bridge sh -lc 'adb connect android_sim:5555 >/dev/null 2>&1 || true; adb -s android_sim:5555 shell wm size 2>/dev/null | tr -d \"\r\" || true; adb -s android_sim:5555 shell wm density 2>/dev/null | tr -d \"\r\" || true' || true
    } >> '${report_file}'
  "
}

capture_android_screen_remote() {
  local report_dir="$1"
  local name="$2"
  local compose
  compose="$(remote_compose_prefix)"

  remote_shell "
    mkdir -p '${report_dir}/screens'
    ${compose} exec -T android_sim_ticket_bridge sh -lc 'adb connect android_sim:5555 >/dev/null 2>&1 || true; adb -s android_sim:5555 exec-out screencap -p' > '${report_dir}/screens/${name}.png' || true
    ${compose} exec -T android_sim_ticket_bridge sh -lc 'adb connect android_sim:5555 >/dev/null 2>&1 || true; adb -s android_sim:5555 shell uiautomator dump /sdcard/window.xml >/dev/null 2>&1; adb -s android_sim:5555 exec-out cat /sdcard/window.xml 2>/dev/null' > '${report_dir}/screens/${name}.xml' || true
  "
}

run_vivi_ui_responsiveness_remote() {
  local report_file="$1"
  local report_dir="$2"
  local bridge_script bridge_cmd

  read -r -d '' bridge_script <<'BRIDGE' || true
set +e
target='__VIVI_PACKAGE__'
adb_target='android_sim:5555'

adb connect "${adb_target}" >/dev/null 2>&1 || true

epoch_ms() {
  value="$(date +%s%3N 2>/dev/null)"
  case "${value}" in
    *N*|'') echo "$(($(date +%s) * 1000))" ;;
    *) echo "${value}" ;;
  esac
}

elapsed_ms() {
  start_ms="$1"
  end_ms="$(epoch_ms)"
  echo "$((end_ms - start_ms))"
}

focus_line() {
  adb -s "${adb_target}" shell dumpsys window windows 2>/dev/null \
    | awk -F= '/mCurrentFocus/ {print $NF; exit}' \
    | tr -d '\r'
}

package_installed() {
  adb -s "${adb_target}" shell pm path "${target}" >/dev/null 2>&1
}

dump_xml() {
  adb -s "${adb_target}" shell uiautomator dump /sdcard/window.xml >/dev/null 2>&1 \
    && adb -s "${adb_target}" exec-out cat /sdcard/window.xml 2>/dev/null | tr -d '\r'
}

screen_hash() {
  dump_xml | cksum | awk '{print $1}'
}

visible_texts() {
  dump_xml \
    | tr '>' '>\n' \
    | sed -n 's/.*text="\([^"]*\)".*/\1/p' \
    | awk 'length($0) > 0 {print}' \
    | head -n 30 \
    | paste -sd '|' -
}

wait_focus_regex() {
  regex="$1"
  limit="$2"
  i=0
  while [ "${i}" -lt "${limit}" ]; do
    focus="$(focus_line)"
    echo "${focus}" | grep -E "${regex}" >/dev/null 2>&1 && return 0
    sleep 1
    i=$((i + 1))
  done
  return 1
}

tap_first() {
  label="$1"
  pattern="$2"
  before_hash="$(screen_hash)"
  node="$(dump_xml | tr '>' '>\n' | grep -Ei 'text="[^"]*('"${pattern}"')[^"]*"|content-desc="[^"]*('"${pattern}"')[^"]*"' | grep 'bounds=' | head -n 1)"
  if [ -z "${node}" ]; then
    echo "tap_${label}_found=no"
    return 1
  fi
  bounds="$(printf '%s\n' "${node}" | sed -n 's/.*bounds="\[\([0-9][0-9]*\),\([0-9][0-9]*\)\]\[\([0-9][0-9]*\),\([0-9][0-9]*\)\]".*/\1 \2 \3 \4/p')"
  if [ -z "${bounds}" ]; then
    echo "tap_${label}_found=no reason=no_bounds"
    return 1
  fi
  set -- ${bounds}
  x=$((($1 + $3) / 2))
  y=$((($2 + $4) / 2))
  start_ms="$(epoch_ms)"
  adb -s "${adb_target}" shell input tap "${x}" "${y}" >/dev/null 2>&1
  changed=no
  i=0
  while [ "${i}" -lt 20 ]; do
    after_hash="$(screen_hash)"
    if [ "${after_hash}" != "${before_hash}" ]; then
      changed=yes
      break
    fi
    sleep 1
    i=$((i + 1))
  done
  echo "tap_${label}_found=yes changed=${changed} tap_to_screen_change_ms=$(elapsed_ms "${start_ms}") x=${x} y=${y}"
  return 0
}

tap_sequence() {
  prefix="$1"
  shift
  for pattern in "$@"; do
    safe="$(printf '%s' "${pattern}" | tr '[:upper:]' '[:lower:]' | tr '|' '_' | tr -cd 'a-z0-9_')"
    tap_first "${prefix}_${safe}" "${pattern}" || true
    if package_installed; then
      echo "vivi_installed_after_${prefix}=yes"
      return 0
    fi
    sleep 2
  done
  echo "vivi_installed_after_${prefix}=no"
  return 1
}

open_uri() {
  label="$1"
  uri="$2"
  pkg="${3:-}"
  start_ms="$(epoch_ms)"
  if [ -n "${pkg}" ]; then
    adb -s "${adb_target}" shell am start -a android.intent.action.VIEW -d "${uri}" "${pkg}" >/tmp/vivi-${label}-am.log 2>&1
  else
    adb -s "${adb_target}" shell am start -a android.intent.action.VIEW -d "${uri}" >/tmp/vivi-${label}-am.log 2>&1
  fi
  am_status=$?
  sleep 5
  echo "open_${label}_ms=$(elapsed_ms "${start_ms}") status=${am_status} focus=$(focus_line)"
  cat /tmp/vivi-${label}-am.log | sed "s/^/open_${label}_am: /"
}

measure_home() {
  start_ms="$(epoch_ms)"
  adb -s "${adb_target}" shell input keyevent HOME >/dev/null 2>&1
  wait_focus_regex 'Launcher|Quickstep|NexusLauncher|launcher' 20
  status=$?
  echo "home_response_ms=$(elapsed_ms "${start_ms}") status=${status} focus=$(focus_line)"
}

measure_vivi_launch() {
  if ! package_installed; then
    echo "vivi_launch_blocked=package_not_installed"
    return 0
  fi
  start_ms="$(epoch_ms)"
  adb -s "${adb_target}" shell monkey -p "${target}" 1 >/tmp/vivi-launch.log 2>&1
  wait_focus_regex "${target}" 30
  status=$?
  echo "vivi_launch_ms=$(elapsed_ms "${start_ms}") status=${status} focus=$(focus_line)"
  cat /tmp/vivi-launch.log | sed 's/^/vivi_launch_monkey: /'
}

echo "initial_focus=$(focus_line)"
echo "initial_visible_texts=$(visible_texts)"
measure_home
echo "after_home_visible_texts=$(visible_texts)"

open_uri accrescent 'https://accrescent.app/app/com.pv.vivi' ''
tap_sequence accrescent 'Install|Get|Download|Open|Continue|OK|Allow' 'Accept|Agree|Continue|OK|Allow' || true

if ! package_installed; then
  open_uri aurora 'market://details?id=com.pv.vivi' 'com.aurora.store'
  tap_sequence aurora_onboarding 'Anonymous|Accept|Agree|Continue|Next|OK|Allow|Skip|Later' 'Install|Get|Download|Open|Continue|OK|Allow' 'Install|Download|Update' || true
fi

if ! package_installed; then
  open_uri play 'market://details?id=com.pv.vivi' 'com.android.vending'
  tap_sequence play 'Install|Get|Download|Open|Continue|OK|Allow' 'Skip|Later|Not now|Cancel' || true
fi

if package_installed; then
  echo 'vivi_installed=yes'
else
  echo 'vivi_installed=no'
fi

measure_vivi_launch
echo "final_focus=$(focus_line)"
echo "final_visible_texts=$(visible_texts)"
BRIDGE

  bridge_script="${bridge_script//__VIVI_PACKAGE__/${ARBUZAS_ANDROID_SIM_VIVI_PACKAGE}}"
  bridge_cmd="$(adb_in_bridge "${bridge_script}")"

  log "Running no-login ViVi store UI and responsiveness checks"
  remote_shell "
    {
      echo
      echo '[responsiveness]'
      ${bridge_cmd}
    } | tee -a '${report_file}'
  "
  capture_android_screen_remote "${report_dir}" "final-state"
}

vivi_test_remote() {
  local report_dir report_file source_status ticket_output ticket_status
  report_dir="$(init_vivi_evidence_remote | tail -n 1)"
  LAST_VIVI_REPORT_DIR="${report_dir}"
  report_file="${report_dir}/summary.txt"

  log "Writing ViVi responsiveness evidence to ${report_dir}"
  capture_responsiveness_snapshot_remote "${report_file}" "before-android-ready"

  measure_step_remote "${report_file}" "boot_installer_readiness" wait_for_android_remote
  measure_step_remote "${report_file}" "display_tuning" apply_display_tuning_remote
  capture_responsiveness_snapshot_remote "${report_file}" "after-android-ready"

  measure_step_remote "${report_file}" "store_apk_download" download_store_apks_remote
  measure_step_remote "${report_file}" "store_client_install" install_store_clients_remote
  capture_responsiveness_snapshot_remote "${report_file}" "after-store-install"

  set +e
  run_vivi_ui_responsiveness_remote "${report_file}" "${report_dir}"
  source_status=$?
  set -e
  append_report_remote "${report_file}" "metric_vivi_ui_flow_status=${source_status}"

  set +e
  ticket_output="$(validate_ticket_remote_sim 2>&1)"
  ticket_status=$?
  set -e
  append_report_remote "${report_file}" ""
  append_report_remote "${report_file}" "[private-ticket]"
  append_report_remote "${report_file}" "${ticket_output}"
  append_report_remote "${report_file}" "metric_private_ticket_health_status=${ticket_status}"

  capture_responsiveness_snapshot_remote "${report_file}" "after-vivi-flow"
  capture_android_screen_remote "${report_dir}" "after-vivi-flow"
  append_report_remote "${report_file}" "evidence_dir=${report_dir}"
  printf '%s\n' "${report_dir}"
  return 0
}

vivi_report_meaningful_remote() {
  local report_dir="$1"
  [[ -n "${report_dir}" ]] || return 1
  remote_shell "
    [[ -f '${report_dir}/summary.txt' ]] || exit 1
    grep -q '^vivi_installed=' '${report_dir}/summary.txt' || exit 1
    grep -q '^metric_vivi_ui_flow_status=' '${report_dir}/summary.txt' || exit 1
  " >/dev/null 2>&1
}

run_one_vivi_test_attempt() {
  local status=0
  set +e
  preflight_remote
  status=$?
  set -e
  if [[ "${status}" != "0" ]]; then
    return "${status}"
  fi

  set +e
  launch_remote
  status=$?
  set -e
  if [[ "${status}" != "0" ]]; then
    stop_remote || true
    return "${status}"
  fi

  set +e
  vivi_test_remote
  status=$?
  set -e

  stop_remote || true
  return "${status}"
}

vivi_test_action() {
  local first_variant status=0
  first_variant="${ARBUZAS_ANDROID_SIM_VARIANT}"

  set +e
  run_one_vivi_test_attempt
  status=$?
  set -e

  if [[ "${variant_arg_seen}" == "0" && "${first_variant}" == "google-apis" && -n "${LAST_VIVI_REPORT_DIR}" ]]; then
    if ! vivi_report_meaningful_remote "${LAST_VIVI_REPORT_DIR}"; then
      log "Google APIs run did not reach a meaningful store-source result; trying Play Store image"
      ARBUZAS_ANDROID_SIM_VARIANT="playstore"
      LAST_VIVI_REPORT_DIR=""
      set +e
      run_one_vivi_test_attempt
      status=$?
      set -e
    fi
  fi

  return "${status}"
}

stop_remote() {
  local compose
  compose="$(remote_compose_prefix)"
  log "Stopping private Android simulator stack"
  remote_shell "
    if [[ -f '$(remote_env_file)' && -f '${REMOTE_COMPOSE_FILE}' ]]; then
      ${compose} down
    else
      echo 'simulator compose/env not staged; nothing to stop'
    fi
  "
}

reset_state_remote() {
  local variant
  variant="$(variant_dir "${ARBUZAS_ANDROID_SIM_VARIANT}")"
  stop_remote || true
  log "Resetting persistent Android AVD state for ${ARBUZAS_ANDROID_SIM_VARIANT}"
  remote_shell "
    docker run --rm \
      -v /srv/arbuzas:/srv/arbuzas \
      '${ROOT_FALLBACK_IMAGE}' \
      sh -c 'rm -rf /srv/arbuzas/android-sim/${variant}/avd && mkdir -p /srv/arbuzas/android-sim/${variant}/avd && chown -R 1000:1000 /srv/arbuzas/android-sim/${variant}' >/dev/null
    echo 'reset-state ok: /srv/arbuzas/android-sim/${variant}/avd'
  "
}

benchmark_remote() {
  validate_remote || true
  log "Sampling active simulator load for 60 seconds"
  remote_shell "sleep 60"
  report_remote
}

parse_args "$@"

case "${action}" in
  preflight)
    preflight_remote
    ;;
  launch)
    preflight_remote
    launch_remote
    ;;
  validate)
    prepare_remote_layout
    validate_remote
    ;;
  vivi-test)
    vivi_test_action
    ;;
  compare-cores)
    vivi_test_action
    ;;
  benchmark)
    prepare_remote_layout
    benchmark_remote
    ;;
  report)
    prepare_remote_layout
    report_remote
    ;;
  reset-state)
    prepare_remote_layout
    reset_state_remote
    ;;
  stop)
    prepare_remote_layout
    stop_remote
    ;;
esac
