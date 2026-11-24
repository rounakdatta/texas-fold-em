package main

import (
	"testing"
	"time"
)

// A refresh is waited out, so the client's timeout must outlast a network
// stall, and the lead must leave room for it before the access token expires.
func TestLoadConfig_RefreshTimings(t *testing.T) {
	t.Setenv("TEXAS_FOLDEM_BROKER_KEY", "broker")
	t.Setenv("TEXAS_FOLDEM_ADMIN_KEY", "admin")
	t.Setenv("TEXAS_FOLDEM_HTTP_TIMEOUT", "")
	t.Setenv("TEXAS_FOLDEM_REFRESH_LEAD", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPTimeout != 90*time.Second || cfg.RefreshLead != 5*time.Minute {
		t.Fatalf("defaults: timeout %v, lead %v; want 90s and 5m", cfg.HTTPTimeout, cfg.RefreshLead)
	}

	t.Setenv("TEXAS_FOLDEM_HTTP_TIMEOUT", "0s")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("a zero timeout was accepted; it would let one refresh hang every caller")
	}
}
