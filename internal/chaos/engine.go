// Package chaos decides which fault, if any, to inject into a request.
//
// Decisions are driven by a seeded PRNG so a failing scenario can be replayed
// exactly by running again with the same seed and the same request sequence.
package chaos

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"
)

// Fault is a kind of failure the proxy can inject.
type Fault string

const (
	// FaultNone forwards the request untouched.
	FaultNone Fault = "none"
	// FaultLatency delays the request, then forwards it normally.
	FaultLatency Fault = "latency"
	// FaultError500 answers with a Stripe-style api_error without forwarding.
	FaultError500 Fault = "error_500"
	// FaultRateLimit answers with a Stripe-style 429 without forwarding.
	FaultRateLimit Fault = "rate_limit_429"
	// FaultDropAfterCommit forwards the request, lets the upstream commit it,
	// then resets the client connection so the client never sees the result.
	// This is the case that causes double charges.
	FaultDropAfterCommit Fault = "drop_after_commit"
	// FaultTimeoutBeforeForward stalls and then answers 504 without forwarding.
	FaultTimeoutBeforeForward Fault = "timeout_before_forward"
	// FaultError500AfterCommit forwards the request, lets the upstream commit
	// it, then answers the client with a 500. Stripe documents that a 500 is
	// indeterminate: the operation may have had side effects.
	FaultError500AfterCommit Fault = "error_500_after_commit"
	// FaultHoldResponse forwards the request, then holds the upstream's
	// response for DelayMS before delivering it. A client whose timeout is
	// shorter gives up on a request that has already committed.
	FaultHoldResponse Fault = "hold_response"
)

var knownFaults = map[Fault]bool{
	FaultNone: true, FaultLatency: true, FaultError500: true, FaultRateLimit: true,
	FaultDropAfterCommit: true, FaultTimeoutBeforeForward: true,
	FaultError500AfterCommit: true, FaultHoldResponse: true,
}

// Match selects which requests a rule applies to. Empty fields match anything.
type Match struct {
	Method     string `json:"method,omitempty"`
	PathPrefix string `json:"path_prefix,omitempty"`
}

// Rule injects Fault into matching requests with the given probability.
type Rule struct {
	Name        string  `json:"name"`
	Match       Match   `json:"match"`
	Fault       Fault   `json:"fault"`
	Probability float64 `json:"probability"`
	// MaxHits caps how many times this rule fires. 0 means unlimited.
	MaxHits int `json:"max_hits,omitempty"`
	// DelayMS is used by latency, timeout_before_forward and hold_response.
	DelayMS int `json:"delay_ms,omitempty"`
	// ShouldRetry, when "true" or "false", is sent as the Stripe-Should-Retry
	// header on injected error responses. Official SDKs obey it. Empty sends
	// no header, leaving the SDK to decide from the status code.
	ShouldRetry string `json:"should_retry,omitempty"`
}

// WebhookRules controls the webhook relay.
type WebhookRules struct {
	// DuplicateProbability is the chance an event is delivered more than once.
	DuplicateProbability float64 `json:"duplicate_probability"`
	// DuplicateCount is how many extra copies a duplicated event gets.
	DuplicateCount int `json:"duplicate_count"`
	// MaxDelayMS adds a random 0..MaxDelayMS delay before each delivery.
	MaxDelayMS int `json:"max_delay_ms"`
	// ReorderWindowMS buffers events for this long and delivers each batch
	// shuffled. 0 disables reordering.
	ReorderWindowMS int `json:"reorder_window_ms"`
}

// Config is the full scenario file.
type Config struct {
	Seed     uint64       `json:"seed"`
	Rules    []Rule       `json:"rules"`
	Webhooks WebhookRules `json:"webhooks"`
	// MutatingPaths are POST path prefixes that move money or create
	// billable objects; these are audited for idempotency.
	MutatingPaths []string `json:"mutating_paths"`
	// MinBackoffMS is the minimum gap the auditor expects between a failed
	// attempt and its retry.
	MinBackoffMS int `json:"min_backoff_ms"`
	// OperationID lists form parameters that identify one business operation,
	// e.g. ["metadata[order_id]"]. Retries are grouped by these values when
	// present, so changed parameters or rotated keys can't hide a duplicate.
	OperationID []string `json:"operation_id,omitempty"`
}

// DefaultMutatingPaths lists Stripe endpoints where a duplicate POST costs money
// or creates a duplicate billable object.
var DefaultMutatingPaths = []string{
	"/v1/payment_intents", "/v1/charges", "/v1/refunds", "/v1/payouts",
	"/v1/transfers", "/v1/checkout/sessions", "/v1/subscriptions",
	"/v1/invoices", "/v1/setup_intents",
}

// DefaultConfig is a scenario that exercises every fault at a moderate rate.
func DefaultConfig() Config {
	return Config{
		Seed: 42,
		Rules: []Rule{
			{Name: "lost-response-on-payment", Match: Match{Method: "POST", PathPrefix: "/v1/payment_intents"}, Fault: FaultDropAfterCommit, Probability: 0.25},
			{Name: "lost-response-on-refund", Match: Match{Method: "POST", PathPrefix: "/v1/refunds"}, Fault: FaultDropAfterCommit, Probability: 0.25},
			{Name: "rate-limited", Match: Match{Method: "POST"}, Fault: FaultRateLimit, Probability: 0.10},
			{Name: "upstream-error", Fault: FaultError500, Probability: 0.05},
			{Name: "slow-network", Fault: FaultLatency, Probability: 0.20, DelayMS: 800},
		},
		Webhooks:      WebhookRules{DuplicateProbability: 0.3, DuplicateCount: 1, MaxDelayMS: 500, ReorderWindowMS: 2000},
		MutatingPaths: DefaultMutatingPaths,
		MinBackoffMS:  200,
	}
}

// LoadConfig reads a JSON scenario file. Missing optional fields get defaults.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(c.MutatingPaths) == 0 {
		c.MutatingPaths = DefaultMutatingPaths
	}
	if c.MinBackoffMS == 0 {
		c.MinBackoffMS = 200
	}
	return c, c.Validate()
}

// Validate rejects unknown faults and out-of-range probabilities.
func (c Config) Validate() error {
	for i, r := range c.Rules {
		if !knownFaults[r.Fault] {
			return fmt.Errorf("rule %d (%q): unknown fault %q", i, r.Name, r.Fault)
		}
		if r.Probability < 0 || r.Probability > 1 {
			return fmt.Errorf("rule %d (%q): probability must be in [0,1]", i, r.Name)
		}
		if r.ShouldRetry != "" && r.ShouldRetry != "true" && r.ShouldRetry != "false" {
			return fmt.Errorf("rule %d (%q): should_retry must be \"true\", \"false\" or empty", i, r.Name)
		}
		if r.MaxHits < 0 || r.DelayMS < 0 {
			return fmt.Errorf("rule %d (%q): max_hits and delay_ms must be >= 0", i, r.Name)
		}
	}
	w := c.Webhooks
	if w.DuplicateProbability < 0 || w.DuplicateProbability > 1 {
		return fmt.Errorf("webhooks.duplicate_probability must be in [0,1]")
	}
	if w.DuplicateCount < 0 || w.MaxDelayMS < 0 || w.ReorderWindowMS < 0 {
		return fmt.Errorf("webhook counts and delays must be >= 0")
	}
	return nil
}

// Decision is the outcome of evaluating the rules for one request.
type Decision struct {
	Rule        string
	Fault       Fault
	Delay       time.Duration
	ShouldRetry string
}

// Engine evaluates rules. It is safe for concurrent use.
type Engine struct {
	mu    sync.Mutex
	rules []Rule
	hits  []int
	rng   *rand.Rand
}

// NewEngine builds an engine from a validated config.
func NewEngine(c Config) *Engine {
	return &Engine{
		rules: append([]Rule(nil), c.Rules...),
		hits:  make([]int, len(c.Rules)),
		rng:   rand.New(rand.NewPCG(c.Seed, c.Seed^0x9e3779b97f4a7c15)), // unchanged so existing seeds replay identically
	}
}

// Decide returns the first rule that matches and fires. Rules are evaluated in
// file order; a matching rule that does not fire falls through to the next.
func (e *Engine) Decide(method, path string) Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, r := range e.rules {
		if r.Match.Method != "" && !strings.EqualFold(r.Match.Method, method) {
			continue
		}
		if r.Match.PathPrefix != "" && !strings.HasPrefix(path, r.Match.PathPrefix) {
			continue
		}
		if r.MaxHits > 0 && e.hits[i] >= r.MaxHits {
			continue
		}
		// Always draw so the sequence stays reproducible regardless of outcome.
		roll := e.rng.Float64()
		if roll >= r.Probability {
			continue
		}
		e.hits[i]++
		delay := time.Duration(r.DelayMS) * time.Millisecond
		if (r.Fault == FaultTimeoutBeforeForward || r.Fault == FaultHoldResponse) && delay == 0 {
			delay = 5 * time.Second
		}
		return Decision{Rule: r.Name, Fault: r.Fault, Delay: delay, ShouldRetry: r.ShouldRetry}
	}
	return Decision{Fault: FaultNone}
}

// Stream is an independent, seeded random stream that is safe for concurrent
// use. Each component gets its own stream (see StreamWebhooks), so webhook
// traffic never shifts the sequence of API fault decisions or vice versa.
type Stream struct {
	mu  sync.Mutex
	rng *rand.Rand
}

// StreamWebhooks identifies the webhook relay's stream. The API fault engine
// keeps its own generator, seeded as before, so existing seeds still replay.
const StreamWebhooks uint64 = 1

// NewStream returns the stream with the given id for a seed.
func NewStream(seed, id uint64) *Stream {
	return &Stream{rng: rand.New(rand.NewPCG(seed^(id*0xbf58476d1ce4e5b9), id^0x94d049bb133111eb))}
}

// Intn returns a reproducible value in [0,n); 0 when n <= 0.
func (s *Stream) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.IntN(n)
}

// Float64 returns a reproducible value in [0,1).
func (s *Stream) Float64() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Float64()
}
