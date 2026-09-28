// Command stubaisa stands in for aisa in the dev stack and in spikes. It implements the two
// endpoints a gateway calls, POST /v1/decide and POST /v1/usage, logs everything it receives
// as JSON lines and keeps the requests for inspection at GET /debug/requests.
//
// Its decisions come from environment variables, not from Vault or Consul:
//
//	STUB_KEYS      key=consumer pairs, comma-separated          dev-key-chat=chat-ui,dev-key-batch=batch-jobs
//	STUB_REWRITES  consumer:from=to rules, comma-separated     batch-jobs:cloud-large=qwen3
//	STUB_DENY      consumers that get 429, comma-separated     blocked
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/hlan-net/aisa/dev/internal/devutil"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "stubaisa:", err)
		os.Exit(1)
	}
}

func run() error {
	healthcheck := flag.String("healthcheck", "", "probe this URL and exit (for container health checks)")
	flag.Parse()
	if *healthcheck != "" {
		return devutil.Probe(*healthcheck)
	}

	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	consumers := make([]string, 0, len(cfg.Keys))
	for _, c := range cfg.Keys {
		consumers = append(consumers, c)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	log.Info("stub aisa configured", "consumers", consumers, "rewrites", cfg.Rewrites, "deny", cfg.Deny)
	return devutil.Serve(devutil.Env("STUB_ADDR", ":8080"), newServer(cfg, log).handler(), log)
}

func configFromEnv() (Config, error) {
	cfg := Config{Keys: map[string]string{}, Rewrites: map[string]map[string]string{}}

	for _, pair := range splitList(os.Getenv("STUB_KEYS")) {
		key, consumer, ok := strings.Cut(pair, "=")
		if !ok || key == "" || consumer == "" {
			return cfg, fmt.Errorf("STUB_KEYS: want key=consumer, got %q", pair)
		}
		cfg.Keys[key] = consumer
	}
	for _, rule := range splitList(os.Getenv("STUB_REWRITES")) {
		consumer, fromTo, ok1 := strings.Cut(rule, ":")
		from, to, ok2 := strings.Cut(fromTo, "=")
		if !ok1 || !ok2 || consumer == "" || from == "" || to == "" {
			return cfg, fmt.Errorf("STUB_REWRITES: want consumer:from=to, got %q", rule)
		}
		if cfg.Rewrites[consumer] == nil {
			cfg.Rewrites[consumer] = map[string]string{}
		}
		cfg.Rewrites[consumer][from] = to
	}
	cfg.Deny = splitList(os.Getenv("STUB_DENY"))
	return cfg, nil
}

func splitList(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
