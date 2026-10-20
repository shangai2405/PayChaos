// stripe-go-client runs real checkouts with the official stripe-go SDK through
// paychaos, in two modes:
//
//	-mode sdk        one SDK call per checkout. stripe-go attaches an
//	                 idempotency key to every POST and reuses it on its own
//	                 retries, so a lost response is recovered safely.
//	-mode job-retry  the same SDK call wrapped in an application-level retry,
//	                 like a job queue re-running a failed job. Every re-run is
//	                 a new SDK call with a new key, so an error that hid a
//	                 committed charge (a 500 the SDK was told not to retry)
//	                 turns into a second charge.
//
// Run against examples/scenarios/sdk.json; see scripts/demo-sdk.sh.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/stripe/stripe-go/v87"
)

func main() {
	base := flag.String("base", "http://127.0.0.1:8080", "API base URL (point at paychaos)")
	key := flag.String("key", "sk_test_demo", "Stripe test key")
	n := flag.Int("n", 20, "number of checkouts to simulate")
	mode := flag.String("mode", "sdk", `"sdk" or "job-retry"`)
	flag.Parse()
	if *mode != "sdk" && *mode != "job-retry" {
		fmt.Fprintln(os.Stderr, `-mode must be "sdk" or "job-retry"`)
		os.Exit(1)
	}

	backend := stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(*base),
		MaxNetworkRetries: stripe.Int64(2), // the SDK default, made explicit
		LeveledLogger:     &stripe.LeveledLogger{Level: stripe.LevelNull},
	})
	sc := stripe.NewClient(*key, stripe.WithBackends(&stripe.Backends{
		API: backend, Connect: backend, Uploads: backend, MeterEvents: backend,
	}))

	charge := func(i int) error {
		params := &stripe.PaymentIntentCreateParams{
			Amount:   stripe.Int64(int64(1000 + i)),
			Currency: stripe.String("inr"),
		}
		params.AddMetadata("order_id", fmt.Sprintf("order_%03d", i))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := sc.V1PaymentIntents.Create(ctx, params)
		return err
	}

	ok, failed := 0, 0
	for i := 0; i < *n; i++ {
		attempts := 1
		if *mode == "job-retry" {
			attempts = 3 // BUG: each attempt is a new SDK call with a new key
		}
		var err error
		for a := 0; a < attempts; a++ {
			if err = charge(i); err == nil {
				break
			}
		}
		if err == nil {
			ok++
		} else {
			failed++
		}
	}
	fmt.Printf("stripe-go client (%s): %d/%d checkouts reported success, %d reported failure\n", *mode, ok, *n, failed)
}
