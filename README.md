# paychaos: chaos testing for payment integrations

[![ci](https://github.com/shangai2405/Chaos-tester-for-payment-integrations/actions/workflows/ci.yml/badge.svg)](https://github.com/shangai2405/Chaos-tester-for-payment-integrations/actions/workflows/ci.yml)
[![licence: MIT](https://img.shields.io/badge/licence-MIT-blue.svg)](LICENSE)

`paychaos` is a fault-injecting proxy, written in Go, that sits between your application and the Stripe API (test mode only) and breaks things the way real networks do. It drops responses after Stripe has already processed a request, returns 500s for requests that actually succeeded, holds responses until clients time out, injects rate limits, and duplicates, delays and reorders webhooks. While it does that, it audits how your integration reacts and reports retry patterns that would **double-charge, double-refund or double-fulfil** in production, naming the duplicate objects it saw created.

Generic fault-injection proxies (Toxiproxy, for example) break TCP connections. `paychaos` understands payment semantics: it knows which requests move money, tracks idempotency keys across retries, groups attempts by business operation, and models Stripe's documented behaviour for 500s, 409s and cached idempotent results.

![make demo: the naive client turns 20 checkouts into 31 PaymentIntents](docs/demo.gif)

## Why

The most expensive payment bug is the one that looks like a network blip:

1. Your server sends `POST /v1/payment_intents`.
2. Stripe charges the card and responds.
3. The response is lost (load balancer reset, timeout, pod restart), or the request comes back as a 500 even though it went through.
4. Your code sees an error and retries.

If the retry carries the same `Idempotency-Key`, Stripe replays the original result. If it carries no key, or a new one, the customer is charged twice. If it doesn't retry at all, your app records a failure for a payment that succeeded. None of this happens in local testing, because local networks don't lose responses. `paychaos` makes it happen on purpose.

## Results from the bundled demos

`make demo` runs three hand-written clients against the built-in mock with `examples/scenarios/lost-responses.json` (seed 7, 20 checkouts each). `make demo-sdk` runs the official `stripe-go` SDK (v87) with `examples/scenarios/sdk.json` (seed 11).

| Client | Reported to the app | PaymentIntents created | Findings |
|---|---|---|---|
| `naive-client`: no idempotency key, instant retries | 20 succeeded | **31** | 11 high, 2 medium, 2 low |
| `rotating-key-client`: a new clock-based key on every attempt | 20 succeeded | **25** | 5 high |
| `safe-client`: one stable key per checkout, exponential backoff | 20 succeeded | 20 | none |
| `stripe-go` SDK alone | 14 succeeded, **6 failed** | 20 | 6 medium: charged but reported as failed |
| `stripe-go` SDK inside a job-queue retry | 20 succeeded | **26** | 6 high |

Every client except the safe one is wrong in a way its own logs can't show. The SDK row is the subtle one: the SDK's own retries are safe because it reuses one key per call, but six requests came back as 500s it was told not to retry, so the app thinks six paid orders failed. Wrapping the same SDK in an application-level retry turns those six into duplicate charges, because each re-run is a new SDK call with a new key.

## Quick start

Requires Go 1.24+. The tool itself has no third-party dependencies.

```bash
go install github.com/shangai2405/Chaos-tester-for-payment-integrations/cmd/paychaos@latest
```

Or from a clone:

```bash
git clone https://github.com/shangai2405/Chaos-tester-for-payment-integrations.git
cd Chaos-tester-for-payment-integrations
make test        # unit and end-to-end tests with the race detector
make demo        # the hand-written clients, no Stripe account needed
make demo-sdk    # the stripe-go SDK example
```

### Against Stripe test mode

```bash
paychaos -upstream https://api.stripe.com -config examples/scenarios/lost-responses.json
```

Point your app's Stripe API base URL at `http://127.0.0.1:8080` and run your checkout flow or test suite. Press Ctrl-C to stop; the summary prints and the full report is written to `paychaos-report.json`. For stripe-go, set `URL` in `stripe.BackendConfig` (see `examples/stripe-go-client`); other SDKs have an equivalent option.

### Webhooks

```bash
stripe listen --forward-to 127.0.0.1:8081/webhooks
paychaos -webhook-listen 127.0.0.1:8081 -webhook-target http://127.0.0.1:3000/stripe/webhook
```

The relay acknowledges each event immediately, then delivers it to your app with injected duplicates, random delays and shuffled order. The `Stripe-Signature` header is forwarded unchanged, so duplicates carry a valid signature, exactly like Stripe's own redeliveries. `examples/scenarios/webhook-storm.json` turns webhook chaos up (most events delivered three times, wide reordering) and injects no API faults, for testing handlers on their own.

## Safety

- **Live keys are refused.** Any request with an `sk_live_` or `rk_live_` key gets a 403 and never leaves the proxy.
- Against a real upstream, only `sk_test_` / `rk_test_` keys are forwarded.
- API keys are never logged or stored; the auditor keeps only a short hash to group requests by account.
- The proxy binds to `127.0.0.1` by default.

## Faults

| Fault | What happens | Reaches upstream? |
|---|---|---|
| `drop_after_commit` | Forwarded and processed, then the client connection is reset (TCP RST) before the response arrives | Yes |
| `error_500_after_commit` | Forwarded and processed, then the client is told `500` | Yes |
| `hold_response` | Forwarded and processed; the response is held for `delay_ms`, so a client with a shorter timeout gives up | Yes |
| `truncate_after_commit` | Forwarded and processed; the client gets the real status and headers but only half the body, then a TCP reset | Yes |
| `timeout_before_forward` | Proxy stalls, then answers 504 | No |
| `rate_limit_429` | Stripe-shaped `rate_limit` error | No |
| `error_500` | Stripe-shaped `api_error` | No |
| `latency` | Delay, then forwarded normally | Yes |

Rules are evaluated in file order; the first matching rule that fires wins. A rule can set `"should_retry": "true"` or `"false"` to send Stripe's `Stripe-Should-Retry` header on its injected errors; official SDKs obey it. It is opt-in rather than automatic because the right value depends on the scenario: stripe-go's source notes that Stripe usually sends `false` on 500s. Print the default scenario with `paychaos scenario`, and check your own files with `paychaos validate my-scenario.json` (it lists the known faults when one is wrong).

## What the auditor flags

Only POSTs to money-moving endpoints (`/v1/payment_intents`, `/v1/charges`, `/v1/refunds`, `/v1/payouts`, `/v1/transfers`, and others; configurable via `mutating_paths`) are audited.

| Code | Severity | Meaning |
|---|---|---|
| `UNSAFE_RETRY_AFTER_LOST_RESPONSE` | HIGH | A request committed upstream but its result never reached the client, and a retry with no key or a different key **also committed**. Names both created objects. |
| `UNSAFE_RETRY_FAILED_THIS_TIME` | MEDIUM | The same unsafe retry, but it failed this time, so no duplicate was created in this run. |
| `COMMITTED_BUT_REPORTED_AS_FAILED` | MEDIUM | A request committed upstream, the client got an error or nothing, and never retried. The app likely recorded a failure for a payment that succeeded. |
| `NEW_KEY_AFTER_INDETERMINATE_ERROR` | MEDIUM | Retried with a new key after a 409, a 5xx or a timeout. Stripe treats these outcomes as indeterminate; a new key can start a second operation. |
| `IDEMPOTENCY_KEY_REUSED_WITH_DIFFERENT_BODY` | MEDIUM | One key used for two different requests; Stripe rejects this. |
| `MISSING_IDEMPOTENCY_KEY` | MEDIUM | A money-moving POST without a key. |
| `RETRY_WITHOUT_BACKOFF` | LOW | Retried a 429/5xx faster than `min_backoff_ms`. |
| `TRUNCATED_SUCCESS_NOT_RETRIED` | LOW | A request committed and the client got a 2xx, but the body was cut short and it never retried. Whether that is a bug depends on whether the client reads the created object from the body. |

### Grouping retries by operation

By default two requests are "the same operation" when they share account, method, path and parameters (form parameters are canonicalised, so order doesn't matter). That misses clients that change something between attempts, such as an attempt counter or a timestamp-based key. Set `operation_id` to the parameters that identify one business operation:

```json
"operation_id": ["metadata[order_id]"]
```

Requests carrying it are grouped by its value instead. Stripe recommends putting a local identifier in metadata for reconciliation, so most integrations already send one. The rotating-key demo client produces 5 duplicates that are only caught this way.

The process exits with status `2` when any HIGH finding exists, so it can gate a CI job (`-fail-on-high=false` to disable). A live JSON report is served at `GET /__paychaos/report`.

## CI integration

Besides the JSON report, a run can write two more formats on exit:

```bash
paychaos -config paychaos.json -junit paychaos-junit.xml -markdown "$GITHUB_STEP_SUMMARY"
```

- `-junit FILE` writes one failing test case per finding, so CI test reporters (GitHub, GitLab, Jenkins, CircleCI) show findings next to ordinary test results. A clean run writes one passing case.
- `-markdown FILE` appends a summary table, suited to `$GITHUB_STEP_SUMMARY` or a PR comment. Use `-` for stdout.

Existing integrations rarely start with zero findings. `paychaos compare` lets a pipeline fail only on findings a change introduces:

```bash
paychaos compare -fail-on medium baseline.json current.json
```

Findings are grouped by code, severity and path, and counted, because request numbers and object ids differ between runs. It prints what got worse (`+`) and better (`-`) and exits `2` when a new finding is at least as severe as `-fail-on` (default `high`). Add `-json` for machine-readable output. Both runs should use the same seed, scenario and workload, otherwise different faults fire; `compare` warns when the seeds differ. `examples/ci/github-actions.yml` is a complete workflow that builds a baseline on the base branch and compares the pull request against it.

## Stripe behaviour that is modelled

From Stripe's [idempotent requests](https://docs.stripe.com/api/idempotent_requests) and [low-level error handling](https://docs.stripe.com/error-low-level) docs:

- **A 500 is indeterminate.** The operation may have had side effects; don't retry with a new key until the outcome is reconciled. Modelled by `error_500_after_commit` and the indeterminate-error finding.
- **Results are cached once execution begins, including 500s.** A same-key retry gets the original response back with `Idempotent-Replayed: true`. The mock does this; `paychaos mock -server-error-every N` makes every Nth create fail with a cached 500 after creating the object.
- **Validation errors and conflicts are not cached.** A request rejected before execution can be fixed and retried with the same key.
- **409 on a concurrent same-key request.** `paychaos mock -exec-delay 2s` keeps each create running long enough for a retry to collide with it.
- **`Stripe-Should-Retry`** guides SDK retries; see Faults above.

## Design decisions

These are the parts that are easy to get subtly wrong.

- **Truncation keeps the real status.** `truncate_after_commit` writes the upstream's own status line and a `Content-Length` for the full body, then sends half of it and resets. The client holds a genuine 200; only parsing fails. Because the client may have accepted that 200, not retrying it is LOW rather than the MEDIUM used when the client was told the request failed.
- **A real TCP reset, not an error response.** `drop_after_commit` hijacks the client connection and closes it with `SO_LINGER` set to 0. A normal close sends FIN after flushing; linger 0 discards buffered data and sends RST, so the client sees "connection reset by peer", which is what a dropped load-balancer connection looks like. Returning a 502 instead would tell the client the request failed, which is a different and easier case.
- **The upstream call outlives the client.** For faults that cut the client off, the upstream request uses `context.WithoutCancel`, so cutting the client doesn't cancel the charge mid-flight. Otherwise the fault would test nothing.
- **Record before reset.** The proxy records the committed attempt before resetting the connection. The client's retry can only start after the reset, so it is guaranteed to see the earlier attempt; recording afterwards would be a race the retry could win.
- **Verdicts wait for the outcome.** Whether a retry duplicated something is decided when the retry completes, not when it arrives, so a retry that itself fails is not reported as a duplicate charge. Retries are traced back to the lost original even when other attempts sit in between.
- **Evidence over inference.** The proxy reads the created object's `id` from each response, so a finding says `pi_A and pi_B were both created` instead of "a duplicate probably happened".
- **Reproducible faults.** Every fault decision comes from a seeded PRNG, and a random number is drawn for every matching rule that still has hits left, whether or not it fires, so one rule's outcome doesn't shift the draws for the next. The webhook relay has its own stream, so webhook traffic can't change which API requests get faults.
- **No dependencies in the tool.** Everything is standard library. The stripe-go example is its own Go module so the tool stays dependency-free.

## When this matters (and when it doesn't)

Stripe's SDKs can generate idempotency keys and retry automatically. stripe-go, for example, attaches one key per call and reuses it on all of that call's retries (checked in its v87 source). If you rely only on that, a lost response on a single call is recovered safely, as the SDK row above shows. The bugs `paychaos` targets live around that:

- hand-written HTTP clients that call the API directly;
- **application-level retries**, such as a job queue re-running a whole checkout function, which generates a fresh key on each run;
- keys derived from timestamps or random values instead of the order;
- treating an error as a definite failure when the payment may have succeeded;
- webhook handlers that assume exactly-once, in-order delivery.

## Layout

```
cmd/paychaos/            CLI: proxy, webhook relay, mock, scenario printer
internal/chaos/          rule engine, seeded streams, scenario loading
internal/proxy/          reverse proxy, key safety checks, faults, connection reset
internal/audit/          fingerprinting, operation grouping, retry-safety findings
internal/webhook/        duplicating / delaying / reordering relay
internal/mock/           small Stripe-shaped API with idempotency semantics
internal/report/         JSON, terminal, JUnit and Markdown reports; baseline comparison
examples/                naive, rotating-key and safe clients; stripe-go client; scenarios; CI workflow
scripts/                 demos and the GIF renderer
```

## Limitations

- The mock implements create/retrieve and idempotency only. It is not a Stripe emulator; use Stripe test mode for realistic behaviour.
- `error_500_after_commit` and `hold_response` change what the client sees. Against real Stripe, a same-key retry then replays the real (successful) result, which is the correct behaviour to test.
- Connection reset relies on HTTP/1.1 hijacking. Over HTTP/2 the proxy falls back to an empty 502.
- Without `operation_id`, two genuine identical purchases in a row can be read as a retry if the first one hit a fault, and retries that change parameters are missed.
- Reproducibility requires the same order of API requests. Concurrent clients can reach the rule engine in a different order between runs.
- The proxy sees requests, not your database. `COMMITTED_BUT_REPORTED_AS_FAILED` can't tell whether your app later reconciled via webhooks, and the webhook relay reports what it delivered rather than what your handler did.

## Licence

MIT. See [LICENSE](LICENSE).

## Author

Shashaank MK ([@shangai2405](https://github.com/shangai2405))
