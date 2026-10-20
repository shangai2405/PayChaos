#!/usr/bin/env bash
# Runs the official stripe-go SDK through paychaos against the bundled mock:
# once as the SDK alone, once wrapped in a job-queue style retry.
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p bin
go build -o bin/paychaos ./cmd/paychaos
(cd examples/stripe-go-client && go build -o ../../bin/stripe-go-client .)

N=${N:-20}
MOCK=127.0.0.1:12111
PROXY=127.0.0.1:8080

created() { curl -s "http://$MOCK/__mock/stats" | sed -n 's/.*"payment_intents":\([0-9]*\).*/\1/p'; }

run_mode() {
  local mode=$1
  ./bin/paychaos mock -listen "$MOCK" 2>/dev/null & local mock_pid=$!
  sleep 0.5
  ./bin/paychaos -listen "$PROXY" -upstream "http://$MOCK" -config examples/scenarios/sdk.json \
    -report "paychaos-report-sdk-$mode.json" -fail-on-high=false 2>/dev/null > "paychaos-sdk-$mode.txt" & local px_pid=$!
  sleep 0.5
  ./bin/stripe-go-client -base "http://$PROXY" -n "$N" -mode "$mode"
  echo "  upstream created $(created) PaymentIntents for $N checkouts"
  kill -INT "$px_pid"; wait "$px_pid" || true
  kill "$mock_pid"; wait "$mock_pid" 2>/dev/null || true
  sed -n '/^Findings/,$p' "paychaos-sdk-$mode.txt" | head -4
  echo
}

echo "== stripe-go SDK alone (its own retries reuse one key per call) =="
run_mode sdk
echo "== stripe-go SDK inside a job-queue retry (new SDK call, new key) =="
run_mode job-retry
