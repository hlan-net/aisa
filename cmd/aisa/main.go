// Command aisa is the governance layer for LLM traffic: identities, token quotas, money
// budgets and usage metrics next to an AI gateway. See docs/concepts/ARCHITECTURE.md.
//
// It is configured with environment variables:
//
//	AISA_ADDR              address to listen on                    :8080
//	AISA_LOG_LEVEL         debug, info, warn or error              info
//	AISA_SHUTDOWN_TIMEOUT  time for requests in flight at the end  10s
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hlan-net/aisa/internal/config"
	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/server"
	"github.com/hlan-net/aisa/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aisa:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("aisa", flag.ContinueOnError)
	showVersion := flags.Bool("version", false, "print the version and exit")
	healthcheck := flags.String("healthcheck", "", "probe this URL and exit (for container health checks)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *showVersion {
		fmt.Println(version.Version)
		return nil
	}
	if *healthcheck != "" {
		return probe(*healthcheck)
	}

	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	log.Info("starting aisa", "version", version.Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(log, metrics.New(version.Version).Handler(), cfg.ShutdownTimeout)
	if err := srv.Run(ctx, cfg.Addr); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	log.Info("stopped")
	return nil
}

// probe performs an HTTP GET and returns an error unless the answer is 2xx. The image has no
// shell or curl, so Docker's health check runs aisa itself.
func probe(url string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe %s: status %d", url, resp.StatusCode)
	}
	return nil
}
