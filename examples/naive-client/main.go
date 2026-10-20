// naive-client is a deliberately buggy payment integration: it retries failed
// requests immediately and without an Idempotency-Key. Run it through paychaos
// to see the duplicate charges it causes.
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
		form := url.Values{"amount": {fmt.Sprint(1000 + i)}, "currency": {"inr"}, "confirm": {"true"},
			"metadata[order_id]": {fmt.Sprintf("order_%03d", i)}}
		for attempt := 1; attempt <= 3; attempt++ {
			req, _ := http.NewRequest(http.MethodPost, *base+"/v1/payment_intents", strings.NewReader(form.Encode()))
			req.Header.Set("Authorization", "Bearer "+*key)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			// BUG: no Idempotency-Key, and no backoff between attempts.
			resp, err := client.Do(req)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					ok++
					break
				}
			}
		}
	}
	fmt.Printf("naive-client: %d/%d checkouts reported success\n", ok, *n)
}
