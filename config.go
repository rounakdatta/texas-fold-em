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
	ListenAddr    string        // TEXAS_FOLDEM_LISTEN_ADDR      default ":8080"
	StatePath     string        // TEXAS_FOLDEM_STATE_PATH       default ~/.texas-fold-em/state.json
	BrokerKey     string        // TEXAS_FOLDEM_BROKER_KEY       required; protects GET /token
	AdminKey      string        // TEXAS_FOLDEM_ADMIN_KEY        required; protects POST /init
	APIBase       string        // TEXAS_FOLDEM_API_BASE         default https://api.fold.money/api
	LogLevel      string        // TEXAS_FOLDEM_LOG_LEVEL        default "info"
	RefreshLead   time.Duration // TEXAS_FOLDEM_REFRESH_LEAD     default 2m
	KeepWarmEvery time.Duration // TEXAS_FOLDEM_KEEPWARM_EVERY   default 1m (0 to disable)
	HTTPTimeout   time.Duration // TEXAS_FOLDEM_HTTP_TIMEOUT     default 15s
	ShutdownGrace time.Duration // TEXAS_FOLDEM_SHUTDOWN_GRACE   default 10s

	// IntegrationEnabled gates the entire fold→firefly classification
	// subsystem. Default false: the broker runs alone, no SQLite is opened,
	// no firefly traffic, no LLM calls. Flip to true to start using it.
	IntegrationEnabled bool // TEXAS_FOLDEM_INTEGRATION_ENABLED  default false

	// StagingDBPath is where the integration's SQLite file lives. Distinct
	// from StatePath so schema migrations in the integration code can never
	// destabilise the broker's refresh chain.
	StagingDBPath string // TEXAS_FOLDEM_STAGING_DB_PATH       default ~/.texas-fold-em/staging.db

	// FireflyBase is the base URL for the firefly host. The default
	// targets the in-cluster Service; tests and local runs override.
	// Required when the integration is enabled.
	FireflyBase string // TEXAS_FOLDEM_FIREFLY_BASE          default http://firefly.apps.svc.cluster.local:8080

	// FireflyPAT is a Firefly III Personal Access Token. Sourced from
	// the firefly-pat key of the texas-fold-em-credentials Secret in
	// production. Required when the integration is enabled.
	FireflyPAT string // TEXAS_FOLDEM_FIREFLY_PAT           required when integration enabled
}

// LoadConfig reads env vars and returns a validated Config. It never reads
// anything from disk; the state file is handled by the Store.
func LoadConfig() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home dir: %w", err)
	}

	cfg := Config{
		ListenAddr:         envStr("TEXAS_FOLDEM_LISTEN_ADDR", ":8080"),
		StatePath:          envStr("TEXAS_FOLDEM_STATE_PATH", filepath.Join(home, ".texas-fold-em", "state.json")),
		BrokerKey:          os.Getenv("TEXAS_FOLDEM_BROKER_KEY"),
		AdminKey:           os.Getenv("TEXAS_FOLDEM_ADMIN_KEY"),
		APIBase:            strings.TrimRight(envStr("TEXAS_FOLDEM_API_BASE", "https://api.fold.money/api"), "/"),
		LogLevel:           envStr("TEXAS_FOLDEM_LOG_LEVEL", "info"),
		RefreshLead:        envDur("TEXAS_FOLDEM_REFRESH_LEAD", 2*time.Minute),
		KeepWarmEvery:      envDur("TEXAS_FOLDEM_KEEPWARM_EVERY", time.Minute),
		HTTPTimeout:        envDur("TEXAS_FOLDEM_HTTP_TIMEOUT", 15*time.Second),
		ShutdownGrace:      envDur("TEXAS_FOLDEM_SHUTDOWN_GRACE", 10*time.Second),
		IntegrationEnabled: envBool("TEXAS_FOLDEM_INTEGRATION_ENABLED", false),
		StagingDBPath:      envStr("TEXAS_FOLDEM_STAGING_DB_PATH", filepath.Join(home, ".texas-fold-em", "staging.db")),
		FireflyBase:        strings.TrimRight(envStr("TEXAS_FOLDEM_FIREFLY_BASE", "http://firefly.apps.svc.cluster.local:8080"), "/"),
		FireflyPAT:         os.Getenv("TEXAS_FOLDEM_FIREFLY_PAT"),
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
	if cfg.IntegrationEnabled {
		if cfg.FireflyBase == "" {
			problems = append(problems, "TEXAS_FOLDEM_FIREFLY_BASE is required when integration is enabled")
		}
		if cfg.FireflyPAT == "" {
			problems = append(problems, "TEXAS_FOLDEM_FIREFLY_PAT is required when integration is enabled")
		}
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

// envBool parses common true-ish values; anything else (or unset) falls
// back to the default. Permissive so "1", "true", "TRUE", "yes" all work.
func envBool(k string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}
