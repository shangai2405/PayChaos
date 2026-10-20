// Package audit watches the requests a client sends through the proxy and flags
// retry behaviour that would duplicate money movement in production.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/chaos"
)

// Severity of a finding.
type Severity string

const (
	High   Severity = "HIGH"
	Medium Severity = "MEDIUM"
	Low    Severity = "LOW"
)

// Finding codes.
const (
	CodeUnsafeRetry      = "UNSAFE_RETRY_AFTER_LOST_RESPONSE"
	CodeWouldDuplicate   = "UNSAFE_RETRY_FAILED_THIS_TIME"
	CodeUnreconciled     = "COMMITTED_BUT_REPORTED_AS_FAILED"
	CodeAmbiguousRetry   = "NEW_KEY_AFTER_INDETERMINATE_ERROR"
	CodeMissingKey       = "MISSING_IDEMPOTENCY_KEY"
	CodeKeyReusedNewBody = "IDEMPOTENCY_KEY_REUSED_WITH_DIFFERENT_BODY"
	CodeNoBackoff        = "RETRY_WITHOUT_BACKOFF"
)

// Finding is one problem the auditor detected.
type Finding struct {
	Code        string    `json:"code"`
	Severity    Severity  `json:"severity"`
	Message     string    `json:"message"`
	Path        string    `json:"path"`
	Fingerprint string    `json:"fingerprint"`
	Requests    []int     `json:"requests"`
	ObjectIDs   []string  `json:"object_ids,omitempty"`
	At          time.Time `json:"at"`
}

// Record is one request seen by the proxy.
type Record struct {
	Seq            int         `json:"seq"`
	At             time.Time   `json:"at"`
	Done           time.Time   `json:"done"`
	Method         string      `json:"method"`
	Path           string      `json:"path"`
	IdempotencyKey string      `json:"idempotency_key,omitempty"`
	Fingerprint    string      `json:"fingerprint"`
	Rule           string      `json:"rule,omitempty"`
	Fault          chaos.Fault `json:"fault"`
	UpstreamStatus int         `json:"upstream_status"`
	// ObjectID is the id of the object the upstream created, when known.
	ObjectID string `json:"object_id,omitempty"`
	// ResponseDelivered is set once the client is known to have received the
	// upstream's response (used for hold_response, where it may give up first).
	ResponseDelivered bool `json:"response_delivered"`
	// Committed is true when the upstream processed the request successfully,
	// whether or not the client received the response.
	Committed bool `json:"committed"`

	// lostOriginal is the seq of an earlier committed attempt whose response
	// was lost and which this request retries without the original key. The
	// verdict waits for Complete, when we know whether this retry committed.
	lostOriginal int
}

// Auditor is safe for concurrent use.
type Auditor struct {
	mu         sync.Mutex
	mutating   []string
	opParams   []string // form params that identify one business operation
	minBackoff time.Duration
	now        func() time.Time

	records  []Record
	byFP     map[string][]int  // fingerprint -> record indexes, in arrival order
	keyFP    map[string]string // idempotency key -> body hash first used with it
	resolved map[int]bool      // lost originals later recovered by a same-key retry
	findings []Finding
	seen     map[string]bool // de-duplicates repeated findings
}

// New creates an auditor.
func New(mutatingPaths []string, minBackoff time.Duration) *Auditor {
	return &Auditor{
		mutating:   mutatingPaths,
		minBackoff: minBackoff,
		now:        time.Now,
		byFP:       map[string][]int{},
		keyFP:      map[string]string{},
		resolved:   map[int]bool{},
		seen:       map[string]bool{},
	}
}

// IsMutating reports whether the request is a money-moving write.
func (a *Auditor) IsMutating(method, path string) bool {
	if method != "POST" {
		return false
	}
	for _, p := range a.mutating {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// Fingerprint identifies "the same logical request": same account, method,
// path and parameters. Form bodies are canonicalised so parameter order does
// not matter. The API key itself is never stored, only a hash prefix.
func Fingerprint(apiKey, method, path, contentType string, body []byte) string {
	canon := string(body)
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") || contentType == "" {
		if v, err := url.ParseQuery(string(body)); err == nil {
			canon = v.Encode() // Encode sorts by key
		}
	}
	h := sha256.Sum256([]byte(hashKey(apiKey) + "|" + method + "|" + path + "|" + canon))
	return hex.EncodeToString(h[:6])
}

func hashKey(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:4])
}

// WithOperationID sets the form parameters that identify one business
// operation, e.g. "metadata[order_id]". Requests carrying any of them are
// grouped by those values instead of by their whole body, so a retry that
// changes another parameter (an attempt counter, a timestamp) or rotates its
// idempotency key is still recognised as the same operation.
func (a *Auditor) WithOperationID(params []string) *Auditor {
	a.opParams = append([]string(nil), params...)
	return a
}

// OperationKey returns a fingerprint built only from the operation-id params,
// and false when the body carries none of them.
func OperationKey(apiKey, method, path, contentType string, body []byte, opParams []string) (string, bool) {
	if len(opParams) == 0 || !(contentType == "" || strings.HasPrefix(contentType, "application/x-www-form-urlencoded")) {
		return "", false
	}
	v, err := url.ParseQuery(string(body))
	if err != nil {
		return "", false
	}
	op := url.Values{}
	for _, p := range opParams {
		if x := v.Get(p); x != "" {
			op.Set(p, x)
		}
	}
	if len(op) == 0 {
		return "", false
	}
	h := sha256.Sum256([]byte(hashKey(apiKey) + "|" + method + "|" + path + "|op|" + op.Encode()))
	return "op-" + hex.EncodeToString(h[:6]), true
}

// BeginRequest fingerprints a raw request and records it (see Begin). The
// operation key, when configured and present, groups retries; the exact body
// hash is still used to detect an idempotency key reused with new parameters.
func (a *Auditor) BeginRequest(apiKey, method, path, contentType, idemKey string, body []byte) int {
	bodyHash := Fingerprint(apiKey, method, path, contentType, body)
	fp := bodyHash
	if k, ok := OperationKey(apiKey, method, path, contentType, body, a.opParams); ok {
		fp = k
	}
	return a.begin(method, path, idemKey, fp, bodyHash)
}

// Begin records an arriving request identified by fp, runs the checks that
// depend on earlier attempts, and returns a sequence number for Complete.
func (a *Auditor) Begin(method, path, idemKey, fp string) int {
	return a.begin(method, path, idemKey, fp, fp)
}

func (a *Auditor) begin(method, path, idemKey, fp, bodyHash string) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	seq := len(a.records) + 1
	rec := Record{Seq: seq, At: now, Method: method, Path: path, IdempotencyKey: idemKey, Fingerprint: fp}

	if idemKey == "" {
		a.add(Finding{
			Code: CodeMissingKey, Severity: Medium, Path: path, Fingerprint: fp, Requests: []int{seq}, At: now,
			Message: fmt.Sprintf("POST %s sent without an Idempotency-Key header; any retry of it can create a duplicate", path),
		}, CodeMissingKey+path)
	} else if prevBody, ok := a.keyFP[idemKey]; ok && prevBody != bodyHash {
		a.add(Finding{
			Code: CodeKeyReusedNewBody, Severity: Medium, Path: path, Fingerprint: fp, Requests: []int{seq}, At: now,
			Message: "Idempotency-Key reused with different parameters; Stripe rejects this with an idempotency_error. Keys must be unique per logical operation",
		}, CodeKeyReusedNewBody+idemKey)
	} else if !ok {
		a.keyFP[idemKey] = bodyHash
	}

	if prev, ok := a.lastWithFP(fp); ok {
		sameKey := idemKey != "" && idemKey == prev.IdempotencyKey
		if what := indeterminate(prev); what != "" && !sameKey && !a.hasLostOriginal(fp, idemKey) {
			a.add(Finding{
				Code: CodeAmbiguousRetry, Severity: Medium, Path: path, Fingerprint: fp, Requests: []int{prev.Seq, seq}, At: now,
				Message: fmt.Sprintf("request #%d ended with %s and was retried as #%d with %s. Stripe treats this outcome as indeterminate: retry with the same key, or reconcile first, because a new key can start a second operation",
					prev.Seq, what, seq, keyDesc(idemKey, prev.IdempotencyKey)),
			}, fmt.Sprintf("%s-%d", CodeAmbiguousRetry, prev.Seq))
		}
		if isRetryable(prev.Fault) && !prev.Done.IsZero() && a.minBackoff > 0 {
			if gap := now.Sub(prev.Done); gap < a.minBackoff {
				a.add(Finding{
					Code: CodeNoBackoff, Severity: Low, Path: path, Fingerprint: fp, Requests: []int{prev.Seq, seq}, At: now,
					Message: fmt.Sprintf("retried %v after a %s; expected at least %v of (ideally exponential, jittered) backoff",
						gap.Round(time.Millisecond), prev.Fault, a.minBackoff),
				}, fmt.Sprintf("%s-%d", CodeNoBackoff, prev.Seq))
			}
		}
	}

	if orig, ok := a.lostOriginalFor(fp); ok && (idemKey == "" || idemKey != orig.IdempotencyKey) {
		rec.lostOriginal = orig.Seq
	}

	a.records = append(a.records, rec)
	a.byFP[fp] = append(a.byFP[fp], len(a.records)-1)
	return seq
}

// lostOriginalFor finds the most recent attempt for fp that committed upstream
// but whose response never reached the client, and that no same-key retry has
// recovered yet. Looking past the latest attempt matters: a retry chain of
// "lost response -> 429 -> retry" must still be traced back to the lost one.
func (a *Auditor) lostOriginalFor(fp string) (Record, bool) {
	idx := a.byFP[fp]
	for i := len(idx) - 1; i >= 0; i-- {
		r := a.records[idx[i]]
		if r.Committed && lostResponse(r) && !a.resolved[r.Seq] {
			return r, true
		}
	}
	return Record{}, false
}

func (a *Auditor) hasLostOriginal(fp, idemKey string) bool {
	orig, ok := a.lostOriginalFor(fp)
	return ok && (idemKey == "" || idemKey != orig.IdempotencyKey)
}

// lostResponse reports whether a committed attempt's result never reached the
// client: the connection was reset, the client was told 500, or the response
// was held until after the client gave up.
func lostResponse(r Record) bool {
	switch r.Fault {
	case chaos.FaultDropAfterCommit, chaos.FaultError500AfterCommit:
		return true
	case chaos.FaultHoldResponse:
		return !r.ResponseDelivered
	}
	return false
}

// indeterminate describes an outcome after which Stripe says a request may or
// may not have taken effect, or "" if the outcome was definite. Rate limits
// (429) are definite: they are rejected before execution starts.
func indeterminate(r Record) string {
	switch {
	case r.Fault == chaos.FaultTimeoutBeforeForward:
		return "a timeout"
	case r.Fault == chaos.FaultError500:
		return "a 500 error"
	case r.UpstreamStatus == 409:
		return "a 409 conflict"
	case r.UpstreamStatus >= 500:
		return fmt.Sprintf("a %d error", r.UpstreamStatus)
	}
	return ""
}

// MarkDelivered records that the client received the response to seq.
func (a *Auditor) MarkDelivered(seq int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq >= 1 && seq <= len(a.records) {
		a.records[seq-1].ResponseDelivered = true
	}
}

// Complete stores the outcome of a request started with Begin.
// objectID is the id of the object the upstream created ("" if unknown).
func (a *Auditor) Complete(seq int, rule string, fault chaos.Fault, upstreamStatus int, committed bool, objectID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq < 1 || seq > len(a.records) {
		return
	}
	r := &a.records[seq-1]
	r.Done = a.now()
	r.Rule = rule
	r.Fault = fault
	r.UpstreamStatus = upstreamStatus
	r.Committed = committed
	r.ObjectID = objectID

	if r.lostOriginal == 0 {
		// A same-key retry that commits recovers any lost original it matches.
		if committed && r.IdempotencyKey != "" {
			if orig, ok := a.lostOriginalFor(r.Fingerprint); ok && orig.Seq != r.Seq && orig.IdempotencyKey == r.IdempotencyKey {
				a.resolved[orig.Seq] = true
			}
		}
		return
	}
	orig := a.records[r.lostOriginal-1]
	ids := nonEmpty(orig.ObjectID, r.ObjectID)
	if committed {
		created := "a second object"
		if len(ids) == 2 {
			created = fmt.Sprintf("%s and %s were both created", ids[0], ids[1])
		}
		a.add(Finding{
			Code: CodeUnsafeRetry, Severity: High, Path: r.Path, Fingerprint: r.Fingerprint,
			Requests: []int{orig.Seq, r.Seq}, ObjectIDs: ids, At: r.Done,
			Message: fmt.Sprintf("request #%d succeeded upstream but its response was lost; retry #%d used %s and also succeeded, so %s for one operation (duplicate charge/refund)",
				orig.Seq, r.Seq, keyDesc(r.IdempotencyKey, orig.IdempotencyKey), created),
		}, fmt.Sprintf("%s-%d", CodeUnsafeRetry, orig.Seq))
		// One duplicate has now happened; later retries are judged afresh.
		a.resolved[orig.Seq] = true
		return
	}
	a.add(Finding{
		Code: CodeWouldDuplicate, Severity: Medium, Path: r.Path, Fingerprint: r.Fingerprint,
		Requests: []int{orig.Seq, r.Seq}, ObjectIDs: ids, At: r.Done,
		Message: fmt.Sprintf("request #%d succeeded upstream but its response was lost; retry #%d used %s. It failed this time (status %d), but had it succeeded it would have created a duplicate",
			orig.Seq, r.Seq, keyDesc(r.IdempotencyKey, orig.IdempotencyKey), upstreamStatus),
	}, fmt.Sprintf("%s-%d-%d", CodeWouldDuplicate, orig.Seq, r.Seq))
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func (a *Auditor) lastWithFP(fp string) (Record, bool) {
	idx := a.byFP[fp]
	if len(idx) == 0 {
		return Record{}, false
	}
	return a.records[idx[len(idx)-1]], true
}

func (a *Auditor) add(f Finding, dedupeKey string) {
	if a.seen[dedupeKey] {
		return
	}
	a.seen[dedupeKey] = true
	a.findings = append(a.findings, f)
}

func isRetryable(f chaos.Fault) bool {
	return f == chaos.FaultError500 || f == chaos.FaultRateLimit
}

func keyDesc(now, prev string) string {
	switch {
	case now == "" && prev == "":
		return "no Idempotency-Key on either attempt"
	case now == "":
		return "no Idempotency-Key"
	default:
		return "a different Idempotency-Key than the original"
	}
}

// unreconciled lists committed attempts whose response the client never got
// and that were never retried: the client believes the operation failed, but
// the upstream performed it. Computed at report time because a later retry
// would change the verdict.
func (a *Auditor) unreconciled() []Finding {
	var out []Finding
	for _, r := range a.records {
		if !r.Committed || !lostResponse(r) || a.resolved[r.Seq] {
			continue
		}
		idx := a.byFP[r.Fingerprint]
		if a.records[idx[len(idx)-1]].Seq != r.Seq {
			continue // something retried it; judged elsewhere
		}
		out = append(out, Finding{
			Code: CodeUnreconciled, Severity: Medium, Path: r.Path, Fingerprint: r.Fingerprint,
			Requests: []int{r.Seq}, ObjectIDs: nonEmpty(r.ObjectID), At: r.Done,
			Message: fmt.Sprintf("request #%d was performed upstream%s, but the client got %s and never retried; it likely recorded a failure for an operation that succeeded. Reconcile via webhooks or by retrieving the object",
				r.Seq, objSuffix(r.ObjectID), lostHow(r.Fault)),
		})
	}
	return out
}

func objSuffix(id string) string {
	if id == "" {
		return ""
	}
	return " (" + id + ")"
}

func lostHow(f chaos.Fault) string {
	switch f {
	case chaos.FaultError500AfterCommit:
		return "a 500 error"
	case chaos.FaultHoldResponse:
		return "no response before giving up"
	}
	return "a reset connection"
}

// Findings returns findings sorted by severity, then time.
func (a *Auditor) Findings() []Finding {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append(append([]Finding(nil), a.findings...), a.unreconciled()...)
	rank := map[Severity]int{High: 0, Medium: 1, Low: 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Severity] != rank[out[j].Severity] {
			return rank[out[i].Severity] < rank[out[j].Severity]
		}
		return out[i].At.Before(out[j].At)
	})
	return out
}

// Records returns a copy of every audited request.
func (a *Auditor) Records() []Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Record(nil), a.records...)
}
