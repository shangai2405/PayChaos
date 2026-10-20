package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/audit"
	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/chaos"
)

var truncateFirst = chaos.Rule{Name: "truncate", Match: chaos.Match{Method: "POST", PathPrefix: "/v1/payment_intents"},
	Fault: chaos.FaultTruncateAfterCommit, Probability: 1, MaxHits: 1}

// postRead is post, but also reports whether the body could be read in full
// and parsed as JSON, which is what a truncated response breaks.
func postRead(t *testing.T, base, idem string) (status int, bodyErr error) {
	t.Helper()
	form := url.Values{"amount": {"5000"}, "currency": {"inr"}, "confirm": {"true"}}
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/payment_intents", strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request failed before the status line: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	var v map[string]any
	return resp.StatusCode, json.Unmarshal(b, &v)
}

// The client gets a 200 whose body is cut off. Treating that as a failure and
// retrying without a key duplicates the payment.
func TestTruncatedResponseThenKeylessRetry(t *testing.T) {
	h := newHarness(t, truncateFirst)
	st, err := postRead(t, h.proxy.URL, "")
	if st != 200 || err == nil {
		t.Fatalf("want a 200 with an unreadable body, got status=%d bodyErr=%v", st, err)
	}
	if st, err := postRead(t, h.proxy.URL, ""); st != 200 || err != nil {
		t.Fatalf("retry: status=%d err=%v", st, err)
	}
	if got := h.mock.Created("payment_intents"); got != 2 {
		t.Fatalf("created %d, want 2", got)
	}
	if codes(h.auditor.Findings())[audit.CodeUnsafeRetry] != audit.High {
		t.Fatalf("expected HIGH unsafe retry, got %+v", h.auditor.Findings())
	}
}

// A same-key retry after a truncated body is replayed and clean.
func TestTruncatedResponseSameKeyRetry(t *testing.T) {
	h := newHarness(t, truncateFirst)
	_, _ = postRead(t, h.proxy.URL, "order-1")
	if st, err := postRead(t, h.proxy.URL, "order-1"); st != 200 || err != nil {
		t.Fatalf("retry: status=%d err=%v", st, err)
	}
	if h.mock.Created("payment_intents") != 1 || len(h.auditor.Findings()) != 0 {
		t.Fatalf("created=%d findings=%+v", h.mock.Created("payment_intents"), h.auditor.Findings())
	}
}

// Not retrying a truncated body is reported, at LOW, because the client did
// see a success status and may have accepted it.
func TestTruncatedResponseNeverRetried(t *testing.T) {
	h := newHarness(t, truncateFirst)
	_, _ = postRead(t, h.proxy.URL, "order-1")
	fs := h.auditor.Findings()
	if len(fs) != 1 || fs[0].Code != audit.CodeTruncatedAccepted || fs[0].Severity != audit.Low || len(fs[0].ObjectIDs) != 1 {
		t.Fatalf("want one LOW %s naming the object, got %+v", audit.CodeTruncatedAccepted, fs)
	}
}
