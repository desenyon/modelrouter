#!/bin/sh
# Run from any directory. No provider API keys or live model calls are needed.
set -eu
cd "$(dirname "$0")/.."
export GOMAXPROCS="${GOMAXPROCS:-2}"
workers="${GO_TEST_PARALLEL:-2}"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  printf 'Run gofmt on:\n%s\n' "$unformatted" >&2
  exit 1
fi
go vet -p "$workers" ./...
go test -race -count=1 -p "$workers" ./...
verify_dir="$(mktemp -d)"
trap 'rm -rf "$verify_dir"' EXIT HUP INT TERM
go build -p "$workers" -o "$verify_dir/modelrouter" .
"$verify_dir/modelrouter" version
"$verify_dir/modelrouter" models --json > "$verify_dir/models.json"
if [ "${MODELROUTER_REQUIRE_EMBEDDER:-0}" = 1 ]; then
  "$verify_dir/modelrouter" embedder status
  MODELROUTER_STATE_PATH="$verify_dir/learned.json" MODELROUTER_DECISION_LOG= \
    "$verify_dir/modelrouter" route --json hi > "$verify_dir/route.json"
fi
printf '%s\n' 'Verification passed: format, vet, race tests, build, CLI smoke.'
