// Package config reads aisa's configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

// Config is aisa's configuration.
type Config struct {
	// Addr is the address the HTTP server listens on (AISA_ADDR).
	Addr string
	// LogLevel is the lowest level that is logged (AISA_LOG_LEVEL).
	LogLevel slog.Level
	// ShutdownTimeout is how long requests in flight get to finish after SIGTERM
	// (AISA_SHUTDOWN_TIMEOUT).
	ShutdownTimeout time.Duration
}

// Defaults are the values used for variables that are unset or empty.
func Defaults() Config {
	return Config{
		Addr:            ":8080",
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 10 * time.Second,
	}
}

// FromEnv reads the configuration with getenv, which is os.Getenv outside tests.
func FromEnv(getenv func(string) string) (Config, error) {
	cfg := Defaults()

	if v := getenv("AISA_ADDR"); v != "" {
		if _, _, err := net.SplitHostPort(v); err != nil {
			return cfg, fmt.Errorf("AISA_ADDR: want host:port or :port, got %q: %w", v, err)
		}
		cfg.Addr = v
	}
	if v := getenv("AISA_LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(strings.ToUpper(v))); err != nil {
			return cfg, fmt.Errorf("AISA_LOG_LEVEL: want debug, info, warn or error, got %q: %w", v, err)
		}
	}
	if v := getenv("AISA_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("AISA_SHUTDOWN_TIMEOUT: want a duration such as 10s, got %q: %w", v, err)
		}
		if d <= 0 {
			return cfg, fmt.Errorf("AISA_SHUTDOWN_TIMEOUT: want a positive duration, got %q", v)
		}
		cfg.ShutdownTimeout = d
	}
	return cfg, nil
}
