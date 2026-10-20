// safe-client is the corrected integration: one Idempotency-Key per logical
// checkout, reused on every retry, with exponential backoff and jitter.
package main

import (
	crand "crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func newKey() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}

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
		// In a real app, persist this key with the order so a crashed and
		// restarted job reuses it too.
		idem := newKey()
		backoff := 250 * time.Millisecond
		for attempt := 1; attempt <= 5; attempt++ {
			req, _ := http.NewRequest(http.MethodPost, *base+"/v1/payment_intents", strings.NewReader(form.Encode()))
			req.Header.Set("Authorization", "Bearer "+*key)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Idempotency-Key", idem)
			resp, err := client.Do(req)
			status := 0
			if err == nil {
				status = resp.StatusCode
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			if status == 200 {
				ok++
				break
			}
			// Retry only on network errors, 409, 429 and 5xx; 4xx means fix the request.
			if err == nil && status != 409 && status != 429 && status < 500 {
				break
			}
			jitter := time.Duration(mrand.Int64N(int64(backoff / 2)))
			time.Sleep(backoff + jitter)
			backoff *= 2
		}
	}
	fmt.Printf("safe-client: %d/%d checkouts reported success\n", ok, *n)
}
