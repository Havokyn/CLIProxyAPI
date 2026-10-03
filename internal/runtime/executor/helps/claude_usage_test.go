package helps

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestParseClaudeUsageHeaders(t *testing.T) {
	headers, err := ParseClaudeUsageHeaders([]byte(`{"five_hour":{"utilization":0.25,"resets_at":"2026-10-02T12:00:00Z","status":"allowed"},"seven_day":{"utilization":"0.5","resets_at":1787695200}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-5h-Utilization"); got != "0.0025" {
		t.Fatalf("five hour utilization = %q", got)
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-weekly-Reset"); got != "1787695200" {
		t.Fatalf("seven day reset = %q", got)
	}
}

func TestParseClaudeUsageHeadersRoundTripsQuotaObservation(t *testing.T) {
	headers, err := ParseClaudeUsageHeaders([]byte(`{"five_hour":{"utilization":25,"resets_at":"2026-10-02T12:00:00Z"},"seven_day_opus":{"utilization":60,"resets_at":1787695200}}`))
	if err != nil {
		t.Fatal(err)
	}
	var quota coreauth.QuotaState
	observedAt := time.Unix(1787279282, 0).UTC()
	if !quota.ObserveResponseHeadersForProvider("claude", headers, observedAt) {
		t.Fatal("quota observation was not recorded")
	}
	if len(quota.Windows) != 2 {
		t.Fatalf("windows=%d, want 2", len(quota.Windows))
	}
	for _, window := range quota.Windows {
		if window.RemainingPercent != 75 && window.RemainingPercent != 40 {
			t.Fatalf("unexpected remaining percent: %v", window.RemainingPercent)
		}
		if window.ResetAt.IsZero() {
			t.Fatal("reset timestamp missing")
		}
	}
}

func TestParseClaudeUsageHeadersRejectsIncompleteWindows(t *testing.T) {
	for _, payload := range []string{
		`{"five_hour":{"utilization":0.2}}`,
		`{"five_hour":{"utilization":101,"resets_at":"2026-10-02T12:00:00Z"}}`,
		`{"five_hour":{"resets_at":"2026-10-02T12:00:00Z"}}`,
		`{"unknown":{"utilization":0.2,"resets_at":"2026-10-02T12:00:00Z"}}`,
	} {
		if _, err := ParseClaudeUsageHeaders([]byte(payload)); err == nil {
			t.Fatalf("payload accepted: %s", payload)
		}
	}
}

func TestParseClaudeUsageHeadersRejectsTrailingJSON(t *testing.T) {
	if _, err := ParseClaudeUsageHeaders([]byte(`{"five_hour":{"utilization":0.2,"resets_at":"2026-10-02T12:00:00Z"}} {}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}
