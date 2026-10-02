package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResetAwareSettingsSurviveUpstreamLayoutSave(t *testing.T) {
	for _, v8 := range []bool{false, true} {
		name := "legacy"
		raw := "port: 8317\n"
		if v8 {
			name = "v8"
			raw = "config-version: 8\nserver: {port: 8317}\nupstream: {codex: {response-steering: true}}\n"
		}
		t.Run(name, func(t *testing.T) {
			raw += `routing:
  strategy: reset-aware
  reset-aware:
    preserve-session-affinity: false
    longest-window-first: false
    use-expiring-capacity-first: false
    refresh-after-reset: false
    min-long-window-remaining-percent: 0
    min-short-window-remaining-percent: 0
`
			file := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(file)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Port = 8318
			if err = SaveConfigPreserveComments(file, cfg, false); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if v8 {
				if err = ValidateV8Config(data); err != nil {
					t.Fatal(err)
				}
			} else if strings.Contains(string(data), "config-version:") || strings.Contains(string(data), "server:") {
				t.Fatal("legacy save unexpectedly migrated layout")
			}
			reloaded, err := LoadConfig(file)
			if err != nil {
				t.Fatal(err)
			}
			policy := reloaded.Routing.ResetAware
			for _, value := range []*bool{policy.PreserveSessionAffinity, policy.LongestWindowFirst, policy.UseExpiringCapacityFirst, policy.RefreshAfterReset} {
				if value == nil || *value {
					t.Fatal("explicit false policy changed during save")
				}
			}
			for _, value := range []*float64{policy.MinLongWindowRemainingPercent, policy.MinShortWindowRemainingPercent} {
				if value == nil || *value != 0 {
					t.Fatal("explicit zero floor changed during save")
				}
			}
			if reloaded.Routing.Strategy != "reset-aware" || reloaded.Port != 8318 || (v8 && !reloaded.Codex.ResponseSteering) {
				t.Fatal("routing or upstream settings changed during save")
			}
		})
	}
}

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
