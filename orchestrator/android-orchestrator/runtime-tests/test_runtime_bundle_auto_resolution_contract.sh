#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
PACKAGE_SCRIPT="${REPO_ROOT}/scripts/android/package_runtime_bundle.sh"

if ! rg -Fq -- '--print-inputs' "${PACKAGE_SCRIPT}"; then
  echo "FAIL: package_runtime_bundle.sh missing --print-inputs mode" >&2
  exit 1
fi

for env_name in \
  PIXEL_RUNTIME_ROOTFS_TARBALL \
  PIXEL_RUNTIME_DROPBEAR_ARTIFACT_DIR \
  PIXEL_RUNTIME_TAILSCALE_BUNDLE \
  PIXEL_RUNTIME_TRAIN_BOT_BUNDLE \
  PIXEL_RUNTIME_SATIKSME_BOT_BUNDLE \
  PIXEL_RUNTIME_SITE_NOTIFIER_BUNDLE; do
  if ! rg -Fq "${env_name}" "${PACKAGE_SCRIPT}"; then
    echo "FAIL: package_runtime_bundle.sh missing ${env_name} override" >&2
    exit 1
  fi
done

if ! rg -Fq 'choose_latest_candidate' "${PACKAGE_SCRIPT}"; then
  echo "FAIL: package_runtime_bundle.sh missing auto-resolution candidate selector" >&2
  exit 1
fi

for required in \
  'workloads/train-bot/.artifacts/train-bot/train-bot-bundle-*.tar' \
  'workloads/train-bot/.artifacts/component-releases/train_bot-*/artifacts/train-bot-bundle-*.tar' \
  'workloads/satiksme-bot/.artifacts/satiksme-bot/satiksme-bot-bundle-*.tar' \
  'workloads/satiksme-bot/.artifacts/component-releases/satiksme_bot-*/artifacts/satiksme-bot-bundle-*.tar' \
  'workloads/site-notifications/.artifacts/site-notifier/site-notifier-bundle-*.tar' \
  'workloads/site-notifications/.artifacts/component-releases/site_notifier-*/artifacts/site-notifier-bundle-*.tar'; do
  if ! rg -Fq "${required}" "${PACKAGE_SCRIPT}"; then
    echo "FAIL: package_runtime_bundle.sh missing workload-local bundle candidate path ${required}" >&2
    exit 1
  fi
done

if ! rg -Fq 'resolved-inputs.json' "${PACKAGE_SCRIPT}"; then
  echo "FAIL: package_runtime_bundle.sh missing resolved-inputs.json output" >&2
  exit 1
fi

help_output="$(bash "${PACKAGE_SCRIPT}" --help)"
for required in '--rootfs-tarball' '--dropbear-artifact-dir' '--tailscale-bundle' '--train-bot-bundle' '--satiksme-bot-bundle' '--site-notifier-bundle' '--platform-only' '--print-inputs'; do
  if ! grep -Fq -- "${required}" <<<"${help_output}"; then
    echo "FAIL: package_runtime_bundle.sh --help missing ${required}" >&2
    exit 1
  fi
done

echo "PASS: runtime bundle auto-resolution contract is present"
