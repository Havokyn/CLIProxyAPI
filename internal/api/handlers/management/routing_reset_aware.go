package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// RefreshRoutingQuota performs a read-only provider usage probe and records its
// normalized observation. It never resets quota or changes credential state.
func (h *Handler) RefreshRoutingQuota(c *gin.Context) {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil || strings.TrimSpace(body.AuthIndex) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota refresh unavailable"})
		return
	}
	auth := h.authByIndex(body.AuthIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}
	if !strings.EqualFold(auth.Provider, "codex") {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "native quota refresh unsupported for provider"})
		return
	}
	updated, errRefresh := h.authManager.RefreshCredentialQuota(c.Request.Context(), auth)
	if errRefresh != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "authoritative quota refresh failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"auth_index": updated.Index, "quota_observation": quotaObservationPayload(updated.Quota)})
}

// GetResetAwareRouting exposes the central selector's nonsecret placement
// explanation. It is intentionally read-only: quota refresh and manual reset
// operations remain separate management actions.
//
// Optional query parameters narrow the explanation to one provider/model pool:
//   - provider=codex
//   - model=gpt-5-codex
//
// With neither parameter, one ranked list is returned per provider represented
// by the current credential snapshot. No token, key, raw auth metadata, or
// provider response body is included.
func (h *Handler) GetResetAwareRouting(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	manager := h.authManager
	h.mu.Unlock()

	strategy := ""
	policy := gin.H{}
	if cfg != nil {
		strategy = strings.TrimSpace(cfg.Routing.Strategy)
		effective := cfg.Routing.ResetAware.WithDefaults()
		boolValue := func(value *bool) bool { return value != nil && *value }
		floatValue := func(value *float64) float64 {
			if value == nil {
				return 0
			}
			return *value
		}
		policy = gin.H{
			"preserve_session_affinity":          boolValue(effective.PreserveSessionAffinity),
			"longest_window_first":               boolValue(effective.LongestWindowFirst),
			"use_expiring_capacity_first":        boolValue(effective.UseExpiringCapacityFirst),
			"min_long_window_remaining_percent":  floatValue(effective.MinLongWindowRemainingPercent),
			"min_short_window_remaining_percent": floatValue(effective.MinShortWindowRemainingPercent),
			"reserve_policy":                     effective.ReservePolicy,
			"auto_use_manual_resets":             boolValue(effective.AutoUseManualResets),
			"refresh_after_reset":                boolValue(effective.RefreshAfterReset),
			"stale_telemetry_policy":             effective.StaleTelemetryPolicy,
			"fallback_strategy":                  effective.FallbackStrategy,
			"telemetry_max_age":                  effective.TelemetryMaxAge,
		}
	}

	response := gin.H{
		"strategy":        strategy,
		"enabled":         false,
		"preview":         false,
		"provider":        strings.TrimSpace(c.Query("provider")),
		"model":           strings.TrimSpace(c.Query("model")),
		"observed_at":     time.Now().UTC(),
		"policy":          policy,
		"candidates":      []coreauth.ResetAwareRoutingCandidate{},
		"selection_scope": "provider/model",
	}
	if manager == nil {
		c.JSON(http.StatusOK, response)
		return
	}
	selector := coreauth.ResetAwareSelectorFrom(manager.Selector())
	if selector == nil && cfg != nil {
		// Keep the endpoint useful while an existing installation still uses a
		// legacy strategy. This is a read-only preview over the same credential
		// snapshot; it does not change routing or create reservations.
		effective := cfg.Routing.ResetAware.WithDefaults()
		boolValue := func(value *bool) bool { return value != nil && *value }
		floatValue := func(value *float64) float64 {
			if value == nil {
				return 0
			}
			return *value
		}
		telemetryMaxAge := coreauth.ResetAwareSelectorConfig{}.TelemetryMaxAge
		if parsed, errParse := time.ParseDuration(strings.TrimSpace(effective.TelemetryMaxAge)); errParse == nil && parsed > 0 {
			telemetryMaxAge = parsed
		}
		selector = coreauth.NewResetAwareSelector(coreauth.ResetAwareSelectorConfig{
			PreserveSessionAffinity:        boolValue(effective.PreserveSessionAffinity),
			LongestWindowFirst:             boolValue(effective.LongestWindowFirst),
			UseExpiringCapacityFirst:       boolValue(effective.UseExpiringCapacityFirst),
			MinLongWindowRemainingPercent:  floatValue(effective.MinLongWindowRemainingPercent),
			MinShortWindowRemainingPercent: floatValue(effective.MinShortWindowRemainingPercent),
			ReservePolicy:                  effective.ReservePolicy,
			AutoUseManualResets:            boolValue(effective.AutoUseManualResets),
			RefreshAfterReset:              boolValue(effective.RefreshAfterReset),
			StaleTelemetryPolicy:           effective.StaleTelemetryPolicy,
			FallbackStrategy:               effective.FallbackStrategy,
			TelemetryMaxAge:                telemetryMaxAge,
		})
		response["preview"] = true
	}
	if selector == nil {
		c.JSON(http.StatusOK, response)
		return
	}

	provider := strings.TrimSpace(c.Query("provider"))
	model := strings.TrimSpace(c.Query("model"))
	candidates := selector.Explain(provider, model, manager.List(), time.Now().UTC())
	response["enabled"] = !response["preview"].(bool)
	response["candidates"] = candidates
	c.JSON(http.StatusOK, response)
}
