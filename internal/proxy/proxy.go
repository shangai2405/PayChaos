// Package proxy is the fault-injecting reverse proxy that sits between an
// application and the Stripe API (test mode only) or the bundled mock.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/audit"
	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/chaos"
)

const maxBody = 1 << 20 // Stripe request bodies are small form posts

// hop-by-hop headers are not forwarded (RFC 9110 §7.6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Options configures a Proxy.
type Options struct {
	Upstream *url.URL
	Engine   *chaos.Engine
	Auditor  *audit.Auditor
	Logger   *slog.Logger
	// AllowAnyKey skips the test-key check. Only for the bundled mock.
	AllowAnyKey bool
	Client      *http.Client
}

// Proxy implements http.Handler.
type Proxy struct {
	opt Options
}

// New returns a proxy. Upstream, Engine and Auditor are required.
func New(opt Options) (*Proxy, error) {
	if opt.Upstream == nil || opt.Engine == nil || opt.Auditor == nil {
		return nil, errors.New("proxy: upstream, engine and auditor are required")
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opt.Client == nil {
		opt.Client = &http.Client{
			Timeout: 80 * time.Second, // Stripe's own SDKs default to 80s
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Proxy{opt: opt}, nil
}

// ServeHTTP applies at most one fault per request and records it for auditing.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := apiKey(r)
	if isLiveKey(key) {
		stripeError(w, http.StatusForbidden, "invalid_request_error", "paychaos_live_key_refused",
			"paychaos refuses live-mode keys. Use an sk_test_/rk_test_ key: fault injection must never touch real money.")
		return
	}
	if !p.opt.AllowAnyKey && !isTestKey(key) {
		stripeError(w, http.StatusUnauthorized, "invalid_request_error", "paychaos_test_key_required",
			"paychaos only forwards requests authenticated with a Stripe test-mode key (sk_test_ or rk_test_).")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		stripeError(w, http.StatusBadRequest, "invalid_request_error", "", "could not read request body")
		return
	}
	if len(body) > maxBody {
		stripeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "", "request body too large")
		return
	}

	d := p.opt.Engine.Decide(r.Method, r.URL.Path)
	audited := p.opt.Auditor.IsMutating(r.Method, r.URL.Path)
	seq := 0
	if audited {
		seq = p.opt.Auditor.BeginRequest(key, r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Idempotency-Key"), body)
	}
	complete := func(status int, committed bool, objectID string) {
		if audited {
			p.opt.Auditor.Complete(seq, d.Rule, d.Fault, status, committed, objectID)
		}
	}
	log := p.opt.Logger.With("seq", seq, "method", r.Method, "path", r.URL.Path, "fault", d.Fault, "rule", d.Rule)

	switch d.Fault {
	case chaos.FaultError500:
		complete(0, false, "")
		log.Info("injected 500")
		setShouldRetry(w, d.ShouldRetry)
		stripeError(w, http.StatusInternalServerError, "api_error", "", "paychaos injected an upstream error")
		return

	case chaos.FaultRateLimit:
		complete(0, false, "")
		log.Info("injected 429")
		setShouldRetry(w, d.ShouldRetry)
		stripeError(w, http.StatusTooManyRequests, "invalid_request_error", "rate_limit", "paychaos injected a rate limit")
		return

	case chaos.FaultTimeoutBeforeForward:
		log.Info("stalling, then 504 without forwarding", "delay", d.Delay)
		select {
		case <-time.After(d.Delay):
		case <-r.Context().Done():
		}
		complete(0, false, "")
		setShouldRetry(w, d.ShouldRetry)
		stripeError(w, http.StatusGatewayTimeout, "api_error", "", "paychaos injected a gateway timeout")
		return

	case chaos.FaultLatency:
		log.Info("adding latency", "delay", d.Delay)
		select {
		case <-time.After(d.Delay):
		case <-r.Context().Done():
			complete(0, false, "")
			return
		}
	}

	// For faults that cut the client off or make it give up, the upstream call
	// must outlive the client; detach it from the client's context.
	ctx := r.Context()
	if d.Fault == chaos.FaultDropAfterCommit || d.Fault == chaos.FaultHoldResponse {
		ctx = context.WithoutCancel(ctx)
	}
	resp, err := p.forward(ctx, r, body)
	if err != nil {
		complete(0, false, "")
		log.Warn("upstream error", "err", err)
		stripeError(w, http.StatusBadGateway, "api_error", "", "paychaos could not reach the upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		complete(resp.StatusCode, false, "")
		stripeError(w, http.StatusBadGateway, "api_error", "", "paychaos could not read the upstream response")
		return
	}
	committed := resp.StatusCode >= 200 && resp.StatusCode < 300
	objectID := ""
	if committed {
		objectID = createdID(respBody)
	}

	if d.Fault == chaos.FaultDropAfterCommit {
		// Record before cutting the connection so the client's retry, which
		// can only start after the reset, already sees a committed attempt.
		complete(resp.StatusCode, committed, objectID)
		log.Info("upstream committed; resetting client connection", "upstream_status", resp.StatusCode)
		resetConnection(w)
		return
	}

	if d.Fault == chaos.FaultError500AfterCommit {
		complete(resp.StatusCode, committed, objectID)
		log.Info("upstream answered; telling the client 500", "upstream_status", resp.StatusCode)
		setShouldRetry(w, d.ShouldRetry)
		stripeError(w, http.StatusInternalServerError, "api_error", "", "paychaos injected a 500 after the upstream processed the request")
		return
	}

	if d.Fault == chaos.FaultHoldResponse {
		// Record first: a client that times out may retry while we hold.
		complete(resp.StatusCode, committed, objectID)
		log.Info("holding upstream response", "delay", d.Delay, "upstream_status", resp.StatusCode)
		select {
		case <-time.After(d.Delay):
		case <-r.Context().Done():
			log.Info("client gave up before the held response was delivered")
			return
		}
		if writeResponse(w, resp, respBody) == nil && audited {
			p.opt.Auditor.MarkDelivered(seq)
		}
		return
	}

	complete(resp.StatusCode, committed, objectID)
	_ = writeResponse(w, resp, respBody)
}

func writeResponse(w http.ResponseWriter, resp *http.Response, body []byte) error {
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, err := w.Write(body)
	if err == nil {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	return err
}

// setShouldRetry sets Stripe's retry-advice header on an injected error.
func setShouldRetry(w http.ResponseWriter, v string) {
	if v != "" {
		w.Header().Set("Stripe-Should-Retry", v)
	}
}

func (p *Proxy) forward(ctx context.Context, r *http.Request, body []byte) (*http.Response, error) {
	target := *p.opt.Upstream
	target.Path = strings.TrimSuffix(target.Path, "/") + r.URL.Path
	target.RawQuery = r.URL.RawQuery
	out, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(out.Header, r.Header)
	out.Header.Del("Accept-Encoding") // keep bodies readable for the audit log
	return p.opt.Client.Do(out)
}

// resetConnection hijacks the client connection and closes it with a TCP RST,
// which is what a client sees when a load balancer or flaky network drops a
// response mid-flight. Falls back to an empty 502 when hijacking is impossible
// (for example HTTP/2).
func resetConnection(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = conn.Close()
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		dst[k] = append([]string(nil), vs...)
	}
	for _, h := range hopHeaders {
		dst.Del(h)
	}
	dst.Del("Content-Length")
}

// createdID returns the "id" field of a Stripe JSON object, or "".
func createdID(body []byte) string {
	var obj struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	return obj.ID
}

// apiKey extracts a Stripe key from Bearer or Basic auth (Stripe accepts both).
func apiKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	if u, _, ok := r.BasicAuth(); ok {
		return u
	}
	return ""
}

func isLiveKey(k string) bool {
	return strings.HasPrefix(k, "sk_live_") || strings.HasPrefix(k, "rk_live_")
}

func isTestKey(k string) bool {
	return strings.HasPrefix(k, "sk_test_") || strings.HasPrefix(k, "rk_test_")
}

func stripeError(w http.ResponseWriter, status int, typ, code, msg string) {
	e := map[string]string{"type": typ, "message": msg}
	if code != "" {
		e["code"] = code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
}

// String is used in logs.
func (p *Proxy) String() string { return fmt.Sprintf("paychaos proxy -> %s", p.opt.Upstream) }
