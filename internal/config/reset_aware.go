package config

import (
	"fmt"
	"strings"
	"time"
)

const (
	DefaultResetAwareMinLongWindowRemainingPercent  = 10.0
	DefaultResetAwareMinShortWindowRemainingPercent = 5.0
	DefaultResetAwareTelemetryMaxAge                = 15 * time.Minute
)

// WithDefaults returns an effective reset-aware configuration while preserving
// the caller's original value. It is used by runtime wiring after YAML/v8
// decoding, so omitted settings do not need to be persisted into the config.
func (c ResetAwareRoutingConfig) WithDefaults() ResetAwareRoutingConfig {
	if c.PreserveSessionAffinity == nil {
		value := true
		c.PreserveSessionAffinity = &value
	}
	if c.LongestWindowFirst == nil {
		value := true
		c.LongestWindowFirst = &value
	}
	if c.UseExpiringCapacityFirst == nil {
		value := true
		c.UseExpiringCapacityFirst = &value
	}
	if c.MinLongWindowRemainingPercent == nil {
		value := DefaultResetAwareMinLongWindowRemainingPercent
		c.MinLongWindowRemainingPercent = &value
	}
	if c.MinShortWindowRemainingPercent == nil {
		value := DefaultResetAwareMinShortWindowRemainingPercent
		c.MinShortWindowRemainingPercent = &value
	}
	if c.ReservePolicy == "" {
		c.ReservePolicy = "last-resort"
	}
	if c.AutoUseManualResets == nil {
		value := false
		c.AutoUseManualResets = &value
	}
	if c.RefreshAfterReset == nil {
		value := true
		c.RefreshAfterReset = &value
	}
	if c.StaleTelemetryPolicy == "" {
		c.StaleTelemetryPolicy = "fallback"
	}
	if c.FallbackStrategy == "" {
		c.FallbackStrategy = "round-robin"
	}
	if c.TelemetryMaxAge == "" {
		c.TelemetryMaxAge = DefaultResetAwareTelemetryMaxAge.String()
	}
	return c
}

// ValidateResetAwareRouting validates settings that can affect safe selection.
func (c ResetAwareRoutingConfig) ValidateResetAwareRouting() error {
	effective := c.WithDefaults()
	if effective.MinLongWindowRemainingPercent == nil || effective.MinShortWindowRemainingPercent == nil {
		return fmt.Errorf("reset-aware capacity floors must be configured")
	}
	for name, value := range map[string]float64{
		"min-long-window-remaining-percent":  *effective.MinLongWindowRemainingPercent,
		"min-short-window-remaining-percent": *effective.MinShortWindowRemainingPercent,
	} {
		if value < 0 || value > 100 {
			return fmt.Errorf("%s must be between 0 and 100", name)
		}
	}
	if effective.AutoUseManualResets != nil && *effective.AutoUseManualResets {
		return fmt.Errorf("reset-aware auto-use-manual-resets must remain false")
	}
	if _, errParse := time.ParseDuration(strings.TrimSpace(effective.TelemetryMaxAge)); errParse != nil {
		return fmt.Errorf("reset-aware telemetry-max-age must be a valid duration: %w", errParse)
	}
	if parsed, errParse := time.ParseDuration(strings.TrimSpace(effective.TelemetryMaxAge)); errParse == nil && parsed <= 0 {
		return fmt.Errorf("reset-aware telemetry-max-age must be positive")
	}
	switch strings.ToLower(strings.TrimSpace(effective.ReservePolicy)) {
	case "last-resort", "normal-only":
	default:
		return fmt.Errorf("reset-aware reserve-policy must be last-resort or normal-only")
	}
	switch strings.ToLower(strings.TrimSpace(effective.StaleTelemetryPolicy)) {
	case "fallback", "exclude":
	default:
		return fmt.Errorf("reset-aware stale-telemetry-policy must be fallback or exclude")
	}
	switch strings.ToLower(strings.TrimSpace(effective.FallbackStrategy)) {
	case "round-robin", "weighted-round-robin", "fill-first":
	default:
		return fmt.Errorf("reset-aware fallback-strategy must be round-robin, weighted-round-robin, or fill-first")
	}
	return nil
}

// ValidateRouting validates routing settings that require cross-field checks.
func (c *Config) ValidateRouting() error {
	if c == nil {
		return nil
	}
	strategy := strings.ToLower(strings.TrimSpace(c.Routing.Strategy))
	if strategy != "reset-aware" && strategy != "resetaware" && strategy != "quota-reset-aware" {
		return nil
	}
	return c.Routing.ResetAware.ValidateResetAwareRouting()
}
