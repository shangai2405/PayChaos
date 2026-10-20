package mock

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type result struct {
	status   int
	body     map[string]any
	replayed bool
}

func do(t *testing.T, srv *httptest.Server, method, path string, form url.Values, key string) result {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, srv.URL+path, body)
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return result{resp.StatusCode, m, resp.Header.Get("Idempotent-Replayed") == "true"}
}

var pi = url.Values{"amount": {"2000"}, "currency": {"inr"}}

func TestCreateAndRetrieve(t *testing.T) {
	m := New()
	srv := httptest.NewServer(m)
	defer srv.Close()
	r := do(t, srv, "POST", "/v1/payment_intents", url.Values{"amount": {"2000"}, "currency": {"inr"}, "confirm": {"true"}}, "")
	if r.status != 200 || !strings.HasPrefix(r.body["id"].(string), "pi_") || r.body["status"] != "succeeded" {
		t.Fatalf("create: %+v", r)
	}
	if r.body["amount"].(float64) != 2000 {
		t.Fatalf("amount not numeric: %v", r.body["amount"])
	}
	got := do(t, srv, "GET", "/v1/payment_intents/"+r.body["id"].(string), nil, "")
	if got.status != 200 || got.body["id"] != r.body["id"] {
		t.Fatalf("retrieve: %+v", got)
	}
	if do(t, srv, "GET", "/v1/payment_intents/pi_missing", nil, "").status != 404 {
		t.Fatal("missing object should 404")
	}
}

func TestIdempotentReplayAndMismatch(t *testing.T) {
	m := New()
	srv := httptest.NewServer(m)
	defer srv.Close()
	a := do(t, srv, "POST", "/v1/payment_intents", pi, "k")
	b := do(t, srv, "POST", "/v1/payment_intents", pi, "k")
	if !b.replayed || a.body["id"] != b.body["id"] || m.Created("payment_intents") != 1 {
		t.Fatalf("replay: a=%v b=%v replayed=%v created=%d", a.body["id"], b.body["id"], b.replayed, m.Created("payment_intents"))
	}
	c := do(t, srv, "POST", "/v1/payment_intents", url.Values{"amount": {"9999"}, "currency": {"inr"}}, "k")
	if c.status != 400 || c.body["error"].(map[string]any)["type"] != "idempotency_error" {
		t.Fatalf("mismatch: %+v", c)
	}
}

func TestValidationErrorsAreNotCached(t *testing.T) {
	m := New()
	srv := httptest.NewServer(m)
	defer srv.Close()
	if do(t, srv, "POST", "/v1/payment_intents", url.Values{"currency": {"inr"}}, "k").status != 400 {
		t.Fatal("missing amount should be rejected")
	}
	// Execution never began, so the key is still free for the fixed request.
	if r := do(t, srv, "POST", "/v1/payment_intents", url.Values{"currency": {"inr"}, "amount": {"10"}}, "k"); r.status != 200 || r.replayed {
		t.Fatalf("fixed request after validation error: %+v", r)
	}
}

func TestServerErrorIsCachedAfterCommit(t *testing.T) {
	m := NewWithOptions(Options{ServerErrorEvery: 2})
	srv := httptest.NewServer(m)
	defer srv.Close()
	if do(t, srv, "POST", "/v1/payment_intents", pi, "k1").status != 200 {
		t.Fatal("1st create should succeed")
	}
	r := do(t, srv, "POST", "/v1/payment_intents", pi, "k2")
	if r.status != 500 || m.Created("payment_intents") != 2 {
		t.Fatalf("2nd create should 500 after creating: %d created=%d", r.status, m.Created("payment_intents"))
	}
	again := do(t, srv, "POST", "/v1/payment_intents", pi, "k2")
	if again.status != 500 || !again.replayed || m.Created("payment_intents") != 2 {
		t.Fatalf("cached 500 not replayed: %+v created=%d", again, m.Created("payment_intents"))
	}
}

func TestConcurrentSameKeyConflicts(t *testing.T) {
	m := NewWithOptions(Options{ExecDelay: 300 * time.Millisecond})
	srv := httptest.NewServer(m)
	defer srv.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	var first result
	go func() { defer wg.Done(); first = do(t, srv, "POST", "/v1/payment_intents", pi, "k") }()
	time.Sleep(80 * time.Millisecond)
	if r := do(t, srv, "POST", "/v1/payment_intents", pi, "k"); r.status != 409 {
		t.Fatalf("concurrent same key: %d, want 409", r.status)
	}
	wg.Wait()
	if first.status != 200 {
		t.Fatalf("first request: %d", first.status)
	}
	// The 409 was not cached: retrying now replays the first result.
	if r := do(t, srv, "POST", "/v1/payment_intents", pi, "k"); r.status != 200 || !r.replayed {
		t.Fatalf("retry after conflict: %+v", r)
	}
	if m.Created("payment_intents") != 1 {
		t.Fatalf("created %d, want 1", m.Created("payment_intents"))
	}
}

func TestUnknownRoutesAndStats(t *testing.T) {
	m := New()
	srv := httptest.NewServer(m)
	defer srv.Close()
	if do(t, srv, "POST", "/v2/things", pi, "").status != 404 || do(t, srv, "DELETE", "/v1/customers/cus_1", nil, "").status != 404 {
		t.Fatal("unknown routes should 404")
	}
	do(t, srv, "POST", "/v1/customers", url.Values{"email": {"a@example.com"}}, "")
	st := do(t, srv, "GET", "/__mock/stats", nil, "")
	if st.body["created"].(map[string]any)["customers"].(float64) != 1 {
		t.Fatalf("stats: %+v", st.body)
	}
}
