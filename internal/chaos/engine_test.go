package chaos

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProbabilityBounds(t *testing.T) {
	never := NewEngine(Config{Seed: 7, Rules: []Rule{{Name: "n", Fault: FaultError500, Probability: 0}}})
	always := NewEngine(Config{Seed: 7, Rules: []Rule{{Name: "a", Fault: FaultError500, Probability: 1}}})
	for i := 0; i < 1000; i++ {
		if d := never.Decide("POST", "/v1/charges"); d.Fault != FaultNone {
			t.Fatal("probability 0 fired")
		}
		if d := always.Decide("POST", "/v1/charges"); d.Fault != FaultError500 {
			t.Fatal("probability 1 did not fire")
		}
	}
}

func TestMatchAndMaxHits(t *testing.T) {
	e := NewEngine(Config{Seed: 1, Rules: []Rule{{
		Name: "r", Match: Match{Method: "POST", PathPrefix: "/v1/refunds"}, Fault: FaultRateLimit, Probability: 1, MaxHits: 2,
	}}})
	if d := e.Decide("GET", "/v1/refunds"); d.Fault != FaultNone {
		t.Fatal("method should not match")
	}
	if d := e.Decide("POST", "/v1/charges"); d.Fault != FaultNone {
		t.Fatal("path should not match")
	}
	hits := 0
	for i := 0; i < 5; i++ {
		if e.Decide("POST", "/v1/refunds").Fault == FaultRateLimit {
			hits++
		}
	}
	if hits != 2 {
		t.Fatalf("hits=%d want 2", hits)
	}
}

func TestSameSeedSameSequence(t *testing.T) {
	cfg := DefaultConfig()
	a, b := NewEngine(cfg), NewEngine(cfg)
	for i := 0; i < 500; i++ {
		if a.Decide("POST", "/v1/payment_intents") != b.Decide("POST", "/v1/payment_intents") {
			t.Fatalf("diverged at %d", i)
		}
	}
}

func TestLoadConfigValidation(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"rules":[{"name":"x","fault":"explode","probability":0.5}]}`), 0o644)
	if _, err := LoadConfig(bad); err == nil {
		t.Fatal("unknown fault accepted")
	}
	typo := filepath.Join(dir, "typo.json")
	os.WriteFile(typo, []byte(`{"rulez":[]}`), 0o644)
	if _, err := LoadConfig(typo); err == nil {
		t.Fatal("unknown field accepted")
	}
	good := filepath.Join(dir, "good.json")
	os.WriteFile(good, []byte(`{"seed":3,"rules":[{"name":"x","fault":"latency","probability":0.5,"delay_ms":10}]}`), 0o644)
	c, err := LoadConfig(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MutatingPaths) == 0 || c.MinBackoffMS != 200 {
		t.Fatal("defaults not applied")
	}
}

// Draws on the webhook stream must not change the API fault sequence.
func TestStreamsAreIndependent(t *testing.T) {
	cfg := DefaultConfig()
	a, b := NewEngine(cfg), NewEngine(cfg)
	wh := NewStream(cfg.Seed, StreamWebhooks)
	for i := 0; i < 200; i++ {
		wh.Float64()
		wh.Intn(10)
		if a.Decide("POST", "/v1/payment_intents") != b.Decide("POST", "/v1/payment_intents") {
			t.Fatalf("webhook draws shifted API decisions at %d", i)
		}
	}
	x, y := NewStream(9, StreamWebhooks), NewStream(9, StreamWebhooks)
	for i := 0; i < 100; i++ {
		if x.Intn(1000) != y.Intn(1000) {
			t.Fatal("same seed and id gave different streams")
		}
	}
}
