#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
VPN_LAUNCH_TEMPLATE="${REPO_ROOT}/android-orchestrator/app/src/main/assets/runtime/templates/vpn/pixel-vpn-launch.sh"

if ! rg -Fq 'PIXEL_SSH_GUARD' "${VPN_LAUNCH_TEMPLATE}"; then
  echo "FAIL: missing IPv4 guard chain name in ${VPN_LAUNCH_TEMPLATE}" >&2
  exit 1
fi

if ! rg -Fq 'PIXEL_SSH_GUARD6' "${VPN_LAUNCH_TEMPLATE}"; then
  echo "FAIL: missing IPv6 guard chain name in ${VPN_LAUNCH_TEMPLATE}" >&2
  exit 1
fi

if ! rg -Fq '"${ipt}" -A "${chain}" -i "${VPN_INTERFACE_NAME}" -p tcp --dport "${SSH_PORT}" -j ACCEPT' "${VPN_LAUNCH_TEMPLATE}"; then
  echo "FAIL: missing VPN interface allow rule in ${VPN_LAUNCH_TEMPLATE}" >&2
  exit 1
fi

if ! rg -Fq '"${ipt}" -A "${chain}" -p tcp --dport "${SSH_PORT}" -j DROP' "${VPN_LAUNCH_TEMPLATE}"; then
  echo "FAIL: missing non-VPN drop rule in ${VPN_LAUNCH_TEMPLATE}" >&2
  exit 1
fi

echo "PASS: pixel-vpn-launch enforces VPN-only SSH guard chains"
