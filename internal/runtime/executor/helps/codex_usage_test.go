package helps

import (
	"testing"
)

func TestParseCodexUsageHeadersNormalWindows(t *testing.T) {
	headers, err := ParseCodexUsageHeaders([]byte(`{
		"rate_limit": {
			"allowed": true,
			"limit_reached": false,
			"primary_window": {"used_percent": 24, "limit_window_seconds": 18000, "reset_after_seconds": 1200},
			"secondary_window": {"used_percent": 61, "limit_window_seconds": 604800, "reset_at": 1785902974}
		}
	}`))
	if err != nil {
		t.Fatalf("ParseCodexUsageHeaders() error = %v", err)
	}
	for key, want := range map[string]string{
		"X-Codex-Allowed":                     "true",
		"X-Codex-Limit-Reached":               "false",
		"X-Codex-Primary-Used-Percent":        "24",
		"X-Codex-Primary-Window-Minutes":      "300",
		"X-Codex-Primary-Reset-After-Seconds": "1200",
		"X-Codex-Secondary-Used-Percent":      "61",
		"X-Codex-Secondary-Window-Minutes":    "10080",
		"X-Codex-Secondary-Reset-At":          "1785902974",
	} {
		if got := headers.Get(key); got != want {
			t.Errorf("header %s = %q, want %q", key, got, want)
		}
	}
}

func TestParseCodexUsageHeadersWeeklyExhaustedAndReserve(t *testing.T) {
	headers, err := ParseCodexUsageHeaders([]byte(`{
		"rate_limit": {
			"allowed": false,
			"limit_reached": true,
			"primary_window": null,
			"secondary_window": {"used_percent": 100, "limit_window_seconds": 604800, "reset_at": 1785902974}
		},
		"additional_rate_limits": [{
			"limit_name": "gpt-reserve",
			"metered_feature": "base_model_inference",
			"normal_model_slug": "gpt-5.6-luna",
			"rate_limit": {
				"allowed": true,
				"limit_reached": false,
				"primary_window": null,
				"secondary_window": {"used_percent": 10, "limit_window_seconds": 604800, "reset_after_seconds": 7200}
			}
		}, {
			"limit_name": "GPT-5.6-Luna",
			"metered_feature": "model-specific-capacity",
			"normal_model_slug": "gpt-5.6-luna",
			"rate_limit": {"secondary_window": {"used_percent": 1, "limit_window_seconds": 604800, "reset_after_seconds": 7200}}
		}]
	}`))
	if err != nil {
		t.Fatalf("ParseCodexUsageHeaders() error = %v", err)
	}
	for key, want := range map[string]string{
		"X-Codex-Allowed":                                         "false",
		"X-Codex-Limit-Reached":                                   "true",
		"X-Codex-Secondary-Used-Percent":                          "100",
		"X-Codex-Secondary-Window-Minutes":                        "10080",
		"X-Codex-Additional-Gpt-Reserve-Limit-Name":               "gpt-reserve",
		"X-Codex-Additional-Gpt-Reserve-Secondary-Used-Percent":   "10",
		"X-Codex-Additional-Gpt-Reserve-Secondary-Window-Minutes": "10080",
		"X-Codex-Additional-Gpt-Reserve-Normal-Model-Slug":        "gpt-5.6-luna",
	} {
		if got := headers.Get(key); got != want {
			t.Errorf("header %s = %q, want %q", key, got, want)
		}
	}
	if got := headers.Get("X-Codex-Additional-GPT-5.6-Luna-Secondary-Used-Percent"); got != "" {
		t.Errorf("model-specific additional limit was included: %q", got)
	}
}

func TestParseCodexUsageHeadersMalformedPayloads(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "invalid json", payload: `{"rate_limit":`},
		{name: "trailing value", payload: `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_after_seconds":1}}} {}`},
		{name: "scalar root", payload: `[]`},
		{name: "wrong allowed type", payload: `{"rate_limit":{"allowed":"yes"}}`},
		{name: "quoted used percent", payload: `{"rate_limit":{"primary_window":{"used_percent":"1","limit_window_seconds":18000,"reset_after_seconds":1}}}`},
		{name: "used percent out of range", payload: `{"rate_limit":{"primary_window":{"used_percent":101,"limit_window_seconds":18000,"reset_after_seconds":1}}}`},
		{name: "fractional duration", payload: `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000.5,"reset_after_seconds":1}}}`},
		{name: "duration over 366 days", payload: `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":31622401,"reset_after_seconds":1}}}`},
		{name: "reset after over 366 days", payload: `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_after_seconds":31622401}}}`},
		{name: "invalid reset", payload: `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":-1}}}`},
		{name: "reset after year 2100", payload: `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":4102444801}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseCodexUsageHeaders([]byte(tt.payload)); err == nil {
				t.Fatal("ParseCodexUsageHeaders() error = nil, want malformed-payload error")
			}
		})
	}
}

func TestParseCodexUsageHeadersRejectsAbsentAndPartialWindows(t *testing.T) {
	for _, payload := range []string{
		`{}`,
		`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}},"additional_rate_limits":[{"limit_name":"gpt-reserve","rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800}}}]}`,
		`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000},"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":10}}}`,
	} {
		if _, err := ParseCodexUsageHeaders([]byte(payload)); err == nil {
			t.Errorf("ParseCodexUsageHeaders(%s) error = nil, want no-complete-window error", payload)
		}
	}

	headers, err := ParseCodexUsageHeaders([]byte(`{"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":10}}}`))
	if err != nil {
		t.Fatalf("ParseCodexUsageHeaders() error = %v", err)
	}
	if headers.Get("X-Codex-Primary-Used-Percent") != "" || headers.Get("X-Codex-Secondary-Used-Percent") != "25" {
		t.Fatalf("null primary and complete secondary windows were not handled: %#v", headers)
	}
}
