package config

import "testing"

func TestParseResetAwareRoutingAndDefaults(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`
config-version: 8
routing:
  strategy: reset-aware
  reset-aware:
    preserve-session-affinity: false
    min-long-window-remaining-percent: 12
    min-short-window-remaining-percent: 6
    stale-telemetry-policy: exclude
    fallback-strategy: fill-first
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.Routing.Strategy != "reset-aware" || cfg.Routing.ResetAware.PreserveSessionAffinity == nil || *cfg.Routing.ResetAware.PreserveSessionAffinity {
		t.Fatalf("reset-aware config did not parse: %+v", cfg.Routing)
	}
	if got := *cfg.Routing.ResetAware.MinLongWindowRemainingPercent; got != 12 {
		t.Fatalf("long floor = %v, want 12", got)
	}
	if got := cfg.Routing.ResetAware.FallbackStrategy; got != "fill-first" {
		t.Fatalf("fallback strategy = %q, want fill-first", got)
	}
}

func TestResetAwareRoutingRejectsAutomaticManualResets(t *testing.T) {
	enabled := true
	cfg := &Config{Routing: RoutingConfig{
		Strategy:   "reset-aware",
		ResetAware: ResetAwareRoutingConfig{AutoUseManualResets: &enabled},
	}}
	if errValidate := cfg.ValidateRouting(); errValidate == nil {
		t.Fatal("automatic manual reset use was accepted")
	}
}

func TestResetAwareRoutingRejectsInvalidFloors(t *testing.T) {
	floor := 101.0
	cfg := &Config{Routing: RoutingConfig{
		Strategy:   "reset-aware",
		ResetAware: ResetAwareRoutingConfig{MinLongWindowRemainingPercent: &floor},
	}}
	if errValidate := cfg.ValidateRouting(); errValidate == nil {
		t.Fatal("invalid reset-aware floor was accepted")
	}
}
