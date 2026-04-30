#!/bin/sh
set -eu

: "${TICKET_PHONE_ADB_TARGET:=100.76.50.43:5555}"
: "${TICKET_PHONE_DEVICE_PORT:=9388}"
: "${TICKET_PHONE_ADB_FORWARD_PORT:=19389}"
: "${TICKET_PHONE_BRIDGE_PORT:=9388}"
: "${TICKET_PHONE_RETRY_DELAY:=2}"

cleanup() {
  adb -s "${TICKET_PHONE_ADB_TARGET}" forward --remove "tcp:${TICKET_PHONE_ADB_FORWARD_PORT}" >/dev/null 2>&1 || true
}

trap cleanup EXIT INT TERM

while :; do
  cleanup
  adb connect "${TICKET_PHONE_ADB_TARGET}" >/dev/null 2>&1 || true
  if adb -s "${TICKET_PHONE_ADB_TARGET}" get-state >/dev/null 2>&1 \
    && adb -s "${TICKET_PHONE_ADB_TARGET}" forward "tcp:${TICKET_PHONE_ADB_FORWARD_PORT}" "tcp:${TICKET_PHONE_DEVICE_PORT}" >/dev/null 2>&1; then
    socat \
      "TCP-LISTEN:${TICKET_PHONE_BRIDGE_PORT},fork,reuseaddr,bind=0.0.0.0" \
      "TCP:127.0.0.1:${TICKET_PHONE_ADB_FORWARD_PORT}" || true
  fi
  sleep "${TICKET_PHONE_RETRY_DELAY}"
done
