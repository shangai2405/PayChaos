package audit

import (
	"testing"
	"time"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/chaos"
)

func newClocked(minBackoff time.Duration) (*Auditor, *time.Time) {
	a := New(chaos.DefaultMutatingPaths, minBackoff)
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }
	return a, &now
}

func has(fs []Finding, code string) bool {
	for _, f := range fs {
		if f.Code == code {
			return true
		}
	}
	return false
}

func TestFingerprintIgnoresParamOrder(t *testing.T) {
	ct := "application/x-www-form-urlencoded"
	a := Fingerprint("sk_test_1", "POST", "/v1/charges", ct, []byte("amount=100&currency=inr"))
	b := Fingerprint("sk_test_1", "POST", "/v1/charges", ct, []byte("currency=inr&amount=100"))
	c := Fingerprint("sk_test_2", "POST", "/v1/charges", ct, []byte("amount=100&currency=inr"))
	if a != b {
		t.Fatal("param order changed the fingerprint")
	}
	if a == c {
		t.Fatal("different accounts share a fingerprint")
	}
}

func TestKeyReusedWithDifferentBody(t *testing.T) {
	a, _ := newClocked(0)
	a.Complete(a.Begin("POST", "/v1/refunds", "k1", "fpA"), "", chaos.FaultNone, 200, true, "")
	a.Begin("POST", "/v1/refunds", "k1", "fpB")
	if !has(a.Findings(), CodeKeyReusedNewBody) {
		t.Fatal("expected key-reuse finding")
	}
}

func TestNoBackoff(t *testing.T) {
	a, now := newClocked(200 * time.Millisecond)
	a.Complete(a.Begin("POST", "/v1/charges", "k", "fp"), "r", chaos.FaultRateLimit, 0, false, "")
	*now = now.Add(20 * time.Millisecond)
	a.Begin("POST", "/v1/charges", "k", "fp")
	if !has(a.Findings(), CodeNoBackoff) {
		t.Fatal("expected no-backoff finding")
	}

	b, now2 := newClocked(200 * time.Millisecond)
	b.Complete(b.Begin("POST", "/v1/charges", "k", "fp"), "r", chaos.FaultRateLimit, 0, false, "")
	*now2 = now2.Add(time.Second)
	b.Begin("POST", "/v1/charges", "k", "fp")
	if has(b.Findings(), CodeNoBackoff) {
		t.Fatal("1s backoff should not be flagged")
	}
}

func TestAmbiguousTimeoutRetry(t *testing.T) {
	a, _ := newClocked(0)
	a.Complete(a.Begin("POST", "/v1/payouts", "", "fp"), "t", chaos.FaultTimeoutBeforeForward, 0, false, "")
	a.Begin("POST", "/v1/payouts", "", "fp")
	if !has(a.Findings(), CodeAmbiguousRetry) {
		t.Fatal("expected ambiguous timeout finding")
	}
}

func TestOnlyMutatingPostsAudited(t *testing.T) {
	a := New(chaos.DefaultMutatingPaths, 0)
	if a.IsMutating("GET", "/v1/payment_intents") || a.IsMutating("POST", "/v1/balance") {
		t.Fatal("non-mutating request classified as mutating")
	}
	if !a.IsMutating("POST", "/v1/payment_intents/pi_1/confirm") {
		t.Fatal("confirm should be audited")
	}
}

func severityOf(fs []Finding, code string) Severity {
	for _, f := range fs {
		if f.Code == code {
			return f.Severity
		}
	}
	return ""
}

// A retry that itself fails cannot have created a duplicate, so it must not be
// reported as one; it is still worth a MEDIUM warning.
func TestFailedUnsafeRetryIsMediumNotHigh(t *testing.T) {
	a, _ := newClocked(0)
	a.Complete(a.Begin("POST", "/v1/payment_intents", "", "fp"), "drop", chaos.FaultDropAfterCommit, 200, true, "pi_A")
	a.Complete(a.Begin("POST", "/v1/payment_intents", "", "fp"), "429", chaos.FaultRateLimit, 0, false, "")
	fs := a.Findings()
	if has(fs, CodeUnsafeRetry) {
		t.Fatalf("failed retry reported as a duplicate: %+v", fs)
	}
	if severityOf(fs, CodeWouldDuplicate) != Medium {
		t.Fatalf("want MEDIUM %s, got %+v", CodeWouldDuplicate, fs)
	}
}

// lost response -> retry hits 429 -> retry succeeds: the duplicate must be
// traced back to the lost original, not to the 429 in between.
func TestRetryChainTracedToLostOriginal(t *testing.T) {
	a, _ := newClocked(0)
	a.Complete(a.Begin("POST", "/v1/payment_intents", "", "fp"), "drop", chaos.FaultDropAfterCommit, 200, true, "pi_A")
	a.Complete(a.Begin("POST", "/v1/payment_intents", "", "fp"), "429", chaos.FaultRateLimit, 0, false, "")
	a.Complete(a.Begin("POST", "/v1/payment_intents", "", "fp"), "", chaos.FaultNone, 200, true, "pi_B")
	var high *Finding
	for _, f := range a.Findings() {
		if f.Code == CodeUnsafeRetry {
			f := f
			high = &f
		}
	}
	if high == nil || high.Severity != High {
		t.Fatalf("expected HIGH finding, got %+v", a.Findings())
	}
	if len(high.ObjectIDs) != 2 || high.ObjectIDs[0] != "pi_A" || high.ObjectIDs[1] != "pi_B" {
		t.Fatalf("object ids = %v, want [pi_A pi_B]", high.ObjectIDs)
	}
	if high.Requests[0] != 1 || high.Requests[1] != 3 {
		t.Fatalf("requests = %v, want [1 3]", high.Requests)
	}
}

// A same-key retry that commits recovers the original; a later identical
// request is not blamed on it.
func TestSameKeyRetryResolvesOriginal(t *testing.T) {
	a, _ := newClocked(0)
	a.Complete(a.Begin("POST", "/v1/payment_intents", "k1", "fp"), "drop", chaos.FaultDropAfterCommit, 200, true, "pi_A")
	a.Complete(a.Begin("POST", "/v1/payment_intents", "k1", "fp"), "", chaos.FaultNone, 200, true, "pi_A")
	a.Complete(a.Begin("POST", "/v1/payment_intents", "k2", "fp"), "", chaos.FaultNone, 200, true, "pi_C")
	if has(a.Findings(), CodeUnsafeRetry) {
		t.Fatalf("recovered original still produced a finding: %+v", a.Findings())
	}
}

func TestUnreconciledCommittedFailure(t *testing.T) {
	a, _ := newClocked(0)
	a.Complete(a.Begin("POST", "/v1/payment_intents", "k1", "fp1"), "500ac", chaos.FaultError500AfterCommit, 200, true, "pi_A")
	if severityOf(a.Findings(), CodeUnreconciled) != Medium {
		t.Fatalf("committed 500 with no retry should be reported, got %+v", a.Findings())
	}
	// Once it is retried with the same key, it is recovered, not unreconciled.
	a.Complete(a.Begin("POST", "/v1/payment_intents", "k1", "fp1"), "", chaos.FaultNone, 200, true, "pi_A")
	if has(a.Findings(), CodeUnreconciled) {
		t.Fatalf("recovered request still reported: %+v", a.Findings())
	}
}
