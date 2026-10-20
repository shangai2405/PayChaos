// Package webhook relays Stripe webhook events to the application under test
// while duplicating, delaying and reordering them.
//
// Stripe itself documents that endpoints may receive the same event more than
// once and that delivery order is not guaranteed. This relay makes both happen
// on purpose, every run, so handlers that assume otherwise fail in testing
// instead of in production.
//
// Typical wiring with the Stripe CLI:
//
//	stripe listen --forward-to localhost:8081/webhooks
//	paychaos -webhook-listen :8081 -webhook-target http://localhost:3000/stripe/webhook
//
// The Stripe-Signature header is forwarded unchanged. Duplicates therefore carry
// a valid signature, exactly like Stripe's own redeliveries; keep MaxDelayMS and
// ReorderWindowMS well under the signature tolerance (5 minutes by default).
package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/chaos"
)

// Random is the subset of chaos.Engine the relay needs.
type Random interface {
	Intn(n int) int
	Float64() float64
}

// Delivery is one attempt to deliver an event to the target.
type Delivery struct {
	EventID   string    `json:"event_id"`
	Type      string    `json:"type"`
	Copy      int       `json:"copy"` // 0 = original, 1.. = injected duplicate
	Received  int       `json:"received_order"`
	Delivered int       `json:"delivered_order"`
	Status    int       `json:"status"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
}

// EventSummary aggregates deliveries per event.
type EventSummary struct {
	EventID    string `json:"event_id"`
	Type       string `json:"type"`
	Deliveries int    `json:"deliveries"`
	Accepted   int    `json:"accepted_2xx"`
	Reordered  bool   `json:"reordered"`
}

type pending struct {
	header   http.Header
	body     []byte
	id, typ  string
	received int
}

// Relay is an http.Handler.
type Relay struct {
	target string
	rules  chaos.WebhookRules
	rng    Random
	client *http.Client
	log    *slog.Logger

	mu         sync.Mutex
	received   int
	delivered  int
	buffer     []pending
	deliveries []Delivery
	wg         sync.WaitGroup
	stop       chan struct{}
	stopOnce   sync.Once
}

// New starts a relay. Call Close to flush buffered events.
func New(target string, rules chaos.WebhookRules, rng Random, log *slog.Logger) *Relay {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r := &Relay{
		target: target, rules: rules, rng: rng, log: log,
		client: &http.Client{Timeout: 30 * time.Second},
		stop:   make(chan struct{}),
	}
	if rules.ReorderWindowMS > 0 {
		r.wg.Add(1)
		go r.flushLoop(time.Duration(rules.ReorderWindowMS) * time.Millisecond)
	}
	return r
}

// ServeHTTP accepts an event, acknowledges it immediately (as a well-behaved
// endpoint should) and schedules chaotic delivery to the target.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	var ev struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &ev)
	if ev.ID == "" {
		ev.ID = "(unparsed)"
	}

	r.mu.Lock()
	r.received++
	p := pending{header: req.Header.Clone(), body: body, id: ev.ID, typ: ev.Type, received: r.received}
	if r.rules.ReorderWindowMS > 0 {
		r.buffer = append(r.buffer, p)
		r.mu.Unlock()
	} else {
		r.mu.Unlock()
		r.wg.Add(1)
		go func() { defer r.wg.Done(); r.deliverWithDuplicates(p) }()
	}
	w.WriteHeader(http.StatusOK)
}

func (r *Relay) flushLoop(window time.Duration) {
	defer r.wg.Done()
	t := time.NewTicker(window)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.flush()
		case <-r.stop:
			r.flush()
			return
		}
	}
}

// flush delivers the buffered batch in a shuffled order (Fisher-Yates using the
// run's seeded PRNG, so the order is reproducible).
func (r *Relay) flush() {
	r.mu.Lock()
	batch := r.buffer
	r.buffer = nil
	r.mu.Unlock()
	for i := len(batch) - 1; i > 0; i-- {
		j := r.rng.Intn(i + 1)
		batch[i], batch[j] = batch[j], batch[i]
	}
	for _, p := range batch {
		r.deliverWithDuplicates(p)
	}
}

func (r *Relay) deliverWithDuplicates(p pending) {
	copies := 0
	if r.rules.DuplicateCount > 0 && r.rng.Float64() < r.rules.DuplicateProbability {
		copies = r.rules.DuplicateCount
	}
	for c := 0; c <= copies; c++ {
		if r.rules.MaxDelayMS > 0 {
			time.Sleep(time.Duration(r.rng.Intn(r.rules.MaxDelayMS+1)) * time.Millisecond)
		}
		r.deliver(p, c)
	}
}

func (r *Relay) deliver(p pending, copyN int) {
	d := Delivery{EventID: p.id, Type: p.typ, Copy: copyN, Received: p.received, At: time.Now()}
	req, err := http.NewRequest(http.MethodPost, r.target, bytes.NewReader(p.body))
	if err == nil {
		for _, h := range []string{"Content-Type", "Stripe-Signature", "User-Agent"} {
			if v := p.header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		var resp *http.Response
		resp, err = r.client.Do(req)
		if err == nil {
			d.Status = resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	if err != nil {
		d.Error = err.Error()
	}
	r.mu.Lock()
	r.delivered++
	d.Delivered = r.delivered
	r.deliveries = append(r.deliveries, d)
	r.mu.Unlock()
	r.log.Info("webhook delivered", "event", p.id, "type", p.typ, "copy", copyN, "status", d.Status, "err", d.Error)
}

// Close flushes buffered events and waits for in-flight deliveries.
func (r *Relay) Close() {
	r.stopOnce.Do(func() { close(r.stop) })
	r.wg.Wait()
}

// Deliveries returns every delivery attempt.
func (r *Relay) Deliveries() []Delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Delivery(nil), r.deliveries...)
}

// Summaries groups deliveries by event. An event is "reordered" when its first
// delivery happened after the first delivery of an event received later.
func (r *Relay) Summaries() []EventSummary {
	ds := r.Deliveries()
	byID := map[string]*EventSummary{}
	firstDelivered := map[string]int{}
	receivedOrder := map[string]int{}
	var order []string
	for _, d := range ds {
		s, ok := byID[d.EventID]
		if !ok {
			s = &EventSummary{EventID: d.EventID, Type: d.Type}
			byID[d.EventID] = s
			order = append(order, d.EventID)
			firstDelivered[d.EventID] = d.Delivered
			receivedOrder[d.EventID] = d.Received
		}
		s.Deliveries++
		if d.Status >= 200 && d.Status < 300 {
			s.Accepted++
		}
	}
	for _, a := range order {
		for _, b := range order {
			if receivedOrder[b] > receivedOrder[a] && firstDelivered[b] < firstDelivered[a] {
				byID[a].Reordered = true
			}
		}
	}
	sort.Slice(order, func(i, j int) bool { return receivedOrder[order[i]] < receivedOrder[order[j]] })
	out := make([]EventSummary, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

// Notes turns the summaries into human-readable observations. The relay cannot
// see the application's database, so it reports what it did and what to check
// rather than claiming a handler is broken.
func Notes(sums []EventSummary) []string {
	var notes []string
	for _, s := range sums {
		if s.Deliveries > 1 && s.Accepted == s.Deliveries {
			notes = append(notes, fmt.Sprintf("%s (%s) was delivered %d times and acknowledged every time; confirm the side effect (fulfilment, email, credit) happened once, e.g. by storing processed event IDs",
				s.EventID, s.Type, s.Deliveries))
		}
		if s.Reordered {
			notes = append(notes, fmt.Sprintf("%s (%s) arrived out of order; confirm the handler re-fetches current object state instead of trusting event order",
				s.EventID, s.Type))
		}
		if s.Accepted < s.Deliveries && s.Accepted > 0 {
			notes = append(notes, fmt.Sprintf("%s (%s): %d of %d deliveries were rejected; Stripe would keep retrying rejected deliveries",
				s.EventID, s.Type, s.Deliveries-s.Accepted, s.Deliveries))
		}
	}
	return notes
}
