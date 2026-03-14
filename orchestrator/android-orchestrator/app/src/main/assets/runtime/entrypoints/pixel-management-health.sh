#!/system/bin/sh
set -eu

REPORT_MODE=0
if [ "${1:-}" = "--report" ] || [ "${PIXEL_MANAGEMENT_HEALTH_REPORT:-0}" = "1" ]; then
  REPORT_MODE=1
fi

STACK_BIN_DIR="${PIXEL_STACK_BIN_DIR:-/data/local/pixel-stack/bin}"
SSH_ROOT="${PIXEL_SSH_ROOT:-/data/local/pixel-stack/ssh}"
SSH_LEGACY_ROOT="${PIXEL_SSH_LEGACY_ROOT:-/data/adb/pixel-stack/ssh}"
VPN_ROOT="${PIXEL_VPN_ROOT:-/data/local/pixel-stack/vpn}"
VPN_HEALTH_BIN="${STACK_BIN_DIR}/pixel-vpn-health.sh"
SSH_CONF_FILE="${SSH_ROOT}/conf/dropbear.env"
PASSWD_FILE="${SSH_ROOT}/etc/passwd"
LEGACY_PASSWD_FILE="${SSH_LEGACY_ROOT}/etc/passwd"
SYSTEM_PASSWD_FILE="${PIXEL_SSH_SYSTEM_PASSWD_FILE:-/system/etc/passwd}"
PASSWORD_HASH_SOURCE_FILE="${PIXEL_SSH_PASSWORD_HASH_SOURCE_FILE:-/data/local/pixel-stack/conf/ssh/root_password.hash}"
AUTHORIZED_KEYS_FILE="${SSH_ROOT}/home/root/.ssh/authorized_keys"
RUNTIME_AUTHORIZED_KEYS_FILE="${PIXEL_SSH_RUNTIME_AUTHORIZED_KEYS_FILE:-/debug_ramdisk/pixel-ssh-auth/authorized_keys}"

if [ -r "${SSH_CONF_FILE}" ]; then
  # shellcheck disable=SC1090
  set -a
  . "${SSH_CONF_FILE}"
  set +a
fi

: "${SSH_PORT:=2222}"
: "${SSH_PASSWORD_AUTH:=1}"
: "${SSH_ALLOW_KEY_AUTH:=1}"

emit() {
  if [ "${REPORT_MODE}" = "1" ]; then
    printf '%s=%s\n' "$1" "$2"
  fi
}

listeners_have_port() {
  local port="$1"
  ss -ltn 2>/dev/null | grep -E "[:.]${port}[[:space:]]" >/dev/null 2>&1
}

read_first_non_empty_line() {
  local file="$1"
  if [ ! -r "${file}" ]; then
    return 0
  fi
  sed -n '/[^[:space:]]/ { s/^[[:space:]]*//; s/[[:space:]]*$//; p; q; }' "${file}" 2>/dev/null || true
}

extract_root_hash() {
  local file="$1"
  if [ ! -r "${file}" ]; then
    return 0
  fi
  awk -F: '$1=="root"{print $2; exit}' "${file}" 2>/dev/null || true
}

normalize_password_hash() {
  local file="$1"
  local line=""
  line="$(read_first_non_empty_line "${file}")"
  case "${line}" in
    root:*)
      printf '%s\n' "${line}" | awk -F: '$1=="root"{print $2; exit}' 2>/dev/null || true
      ;;
    '$6$'*)
      printf '%s\n' "${line}"
      ;;
    *)
      ;;
  esac
}

valid_password_hash() {
  case "${1:-}" in
    ""|"x"|"*"|"!"|"!!")
      return 1
      ;;
    '$6$'*)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

files_match() {
  local first="$1"
  local second="$2"

  [ -r "${first}" ] || return 1
  [ -r "${second}" ] || return 1

  if command -v cmp >/dev/null 2>&1; then
    cmp -s "${first}" "${second}" >/dev/null 2>&1
    return $?
  fi

  [ "$(cat "${first}" 2>/dev/null || true)" = "$(cat "${second}" 2>/dev/null || true)" ]
}

remote_uid="$(id -u 2>/dev/null || true)"
pm_path="$(command -v pm 2>/dev/null || true)"
am_path="$(command -v am 2>/dev/null || true)"
logcat_path="$(command -v logcat 2>/dev/null || true)"

vpn_enabled="0"
vpn_health="0"
tailscaled_live="0"
tailscaled_sock="0"
tailnet_ipv4=""
guard_chain_ipv4="0"
guard_chain_ipv6="0"

if [ -x "${VPN_HEALTH_BIN}" ]; then
  if PIXEL_VPN_HEALTH_REPORT=1 "${VPN_HEALTH_BIN}" --report >/tmp/pixel-management-health.$$ 2>/dev/null; then
    vpn_health="1"
  fi
  if [ -f /tmp/pixel-management-health.$$ ]; then
    while IFS='=' read -r key value; do
      case "${key}" in
        vpn_enabled) vpn_enabled="${value}" ;;
        tailscaled_live) tailscaled_live="${value}" ;;
        tailscaled_sock) tailscaled_sock="${value}" ;;
        tailnet_ipv4) tailnet_ipv4="${value}" ;;
        guard_chain_ipv4) guard_chain_ipv4="${value}" ;;
        guard_chain_ipv6) guard_chain_ipv6="${value}" ;;
      esac
    done < /tmp/pixel-management-health.$$
    rm -f /tmp/pixel-management-health.$$ >/dev/null 2>&1 || true
  fi
fi

ssh_listener="0"
if listeners_have_port "${SSH_PORT}"; then
  ssh_listener="1"
fi

ssh_password_auth_requested="0"
ssh_key_auth_requested="0"
if [ "${SSH_PASSWORD_AUTH}" = "1" ]; then
  ssh_password_auth_requested="1"
fi
if [ "${SSH_ALLOW_KEY_AUTH}" = "1" ]; then
  ssh_key_auth_requested="1"
fi

ssh_auth_mode="key_password"
if [ "${ssh_password_auth_requested}" = "1" ] && [ "${ssh_key_auth_requested}" != "1" ]; then
  ssh_auth_mode="password_only"
elif [ "${ssh_password_auth_requested}" != "1" ] && [ "${ssh_key_auth_requested}" = "1" ]; then
  ssh_auth_mode="key_only"
elif [ "${ssh_password_auth_requested}" != "1" ] && [ "${ssh_key_auth_requested}" != "1" ]; then
  ssh_auth_mode="disabled"
fi

password_hash_source_ready="0"
password_runtime_local_ready="0"
password_runtime_legacy_present="0"
password_runtime_legacy_ready="1"
password_runtime_system_ready="1"
password_runtime_mismatch="0"
source_password_hash="$(normalize_password_hash "${PASSWORD_HASH_SOURCE_FILE}")"
local_root_hash="$(extract_root_hash "${PASSWD_FILE}")"
legacy_root_hash=""
system_root_hash=""

if valid_password_hash "${source_password_hash}"; then
  password_hash_source_ready="1"
fi
if [ "${password_hash_source_ready}" = "1" ] && [ "${local_root_hash}" = "${source_password_hash}" ]; then
  password_runtime_local_ready="1"
fi
if [ -e "${LEGACY_PASSWD_FILE}" ]; then
  password_runtime_legacy_present="1"
  password_runtime_legacy_ready="0"
  legacy_root_hash="$(extract_root_hash "${LEGACY_PASSWD_FILE}")"
  if [ "${password_hash_source_ready}" = "1" ] && [ "${legacy_root_hash}" = "${source_password_hash}" ]; then
    password_runtime_legacy_ready="1"
  fi
fi
if [ "${ssh_listener}" = "1" ]; then
  password_runtime_system_ready="0"
  system_root_hash="$(extract_root_hash "${SYSTEM_PASSWD_FILE}")"
  if [ "${password_hash_source_ready}" = "1" ] && [ "${system_root_hash}" = "${source_password_hash}" ]; then
    password_runtime_system_ready="1"
  fi
fi
if [ "${password_hash_source_ready}" = "1" ] && {
  [ "${password_runtime_local_ready}" != "1" ] ||
  [ "${password_runtime_legacy_ready}" != "1" ] ||
  [ "${password_runtime_system_ready}" != "1" ];
}; then
  password_runtime_mismatch="1"
fi

ssh_password_auth_ready="0"
if [ "${password_hash_source_ready}" = "1" ] &&
   [ "${password_runtime_local_ready}" = "1" ] &&
   [ "${password_runtime_legacy_ready}" = "1" ] &&
   [ "${password_runtime_system_ready}" = "1" ]; then
  ssh_password_auth_ready="1"
fi

key_source_ready="0"
key_runtime_ready="1"
key_runtime_mismatch="0"
ssh_key_auth_ready="0"
if [ -s "${AUTHORIZED_KEYS_FILE}" ]; then
  key_source_ready="1"
fi
if [ "${ssh_listener}" = "1" ]; then
  key_runtime_ready="0"
  if [ "${key_source_ready}" = "1" ] && files_match "${AUTHORIZED_KEYS_FILE}" "${RUNTIME_AUTHORIZED_KEYS_FILE}"; then
    key_runtime_ready="1"
  fi
fi
if [ "${key_source_ready}" = "1" ] && [ "${key_runtime_ready}" != "1" ]; then
  key_runtime_mismatch="1"
fi
if [ "${key_source_ready}" = "1" ] && [ "${key_runtime_ready}" = "1" ]; then
  ssh_key_auth_ready="1"
fi

management_enabled="0"
management_healthy="1"
management_reason="disabled"

if [ "${vpn_enabled}" = "1" ]; then
  management_enabled="1"
  management_healthy="0"
  management_reason="vpn_unhealthy"

  if [ "${remote_uid}" != "0" ]; then
    management_reason="root_unavailable"
  elif [ -z "${pm_path}" ] || [ -z "${am_path}" ] || [ -z "${logcat_path}" ]; then
    management_reason="android_command_missing"
  elif [ "${vpn_health}" != "1" ]; then
    management_reason="vpn_unhealthy"
  elif [ -z "${tailnet_ipv4}" ]; then
    management_reason="tailnet_ip_missing"
  elif [ "${ssh_listener}" != "1" ]; then
    management_reason="ssh_listener_missing"
  elif [ "${ssh_auth_mode}" = "disabled" ]; then
    management_reason="ssh_auth_unconfigured"
  elif [ "${ssh_password_auth_requested}" = "1" ] && [ "${password_runtime_mismatch}" = "1" ]; then
    management_reason="password_auth_runtime_mismatch"
  elif [ "${ssh_key_auth_requested}" = "1" ] && [ "${key_runtime_mismatch}" = "1" ]; then
    management_reason="key_auth_runtime_mismatch"
  elif [ "${ssh_password_auth_requested}" = "1" ] && [ "${ssh_key_auth_requested}" != "1" ] && [ "${ssh_password_auth_ready}" != "1" ]; then
    management_reason="password_auth_not_ready"
  elif [ "${ssh_password_auth_requested}" != "1" ] && [ "${ssh_key_auth_requested}" = "1" ] && [ "${ssh_key_auth_ready}" != "1" ]; then
    management_reason="key_auth_not_ready"
  elif [ "${ssh_password_auth_ready}" != "1" ] && [ "${ssh_key_auth_ready}" != "1" ]; then
    management_reason="ssh_auth_not_ready"
  else
    management_healthy="1"
    management_reason="ok"
  fi
fi

emit "remote_uid" "${remote_uid}"
emit "pm_path" "${pm_path}"
emit "am_path" "${am_path}"
emit "logcat_path" "${logcat_path}"
emit "vpn_enabled" "${vpn_enabled}"
emit "vpn_health" "${vpn_health}"
emit "tailscaled_live" "${tailscaled_live}"
emit "tailscaled_sock" "${tailscaled_sock}"
emit "tailnet_ipv4" "${tailnet_ipv4}"
emit "guard_chain_ipv4" "${guard_chain_ipv4}"
emit "guard_chain_ipv6" "${guard_chain_ipv6}"
emit "ssh_port" "${SSH_PORT}"
emit "ssh_listener" "${ssh_listener}"
emit "ssh_auth_mode" "${ssh_auth_mode}"
emit "ssh_password_auth_requested" "${ssh_password_auth_requested}"
emit "ssh_password_auth_ready" "${ssh_password_auth_ready}"
emit "ssh_password_hash_source_ready" "${password_hash_source_ready}"
emit "ssh_password_runtime_local_ready" "${password_runtime_local_ready}"
emit "ssh_password_runtime_legacy_present" "${password_runtime_legacy_present}"
emit "ssh_password_runtime_legacy_ready" "${password_runtime_legacy_ready}"
emit "ssh_password_runtime_system_ready" "${password_runtime_system_ready}"
emit "ssh_password_runtime_mismatch" "${password_runtime_mismatch}"
emit "ssh_key_auth_requested" "${ssh_key_auth_requested}"
emit "ssh_key_auth_ready" "${ssh_key_auth_ready}"
emit "ssh_key_source_ready" "${key_source_ready}"
emit "ssh_key_runtime_ready" "${key_runtime_ready}"
emit "ssh_key_runtime_mismatch" "${key_runtime_mismatch}"
emit "management_enabled" "${management_enabled}"
emit "management_healthy" "${management_healthy}"
emit "management_reason" "${management_reason}"

if [ "${management_enabled}" != "1" ]; then
  exit 0
fi

[ "${management_healthy}" = "1" ]
