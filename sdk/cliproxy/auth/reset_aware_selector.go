package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const resetAwareProviderOrderMetadataKey = "__cliproxy_reset_aware_provider_order"

// ResetAwareSelectorConfig contains the effective runtime policy for reset-aware
// cold-session placement. Configuration decoding lives in internal/config;
// this value type keeps the selector independent from YAML.
type ResetAwareSelectorConfig struct {
	PreserveSessionAffinity        bool
	LongestWindowFirst             bool
	UseExpiringCapacityFirst       bool
	MinLongWindowRemainingPercent  float64
	MinShortWindowRemainingPercent float64
	ReservePolicy                  string
	AutoUseManualResets            bool
	RefreshAfterReset              bool
	StaleTelemetryPolicy           string
	Fallback                       Selector
	FallbackStrategy               string
	TelemetryMaxAge                time.Duration
}

// ResetAwareQuotaRefreshFunc performs one authoritative quota refresh for a
// credential whose recorded reset has passed. It returns the refreshed runtime
// credential snapshot; returning nil/error keeps the configured stale-data
// fallback in control of the request.
type ResetAwareQuotaRefreshFunc func(context.Context, *Auth) (*Auth, error)

// ResetAwareSelector ranks credentials inside one provider/model pool for new
// selections. Its mutex is deliberately shared by every request using this
// selector, so fleet clients entering the same gateway cannot make concurrent
// cold-placement decisions from an uncoordinated snapshot.
type ResetAwareSelector struct {
	mu           sync.Mutex
	config       ResetAwareSelectorConfig
	fallback     Selector
	now          func() time.Time
	reservations map[string][]time.Time
	quotaRefresh ResetAwareQuotaRefreshFunc
	refreshCache map[string]resetAwareRefreshCache
}

type resetAwareRefreshCache struct {
	auth       *Auth
	refreshed  time.Time
	epoch      uint64
	generation uint64
	inFlight   bool
}

// ResetAwareRoutingCandidate is the nonsecret explanation exposed to management
// clients. It contains routing metadata only; it never contains auth material.
type ResetAwareRoutingCandidate struct {
	AuthID                        string        `json:"auth_id"`
	AuthIndex                     string        `json:"auth_index,omitempty"`
	Provider                      string        `json:"provider"`
	Model                         string        `json:"model,omitempty"`
	Eligible                      bool          `json:"eligible"`
	Selected                      bool          `json:"selected"`
	Rank                          int           `json:"rank,omitempty"`
	Normal                        bool          `json:"normal"`
	Reserve                       bool          `json:"reserve"`
	TelemetryFresh                bool          `json:"telemetry_fresh"`
	TelemetryState                string        `json:"telemetry_state"`
	ObservedAt                    time.Time     `json:"quota_observed_at,omitempty"`
	Windows                       []QuotaWindow `json:"quota_windows,omitempty"`
	LongestWindowName             string        `json:"longest_window_name,omitempty"`
	LongestWindowRemainingPercent *float64      `json:"longest_window_remaining_percent,omitempty"`
	LongestWindowResetAt          *time.Time    `json:"longest_window_reset_at,omitempty"`
	ShortWindowName               string        `json:"short_window_name,omitempty"`
	ShortWindowRemainingPercent   *float64      `json:"short_window_remaining_percent,omitempty"`
	ShortWindowResetAt            *time.Time    `json:"short_window_reset_at,omitempty"`
	SafetyFloor                   string        `json:"safety_floor,omitempty"`
	Cooldown                      bool          `json:"cooldown"`
	CooldownUntil                 *time.Time    `json:"cooldown_until,omitempty"`
	ActiveReservationCount        int           `json:"active_reservation_count,omitempty"`
	Reason                        string        `json:"reason"`
}

// NewResetAwareSelector constructs a selector with safe defaults. A nil
// fallback preserves the existing round-robin behavior when telemetry cannot
// support an explainable reset-aware decision.
func NewResetAwareSelector(cfg ResetAwareSelectorConfig) *ResetAwareSelector {
	if cfg.MinLongWindowRemainingPercent < 0 || cfg.MinLongWindowRemainingPercent > 100 {
		cfg.MinLongWindowRemainingPercent = 10
	}
	if cfg.MinShortWindowRemainingPercent < 0 || cfg.MinShortWindowRemainingPercent > 100 {
		cfg.MinShortWindowRemainingPercent = 5
	}
	if cfg.ReservePolicy == "" {
		cfg.ReservePolicy = "last-resort"
	}
	if cfg.StaleTelemetryPolicy == "" {
		cfg.StaleTelemetryPolicy = "fallback"
	}
	if cfg.TelemetryMaxAge <= 0 {
		cfg.TelemetryMaxAge = 15 * time.Minute
	}
	if cfg.Fallback == nil {
		switch strings.ToLower(strings.TrimSpace(cfg.FallbackStrategy)) {
		case "weighted-round-robin", "weightedroundrobin", "wrr":
			cfg.Fallback = &WeightedRoundRobinSelector{}
		case "fill-first", "fillfirst", "ff":
			cfg.Fallback = &FillFirstSelector{}
		default:
			cfg.Fallback = &RoundRobinSelector{}
		}
	}
	return &ResetAwareSelector{
		config:       cfg,
		fallback:     cfg.Fallback,
		now:          time.Now,
		reservations: make(map[string][]time.Time),
		refreshCache: make(map[string]resetAwareRefreshCache),
	}
}

// SetQuotaRefreshFunc attaches the host's authoritative quota refresh path.
// The selector remains usable without one; in that case reset-due telemetry is
// handled by the configured stale-telemetry policy.
func (s *ResetAwareSelector) SetQuotaRefreshFunc(refresh ResetAwareQuotaRefreshFunc) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.quotaRefresh = refresh
	if refresh == nil {
		s.refreshCache = make(map[string]resetAwareRefreshCache)
	}
	s.mu.Unlock()
}

// UsesAllPriorityTiers tells session affinity that reset-aware capacity is
// ordered before the legacy priority tie-breaker.
func (s *ResetAwareSelector) UsesAllPriorityTiers() bool { return s != nil }

// AffinityUsable checks capacity, not cold-placement ranking or safety floors.
// A usable continuation stays sticky even when another credential ranks better.
func (s *ResetAwareSelector) AffinityUsable(provider, model string, auth *Auth) bool {
	if s == nil || auth == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	assessment := s.assessLocked(canonicalSchedulingProvider(auth.Provider), model, auth, s.nowTime())
	return assessment != nil && !assessment.blocked
}

// ResetAwareSelectorFrom unwraps the optional session-affinity wrapper and
// returns the central reset-aware selector when it is active. Management
// handlers use this narrow view to expose diagnostics without knowing the
// wrapper's internal binding implementation.
func ResetAwareSelectorFrom(selector Selector) *ResetAwareSelector {
	if selector == nil {
		return nil
	}
	if resetAware, ok := selector.(*ResetAwareSelector); ok {
		return resetAware
	}
	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity != nil {
		return ResetAwareSelectorFrom(affinity.fallback)
	}
	return nil
}

// Config returns the immutable effective policy used by the selector.
func (s *ResetAwareSelector) Config() ResetAwareSelectorConfig {
	if s == nil {
		return ResetAwareSelectorConfig{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

// Pick selects a credential for a new/cold session. Existing session affinity
// is handled by SessionAffinitySelector before this method is reached.
func (s *ResetAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if s == nil {
		return nil, &Error{Code: "auth_not_found", Message: "reset-aware selector is unavailable"}
	}
	refreshedAttempt := false
	for {
		s.mu.Lock()
		now := s.nowTime()
		groups := s.groupCandidates(provider, model, auths, now, false)
		if len(groups) == 0 {
			s.mu.Unlock()
			return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		refresh := s.quotaRefresh
		refreshAfterReset := s.config.RefreshAfterReset
		due := s.resetDueCandidatesLocked(groups, model, now)
		if !refreshedAttempt && refreshAfterReset && refresh != nil && len(due) > 0 {
			s.mu.Unlock()
			refreshed := s.refreshDue(ctx, due, refresh)
			if len(refreshed) > 0 {
				auths = replaceRefreshedAuths(auths, refreshed)
			}
			refreshedAttempt = true
			continue
		}
		selected, errPick := s.pickLocked(ctx, provider, model, opts, groups, now)
		s.mu.Unlock()
		return selected, errPick
	}
}

func (s *ResetAwareSelector) pickLocked(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, groups map[string][]*Auth, now time.Time) (*Auth, error) {

	// A mixed route must choose a provider pool first. Quota values are never
	// compared across providers; provider order comes from the route, with a
	// deterministic lexical fallback for direct selector callers.
	providerKeys := s.providerOrder(provider, opts, groups)
	for _, providerKey := range providerKeys {
		candidates := groups[providerKey]
		plan := s.planLocked(providerKey, model, candidates, now)
		if plan.err != nil {
			continue
		}
		selected := plan.selected
		if plan.useFallback {
			selected = s.pickFallbackLocked(ctx, providerKey, model, opts, candidates)
		}
		if selected == nil {
			continue
		}
		s.reserveLocked(selected.ID, now)
		return selected, nil
	}

	return nil, &Error{Code: "auth_unavailable", Message: "all reset-aware credentials are below their safety floor or unavailable"}
}

func (s *ResetAwareSelector) resetDueCandidatesLocked(groups map[string][]*Auth, model string, now time.Time) []*Auth {
	if len(groups) == 0 {
		return nil
	}
	due := make([]*Auth, 0)
	for provider, auths := range groups {
		for _, auth := range auths {
			assessment := s.assessLocked(provider, model, auth, now)
			if assessment != nil && !assessment.blocked && (assessment.telemetryState == "reset-due-refresh" || assessment.telemetryState == "missing" || assessment.telemetryState == "stale" || assessment.telemetryState == "missing-reset") {
				due = append(due, auth)
			}
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].ID < due[j].ID })
	return due
}

func (s *ResetAwareSelector) refreshDue(ctx context.Context, candidates []*Auth, refresh ResetAwareQuotaRefreshFunc) []*Auth {
	if len(candidates) == 0 || refresh == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := s.nowTime()
	refreshed := make([]*Auth, 0, len(candidates))
	attempts := 0
	for _, candidate := range candidates {
		if candidate == nil || strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		s.mu.Lock()
		cached := s.refreshCache[candidate.ID]
		if cached.epoch == candidate.RegistrationEpoch && cached.inFlight {
			s.mu.Unlock()
			continue
		}
		if cached.epoch == candidate.RegistrationEpoch && !cached.refreshed.IsZero() && now.Sub(cached.refreshed) < 30*time.Second && cached.generation >= candidate.Generation && (cached.auth == nil || !CredentialsChanged(cached.auth, candidate)) {
			s.mu.Unlock()
			if cached.auth != nil {
				refreshed = append(refreshed, cached.auth.Clone())
			}
			continue
		}
		if attempts >= 2 {
			s.mu.Unlock()
			break
		}
		s.refreshCache[candidate.ID] = resetAwareRefreshCache{epoch: candidate.RegistrationEpoch, generation: candidate.Generation, inFlight: true}
		s.mu.Unlock()
		attempts++
		updated, errRefresh := refresh(ctx, candidate.Clone())
		s.mu.Lock()
		if s.refreshCache == nil {
			s.refreshCache = make(map[string]resetAwareRefreshCache)
		}
		s.refreshCache[candidate.ID] = resetAwareRefreshCache{refreshed: now, epoch: candidate.RegistrationEpoch, generation: candidate.Generation}
		if errors.Is(errRefresh, ErrQuotaRefreshBusy) {
			delete(s.refreshCache, candidate.ID)
		}
		if errRefresh == nil && updated != nil && updated.ID == candidate.ID {
			s.refreshCache[candidate.ID] = resetAwareRefreshCache{auth: updated.Clone(), refreshed: now, epoch: updated.RegistrationEpoch, generation: updated.Generation}
		}
		s.mu.Unlock()
		if errRefresh == nil && updated != nil && updated.ID == candidate.ID {
			refreshed = append(refreshed, updated.Clone())
		}
	}
	return refreshed
}

func replaceRefreshedAuths(auths, refreshed []*Auth) []*Auth {
	if len(refreshed) == 0 {
		return auths
	}
	byID := make(map[string]*Auth, len(refreshed))
	for _, auth := range refreshed {
		if auth != nil && auth.ID != "" {
			byID[auth.ID] = auth
		}
	}
	if len(byID) == 0 {
		return auths
	}
	result := append([]*Auth(nil), auths...)
	for index, auth := range result {
		if auth == nil {
			continue
		}
		if updated := byID[auth.ID]; updated != nil {
			result[index] = updated
		}
	}
	return result
}

// OnResult releases one cold-placement reservation after the existing manager
// records a request result. Reservations also expire automatically so a caller
// that disappears before reporting a result cannot pin a credential forever.
func (s *ResetAwareSelector) OnResult(result Result) {
	if s == nil || strings.TrimSpace(result.AuthID) == "" {
		return
	}
	s.ReleaseReservation(result.AuthID)
}

// ReleaseReservation releases one cold-placement reservation without recording
// an execution result or changing any credential or affinity state.
func (s *ResetAwareSelector) ReleaseReservation(authID string) {
	if s == nil || strings.TrimSpace(authID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	authID = strings.TrimSpace(authID)
	reservations := s.reservations[authID]
	if len(reservations) == 0 {
		return
	}
	if len(reservations) == 1 {
		delete(s.reservations, authID)
		return
	}
	s.reservations[authID] = reservations[1:]
}

// Explain returns the current nonsecret ranking for a provider/model pool.
// It shares the same assessment code as Pick but does not create a reservation.
func (s *ResetAwareSelector) Explain(provider, model string, auths []*Auth, now time.Time) []ResetAwareRoutingCandidate {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.IsZero() {
		now = s.nowTime()
	}
	groups := s.groupCandidates(provider, model, auths, now, true)
	providerKeys := make([]string, 0, len(groups))
	for key := range groups {
		providerKeys = append(providerKeys, key)
	}
	sort.Strings(providerKeys)
	result := make([]ResetAwareRoutingCandidate, 0, len(auths))
	for _, providerKey := range providerKeys {
		plan := s.planLocked(providerKey, model, groups[providerKey], now)
		result = append(result, s.explanationsLocked(providerKey, model, plan, now)...)
	}
	return result
}

type resetAwarePlan struct {
	assessments []*resetAwareAssessment
	ordered     []*resetAwareAssessment
	selected    *Auth
	useFallback bool
	fallbackWhy string
	err         error
}

type resetAwareAssessment struct {
	auth            *Auth
	provider        string
	model           string
	telemetryFresh  bool
	telemetryState  string
	long            *QuotaWindow
	short           *QuotaWindow
	normalEligible  bool
	reserveEligible bool
	normal          bool
	reserve         bool
	blocked         bool
	safetyFloor     string
	reason          string
	cooldown        bool
	cooldownUntil   time.Time
	reservations    int
}

func (s *ResetAwareSelector) nowTime() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *ResetAwareSelector) groupCandidates(provider, model string, auths []*Auth, now time.Time, includeBlocked bool) map[string][]*Auth {
	groups := make(map[string][]*Auth)
	requestedProvider := canonicalSchedulingProvider(provider)
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		providerKey := canonicalSchedulingProvider(executorKeyFromAuth(auth))
		if providerKey == "" {
			providerKey = canonicalSchedulingProvider(auth.Provider)
		}
		if providerKey == "" || (requestedProvider != "" && requestedProvider != "mixed" && providerKey != requestedProvider) {
			continue
		}
		if blocked, _, _ := isAuthBlockedForModel(auth, canonicalModelKey(model), now); blocked && !includeBlocked {
			continue
		}
		if !authMatchesQuotaModel(auth, model) {
			continue
		}
		groups[providerKey] = append(groups[providerKey], auth)
	}
	for providerKey := range groups {
		sort.Slice(groups[providerKey], func(i, j int) bool { return groups[providerKey][i].ID < groups[providerKey][j].ID })
	}
	return groups
}

func (s *ResetAwareSelector) providerOrder(provider string, opts cliproxyexecutor.Options, groups map[string][]*Auth) []string {
	ordered := make([]string, 0, len(groups))
	seen := make(map[string]struct{}, len(groups))
	if opts.Metadata != nil {
		if raw, ok := opts.Metadata[resetAwareProviderOrderMetadataKey].([]string); ok {
			for _, value := range raw {
				key := canonicalSchedulingProvider(value)
				if _, exists := groups[key]; exists {
					if _, added := seen[key]; !added {
						seen[key] = struct{}{}
						ordered = append(ordered, key)
					}
				}
			}
		}
	}
	if requested := canonicalSchedulingProvider(provider); requested != "" && requested != "mixed" {
		if _, exists := groups[requested]; exists {
			return []string{requested}
		}
	}
	remaining := make([]string, 0, len(groups))
	for key := range groups {
		if _, added := seen[key]; !added {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	return append(ordered, remaining...)
}

func (s *ResetAwareSelector) planLocked(provider, model string, auths []*Auth, now time.Time) resetAwarePlan {
	plan := resetAwarePlan{assessments: make([]*resetAwareAssessment, 0, len(auths))}
	for _, auth := range auths {
		assessment := s.assessLocked(provider, model, auth, now)
		if assessment != nil {
			plan.assessments = append(plan.assessments, assessment)
		}
	}
	if len(plan.assessments) == 0 {
		plan.err = &Error{Code: "auth_not_found", Message: "no auth available"}
		return plan
	}
	if len(plan.assessments) == 1 {
		// A single credential needs no ranking, but still obeys the configured
		// telemetry policy before it can be used directly.
		candidate := plan.assessments[0]
		candidate.normal = candidate.normalEligible
		candidate.reserve = candidate.reserveEligible && !candidate.normalEligible
		allowTelemetryFallback := !candidate.telemetryFresh && candidate.telemetryState != "reset-due-refresh" && strings.EqualFold(s.config.StaleTelemetryPolicy, "fallback")
		if !candidate.blocked && (candidate.normalEligible || candidate.reserveEligible || allowTelemetryFallback) {
			plan.selected = candidate.auth
			return plan
		}
	}
	for _, assessment := range plan.assessments {
		if assessment.blocked {
			continue
		}
		if !assessment.telemetryFresh && strings.EqualFold(s.config.StaleTelemetryPolicy, "fallback") {
			plan.useFallback = true
			plan.fallbackWhy = assessment.telemetryState
			plan.selected = nil
			return plan
		}
	}
	eligible := make([]*resetAwareAssessment, 0, len(plan.assessments))
	for _, assessment := range plan.assessments {
		if assessment.blocked {
			continue
		}
		if assessment.normalEligible {
			assessment.normal = true
			eligible = append(eligible, assessment)
		}
	}
	if len(eligible) == 0 && strings.EqualFold(s.config.ReservePolicy, "last-resort") {
		for _, assessment := range plan.assessments {
			if assessment.blocked {
				continue
			}
			if assessment.reserveEligible {
				assessment.reserve = true
				eligible = append(eligible, assessment)
			}
		}
	}
	if len(eligible) == 0 {
		plan.err = &Error{Code: "auth_unavailable", Message: "all reset-aware credentials are below their safety floor or exhausted"}
		return plan
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return resetAwareAssessmentLess(eligible[i], eligible[j], s.config)
	})
	plan.ordered = eligible
	available := s.unreservedAssessmentsLocked(eligible, now)
	if len(available) > 0 {
		plan.ordered = available
	}
	plan.selected = plan.ordered[0].auth
	return plan
}

func (s *ResetAwareSelector) assessLocked(provider, model string, auth *Auth, now time.Time) *resetAwareAssessment {
	if auth == nil {
		return nil
	}
	assessment := &resetAwareAssessment{auth: auth, provider: provider, model: model}
	if blocked, reason, next := isAuthBlockedForModel(auth, canonicalModelKey(model), now); blocked {
		assessment.blocked = true
		assessment.cooldown = reason == blockReasonCooldown
		assessment.cooldownUntil = next
		assessment.telemetryState = "unavailable"
		if assessment.cooldown {
			assessment.safetyFloor = "cooldown"
		}
	}
	windows, observedAt := quotaWindowsForRequest(auth, model)
	if len(windows) == 0 {
		assessment.telemetryState = "missing"
		return assessment
	}
	applicable := make([]QuotaWindow, 0, len(windows))
	manualResetCount := 0
	for _, window := range windows {
		if window.Provider != "" && !strings.EqualFold(strings.TrimSpace(window.Provider), strings.TrimSpace(provider)) {
			continue
		}
		if window.Model != "" && canonicalModelKey(window.Model) != canonicalModelKey(model) {
			continue
		}
		if window.ManualReset {
			manualResetCount++
			continue
		}
		applicable = append(applicable, window)
	}
	if len(applicable) == 0 {
		if manualResetCount > 0 {
			assessment.blocked = true
			assessment.telemetryState = "manual-reset-only"
			assessment.safetyFloor = "manual-reset-protected"
			return assessment
		}
		assessment.telemetryState = "missing-model-window"
		return assessment
	}
	// An exhausted window remains authoritative until its reset. Aging other
	// telemetry must not turn known exhaustion into round-robin eligibility.
	if reason := exhaustedQuotaWindowReason(applicable, now, s.config.ReservePolicy); reason != "" {
		assessment.blocked = true
		assessment.safetyFloor = reason
	}
	if observedAt.IsZero() {
		for _, window := range applicable {
			if window.ObservedAt.After(observedAt) {
				observedAt = window.ObservedAt
			}
		}
	}
	markStale := func() {
		assessment.telemetryState = "stale"
		// Stale reserve data cannot authorize spending reserve ahead of a
		// healthy normal account when normal capacity is known exhausted.
		if reason := exhaustedQuotaWindowReason(applicable, now, "normal-only"); reason != "" {
			assessment.blocked = true
			assessment.safetyFloor = reason
		}
	}
	if observedAt.IsZero() || now.Sub(observedAt) > s.config.TelemetryMaxAge {
		markStale()
		return assessment
	}
	for _, window := range applicable {
		if !window.ObservedAt.IsZero() && (window.ObservedAt.After(now.Add(time.Minute)) || now.Sub(window.ObservedAt) > s.config.TelemetryMaxAge) {
			markStale()
			return assessment
		}
		if inactiveClaudeShortWindow(window) {
			continue
		}
		if window.ResetAt.IsZero() {
			assessment.telemetryState = "missing-reset"
			return assessment
		}
		if !window.ResetAt.After(now) {
			assessment.telemetryState = "reset-due-refresh"
			return assessment
		}
	}
	assessment.telemetryFresh = true
	assessment.telemetryState = "fresh"
	normalWindows := make([]QuotaWindow, 0, len(applicable))
	reserveWindows := make([]QuotaWindow, 0, len(applicable))
	for _, window := range applicable {
		if inactiveClaudeShortWindow(window) {
			continue
		}
		if window.Exhausted || window.RemainingPercent <= 0 {
			if !window.Reserve {
				normalWindows = append(normalWindows, window)
			} else {
				reserveWindows = append(reserveWindows, window)
			}
			continue
		}
		if window.Reserve {
			reserveWindows = append(reserveWindows, window)
		} else {
			normalWindows = append(normalWindows, window)
		}
	}
	assessment.long = longestQuotaWindow(normalWindows, observedAt)
	assessment.short = shortestSecondaryQuotaWindow(normalWindows, assessment.long, observedAt)
	if assessment.blocked {
		return assessment
	}
	assessment.normalEligible, assessment.safetyFloor = quotaWindowSetEligible(assessment.long, assessment.short, s.config)
	if !assessment.normalEligible && assessment.safetyFloor == "" && assessment.long != nil {
		assessment.safetyFloor = "long-window-exhausted"
	}
	reserveLong := longestQuotaWindow(reserveWindows, observedAt)
	reserveShort := shortestSecondaryQuotaWindow(reserveWindows, reserveLong, observedAt)
	if assessment.long == nil {
		assessment.long = reserveLong
		assessment.short = reserveShort
	}
	assessment.reserveEligible, _ = quotaWindowSetEligible(reserveLong, reserveShort, s.config)
	if strings.EqualFold(strings.TrimSpace(s.config.ReservePolicy), "normal-only") {
		assessment.reserveEligible = false
	}
	if !assessment.normalEligible && assessment.reserveEligible {
		// A reserve selection should explain the reserve window that supplies
		// capacity, rather than an exhausted normal window.
		assessment.long = reserveLong
		assessment.short = reserveShort
	}
	if assessment.normalEligible {
		assessment.reason = "normal capacity above safety floor"
	} else if assessment.reserveEligible {
		assessment.reason = "normal capacity exhausted or protected; reserve held for last-resort routing"
	} else if assessment.safetyFloor == "" {
		assessment.safetyFloor = "no-usable-window"
	}
	assessment.reservations = s.activeReservationsLocked(auth.ID, now)
	return assessment
}

func quotaWindowSetEligible(long, short *QuotaWindow, cfg ResetAwareSelectorConfig) (bool, string) {
	if long == nil {
		return false, "no-long-window"
	}
	if long.Exhausted || long.RemainingPercent <= 0 {
		return false, "long-window-exhausted"
	}
	if long.RemainingPercent < cfg.MinLongWindowRemainingPercent {
		return false, fmt.Sprintf("long-window %.0f%% below %.0f%% floor", long.RemainingPercent, cfg.MinLongWindowRemainingPercent)
	}
	if short != nil {
		if short.Exhausted || short.RemainingPercent <= 0 {
			return false, "short-window-exhausted"
		}
		if short.RemainingPercent < cfg.MinShortWindowRemainingPercent {
			return false, fmt.Sprintf("short-window %.0f%% below %.0f%% floor", short.RemainingPercent, cfg.MinShortWindowRemainingPercent)
		}
	}
	return true, ""
}

func exhaustedQuotaWindowReason(windows []QuotaWindow, now time.Time, reservePolicy string) string {
	normalReason, reserveReason := "", ""
	hasReserve, hasNormal, reserveUsable := false, false, true
	for _, window := range windows {
		if window.ManualReset || inactiveClaudeShortWindow(window) {
			continue
		}
		if window.Reserve {
			hasReserve = true
			if window.Exhausted || window.RemainingPercent <= 0 || !window.ResetAt.After(now) {
				reserveUsable = false
			}
		} else {
			hasNormal = true
		}
		if (!window.Exhausted && window.RemainingPercent > 0) || !window.ResetAt.After(now) {
			continue
		}
		reason := "long-window-exhausted"
		if quotaWindowDuration(window, window.ObservedAt) < 24*time.Hour {
			reason = "short-window-exhausted"
		}
		if window.Reserve {
			reserveReason = reason
		} else {
			normalReason = reason
		}
	}
	if !hasNormal && hasReserve && reserveReason != "" {
		return reserveReason
	}
	if normalReason != "" && hasReserve && reserveUsable && strings.EqualFold(reservePolicy, "last-resort") {
		return ""
	}
	return normalReason
}

func inactiveClaudeShortWindow(window QuotaWindow) bool {
	return window.Inactive && strings.EqualFold(window.Provider, "claude") && window.Name == "5h" && window.Model == "" && !window.Reserve && !window.ManualReset && !window.Exhausted && window.RemainingPercent == 100 && window.ResetAt.IsZero()
}

func longestQuotaWindow(windows []QuotaWindow, observedAt time.Time) *QuotaWindow {
	if len(windows) == 0 {
		return nil
	}
	ordered := append([]QuotaWindow(nil), windows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left := quotaWindowDuration(ordered[i], observedAt)
		right := quotaWindowDuration(ordered[j], observedAt)
		if left != right {
			return left > right
		}
		return ordered[i].Name < ordered[j].Name
	})
	return &ordered[0]
}

func shortestSecondaryQuotaWindow(windows []QuotaWindow, long *QuotaWindow, observedAt time.Time) *QuotaWindow {
	if len(windows) < 2 {
		return nil
	}
	secondary := make([]QuotaWindow, 0, len(windows)-1)
	for _, window := range windows {
		if long != nil && window.Name == long.Name && window.ResetAt.Equal(long.ResetAt) && window.RemainingPercent == long.RemainingPercent {
			continue
		}
		secondary = append(secondary, window)
	}
	if len(secondary) == 0 {
		return nil
	}
	sort.SliceStable(secondary, func(i, j int) bool {
		left := quotaWindowDuration(secondary[i], observedAt)
		right := quotaWindowDuration(secondary[j], observedAt)
		if left != right {
			return left < right
		}
		return secondary[i].ResetAt.Before(secondary[j].ResetAt)
	})
	return &secondary[0]
}

func quotaWindowDuration(window QuotaWindow, observedAt time.Time) time.Duration {
	if window.DurationSeconds > 0 {
		return time.Duration(window.DurationSeconds) * time.Second
	}
	if !observedAt.IsZero() && window.ResetAt.After(observedAt) {
		return window.ResetAt.Sub(observedAt)
	}
	return 0
}

func resetAwareAssessmentLess(left, right *resetAwareAssessment, cfg ResetAwareSelectorConfig) bool {
	if left == nil || right == nil {
		return left != nil
	}
	if left.long != nil && right.long != nil {
		if cfg.LongestWindowFirst {
			if !left.long.ResetAt.Equal(right.long.ResetAt) {
				return left.long.ResetAt.Before(right.long.ResetAt)
			}
			if cfg.UseExpiringCapacityFirst {
				leftPressure := expirationPressure(*left.long)
				rightPressure := expirationPressure(*right.long)
				if leftPressure != rightPressure {
					return leftPressure > rightPressure
				}
			}
		}
		if left.long.RemainingPercent != right.long.RemainingPercent {
			return left.long.RemainingPercent < right.long.RemainingPercent
		}
	}
	if left.short != nil && right.short != nil && !left.short.ResetAt.Equal(right.short.ResetAt) {
		return left.short.ResetAt.Before(right.short.ResetAt)
	}
	if left.short != nil && right.short == nil {
		return true
	}
	if left.short == nil && right.short != nil {
		return false
	}
	if left.short != nil && right.short != nil && left.short.RemainingPercent != right.short.RemainingPercent {
		return left.short.RemainingPercent < right.short.RemainingPercent
	}
	leftPriority := authPriority(left.auth)
	rightPriority := authPriority(right.auth)
	if leftPriority != rightPriority {
		return leftPriority > rightPriority
	}
	leftWeight := authWeight(left.auth)
	rightWeight := authWeight(right.auth)
	if leftWeight != rightWeight {
		return leftWeight > rightWeight
	}
	return left.auth.ID < right.auth.ID
}

func expirationPressure(window QuotaWindow) float64 {
	seconds := float64(window.DurationSeconds)
	if seconds <= 0 {
		seconds = 1
	}
	return window.RemainingPercent / seconds
}

func (s *ResetAwareSelector) unreservedAssessmentsLocked(assessments []*resetAwareAssessment, now time.Time) []*resetAwareAssessment {
	available := make([]*resetAwareAssessment, 0, len(assessments))
	for _, assessment := range assessments {
		if assessment == nil || assessment.auth == nil {
			continue
		}
		active := s.activeReservationsLocked(assessment.auth.ID, now)
		assessment.reservations = active
		if active == 0 {
			available = append(available, assessment)
		}
	}
	return available
}

func (s *ResetAwareSelector) activeReservationsLocked(authID string, now time.Time) int {
	reservations := s.reservations[authID]
	if len(reservations) == 0 {
		return 0
	}
	cutoff := now.Add(-30 * time.Second)
	kept := reservations[:0]
	for _, expiresAt := range reservations {
		if expiresAt.After(cutoff) {
			kept = append(kept, expiresAt)
		}
	}
	if len(kept) == 0 {
		delete(s.reservations, authID)
		return 0
	}
	s.reservations[authID] = kept
	return len(kept)
}

func (s *ResetAwareSelector) reserveLocked(authID string, now time.Time) {
	if authID == "" {
		return
	}
	s.activeReservationsLocked(authID, now)
	s.reservations[authID] = append(s.reservations[authID], now.Add(30*time.Second))
}

func (s *ResetAwareSelector) pickFallbackLocked(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) *Auth {
	if s.fallback == nil {
		return nil
	}
	usable := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if assessment := s.assessLocked(provider, model, auth, s.nowTime()); assessment != nil && !assessment.blocked {
			usable = append(usable, auth)
		}
	}
	selected, errPick := s.fallback.Pick(ctx, provider, model, opts, usable)
	if errPick != nil {
		return nil
	}
	return selected
}

func (s *ResetAwareSelector) explanationsLocked(provider, model string, plan resetAwarePlan, now time.Time) []ResetAwareRoutingCandidate {
	if len(plan.assessments) == 0 {
		return nil
	}
	ordered := append([]*resetAwareAssessment(nil), plan.assessments...)
	if len(plan.ordered) > 0 {
		ordered = append([]*resetAwareAssessment(nil), plan.ordered...)
		seen := make(map[string]struct{}, len(ordered))
		for _, assessment := range ordered {
			seen[assessment.auth.ID] = struct{}{}
		}
		for _, assessment := range plan.assessments {
			if _, exists := seen[assessment.auth.ID]; !exists {
				ordered = append(ordered, assessment)
			}
		}
	} else {
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].auth.ID < ordered[j].auth.ID })
	}
	selectedID := ""
	if plan.selected != nil {
		selectedID = plan.selected.ID
	}
	result := make([]ResetAwareRoutingCandidate, 0, len(ordered))
	for index, assessment := range ordered {
		if assessment == nil || assessment.auth == nil {
			continue
		}
		candidate := resetAwareCandidateView(assessment, model, now)
		candidate.Rank = index + 1
		candidate.Selected = assessment.auth.ID == selectedID
		if plan.useFallback {
			candidate.Eligible = !assessment.blocked
			if assessment.blocked {
				if assessment.safetyFloor != "" {
					candidate.Reason = "protected: " + assessment.safetyFloor
				} else {
					candidate.Reason = "not eligible: credential is unavailable or in cooldown"
				}
			} else {
				candidate.Reason = "fallback " + strings.ToLower(strings.TrimSpace(s.config.FallbackStrategy)) + ": quota telemetry is " + plan.fallbackWhy
			}
		} else if candidate.Selected {
			candidate.Reason = selectedReason(assessment, s.config)
		} else if assessment.blocked {
			if assessment.safetyFloor != "" {
				candidate.Reason = "protected: " + assessment.safetyFloor
			} else {
				candidate.Reason = "not eligible: credential is unavailable or in cooldown"
			}
		} else if !assessment.telemetryFresh {
			candidate.Reason = "not selected: quota telemetry is " + assessment.telemetryState
		} else if assessment.safetyFloor != "" && !assessment.normalEligible && !assessment.reserveEligible {
			candidate.Reason = "protected: " + assessment.safetyFloor
		} else if assessment.reserveEligible && !assessment.normalEligible {
			candidate.Reason = "reserve held for last-resort routing while normal capacity is available"
		} else if len(plan.ordered) > 0 && plan.ordered[0] != assessment {
			candidate.Reason = fmt.Sprintf("not selected: ranked credential %q has an earlier applicable reset", plan.ordered[0].auth.ID)
		}
		result = append(result, candidate)
	}
	return result
}

func resetAwareCandidateView(assessment *resetAwareAssessment, model string, now time.Time) ResetAwareRoutingCandidate {
	view := ResetAwareRoutingCandidate{
		AuthID:                 assessment.auth.ID,
		AuthIndex:              assessment.auth.Index,
		Provider:               assessment.provider,
		Model:                  model,
		Eligible:               !assessment.blocked && (assessment.normalEligible || assessment.reserveEligible || !assessment.telemetryFresh),
		Normal:                 assessment.normalEligible,
		Reserve:                assessment.reserveEligible && !assessment.normalEligible,
		TelemetryFresh:         assessment.telemetryFresh,
		TelemetryState:         assessment.telemetryState,
		ObservedAt:             assessment.auth.Quota.ObservedAt,
		Windows:                cloneQuotaWindows(assessment.auth.Quota.Windows),
		SafetyFloor:            assessment.safetyFloor,
		Cooldown:               assessment.cooldown,
		ActiveReservationCount: assessment.reservations,
	}
	if !assessment.cooldownUntil.IsZero() {
		cooldownUntil := assessment.cooldownUntil
		view.CooldownUntil = &cooldownUntil
	}
	if assessment.long != nil {
		view.LongestWindowName = assessment.long.Name
		remaining := assessment.long.RemainingPercent
		view.LongestWindowRemainingPercent = &remaining
		resetAt := assessment.long.ResetAt
		view.LongestWindowResetAt = &resetAt
	}
	if assessment.short != nil {
		view.ShortWindowName = assessment.short.Name
		remaining := assessment.short.RemainingPercent
		view.ShortWindowRemainingPercent = &remaining
		resetAt := assessment.short.ResetAt
		view.ShortWindowResetAt = &resetAt
	}
	_ = now
	return view
}

func selectedReason(assessment *resetAwareAssessment, cfg ResetAwareSelectorConfig) string {
	if assessment.reserve {
		return "selected because normal capacity is unavailable; reserve capacity is used as last resort"
	}
	if assessment.long == nil {
		return "selected directly because this is the only eligible credential"
	}
	return fmt.Sprintf("selected because longest normal quota window resets first at %s; %.0f%% remaining; above %.0f%% new-session floor", assessment.long.ResetAt.UTC().Format(time.RFC3339), assessment.long.RemainingPercent, cfg.MinLongWindowRemainingPercent)
}

func quotaWindowsForRequest(auth *Auth, model string) ([]QuotaWindow, time.Time) {
	if auth == nil {
		return nil, time.Time{}
	}
	windows := make([]QuotaWindow, 0, len(auth.Quota.Windows)+4)
	observedAt := auth.Quota.ObservedAt
	appendQuota := func(quota QuotaState) {
		candidate := quota.Windows
		if len(candidate) == 0 {
			candidate = quotaWindowsFromSignals(auth.Provider, quota.Signals, quota.ObservedAt)
		}
		if quota.ObservedAt.After(observedAt) {
			observedAt = quota.ObservedAt
		}
		for _, window := range candidate {
			if window.Provider == "" {
				window.Provider = auth.Provider
			}
			if window.ObservedAt.IsZero() {
				window.ObservedAt = quota.ObservedAt
			}
			windows = append(windows, window)
		}
	}
	appendQuota(auth.Quota)
	modelKey := canonicalModelKey(model)
	for stateModel, state := range auth.ModelStates {
		if state == nil || canonicalModelKey(stateModel) != modelKey {
			continue
		}
		appendQuota(state.Quota)
	}
	deduped := make([]QuotaWindow, 0, len(windows))
	seen := make(map[string]int, len(windows))
	for _, window := range windows {
		name := window.Name
		if strings.EqualFold(window.Provider, "claude") {
			// The usage probe calls the shared 7d window "weekly"; passive
			// inference headers call it "7d". They are the same observation.
			switch strings.ToLower(name) {
			case "weekly":
				name = "7d"
			}
		}
		key := strings.Join([]string{name, window.Model, window.Provider}, "|")
		if index, exists := seen[key]; exists {
			if window.ObservedAt.After(deduped[index].ObservedAt) {
				deduped[index] = window
			}
			continue
		}
		seen[key] = len(deduped)
		deduped = append(deduped, window)
	}
	return deduped, observedAt
}

func authMatchesQuotaModel(auth *Auth, model string) bool {
	if auth == nil || strings.TrimSpace(model) == "" {
		return true
	}
	windows, _ := quotaWindowsForRequest(auth, model)
	hasModelSpecific := false
	for _, window := range windows {
		if strings.TrimSpace(window.Model) == "" {
			if !window.Reserve {
				return true
			}
			continue
		}
		hasModelSpecific = true
		if canonicalModelKey(window.Model) == canonicalModelKey(model) {
			return true
		}
	}
	return !hasModelSpecific
}
