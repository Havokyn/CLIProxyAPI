package helps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var errInvalidClaudeUsagePayload = fmt.Errorf("invalid Claude usage payload")

var claudeUsageWindows = map[string]string{
	"five_hour": "5h", "seven_day": "weekly", "seven_day_overage_included": "weekly-overage",
	"seven_day_opus": "7d-opus", "seven_day_sonnet": "7d-sonnet", "seven_day_cowork": "7d-cowork",
}

// ParseClaudeUsageHeaders converts the read-only Claude OAuth usage response
// into the bounded header shape consumed by QuotaState observation.
func ParseClaudeUsageHeaders(payload []byte) (http.Header, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var root map[string]json.RawMessage
	if err := decoder.Decode(&root); err != nil || root == nil {
		return nil, errInvalidClaudeUsagePayload
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errInvalidClaudeUsagePayload
	}
	headers := make(http.Header)
	for name, normalizedName := range claudeUsageWindows {
		raw, ok := root[name]
		if !ok {
			continue
		}
		var window map[string]json.RawMessage
		if json.Unmarshal(raw, &window) != nil || window == nil {
			continue
		}
		utilization, okUtil := claudeUsageNumber(window["utilization"])
		reset, okReset := claudeUsageReset(window["resets_at"])
		if !okUtil || !okReset || math.IsNaN(utilization) || math.IsInf(utilization, 0) || utilization < 0 || utilization > 100 || strings.TrimSpace(reset) == "" {
			continue
		}
		prefix := "Anthropic-Ratelimit-Unified-" + normalizedName + "-"
		headers.Set(prefix+"Utilization", strconv.FormatFloat(utilization/100, 'f', -1, 64))
		headers.Set(prefix+"Reset", reset)
		if status, ok := claudeUsageString(window["status"]); ok && status != "" {
			headers.Set(prefix+"Status", status)
		}
	}
	if len(headers) == 0 {
		return nil, fmt.Errorf("%w: no complete quota windows", errInvalidClaudeUsagePayload)
	}
	return headers, nil
}

func claudeUsageReset(raw json.RawMessage) (string, bool) {
	value, ok := claudeUsageString(raw)
	if !ok || value == "" {
		return "", false
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil && unix > 0 {
		return strconv.FormatInt(unix, 10), true
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(parsed.Unix(), 10), true
}

func claudeUsageNumber(raw json.RawMessage) (float64, bool) {
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		value, err := number.Float64()
		return value, err == nil
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	return value, err == nil
}

func claudeUsageString(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text), true
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String(), true
	}
	return "", false
}
