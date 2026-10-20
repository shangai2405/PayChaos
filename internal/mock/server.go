// Package mock is a tiny Stripe-shaped API used for demos and tests, so the
// tool can be tried without a Stripe account. It implements create/retrieve for
// any /v1/<resource> collection and Stripe's idempotency semantics:
//
//   - same Idempotency-Key + same parameters  -> the original response is replayed
//
//   - same Idempotency-Key + different params -> 400 idempotency_error
//
//   - same Idempotency-Key while the first request is still executing -> 409
//
//   - a failed execution (500) is cached and replayed like a success
//
// Options can slow execution down (to produce 409s) and make some executions
// fail with a 500 after the object was created, which Stripe documents as
// possible: a 500 is indeterminate.
//
// It is NOT a full Stripe emulator; use stripe-mock or test mode for that.
package mock

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var prefixes = map[string]string{
	"payment_intents": "pi", "charges": "ch", "refunds": "re", "customers": "cus",
	"payouts": "po", "transfers": "tr", "setup_intents": "seti", "subscriptions": "sub",
	"invoices": "in",
}

type cached struct {
	paramsHash string
	status     int
	body       []byte
}

// Options tune the mock's behaviour.
type Options struct {
	// ExecDelay is how long each create takes to execute. A retry that
	// reuses the key while the first attempt is still running gets a 409.
	ExecDelay time.Duration
	// ServerErrorEvery makes every Nth create return 500 after the object has
	// been created (0 disables). The 500 is cached under the idempotency key.
	ServerErrorEvery int
}

// Server is an http.Handler.
type Server struct {
	opt      Options
	inflight map[string]bool // apiKey|idemKey currently executing
	attempts int             // creates executed, for ServerErrorEvery

	mu      sync.Mutex
	objects map[string]map[string]any // resource -> id -> object
	idem    map[string]cached         // apiKey|idemKey -> first response
	created map[string]int            // resource -> count
}

// New returns an empty mock.
func New() *Server { return NewWithOptions(Options{}) }

// NewWithOptions returns an empty mock with the given options.
func NewWithOptions(o Options) *Server {
	return &Server{opt: o, inflight: map[string]bool{},
		objects: map[string]map[string]any{}, idem: map[string]cached{}, created: map[string]int{}}
}

// Created reports how many objects of a resource were created.
func (s *Server) Created(resource string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.created[resource]
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__mock/stats" {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"created": s.created})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" {
		apiErr(w, http.StatusNotFound, "invalid_request_error", "", "Unrecognized request URL")
		return
	}
	resource := parts[1]
	switch {
	case r.Method == http.MethodPost && len(parts) == 2:
		s.create(w, r, resource)
	case r.Method == http.MethodGet && len(parts) == 3:
		s.retrieve(w, resource, parts[2])
	default:
		apiErr(w, http.StatusNotFound, "invalid_request_error", "", "Unrecognized request URL")
	}
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, resource string) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	params, err := url.ParseQuery(string(raw))
	if err != nil {
		apiErr(w, http.StatusBadRequest, "invalid_request_error", "", "malformed form body")
		return
	}
	apiKey := r.Header.Get("Authorization")
	idemKey := r.Header.Get("Idempotency-Key")
	h := sha256.Sum256([]byte(r.URL.Path + "|" + params.Encode()))
	paramsHash := hex.EncodeToString(h[:])

	cacheKey := apiKey + "|" + idemKey
	s.mu.Lock()
	defer s.mu.Unlock()

	if idemKey != "" {
		if s.inflight[cacheKey] {
			// Not cached: Stripe saves no result for a conflicting request.
			apiErr(w, http.StatusConflict, "idempotency_error", "",
				"There is currently another in-progress request using this idempotency key. Retry with the same key once it completes.")
			return
		}
		if c, ok := s.idem[cacheKey]; ok {
			if c.paramsHash != paramsHash {
				apiErr(w, http.StatusBadRequest, "idempotency_error", "",
					"Keys for idempotent requests can only be used with the same parameters they were first used with.")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(c.status)
			_, _ = w.Write(c.body)
			return
		}
	}

	if resource == "payment_intents" || resource == "charges" {
		amt, err := strconv.Atoi(params.Get("amount"))
		if err != nil || amt <= 0 || params.Get("currency") == "" {
			apiErr(w, http.StatusBadRequest, "invalid_request_error", "parameter_missing", "amount and currency are required")
			return
		}
	}

	// Execution begins here. Results from now on are cached under the key.
	if idemKey != "" {
		s.inflight[cacheKey] = true
		defer delete(s.inflight, cacheKey)
	}
	if s.opt.ExecDelay > 0 {
		s.mu.Unlock()
		time.Sleep(s.opt.ExecDelay)
		s.mu.Lock()
	}
	s.attempts++
	failAfter := s.opt.ServerErrorEvery > 0 && s.attempts%s.opt.ServerErrorEvery == 0

	prefix := prefixes[resource]
	if prefix == "" {
		prefix = "obj"
	}
	id := prefix + "_" + randID()
	obj := map[string]any{"id": id, "object": strings.TrimSuffix(resource, "s"), "created": time.Now().Unix(), "livemode": false}
	for k := range params {
		obj[k] = params.Get(k)
	}
	if v, ok := obj["amount"].(string); ok {
		if n, err := strconv.Atoi(v); err == nil {
			obj["amount"] = n
		}
	}
	if resource == "payment_intents" {
		obj["status"] = "requires_payment_method"
		if params.Get("confirm") == "true" {
			obj["status"] = "succeeded"
		}
	}
	if s.objects[resource] == nil {
		s.objects[resource] = map[string]any{}
	}
	s.objects[resource][id] = obj
	s.created[resource]++

	status := http.StatusOK
	body, _ := json.Marshal(obj)
	if failAfter {
		// The object exists, but the caller is told the request failed.
		status = http.StatusInternalServerError
		body, _ = json.Marshal(map[string]any{"error": map[string]string{
			"type": "api_error", "message": "mock: internal error after the object was created"}})
	}
	if idemKey != "" {
		s.idem[cacheKey] = cached{paramsHash: paramsHash, status: status, body: body}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) retrieve(w http.ResponseWriter, resource, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[resource][id]
	if !ok {
		apiErr(w, http.StatusNotFound, "invalid_request_error", "resource_missing", "No such "+resource+": '"+id+"'")
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func randID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, status int, typ, code, msg string) {
	e := map[string]string{"type": typ, "message": msg}
	if code != "" {
		e["code"] = code
	}
	writeJSON(w, status, map[string]any{"error": e})
}
