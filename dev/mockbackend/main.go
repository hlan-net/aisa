// Command mockbackend is an OpenAI-compatible model backend for the dev stack and integration
// tests. It generates deterministic token counts, so usage events can be checked exactly:
//
//   - prompt_tokens is the number of whitespace-separated words in all message contents
//   - completion_tokens is MOCK_DEFAULT_COMPLETION_TOKENS, lowered by max_tokens or
//     max_completion_tokens, or set exactly by the X-Mock-Completion-Tokens request header
//
// Streamed responses send one chunk per token and, depending on MOCK_STREAM_USAGE, a final
// usage chunk. Every response carries X-Mock-Backend so tests can see which instance served it.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hlan-net/aisa/dev/internal/devutil"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mockbackend:", err)
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
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	log.Info("mock backend configured",
		"name", cfg.Name, "models", cfg.Models, "default_completion_tokens", cfg.DefaultCompletionTokens,
		"ttft", cfg.TTFT.String(), "token_delay", cfg.TokenDelay.String(), "stream_usage", cfg.StreamUsage)
	return devutil.Serve(devutil.Env("MOCK_ADDR", ":8080"), newServer(cfg, log), log)
}

func configFromEnv() (Config, error) {
	cfg := Config{
		Name:        devutil.Env("MOCK_NAME", "mock"),
		StreamUsage: devutil.Env("MOCK_STREAM_USAGE", usageAuto),
	}
	for m := range strings.SplitSeq(devutil.Env("MOCK_MODELS", ""), ",") {
		if m = strings.TrimSpace(m); m != "" {
			cfg.Models = append(cfg.Models, m)
		}
	}
	switch cfg.StreamUsage {
	case usageAuto, usageAlways, usageNever:
	default:
		return cfg, fmt.Errorf("MOCK_STREAM_USAGE must be %s, %s or %s, got %q", usageAuto, usageAlways, usageNever, cfg.StreamUsage)
	}

	n, err := strconv.Atoi(devutil.Env("MOCK_DEFAULT_COMPLETION_TOKENS", "16"))
	if err != nil || n < 0 || n > maxCompletionTokens {
		return cfg, fmt.Errorf("MOCK_DEFAULT_COMPLETION_TOKENS must be an integer in 0..%d", maxCompletionTokens)
	}
	cfg.DefaultCompletionTokens = n

	if cfg.TTFT, err = time.ParseDuration(devutil.Env("MOCK_TTFT", "0s")); err != nil {
		return cfg, fmt.Errorf("parse MOCK_TTFT: %w", err)
	}
	if cfg.TokenDelay, err = time.ParseDuration(devutil.Env("MOCK_TOKEN_DELAY", "0s")); err != nil {
		return cfg, fmt.Errorf("parse MOCK_TOKEN_DELAY: %w", err)
	}
	return cfg, nil
}
