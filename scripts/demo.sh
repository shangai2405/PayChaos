#!/usr/bin/env bash
# Runs the naive and safe example clients through paychaos against the bundled
# mock and prints how many PaymentIntents the upstream actually created.
# No Stripe account needed.
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p bin
go build -o bin/paychaos ./cmd/paychaos
go build -o bin/naive-client ./examples/naive-client
go build -o bin/safe-client ./examples/safe-client
go build -o bin/rotating-key-client ./examples/rotating-key-client

N=${N:-20}
MOCK=127.0.0.1:12111
PROXY=127.0.0.1:8080

created() { curl -s "http://$MOCK/__mock/stats" | sed -n 's/.*"payment_intents":\([0-9]*\).*/\1/p'; }

run_case() {
  local name=$1
  ./bin/paychaos mock -listen "$MOCK" 2>/dev/null & local mock_pid=$!
  sleep 0.5
  ./bin/paychaos -listen "$PROXY" -upstream "http://$MOCK" \
    -config examples/scenarios/lost-responses.json \
    -report "paychaos-report-$name.json" -fail-on-high=false 2>/dev/null > "paychaos-$name.txt" & local px_pid=$!
  sleep 0.5
  "./bin/$name-client" -base "http://$PROXY" -n "$N"
  echo "  upstream created $(created) PaymentIntents for $N checkouts"
  kill -INT "$px_pid"; wait "$px_pid" || true
  kill "$mock_pid"; wait "$mock_pid" 2>/dev/null || true
  sed -n '/^Findings/,$p' "paychaos-$name.txt" | head -8
  echo
}

echo "== naive client (no idempotency key, instant retries) =="
run_case naive
echo "== rotating-key client (sends a key, but a new one per attempt) =="
run_case rotating-key
echo "== safe client (stable idempotency key, exponential backoff) =="
run_case safe
