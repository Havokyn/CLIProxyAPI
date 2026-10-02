package auth

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaObservationNormalizesCodexWindows(t *testing.T) {
	observedAt := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	resetWeekly := observedAt.Add(24 * time.Hour)
	resetShort := observedAt.Add(2 * time.Hour)
	state := QuotaState{}
	ok := state.ObserveResponseHeadersForProvider("codex", http.Header{
		"x-codex-primary-used-percent":     {"73"},
		"x-codex-primary-window-minutes":   {"10080"},
		"x-codex-primary-reset-at":         {resetWeekly.Format(time.RFC3339)},
		"x-codex-secondary-used-percent":   {"8"},
		"x-codex-secondary-window-minutes": {"300"},
		"x-codex-secondary-reset-at":       {resetShort.Format(time.RFC3339)},
		"x-codex-plan-type":                {"plus"},
	}, observedAt)
	if !ok {
		t.Fatal("quota observation was not accepted")
	}
	if len(state.Windows) != 2 {
		t.Fatalf("normalized windows = %+v, want 2", state.Windows)
	}
	byName := make(map[string]QuotaWindow, len(state.Windows))
	for _, window := range state.Windows {
		byName[window.Name] = window
	}
	if weekly := byName["weekly"]; weekly.RemainingPercent != 27 || !weekly.ResetAt.Equal(resetWeekly) || weekly.DurationSeconds != 7*24*60*60 {
		t.Fatalf("weekly window = %+v", weekly)
	}
	if short := byName["5h"]; short.RemainingPercent != 92 || !short.ResetAt.Equal(resetShort) || short.DurationSeconds != 5*60*60 {
		t.Fatalf("short window = %+v", short)
	}
}

func TestQuotaObservationNormalizesGenericRemainingAndResetSignals(t *testing.T) {
	observedAt := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	resetAt := observedAt.Add(7 * 24 * time.Hour)
	windows := quotaWindowsFromSignals("devin", map[string]string{
		"weekly_quota_remaining_percent": "50%",
		"weekly_quota_reset_at":          resetAt.Format(time.RFC3339),
	}, observedAt)
	if len(windows) != 1 || windows[0].Name != "weekly" || windows[0].RemainingPercent != 50 || !windows[0].ResetAt.Equal(resetAt) {
		t.Fatalf("generic windows = %+v", windows)
	}
}

func TestQuotaWindowsFromNormalizedPluginGroups(t *testing.T) {
	now := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	windows := QuotaWindowsFromNormalizedGroups("codex", []pluginapi.QuotaGroup{
		{DisplayName: "Normal", Buckets: []pluginapi.QuotaBucket{{Window: "weekly", RemainingFraction: 0.27, ResetTime: "2026-10-03T15:47:00Z"}}},
		{DisplayName: "GPT Reserve", Buckets: []pluginapi.QuotaBucket{{Window: "weekly", RemainingFraction: 0.64, ResetTime: "2026-10-08T16:46:00Z"}}},
	}, now)
	if len(windows) != 2 {
		t.Fatalf("normalized plugin windows = %#v, want 2 windows", windows)
	}
	if windows[0].RemainingPercent != 27 || windows[0].Reserve {
		t.Fatalf("normal plugin window = %#v", windows[0])
	}
	if windows[1].RemainingPercent != 64 || !windows[1].Reserve {
		t.Fatalf("reserve plugin window = %#v", windows[1])
	}
}

func TestQuotaWindowsFromNormalizedPluginGroupsRejectsMissingReset(t *testing.T) {
	windows := QuotaWindowsFromNormalizedGroups("codex", []pluginapi.QuotaGroup{{Buckets: []pluginapi.QuotaBucket{{Window: "weekly", RemainingFraction: 0.9}}}}, time.Now().UTC())
	if len(windows) != 0 {
		t.Fatalf("plugin bucket without reset became routable: %#v", windows)
	}
}
