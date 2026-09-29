package config

import (
	"log/slog"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromEnvDefaults(t *testing.T) {
	cfg, err := FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg != Defaults() {
		t.Errorf("cfg = %+v, want the defaults %+v", cfg, Defaults())
	}
}

func TestFromEnv(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{
		"AISA_ADDR":             "127.0.0.1:9000",
		"AISA_LOG_LEVEL":        "debug",
		"AISA_SHUTDOWN_TIMEOUT": "30s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Addr: "127.0.0.1:9000", LogLevel: slog.LevelDebug, ShutdownTimeout: 30 * time.Second}
	if cfg != want {
		t.Errorf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestFromEnvLogLevels(t *testing.T) {
	for v, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError,
		"WARN": slog.LevelWarn,
	} {
		cfg, err := FromEnv(env(map[string]string{"AISA_LOG_LEVEL": v}))
		if err != nil {
			t.Errorf("%s: %v", v, err)
		}
		if cfg.LogLevel != want {
			t.Errorf("%s: level = %v, want %v", v, cfg.LogLevel, want)
		}
	}
}

func TestFromEnvRejects(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"address without a port": {"AISA_ADDR": "localhost"},
		"unknown log level":      {"AISA_LOG_LEVEL": "loud"},
		"log level with offset":  {"AISA_LOG_LEVEL": "info+1"},
		"timeout not a duration": {"AISA_SHUTDOWN_TIMEOUT": "ten"},
		"timeout of zero":        {"AISA_SHUTDOWN_TIMEOUT": "0s"},
		"negative timeout":       {"AISA_SHUTDOWN_TIMEOUT": "-5s"},
	} {
		if _, err := FromEnv(env(vars)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
