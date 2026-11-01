#!/usr/bin/env bash
# Parity check: replay one scripted billing/admission/scheduling scenario
# against the pristine upstream branch and the current suite tree, then diff.
# Also imports the upstream-written database into the suite build.
set -euo pipefail
cd "$(dirname "$0")/.."
UPSTREAM_REF="${1:-upstream}"
work="$(mktemp -d)"
mkdir -p "$work"; trap 'git worktree remove --force "$work/upstream" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT

git worktree add --detach "$work/upstream" "$UPSTREAM_REF" >/dev/null
# Harness files only use upstream APIs; copy them in verbatim.
cp internal/plugin/suite_parity_test.go "$work/upstream/internal/plugin/"
cp internal/billing/suite_clock.go "$work/upstream/internal/billing/"

mkdir -p "$work/up" "$work/suite"
(cd "$work/upstream" && SUITE_PARITY_OUT="$work/up.json" SUITE_PARITY_DIR="$work/up" \
  go test -count=1 -run '^TestParityTranscript$' ./internal/plugin/ >/dev/null)
SUITE_PARITY_OUT="$work/suite.json" SUITE_PARITY_DIR="$work/suite" \
  go test -count=1 -run '^TestParityTranscript$' ./internal/plugin/ >/dev/null

python3 scripts/parity_normalize.py "$work/up.json" "$work/up" > "$work/up.norm.json"
python3 scripts/parity_normalize.py "$work/suite.json" "$work/suite" > "$work/suite.norm.json"
steps=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "$work/up.json")
if ! diff -u "$work/up.norm.json" "$work/suite.norm.json"; then
  echo "PARITY FAILED: suite behaviour differs from upstream ($UPSTREAM_REF)" >&2
  exit 1
fi
echo "PARITY OK: $steps transcript steps identical to upstream ($UPSTREAM_REF @ $(git rev-parse --short "$UPSTREAM_REF^{commit}"))"

SUITE_UPSTREAM_DB="$work/up/parity.db" go test -count=1 -run '^TestImportRealUpstreamDatabase$' -v ./internal/plugin/ > "$work/import.log" 2>&1 || { cat "$work/import.log"; echo "IMPORT FAILED" >&2; exit 1; }
grep -E "^(--- |ok)" "$work/import.log"
echo "IMPORT OK: database written by upstream imported row-for-row"
