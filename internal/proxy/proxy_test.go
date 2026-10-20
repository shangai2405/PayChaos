package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/audit"
	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/chaos"
	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/mock"
)

type harness struct {
	mock    *mock.Server
	auditor *audit.Auditor
	proxy   *httptest.Server
}

func newHarness(t *testing.T, rules ...chaos.Rule) harness {
	t.Helper()
	return newHarnessOp(t, nil, rules...)
}

func newHarnessOp(t *testing.T, opParams []string, rules ...chaos.Rule) harness {
	t.Helper()
	m := mock.New()
	up := httptest.NewServer(m)
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	cfg := chaos.Config{Seed: 1, Rules: rules, MutatingPaths: chaos.DefaultMutatingPaths}
	a := audit.New(cfg.MutatingPaths, 0).WithOperationID(opParams)
	px, err := New(Options{Upstream: u, Engine: chaos.NewEngine(cfg), Auditor: a})
	if err != nil {
		t.Fatal(err)
	}
	ps := httptest.NewServer(px)
	t.Cleanup(ps.Close)
	return harness{mock: m, auditor: a, proxy: ps}
}

func post(t *testing.T, base, key, idem string) (int, error) {
	t.Helper()
	form := url.Values{"amount": {"5000"}, "currency": {"inr"}, "confirm": {"true"}}
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/payment_intents", strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

var dropFirst = chaos.Rule{Name: "drop", Match: chaos.Match{Method: "POST", PathPrefix: "/v1/payment_intents"},
	Fault: chaos.FaultDropAfterCommit, Probability: 1, MaxHits: 1}

func codes(fs []audit.Finding) map[string]audit.Severity {
	m := map[string]audit.Severity{}
	for _, f := range fs {
		m[f.Code] = f.Severity
	}
	return m
}

// A client that retries a lost response without an idempotency key creates two
// PaymentIntents upstream, and the auditor flags it.
func TestNaiveRetryDoubleCharges(t *testing.T) {
	h := newHarness(t, dropFirst)
	if _, err := post(t, h.proxy.URL, "sk_test_x", ""); err == nil {
		t.Fatal("expected the first attempt to fail with a reset connection")
	}
	if st, err := post(t, h.proxy.URL, "sk_test_x", ""); err != nil || st != 200 {
		t.Fatalf("retry: status=%d err=%v", st, err)
	}
	if got := h.mock.Created("payment_intents"); got != 2 {
		t.Fatalf("upstream created %d payment intents, want 2 (the duplicate charge)", got)
	}
	if sev := codes(h.auditor.Findings())[audit.CodeUnsafeRetry]; sev != audit.High {
		t.Fatalf("want HIGH %s, got findings %+v", audit.CodeUnsafeRetry, h.auditor.Findings())
	}
	for _, f := range h.auditor.Findings() {
		if f.Code == audit.CodeUnsafeRetry && (len(f.ObjectIDs) != 2 || f.ObjectIDs[0] == f.ObjectIDs[1]) {
			t.Fatalf("finding should name both created PaymentIntents, got %v", f.ObjectIDs)
		}
	}
}

// The same retry with a stable idempotency key is replayed, not duplicated.
func TestIdempotentRetryIsSafe(t *testing.T) {
	h := newHarness(t, dropFirst)
	if _, err := post(t, h.proxy.URL, "sk_test_x", "order-42"); err == nil {
		t.Fatal("expected the first attempt to fail")
	}
	if st, err := post(t, h.proxy.URL, "sk_test_x", "order-42"); err != nil || st != 200 {
		t.Fatalf("retry: status=%d err=%v", st, err)
	}
	if got := h.mock.Created("payment_intents"); got != 1 {
		t.Fatalf("upstream created %d payment intents, want 1", got)
	}
	for _, f := range h.auditor.Findings() {
		if f.Severity == audit.High {
			t.Fatalf("unexpected HIGH finding: %+v", f)
		}
	}
}

// Rotating the key on each retry is as bad as having no key.
func TestNewKeyPerRetryDoubleCharges(t *testing.T) {
	h := newHarness(t, dropFirst)
	_, _ = post(t, h.proxy.URL, "sk_test_x", "attempt-1")
	_, _ = post(t, h.proxy.URL, "sk_test_x", "attempt-2")
	if got := h.mock.Created("payment_intents"); got != 2 {
		t.Fatalf("created %d, want 2", got)
	}
	if codes(h.auditor.Findings())[audit.CodeUnsafeRetry] != audit.High {
		t.Fatal("expected HIGH unsafe retry finding")
	}
}

func TestLiveKeysAreRefused(t *testing.T) {
	h := newHarness(t)
	st, err := post(t, h.proxy.URL, "sk_live_abc", "")
	if err != nil || st != http.StatusForbidden {
		t.Fatalf("status=%d err=%v, want 403", st, err)
	}
	if h.mock.Created("payment_intents") != 0 {
		t.Fatal("live-key request must never reach the upstream")
	}
}

func TestNonTestKeysAreRefusedForRealUpstream(t *testing.T) {
	h := newHarness(t)
	st, _ := post(t, h.proxy.URL, "garbage", "")
	if st != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", st)
	}
}

func TestInjectedErrorsDoNotReachUpstream(t *testing.T) {
	h := newHarness(t,
		chaos.Rule{Name: "429", Fault: chaos.FaultRateLimit, Probability: 1, MaxHits: 1},
		chaos.Rule{Name: "500", Fault: chaos.FaultError500, Probability: 1, MaxHits: 1},
	)
	if st, _ := post(t, h.proxy.URL, "sk_test_x", "k"); st != 429 {
		t.Fatalf("first: %d want 429", st)
	}
	if st, _ := post(t, h.proxy.URL, "sk_test_x", "k"); st != 500 {
		t.Fatalf("second: %d want 500", st)
	}
	if st, _ := post(t, h.proxy.URL, "sk_test_x", "k"); st != 200 {
		t.Fatalf("third: %d want 200", st)
	}
	if got := h.mock.Created("payment_intents"); got != 1 {
		t.Fatalf("created %d, want 1", got)
	}
}

func postForm(t *testing.T, base string, form url.Values, idem string) (int, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/payment_intents", strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// A client that rotates its key and bumps an attempt counter on every retry
// changes the request body, so body-hash grouping cannot see the duplicate.
// Grouping by operation id (metadata[order_id]) does.
func TestOperationIDCatchesChangedRetry(t *testing.T) {
	attempt := func(n int) url.Values {
		return url.Values{"amount": {"5000"}, "currency": {"inr"},
			"metadata[order_id]": {"order_1042"}, "metadata[attempt]": {fmt.Sprint(n)}}
	}
	run := func(op []string) harness {
		h := newHarnessOp(t, op, dropFirst)
		_, _ = postForm(t, h.proxy.URL, attempt(1), fmt.Sprintf("ts-%d", 1))
		if st, err := postForm(t, h.proxy.URL, attempt(2), fmt.Sprintf("ts-%d", 2)); err != nil || st != 200 {
			t.Fatalf("retry: %d %v", st, err)
		}
		if got := h.mock.Created("payment_intents"); got != 2 {
			t.Fatalf("created %d, want 2", got)
		}
		return h
	}
	if codes(run(nil).auditor.Findings())[audit.CodeUnsafeRetry] != "" {
		t.Fatal("without operation_id the changed body should not be matched (documented limitation)")
	}
	if codes(run([]string{"metadata[order_id]"}).auditor.Findings())[audit.CodeUnsafeRetry] != audit.High {
		t.Fatal("operation_id grouping should flag the duplicate as HIGH")
	}
}

// Grouping by operation must not break key-reuse detection, which compares
// exact parameters.
func TestKeyReuseStillUsesExactBody(t *testing.T) {
	h := newHarnessOp(t, []string{"metadata[order_id]"})
	f := url.Values{"amount": {"5000"}, "currency": {"inr"}, "metadata[order_id]": {"o1"}}
	_, _ = postForm(t, h.proxy.URL, f, "k1")
	f.Set("amount", "6000")
	if st, _ := postForm(t, h.proxy.URL, f, "k1"); st != 400 {
		t.Fatalf("mock should reject reused key with new params, got %d", st)
	}
	if _, ok := codes(h.auditor.Findings())[audit.CodeKeyReusedNewBody]; !ok {
		t.Fatal("expected key-reuse finding")
	}
}

func newHarnessMock(t *testing.T, m *mock.Server, rules ...chaos.Rule) harness {
	t.Helper()
	up := httptest.NewServer(m)
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	cfg := chaos.Config{Seed: 1, Rules: rules, MutatingPaths: chaos.DefaultMutatingPaths}
	a := audit.New(cfg.MutatingPaths, 0)
	px, err := New(Options{Upstream: u, Engine: chaos.NewEngine(cfg), Auditor: a})
	if err != nil {
		t.Fatal(err)
	}
	ps := httptest.NewServer(px)
	t.Cleanup(ps.Close)
	return harness{mock: m, auditor: a, proxy: ps}
}

func postTimeout(t *testing.T, base, idem string, timeout time.Duration) (*http.Response, error) {
	t.Helper()
	form := url.Values{"amount": {"5000"}, "currency": {"inr"}}
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/payment_intents", strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	c := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return resp, err
}

var fiveHundredAfterCommit = chaos.Rule{Name: "500ac", Match: chaos.Match{PathPrefix: "/v1/payment_intents"},
	Fault: chaos.FaultError500AfterCommit, Probability: 1, MaxHits: 1}

// Stripe: a 500 may have committed. Retrying with a new key duplicates; with
// the same key the original result is replayed.
func TestError500AfterCommit(t *testing.T) {
	h := newHarness(t, fiveHundredAfterCommit)
	if st, _ := post(t, h.proxy.URL, "sk_test_x", "a"); st != 500 {
		t.Fatalf("first: %d want 500", st)
	}
	_, _ = post(t, h.proxy.URL, "sk_test_x", "b")
	if h.mock.Created("payment_intents") != 2 || codes(h.auditor.Findings())[audit.CodeUnsafeRetry] != audit.High {
		t.Fatalf("new-key retry after committed 500: created=%d findings=%+v", h.mock.Created("payment_intents"), h.auditor.Findings())
	}

	h2 := newHarness(t, fiveHundredAfterCommit)
	_, _ = post(t, h2.proxy.URL, "sk_test_x", "same")
	if st, _ := post(t, h2.proxy.URL, "sk_test_x", "same"); st != 200 {
		t.Fatalf("same-key retry: %d want replayed 200", st)
	}
	if h2.mock.Created("payment_intents") != 1 || len(codes(h2.auditor.Findings())) != 0 {
		t.Fatalf("same-key retry should be clean: created=%d findings=%+v", h2.mock.Created("payment_intents"), h2.auditor.Findings())
	}
}

// A client whose timeout is shorter than the held response gives up on a
// committed request; a keyless retry duplicates it.
func TestHoldResponseClientGivesUp(t *testing.T) {
	hold := chaos.Rule{Name: "hold", Fault: chaos.FaultHoldResponse, Probability: 1, MaxHits: 1, DelayMS: 800}
	h := newHarness(t, hold)
	if _, err := postTimeout(t, h.proxy.URL, "", 200*time.Millisecond); err == nil {
		t.Fatal("expected client timeout")
	}
	if resp, err := postTimeout(t, h.proxy.URL, "", 2*time.Second); err != nil || resp.StatusCode != 200 {
		t.Fatalf("retry failed: %v", err)
	}
	if h.mock.Created("payment_intents") != 2 || codes(h.auditor.Findings())[audit.CodeUnsafeRetry] != audit.High {
		t.Fatalf("created=%d findings=%+v", h.mock.Created("payment_intents"), h.auditor.Findings())
	}
}

// A held response the client waits for is delivered; a later identical
// request is not blamed on it.
func TestHoldResponseDelivered(t *testing.T) {
	hold := chaos.Rule{Name: "hold", Fault: chaos.FaultHoldResponse, Probability: 1, MaxHits: 1, DelayMS: 100}
	h := newHarness(t, hold)
	if resp, err := postTimeout(t, h.proxy.URL, "k1", 2*time.Second); err != nil || resp.StatusCode != 200 {
		t.Fatalf("held request: %v", err)
	}
	_, _ = postTimeout(t, h.proxy.URL, "k2", 2*time.Second)
	if _, ok := codes(h.auditor.Findings())[audit.CodeUnsafeRetry]; ok {
		t.Fatalf("delivered response treated as lost: %+v", h.auditor.Findings())
	}
}

func TestShouldRetryHeader(t *testing.T) {
	h := newHarness(t,
		chaos.Rule{Name: "429", Fault: chaos.FaultRateLimit, Probability: 1, MaxHits: 1, ShouldRetry: "true"},
		chaos.Rule{Name: "500", Fault: chaos.FaultError500, Probability: 1, MaxHits: 1},
	)
	r1, _ := postTimeout(t, h.proxy.URL, "k", time.Second)
	if r1.StatusCode != 429 || r1.Header.Get("Stripe-Should-Retry") != "true" {
		t.Fatalf("429: status %d header %q", r1.StatusCode, r1.Header.Get("Stripe-Should-Retry"))
	}
	r2, _ := postTimeout(t, h.proxy.URL, "k", time.Second)
	if r2.StatusCode != 500 || r2.Header.Get("Stripe-Should-Retry") != "" {
		t.Fatalf("500 without should_retry must not send the header, got %q", r2.Header.Get("Stripe-Should-Retry"))
	}
}

// The upstream's own 500 is cached under the key. A same-key retry gets the
// replayed 500; a new-key retry is flagged as unsafe after an indeterminate
// error, and here it really does duplicate.
func TestUpstream500CachedAndNewKeyFlagged(t *testing.T) {
	h := newHarnessMock(t, mock.NewWithOptions(mock.Options{ServerErrorEvery: 1}))
	r1, _ := postTimeout(t, h.proxy.URL, "k1", time.Second)
	r2, _ := postTimeout(t, h.proxy.URL, "k1", time.Second)
	if r1.StatusCode != 500 || r2.StatusCode != 500 || r2.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("want cached 500 replay, got %d then %d (replayed=%q)", r1.StatusCode, r2.StatusCode, r2.Header.Get("Idempotent-Replayed"))
	}
	if h.mock.Created("payment_intents") != 1 {
		t.Fatalf("same-key replay created another object")
	}
	_, _ = postTimeout(t, h.proxy.URL, "k2", time.Second)
	if h.mock.Created("payment_intents") != 2 {
		t.Fatal("new key after a 500 should create a second object")
	}
	if codes(h.auditor.Findings())[audit.CodeAmbiguousRetry] != audit.Medium {
		t.Fatalf("want MEDIUM %s, got %+v", audit.CodeAmbiguousRetry, h.auditor.Findings())
	}
}

// Reusing a key while the first request is still executing gets a 409;
// switching to a new key afterwards is flagged.
func TestConflictThenNewKey(t *testing.T) {
	h := newHarnessMock(t, mock.NewWithOptions(mock.Options{ExecDelay: 400 * time.Millisecond}))
	done := make(chan struct{})
	go func() { _, _ = postTimeout(t, h.proxy.URL, "k1", 2*time.Second); close(done) }()
	time.Sleep(100 * time.Millisecond)
	r, err := postTimeout(t, h.proxy.URL, "k1", 2*time.Second)
	if err != nil || r.StatusCode != 409 {
		t.Fatalf("want 409 for concurrent same key, got %v %v", r, err)
	}
	<-done
	_, _ = postTimeout(t, h.proxy.URL, "k2", 2*time.Second)
	if codes(h.auditor.Findings())[audit.CodeAmbiguousRetry] != audit.Medium {
		t.Fatalf("want MEDIUM after 409 + new key, got %+v", h.auditor.Findings())
	}
}
