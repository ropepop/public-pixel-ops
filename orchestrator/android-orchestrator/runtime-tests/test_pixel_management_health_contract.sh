#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
MANAGEMENT_SCRIPT="${REPO_ROOT}/android-orchestrator/app/src/main/assets/runtime/entrypoints/pixel-management-health.sh"

tmpdir="$(mktemp -d)"
fake_bin_dir="${tmpdir}/fake-bin"
stack_bin_dir="${tmpdir}/stack-bin"
ssh_root="${tmpdir}/ssh"
ssh_legacy_root="${tmpdir}/ssh-legacy"
vpn_report_file="${tmpdir}/vpn-report.env"
ss_output_file="${tmpdir}/ss-output.txt"
password_hash_source_file="${tmpdir}/conf/root_password.hash"
runtime_authorized_keys_file="${tmpdir}/runtime-auth/authorized_keys"
system_passwd_file="${tmpdir}/system/etc/passwd"

cleanup() {
  rm -rf "${tmpdir}"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

get_value() {
  local payload="$1"
  local key="$2"
  printf '%s\n' "${payload}" | awk -F= -v key="${key}" '$1 == key { print substr($0, index($0, "=") + 1); exit }'
}

assert_value() {
  local payload="$1"
  local key="$2"
  local expected="$3"
  local actual=""
  actual="$(get_value "${payload}" "${key}")"
  if [[ "${actual}" != "${expected}" ]]; then
    fail "expected ${key}=${expected}, got ${actual:-<empty>}"
  fi
}

assert_non_empty() {
  local payload="$1"
  local key="$2"
  local actual=""
  actual="$(get_value "${payload}" "${key}")"
  if [[ -z "${actual}" ]]; then
    fail "expected non-empty ${key}"
  fi
}

RUN_CONTRACT_RC=0
RUN_CONTRACT_OUTPUT=""

run_contract() {
  local output=""
  set +e
  output="$(
    PATH="${fake_bin_dir}:$PATH" \
      PIXEL_STACK_BIN_DIR="${stack_bin_dir}" \
      PIXEL_SSH_ROOT="${ssh_root}" \
      PIXEL_SSH_LEGACY_ROOT="${ssh_legacy_root}" \
      PIXEL_VPN_ROOT="${tmpdir}/vpn" \
      PIXEL_SSH_PASSWORD_HASH_SOURCE_FILE="${password_hash_source_file}" \
      PIXEL_SSH_RUNTIME_AUTHORIZED_KEYS_FILE="${runtime_authorized_keys_file}" \
      PIXEL_SSH_SYSTEM_PASSWD_FILE="${system_passwd_file}" \
      FAKE_VPN_REPORT_FILE="${vpn_report_file}" \
      FAKE_SS_OUTPUT_FILE="${ss_output_file}" \
      FAKE_ID_UID="0" \
      bash "${MANAGEMENT_SCRIPT}" --report
  )"
  RUN_CONTRACT_RC=$?
  set -e
  RUN_CONTRACT_OUTPUT="${output}"
}

write_vpn_report() {
  cat > "${vpn_report_file}" <<EOF
vpn_enabled=1
vpn_health=$1
tailscaled_live=1
tailscaled_sock=1
tailnet_ipv4=$2
guard_chain_ipv4=1
guard_chain_ipv6=1
EOF
}

write_ss_output() {
  cat > "${ss_output_file}" <<EOF
$1
EOF
}

write_password_env() {
  cat > "${ssh_root}/conf/dropbear.env" <<EOF
SSH_PORT=2222
SSH_PASSWORD_AUTH=$1
SSH_ALLOW_KEY_AUTH=$2
EOF
}

write_passwd() {
  cat > "${ssh_root}/etc/passwd" <<EOF
root:$1:0:0:root:/root:/system/bin/sh
EOF
}

write_legacy_passwd() {
  cat > "${ssh_legacy_root}/etc/passwd" <<EOF
root:$1:0:0:root:/root:/system/bin/sh
EOF
}

write_password_hash_source() {
  printf '%s\n' "${1}" > "${password_hash_source_file}"
}

write_system_passwd() {
  mkdir -p "$(dirname "${system_passwd_file}")"
  cat > "${system_passwd_file}" <<EOF
root:$1:0:0:root:/root:/system/bin/sh
EOF
}

write_authorized_keys() {
  mkdir -p "${ssh_root}/home/root/.ssh"
  if [[ -n "${1}" ]]; then
    printf '%s\n' "${1}" > "${ssh_root}/home/root/.ssh/authorized_keys"
  else
    : > "${ssh_root}/home/root/.ssh/authorized_keys"
  fi
}

write_runtime_authorized_keys() {
  mkdir -p "$(dirname "${runtime_authorized_keys_file}")"
  if [[ -n "${1}" ]]; then
    printf '%s\n' "${1}" > "${runtime_authorized_keys_file}"
  else
    : > "${runtime_authorized_keys_file}"
  fi
}

mkdir -p "${fake_bin_dir}" "${stack_bin_dir}" "${ssh_root}/conf" "${ssh_root}/etc" "${ssh_root}/home/root/.ssh" "${ssh_legacy_root}/etc" "$(dirname "${password_hash_source_file}")" "$(dirname "${runtime_authorized_keys_file}")"

cat > "${stack_bin_dir}/pixel-vpn-health.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cat "${FAKE_VPN_REPORT_FILE}"
if grep -q '^vpn_health=1$' "${FAKE_VPN_REPORT_FILE}"; then
  exit 0
fi
exit 1
EOF
chmod +x "${stack_bin_dir}/pixel-vpn-health.sh"

cat > "${fake_bin_dir}/ss" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cat "${FAKE_SS_OUTPUT_FILE}" 2>/dev/null || true
EOF
chmod +x "${fake_bin_dir}/ss"

cat > "${fake_bin_dir}/id" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-u" ]]; then
  printf '%s\n' "${FAKE_ID_UID:-0}"
  exit 0
fi
exit 1
EOF
chmod +x "${fake_bin_dir}/id"

for command_name in pm am logcat; do
  cat > "${fake_bin_dir}/${command_name}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
exit 0
EOF
  chmod +x "${fake_bin_dir}/${command_name}"
done

write_password_env 1 1
write_passwd '$6$healthyhash'
write_legacy_passwd '$6$healthyhash'
write_system_passwd '$6$healthyhash'
write_password_hash_source '$6$healthyhash'
write_authorized_keys 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKey pixel@test'
write_runtime_authorized_keys 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKey pixel@test'
write_vpn_report 1 '100.64.0.10'
write_ss_output 'LISTEN 0 128 0.0.0.0:2222 0.0.0.0:*'
run_contract
healthy_rc="${RUN_CONTRACT_RC}"
healthy_output="${RUN_CONTRACT_OUTPUT}"
[[ "${healthy_rc}" == "0" ]] || fail "healthy contract should exit 0"
assert_value "${healthy_output}" "management_enabled" "1"
assert_value "${healthy_output}" "management_healthy" "1"
assert_value "${healthy_output}" "management_reason" "ok"
assert_value "${healthy_output}" "ssh_auth_mode" "key_password"
assert_value "${healthy_output}" "ssh_password_runtime_mismatch" "0"
assert_non_empty "${healthy_output}" "pm_path"
assert_non_empty "${healthy_output}" "am_path"
assert_non_empty "${healthy_output}" "logcat_path"

write_vpn_report 1 ''
run_contract
missing_tailnet_rc="${RUN_CONTRACT_RC}"
missing_tailnet_output="${RUN_CONTRACT_OUTPUT}"
[[ "${missing_tailnet_rc}" != "0" ]] || fail "missing tailnet IP should fail"
assert_value "${missing_tailnet_output}" "management_healthy" "0"
assert_value "${missing_tailnet_output}" "management_reason" "tailnet_ip_missing"

write_vpn_report 1 '100.64.0.10'
write_ss_output ''
run_contract
missing_listener_rc="${RUN_CONTRACT_RC}"
missing_listener_output="${RUN_CONTRACT_OUTPUT}"
[[ "${missing_listener_rc}" != "0" ]] || fail "missing ssh listener should fail"
assert_value "${missing_listener_output}" "management_reason" "ssh_listener_missing"

write_ss_output 'LISTEN 0 128 0.0.0.0:2222 0.0.0.0:*'
write_password_env 1 0
write_passwd '*'
write_legacy_passwd '*'
write_system_passwd '*'
write_password_hash_source '$6$healthyhash'
write_authorized_keys ''
write_runtime_authorized_keys ''
run_contract
password_unready_rc="${RUN_CONTRACT_RC}"
password_unready_output="${RUN_CONTRACT_OUTPUT}"
[[ "${password_unready_rc}" != "0" ]] || fail "password-only auth without runtime hash should fail"
assert_value "${password_unready_output}" "ssh_auth_mode" "password_only"
assert_value "${password_unready_output}" "management_reason" "password_auth_runtime_mismatch"

write_password_env 0 1
write_passwd '$6$healthyhash'
write_legacy_passwd '$6$healthyhash'
write_system_passwd '$6$healthyhash'
write_password_hash_source '$6$healthyhash'
write_authorized_keys ''
write_runtime_authorized_keys ''
run_contract
key_unready_rc="${RUN_CONTRACT_RC}"
key_unready_output="${RUN_CONTRACT_OUTPUT}"
[[ "${key_unready_rc}" != "0" ]] || fail "key-only auth without authorized_keys should fail"
assert_value "${key_unready_output}" "ssh_auth_mode" "key_only"
assert_value "${key_unready_output}" "management_reason" "key_auth_not_ready"

write_password_env 1 0
write_passwd '$6$healthyhash'
write_legacy_passwd '$6$stalelegacy'
write_system_passwd '$6$healthyhash'
write_password_hash_source '$6$healthyhash'
write_authorized_keys ''
write_runtime_authorized_keys ''
run_contract
password_mismatch_rc="${RUN_CONTRACT_RC}"
password_mismatch_output="${RUN_CONTRACT_OUTPUT}"
[[ "${password_mismatch_rc}" != "0" ]] || fail "legacy runtime mismatch should fail"
assert_value "${password_mismatch_output}" "ssh_password_runtime_legacy_present" "1"
assert_value "${password_mismatch_output}" "ssh_password_runtime_mismatch" "1"
assert_value "${password_mismatch_output}" "management_reason" "password_auth_runtime_mismatch"

echo "PASS: pixel-management-health reports healthy and auth-specific management failure contracts"
