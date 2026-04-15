package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config holds all tunables. Values come from env vars with a TEXAS_FOLDEM_
// prefix so nothing collides with anything else on a shared host.
type Config struct {
	ListenAddr     string        // TEXAS_FOLDEM_LISTEN_ADDR      default ":8080"
	StatePath      string        // TEXAS_FOLDEM_STATE_PATH       default ~/.texas-fold-em/state.json
	BrokerKey      string        // TEXAS_FOLDEM_BROKER_KEY       required; protects GET /token
	AdminKey       string        // TEXAS_FOLDEM_ADMIN_KEY        required; protects POST /init
	APIBase        string        // TEXAS_FOLDEM_API_BASE         default https://api.fold.money/api
	LogLevel       string        // TEXAS_FOLDEM_LOG_LEVEL        default "info"
	RefreshLead    time.Duration // TEXAS_FOLDEM_REFRESH_LEAD     default 2m
	KeepWarmEvery  time.Duration // TEXAS_FOLDEM_KEEPWARM_EVERY   default 1m (0 to disable)
	HTTPTimeout    time.Duration // TEXAS_FOLDEM_HTTP_TIMEOUT     default 15s
	ShutdownGrace  time.Duration // TEXAS_FOLDEM_SHUTDOWN_GRACE   default 10s
}

// LoadConfig reads env vars and returns a validated Config. It never reads
// anything from disk; the state file is handled by the Store.
func LoadConfig() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home dir: %w", err)
	}

	cfg := Config{
		ListenAddr:    envStr("TEXAS_FOLDEM_LISTEN_ADDR", ":8080"),
		StatePath:     envStr("TEXAS_FOLDEM_STATE_PATH", filepath.Join(home, ".texas-fold-em", "state.json")),
		BrokerKey:     os.Getenv("TEXAS_FOLDEM_BROKER_KEY"),
		AdminKey:      os.Getenv("TEXAS_FOLDEM_ADMIN_KEY"),
		APIBase:       strings.TrimRight(envStr("TEXAS_FOLDEM_API_BASE", "https://api.fold.money/api"), "/"),
		LogLevel:      envStr("TEXAS_FOLDEM_LOG_LEVEL", "info"),
		RefreshLead:   envDur("TEXAS_FOLDEM_REFRESH_LEAD", 2*time.Minute),
		KeepWarmEvery: envDur("TEXAS_FOLDEM_KEEPWARM_EVERY", time.Minute),
		HTTPTimeout:   envDur("TEXAS_FOLDEM_HTTP_TIMEOUT", 15*time.Second),
		ShutdownGrace: envDur("TEXAS_FOLDEM_SHUTDOWN_GRACE", 10*time.Second),
	}

	var problems []string
	if cfg.BrokerKey == "" {
		problems = append(problems, "TEXAS_FOLDEM_BROKER_KEY is required")
	}
	if cfg.AdminKey == "" {
		problems = append(problems, "TEXAS_FOLDEM_ADMIN_KEY is required")
	}
	if cfg.BrokerKey != "" && cfg.BrokerKey == cfg.AdminKey {
		problems = append(problems, "TEXAS_FOLDEM_BROKER_KEY and TEXAS_FOLDEM_ADMIN_KEY must differ (blast radius isolation)")
	}
	if cfg.RefreshLead < 30*time.Second {
		problems = append(problems, "TEXAS_FOLDEM_REFRESH_LEAD must be at least 30s")
	}
	if len(problems) > 0 {
		return Config{}, errors.New("invalid config: " + strings.Join(problems, "; "))
	}
	return cfg, nil
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
