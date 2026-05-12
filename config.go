package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

	// LLMAPIKey is an OpenAI-compatible API key — DeepSeek by default,
	// but any host speaking the OpenAI chat-completions protocol works
	// (OpenAI itself, Groq, a local vLLM, etc.). Optional: when unset,
	// Tier 3 is skipped and unmatched fold txns go straight to
	// needs_review for human disposition.
	LLMAPIKey string // TEXAS_FOLDEM_LLM_API_KEY          optional

	// LLMModel selects the model. Defaults to deepseek-v4-flash — the
	// cost-efficient flagship in the DeepSeek-V4 family. Override to
	// deepseek-v4-pro for higher accuracy at higher cost, or to any
	// model name supported by your LLMBaseURL provider.
	LLMModel string // TEXAS_FOLDEM_LLM_MODEL            default deepseek-v4-flash

	// LLMBaseURL is the OpenAI-compatible v1 base URL. Defaults to
	// DeepSeek; set to https://api.openai.com/v1 (or similar) to use
	// a different provider without code changes.
	LLMBaseURL string // TEXAS_FOLDEM_LLM_BASE_URL         default https://api.deepseek.com/v1

	// FireflyReadOnly is the operator-facing kill-switch for the push
	// endpoint. When true, /admin/push will refuse confirmed writes
	// (preview mode still works). The flag exists so an operator who
	// has any doubt about the integration can stop all writes at the
	// HTTP boundary without redeploying.
	FireflyReadOnly bool // TEXAS_FOLDEM_FIREFLY_READONLY     default false

	// UICookieAuth toggles cookie-based auth for /admin/ui/* routes.
	// false (default): trust upstream proxy auth (tinyauth ForwardAuth
	// at the cluster ingress). true: require a tfe-admin cookie set
	// via GET /admin/ui/login?key=<admin>. Use cookie mode for local
	// development against a port-forward.
	UICookieAuth bool // TEXAS_FOLDEM_UI_COOKIE_AUTH       default false

	// PeriodicSyncEvery is the cadence for the integrated cron loop:
	// fold sync → classify pending. 0 disables the loop entirely
	// (manual /admin/* triggers still work). Recommended cadence is
	// 1h once everything's stable; start at 0 (manual only) and bump
	// once you trust the classifier proposals.
	PeriodicSyncEvery time.Duration // TEXAS_FOLDEM_PERIODIC_SYNC_EVERY   default 0 (disabled)

	// PeriodicSyncLimit is the per-cycle hard cap on transactions
	// fetched from fold. The cron uses the gap-fill (since-firefly)
	// mode, so this is a safety belt on a single tick rather than the
	// expected workload — a steady hourly cron sees only the new ones.
	// 2000 covers >18 months of typical activity (~150 txns/month) so
	// any realistic outage self-heals on the next tick.
	PeriodicSyncLimit int // TEXAS_FOLDEM_PERIODIC_SYNC_LIMIT   default 2000
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
		LLMAPIKey:          os.Getenv("TEXAS_FOLDEM_LLM_API_KEY"),
		LLMModel:           envStr("TEXAS_FOLDEM_LLM_MODEL", "deepseek-v4-flash"),
		LLMBaseURL:         strings.TrimRight(envStr("TEXAS_FOLDEM_LLM_BASE_URL", "https://api.deepseek.com/v1"), "/"),
		FireflyReadOnly:    envBool("TEXAS_FOLDEM_FIREFLY_READONLY", false),
		UICookieAuth:       envBool("TEXAS_FOLDEM_UI_COOKIE_AUTH", false),
		PeriodicSyncEvery:  envDur("TEXAS_FOLDEM_PERIODIC_SYNC_EVERY", 0),
		PeriodicSyncLimit:  envInt("TEXAS_FOLDEM_PERIODIC_SYNC_LIMIT", 2000),
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

// envInt parses an integer env var, falling back to def on parse error
// or empty value.
func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
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
