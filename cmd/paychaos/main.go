// Command paychaos is a chaos-testing proxy for Stripe payment integrations.
//
//	paychaos [flags]          run the fault-injecting proxy (and optional webhook relay)
//	paychaos mock [-listen]   run the bundled Stripe-shaped mock API
//	paychaos scenario         print the default scenario as JSON
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/audit"
	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/chaos"
	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/mock"
	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/proxy"
	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/report"
	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/webhook"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mock":
			os.Exit(runMock(os.Args[2:]))
		case "scenario":
			b, _ := json.MarshalIndent(chaos.DefaultConfig(), "", "  ")
			fmt.Println(string(b))
			return
		}
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("paychaos", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "address the API proxy listens on")
	upstream := fs.String("upstream", "https://api.stripe.com", `upstream API base URL, or "mock" for the built-in mock`)
	cfgPath := fs.String("config", "", "scenario JSON file (default: built-in scenario; see `paychaos scenario`)")
	seed := fs.Int64("seed", -1, "override the scenario seed (-1 keeps the file's seed)")
	whListen := fs.String("webhook-listen", "", "address for the webhook relay, e.g. 127.0.0.1:8081 (disabled if empty)")
	whTarget := fs.String("webhook-target", "", "your app's webhook endpoint the relay delivers to")
	reportPath := fs.String("report", "paychaos-report.json", "where to write the JSON report on exit")
	duration := fs.Duration("duration", 0, "stop automatically after this long (0 = until Ctrl-C)")
	failOnHigh := fs.Bool("fail-on-high", true, "exit with status 2 if any HIGH finding was detected (for CI)")
	mockDelay := fs.Duration("mock-exec-delay", 0, "with -upstream mock: how long each create executes (retries reusing the key meanwhile get 409)")
	mockErrEvery := fs.Int("mock-server-error-every", 0, "with -upstream mock: every Nth create returns a cached 500 after creating the object")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := chaos.DefaultConfig()
	if *cfgPath != "" {
		var err error
		if cfg, err = chaos.LoadConfig(*cfgPath); err != nil {
			log.Error("invalid scenario", "err", err)
			return 1
		}
	}
	if *seed >= 0 {
		cfg.Seed = uint64(*seed)
	}

	engine := chaos.NewEngine(cfg)
	auditor := audit.New(cfg.MutatingPaths, time.Duration(cfg.MinBackoffMS)*time.Millisecond).
		WithOperationID(cfg.OperationID)

	allowAny := false
	var up *url.URL
	if *upstream == "mock" {
		m := httptest.NewServer(mock.NewWithOptions(mock.Options{ExecDelay: *mockDelay, ServerErrorEvery: *mockErrEvery}))
		defer m.Close()
		up, _ = url.Parse(m.URL)
		allowAny = true
		log.Info("using built-in mock upstream", "url", m.URL)
	} else {
		var err error
		if up, err = url.Parse(*upstream); err != nil || up.Scheme == "" {
			log.Error("invalid -upstream", "value", *upstream)
			return 1
		}
	}

	px, err := proxy.New(proxy.Options{Upstream: up, Engine: engine, Auditor: auditor, Logger: log, AllowAnyKey: allowAny})
	if err != nil {
		log.Error("proxy", "err", err)
		return 1
	}

	var relay *webhook.Relay
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__paychaos/report", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report.Build(cfg.Seed, auditor, relay))
	})
	mux.Handle("/", px)
	servers := []*http.Server{{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}}

	if *whListen != "" {
		if *whTarget == "" {
			log.Error("-webhook-listen needs -webhook-target")
			return 1
		}
		relay = webhook.New(*whTarget, cfg.Webhooks, chaos.NewStream(cfg.Seed, chaos.StreamWebhooks), log)
		servers = append(servers, &http.Server{Addr: *whListen, Handler: relay, ReadHeaderTimeout: 10 * time.Second})
	}

	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(s)
	}
	log.Info("paychaos running", "proxy", *listen, "upstream", up.String(), "seed", cfg.Seed, "webhook_relay", *whListen)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		log.Error("server failed", "err", err)
		return 1
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	if relay != nil {
		relay.Close()
	}

	rep := report.Build(cfg.Seed, auditor, relay)
	rep.WriteText(os.Stdout)
	if err := rep.WriteJSON(*reportPath); err != nil {
		log.Error("writing report", "err", err)
	} else {
		fmt.Printf("\nFull report: %s\n", *reportPath)
	}
	if *failOnHigh && rep.HasHigh() {
		return 2
	}
	return 0
}

func runMock(args []string) int {
	fs := flag.NewFlagSet("mock", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:12111", "address for the mock API")
	delay := fs.Duration("exec-delay", 0, "how long each create executes (retries reusing the key meanwhile get 409)")
	errEvery := fs.Int("server-error-every", 0, "every Nth create returns a cached 500 after creating the object (0 = never)")
	_ = fs.Parse(args)
	fmt.Fprintf(os.Stderr, "mock Stripe-shaped API on http://%s (stats at /__mock/stats)\n", *listen)
	srv := &http.Server{Addr: *listen, Handler: mock.NewWithOptions(mock.Options{ExecDelay: *delay, ServerErrorEvery: *errEvery}),
		ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
