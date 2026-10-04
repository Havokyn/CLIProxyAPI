package auth

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaWindowsFromSignals normalizes the passive headers already stored in
// QuotaState. It intentionally returns no window when a provider omits either
// usable capacity or a reset timestamp; reset-aware routing must not infer an
// unlimited quota from incomplete telemetry.
func quotaWindowsFromSignals(provider string, signals map[string]string, observedAt time.Time) []QuotaWindow {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		return codexQuotaWindows(signals, observedAt)
	case "claude":
		return claudeQuotaWindows(signals, observedAt)
	default:
		return genericQuotaWindows(signals, observedAt)
	}
}

// QuotaWindowsFromNormalizedGroups converts the existing plugin quota contract
// into the same runtime representation used by passive response-header
// observations. The conversion is intentionally conservative: a bucket without
// a valid reset time is omitted so reset-aware routing never treats an unknown
// window as unlimited capacity.
func QuotaWindowsFromNormalizedGroups(provider string, groups []pluginapi.QuotaGroup, observedAt time.Time) []QuotaWindow {
	if len(groups) == 0 {
		return nil
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	provider = strings.TrimSpace(provider)
	windows := make([]QuotaWindow, 0, len(groups))
	for _, group := range groups {
		groupName := strings.TrimSpace(group.DisplayName)
		for _, bucket := range group.Buckets {
			name := strings.TrimSpace(bucket.Window)
			if name == "" {
				continue
			}
			resetAt := parseQuotaTimestamp(bucket.ResetTime)
			if resetAt.IsZero() {
				continue
			}
			remaining := bucket.RemainingFraction * 100
			// A few plugin implementations historically returned a percent
			// despite the normalized field being named Fraction. Accept that
			// bounded form while rejecting malformed/unbounded values.
			if remaining > 100 && bucket.RemainingFraction <= 100 {
				remaining = bucket.RemainingFraction
			}
			if math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < 0 || remaining > 100 {
				continue
			}
			reserve := quotaWindowNameIsReserve(groupName) || quotaWindowNameIsReserve(name)
			manual := quotaWindowNameIsManual(groupName) || quotaWindowNameIsManual(name)
			qualifiedName := name
			if groupName != "" && (reserve || manual) {
				qualifiedName = groupName + "/" + name
			}
			windows = append(windows, QuotaWindow{
				Name:             qualifiedName,
				RemainingPercent: remaining,
				ResetAt:          resetAt,
				DurationSeconds:  quotaWindowDurationSeconds(name),
				ObservedAt:       observedAt,
				Reserve:          reserve,
				ManualReset:      manual,
				Exhausted:        remaining <= 0,
				Provider:         provider,
			})
		}
	}
	return windows
}

func codexQuotaWindows(signals map[string]string, observedAt time.Time) []QuotaWindow {
	if len(signals) == 0 {
		return nil
	}
	normalized := make(map[string]string, len(signals))
	for key, value := range signals {
		normalized[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	windows := make([]QuotaWindow, 0, 4)
	seen := make(map[string]struct{})
	for key, rawUsed := range normalized {
		const marker = "-used-percent"
		idx := strings.Index(key, marker)
		if idx < 0 || !strings.HasPrefix(key, "x-codex-") {
			continue
		}
		prefix := strings.TrimSuffix(key[:idx], "-")
		windowName := ""
		for _, candidate := range []string{"primary", "secondary"} {
			if strings.HasSuffix(prefix, "-"+candidate) {
				windowName = candidate
				prefix = strings.TrimSuffix(prefix, "-"+candidate)
				break
			}
		}
		if windowName == "" {
			continue
		}
		used, errUsed := parseQuotaPercent(rawUsed)
		if errUsed != nil || used < 0 || used > 100 {
			continue
		}
		minutesRaw := normalized[prefix+"-"+windowName+"-window-minutes"]
		minutes, errMinutes := strconv.ParseInt(minutesRaw, 10, 64)
		if errMinutes != nil || minutes <= 0 {
			continue
		}
		resetAt := parseQuotaTimestamp(normalized[prefix+"-"+windowName+"-reset-at"])
		if resetAt.IsZero() {
			if resetAfter, errAfter := strconv.ParseInt(normalized[prefix+"-"+windowName+"-reset-after-seconds"], 10, 64); errAfter == nil && resetAfter >= 0 && !observedAt.IsZero() {
				resetAt = observedAt.Add(time.Duration(resetAfter) * time.Second)
			}
		}
		if resetAt.IsZero() {
			continue
		}
		group := strings.TrimPrefix(prefix, "x-codex")
		group = strings.TrimPrefix(group, "-")
		// Code-review and unrelated model-specific limits must not gate base
		// inference. Only explicitly identified reserve groups supplement it.
		if group != "" && !quotaWindowNameIsReserve(group) {
			continue
		}
		name := codexWindowName(group, minutes)
		identity := group + ":" + windowName
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		limitReached := strings.EqualFold(normalized[prefix+"-limit-reached"], "true")
		windows = append(windows, QuotaWindow{
			Name:             name,
			RemainingPercent: 100 - used,
			ResetAt:          resetAt,
			DurationSeconds:  minutes * 60,
			ObservedAt:       observedAt,
			Reserve:          quotaWindowNameIsReserve(group),
			ManualReset:      quotaWindowNameIsManual(group),
			Exhausted:        used >= 100 || limitReached,
			Provider:         "codex",
			Model:            normalized[prefix+"-normal-model-slug"],
		})
	}
	return windows
}

func codexWindowName(group string, minutes int64) string {
	horizon := quotaHorizonName(minutes)
	if group == "" || group == "primary" {
		return horizon
	}
	return group + "/" + horizon
}

func claudeQuotaWindows(signals map[string]string, observedAt time.Time) []QuotaWindow {
	if len(signals) == 0 {
		return nil
	}
	normalized := make(map[string]string, len(signals))
	for key, value := range signals {
		normalized[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	const prefix = "anthropic-ratelimit-unified-"
	windows := make([]QuotaWindow, 0, 4)
	seen := make(map[string]struct{})
	for key, rawUtilization := range normalized {
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, "-utilization") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "-utilization")
		if name == "" {
			continue
		}
		utilization, errUtilization := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(rawUtilization, "%")), 64)
		if errUtilization != nil {
			continue
		}
		if utilization > 1 && utilization <= 100 {
			utilization /= 100
		}
		if utilization < 0 || utilization > 1 {
			continue
		}
		resetAt := parseQuotaTimestamp(normalized[prefix+name+"-reset"])
		inactive := name == "5h" && utilization == 0 && normalized[prefix+name+"-status"] == "inactive" && normalized[prefix+name+"-reset"] == ""
		if resetAt.IsZero() && !inactive {
			continue
		}
		identity := name
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		status := strings.ToLower(normalized[prefix+name+"-status"])
		windows = append(windows, QuotaWindow{
			Name:             name,
			RemainingPercent: (1 - utilization) * 100,
			ResetAt:          resetAt,
			DurationSeconds:  claudeWindowDurationSeconds(name),
			ObservedAt:       observedAt,
			Reserve:          quotaWindowNameIsReserve(name),
			ManualReset:      quotaWindowNameIsManual(name),
			Exhausted:        utilization >= 1 || status == "rejected",
			Inactive:         inactive,
		})
	}
	return windows
}

// genericQuotaWindows handles provider records that expose explicit remaining
// percentages and reset timestamps without Codex or Anthropic's header shape.
// It is deliberately conservative: a percentage without its matching reset is
// missing telemetry rather than an unlimited credential.
func genericQuotaWindows(signals map[string]string, observedAt time.Time) []QuotaWindow {
	if len(signals) == 0 {
		return nil
	}
	normalized := make(map[string]string, len(signals))
	for key, value := range signals {
		normalized[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	windows := make([]QuotaWindow, 0, 4)
	seen := make(map[string]struct{})
	for key, rawRemaining := range normalized {
		name, ok := genericRemainingWindowName(key)
		if !ok {
			continue
		}
		remaining, errRemaining := parseQuotaPercent(rawRemaining)
		if errRemaining != nil || remaining < 0 || remaining > 100 {
			continue
		}
		resetKey := ""
		switch {
		case strings.HasSuffix(key, "_remaining_percent"):
			resetKey = strings.TrimSuffix(key, "_remaining_percent") + "_reset_at"
		case strings.HasSuffix(key, "-remaining-percent"):
			resetKey = strings.TrimSuffix(key, "-remaining-percent") + "-reset-at"
		}
		resetAt := parseQuotaTimestamp(normalized[resetKey])
		if resetAt.IsZero() {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		windows = append(windows, QuotaWindow{
			Name:             name,
			RemainingPercent: remaining,
			ResetAt:          resetAt,
			ObservedAt:       observedAt,
			Reserve:          quotaWindowNameIsReserve(name),
			ManualReset:      quotaWindowNameIsManual(name),
			Exhausted:        remaining <= 0,
		})
	}
	return windows
}

func genericRemainingWindowName(key string) (string, bool) {
	for _, suffix := range []string{"_quota_remaining_percent", "-quota-remaining-percent", "_remaining_percent", "-remaining-percent"} {
		if strings.HasSuffix(key, suffix) {
			name := strings.TrimSuffix(key, suffix)
			name = strings.TrimSuffix(name, "_quota")
			name = strings.TrimSuffix(name, "-quota")
			name = strings.Trim(name, "_-")
			if name != "" {
				return name, true
			}
		}
	}
	return "", false
}

func parseQuotaPercent(raw string) (float64, error) {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	return strconv.ParseFloat(raw, 64)
}

func parseQuotaTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if unix, errUnix := strconv.ParseInt(raw, 10, 64); errUnix == nil && unix > 0 {
		if unix > 100000000000 {
			return time.UnixMilli(unix).UTC()
		}
		return time.Unix(unix, 0).UTC()
	}
	if parsed, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
		return parsed.UTC()
	}
	if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
		return parsed.UTC()
	}
	return time.Time{}
}

func quotaHorizonName(minutes int64) string {
	switch minutes {
	case 60 * 5:
		return "5h"
	case 60 * 24:
		return "daily"
	case 60 * 24 * 7:
		return "weekly"
	default:
		return "window-" + strconv.FormatInt(minutes, 10) + "m"
	}
}

func claudeWindowDurationSeconds(name string) int64 {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "5h":
		return 5 * 60 * 60
	case "7d", "weekly":
		return 7 * 24 * 60 * 60
	case "daily", "1d":
		return 24 * 60 * 60
	default:
		return 0
	}
}

func quotaWindowDurationSeconds(name string) int64 {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.Contains(lower, "5h"), strings.Contains(lower, "5-hour"), strings.Contains(lower, "5_hour"):
		return 5 * 60 * 60
	case strings.Contains(lower, "7d"), strings.Contains(lower, "7-day"), strings.Contains(lower, "7_day"), strings.Contains(lower, "week"):
		return 7 * 24 * 60 * 60
	case strings.Contains(lower, "daily"), strings.Contains(lower, "1d"), strings.Contains(lower, "day"):
		return 24 * 60 * 60
	default:
		return 0
	}
}

func quotaWindowNameIsReserve(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(lower, "reserve") || strings.Contains(lower, "fallback") || strings.Contains(lower, "overage")
}

func quotaWindowNameIsManual(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(lower, "manual") || strings.Contains(lower, "reset-credit") || strings.Contains(lower, "reset_credit")
}

func cloneQuotaWindows(windows []QuotaWindow) []QuotaWindow {
	if len(windows) == 0 {
		return nil
	}
	return append([]QuotaWindow(nil), windows...)
}
