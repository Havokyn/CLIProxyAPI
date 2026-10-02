package helps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

const maxCodexUsageAdditionalLimits = 8
const maxCodexUsageWindowSeconds = 366 * 24 * 60 * 60
const maxCodexUsageResetAt = 4102444800 // 2100-01-01T00:00:00Z

var errInvalidCodexUsagePayload = errors.New("invalid Codex usage payload")

type codexUsageWindow struct {
	UsedPercent       codexUsageNumber `json:"used_percent"`
	LimitWindowSecond codexUsageNumber `json:"limit_window_seconds"`
	ResetAt           codexUsageNumber `json:"reset_at"`
	ResetAfterSeconds codexUsageNumber `json:"reset_after_seconds"`
}

type codexUsageNumber string

func (n *codexUsageNumber) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || (data[0] != '-' && (data[0] < '0' || data[0] > '9')) || !json.Valid(data) {
		return errInvalidCodexUsagePayload
	}
	*n = codexUsageNumber(data)
	return nil
}

type codexUsageRateLimit struct {
	Allowed      *bool             `json:"allowed"`
	LimitReached *bool             `json:"limit_reached"`
	Primary      *codexUsageWindow `json:"primary_window"`
	Secondary    *codexUsageWindow `json:"secondary_window"`
}

type codexUsageAdditionalLimit struct {
	LimitName       string               `json:"limit_name"`
	MeteredFeature  string               `json:"metered_feature"`
	NormalModelSlug string               `json:"normal_model_slug"`
	RateLimit       *codexUsageRateLimit `json:"rate_limit"`
}

type codexUsagePayload struct {
	RateLimit            *codexUsageRateLimit        `json:"rate_limit"`
	AdditionalRateLimits []codexUsageAdditionalLimit `json:"additional_rate_limits"`
}

// ParseCodexUsageHeaders converts an authoritative /wham/usage response into
// the bounded X-Codex-* signal format used by runtime quota observations.
func ParseCodexUsageHeaders(payload []byte) (http.Header, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var root json.RawMessage
	if err := decoder.Decode(&root); err != nil {
		return nil, errInvalidCodexUsagePayload
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errInvalidCodexUsagePayload
	}
	if len(root) == 0 || root[0] != '{' || bytes.Equal(bytes.TrimSpace(root), []byte("null")) {
		return nil, errInvalidCodexUsagePayload
	}
	var usage codexUsagePayload
	if err := json.Unmarshal(root, &usage); err != nil {
		return nil, errInvalidCodexUsagePayload
	}
	if len(usage.AdditionalRateLimits) > maxCodexUsageAdditionalLimits {
		return nil, errInvalidCodexUsagePayload
	}

	headers := make(http.Header)
	hasQuota := false
	if usage.RateLimit != nil {
		added, err := addCodexUsageRateLimitHeaders(headers, "X-Codex-", usage.RateLimit)
		if err != nil {
			return nil, err
		}
		hasQuota = added || hasQuota
	}
	for _, additional := range usage.AdditionalRateLimits {
		if additional.RateLimit == nil || !isCodexReserveOrFallbackLimit(additional) {
			continue
		}
		name := strings.TrimSpace(additional.LimitName)
		if name == "" {
			name = strings.TrimSpace(additional.MeteredFeature)
		}
		if name == "" {
			name = strings.TrimSpace(additional.NormalModelSlug)
		}
		if !validCodexQuotaEventText(name) {
			return nil, errInvalidCodexUsagePayload
		}
		identifier := normalizeCodexQuotaHeaderIdentifier(name)
		if identifier == "" {
			return nil, errInvalidCodexUsagePayload
		}
		prefix := codexQuotaAdditionalHeaderKey + identifier + "-"
		if !strings.Contains(strings.ToLower(identifier), "reserve") &&
			!strings.Contains(strings.ToLower(identifier), "fallback") &&
			!strings.Contains(strings.ToLower(identifier), "overage") {
			prefix = codexQuotaAdditionalHeaderKey + "Reserve-" + identifier + "-"
		}
		added, err := addCodexUsageRateLimitHeaders(headers, prefix, additional.RateLimit)
		if err != nil {
			return nil, err
		}
		if added {
			headers.Set(prefix+"Limit-Name", name)
			if slug := strings.TrimSpace(additional.NormalModelSlug); validCodexQuotaEventIdentifier(slug) {
				headers.Set(prefix+"Normal-Model-Slug", slug)
			}
			hasQuota = true
		}
	}
	if !hasQuota {
		return nil, fmt.Errorf("%w: no complete quota windows", errInvalidCodexUsagePayload)
	}
	return headers, nil
}

func addCodexUsageRateLimitHeaders(headers http.Header, prefix string, rate *codexUsageRateLimit) (bool, error) {
	if rate == nil {
		return false, nil
	}
	if rate.Allowed != nil {
		headers.Set(prefix+"Allowed", strconv.FormatBool(*rate.Allowed))
	}
	if rate.LimitReached != nil {
		headers.Set(prefix+"Limit-Reached", strconv.FormatBool(*rate.LimitReached))
	}
	added := false
	for _, item := range []struct {
		name   string
		window *codexUsageWindow
	}{{"Primary", rate.Primary}, {"Secondary", rate.Secondary}} {
		if item.window == nil {
			continue
		}
		used, okUsed := strictFiniteNumber(item.window.UsedPercent)
		seconds, okSeconds := strictInteger(item.window.LimitWindowSecond)
		resetAt, hasResetAt := strictInteger(item.window.ResetAt)
		resetAfter, hasResetAfter := strictInteger(item.window.ResetAfterSeconds)
		if item.window.UsedPercent == "" || item.window.LimitWindowSecond == "" ||
			(item.window.ResetAt == "" && item.window.ResetAfterSeconds == "") {
			return false, errInvalidCodexUsagePayload
		}
		if !okUsed || used < 0 || used > 100 || !okSeconds || seconds <= 0 || seconds%60 != 0 || seconds > maxCodexUsageWindowSeconds {
			return false, errInvalidCodexUsagePayload
		}
		if (item.window.ResetAt != "" && (!hasResetAt || resetAt <= 0 || resetAt > maxCodexUsageResetAt)) ||
			(item.window.ResetAfterSeconds != "" && (!hasResetAfter || resetAfter < 0 || resetAfter > maxCodexUsageWindowSeconds)) {
			return false, errInvalidCodexUsagePayload
		}
		if !hasResetAt && !hasResetAfter {
			continue
		}
		windowPrefix := prefix + item.name + "-"
		headers.Set(windowPrefix+"Used-Percent", strconv.FormatFloat(used, 'f', -1, 64))
		headers.Set(windowPrefix+"Window-Minutes", strconv.FormatInt(seconds/60, 10))
		if hasResetAt {
			headers.Set(windowPrefix+"Reset-At", strconv.FormatInt(resetAt, 10))
		}
		if hasResetAfter {
			headers.Set(windowPrefix+"Reset-After-Seconds", strconv.FormatInt(resetAfter, 10))
		}
		added = true
	}
	return added, nil
}

func strictFiniteNumber(value codexUsageNumber) (float64, bool) {
	if value == "" {
		return 0, false
	}
	number, err := strconv.ParseFloat(string(value), 64)
	return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
}

func strictInteger(value codexUsageNumber) (int64, bool) {
	if value == "" {
		return 0, false
	}
	integer, err := strconv.ParseInt(string(value), 10, 64)
	return integer, err == nil
}

func isCodexReserveOrFallbackLimit(limit codexUsageAdditionalLimit) bool {
	for _, raw := range []string{limit.LimitName, limit.MeteredFeature} {
		name := strings.ToLower(strings.TrimSpace(raw))
		for _, marker := range []string{"reserve", "fallback", "overage"} {
			if strings.Contains(name, marker) {
				return true
			}
		}
	}
	return false
}
