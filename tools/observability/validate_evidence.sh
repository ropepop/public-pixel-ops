#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT}"

if [ ! -d ops/evidence ]; then
  echo "no ops/evidence directory present; skipping evidence validation"
  exit 0
fi

find ops/evidence -type f -name '*.json' -print0 | while IFS= read -r -d '' f; do
  if ! jq -e . "$f" >/dev/null 2>&1; then
    echo "invalid JSON evidence file: $f" >&2
    exit 1
  fi
done

echo "evidence validation passed"
