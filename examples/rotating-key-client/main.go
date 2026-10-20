// rotating-key-client looks careful but isn't: it sends an Idempotency-Key and
// backs off, yet it builds a fresh key from the clock on every attempt and
// records the attempt number in metadata. Each retry is therefore a brand-new
// operation to Stripe. Body-hash grouping can't link the attempts because the
// parameters differ; run paychaos with "operation_id": ["metadata[order_id]"]
// to catch it.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func main() {
	base := flag.String("base", "http://127.0.0.1:8080", "API base URL (point at paychaos)")
	key := flag.String("key", "sk_test_demo", "Stripe test key")
	n := flag.Int("n", 20, "number of checkouts to simulate")
	flag.Parse()

	client := &http.Client{Timeout: 10 * time.Second}
	ok := 0
	for i := 0; i < *n; i++ {
		backoff := 250 * time.Millisecond
		for attempt := 1; attempt <= 3; attempt++ {
			form := url.Values{"amount": {fmt.Sprint(1000 + i)}, "currency": {"inr"}, "confirm": {"true"},
				"metadata[order_id]": {fmt.Sprintf("order_%03d", i)},
				"metadata[attempt]":  {fmt.Sprint(attempt)}}
			req, _ := http.NewRequest(http.MethodPost, *base+"/v1/payment_intents", strings.NewReader(form.Encode()))
			req.Header.Set("Authorization", "Bearer "+*key)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			// BUG: the key is derived from the clock, so every attempt gets a new one.
			req.Header.Set("Idempotency-Key", fmt.Sprintf("order_%03d-%d", i, time.Now().UnixNano()))
			resp, err := client.Do(req)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					ok++
					break
				}
			}
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	fmt.Printf("rotating-key-client: %d/%d checkouts reported success\n", ok, *n)
}
