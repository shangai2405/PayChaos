package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/chaos"
)

type target struct {
	mu   sync.Mutex
	ids  []string
	sigs []string
}

func (t *target) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var ev struct{ ID string }
	_ = json.Unmarshal(b, &ev)
	t.mu.Lock()
	t.ids = append(t.ids, ev.ID)
	t.sigs = append(t.sigs, r.Header.Get("Stripe-Signature"))
	t.mu.Unlock()
}

func send(t *testing.T, url, id string) {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"type":"payment_intent.succeeded"}`, id)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	req.Header.Set("Stripe-Signature", "t=1,v1=sig-"+id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("relay did not ack: %v", err)
	}
	resp.Body.Close()
}

func TestDuplicatesKeepSignature(t *testing.T) {
	tg := &target{}
	ts := httptest.NewServer(tg)
	defer ts.Close()
	eng := chaos.NewStream(1, chaos.StreamWebhooks)
	r := New(ts.URL, chaos.WebhookRules{DuplicateProbability: 1, DuplicateCount: 2}, eng, nil)
	rs := httptest.NewServer(r)
	defer rs.Close()

	send(t, rs.URL, "evt_1")
	r.Close()

	if len(tg.ids) != 3 {
		t.Fatalf("target got %d deliveries, want 3", len(tg.ids))
	}
	for _, s := range tg.sigs {
		if s != "t=1,v1=sig-evt_1" {
			t.Fatalf("signature not forwarded: %q", s)
		}
	}
	sums := r.Summaries()
	if len(sums) != 1 || sums[0].Deliveries != 3 || sums[0].Accepted != 3 {
		t.Fatalf("bad summary %+v", sums)
	}
	if len(Notes(sums)) == 0 {
		t.Fatal("expected a duplicate-delivery note")
	}
}

func TestReorderDeliversEveryEvent(t *testing.T) {
	tg := &target{}
	ts := httptest.NewServer(tg)
	defer ts.Close()
	eng := chaos.NewStream(3, chaos.StreamWebhooks)
	r := New(ts.URL, chaos.WebhookRules{ReorderWindowMS: 60_000}, eng, nil) // flushed only on Close
	rs := httptest.NewServer(r)
	defer rs.Close()

	want := map[string]bool{}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("evt_%02d", i)
		want[id] = true
		send(t, rs.URL, id)
	}
	r.Close()

	if len(tg.ids) != 20 {
		t.Fatalf("got %d deliveries want 20", len(tg.ids))
	}
	inOrder := true
	for i, id := range tg.ids {
		delete(want, id)
		if id != fmt.Sprintf("evt_%02d", i) {
			inOrder = false
		}
	}
	if len(want) != 0 {
		t.Fatalf("events lost: %v", want)
	}
	if inOrder {
		t.Fatal("20 events delivered in original order; shuffle had no effect")
	}
	reordered := 0
	for _, s := range r.Summaries() {
		if s.Reordered {
			reordered++
		}
	}
	if reordered == 0 {
		t.Fatal("summaries report no reordering")
	}
}
