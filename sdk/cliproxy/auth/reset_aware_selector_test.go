package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestResetAwareMissingAndStaleRefreshFailuresAreBackedOff(t *testing.T) {
	for _, state := range []string{"missing", "stale"} {
		t.Run(state, func(t *testing.T) {
			selector := resetAwareFixtureSelector(&RoundRobinSelector{})
			selector.config.RefreshAfterReset = true
			a := resetAwareFixtureAuth("a", "codex")
			if state == "stale" {
				a.Quota.Windows = []QuotaWindow{resetAwareFixtureWindow("weekly", 27, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour)}
				a.Quota.ObservedAt = resetAwareFixtureNow.Add(-2 * time.Hour)
			}
			b := resetAwareFixtureAuth("b", "codex", resetAwareFixtureWindow("weekly", 52, resetAwareFixtureNow.Add(2*time.Hour), 7*24*time.Hour))
			calls := 0
			selector.SetQuotaRefreshFunc(func(context.Context, *Auth) (*Auth, error) { calls++; return nil, errors.New("unavailable") })
			for i := 0; i < 10; i++ {
				_ = resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b})
			}
			if calls != 1 {
				t.Fatalf("failed refresh storm: calls=%d", calls)
			}
			selector.now = func() time.Time { return resetAwareFixtureNow.Add(31 * time.Second) }
			_ = resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b})
			if calls != 2 {
				t.Fatalf("failure did not retry: calls=%d", calls)
			}
		})
	}
}

var resetAwareFixtureNow = time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)

func TestResetAwareRefreshBoundsFirstPickAndDoesNotCacheBusy(t *testing.T) {
	selector := resetAwareFixtureSelector(nil)
	selector.config.RefreshAfterReset = true
	auths := make([]*Auth, 20)
	for i := range auths {
		auths[i] = resetAwareFixtureAuth(fmt.Sprintf("a%02d", i), "codex")
	}
	calls := 0
	selector.SetQuotaRefreshFunc(func(context.Context, *Auth) (*Auth, error) { calls++; return nil, ErrQuotaRefreshBusy })
	_ = resetAwarePickedID(t, selector, "codex", "sol", auths)
	if calls != 2 {
		t.Fatalf("unbounded first-pick fanout: %d", calls)
	}
	_ = resetAwarePickedID(t, selector, "codex", "sol", auths)
	if calls != 4 {
		t.Fatalf("busy cached as provider failure: %d", calls)
	}
}

func TestResetAwareRefreshReRegisteredCredentialDoesNotReuseFailureCache(t *testing.T) {
	selector := resetAwareFixtureSelector(nil)
	selector.config.RefreshAfterReset = true
	a := resetAwareFixtureAuth("a", "codex")
	b := resetAwareFixtureAuth("b", "codex", resetAwareFixtureWindow("weekly", 52, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour))
	calls := 0
	selector.SetQuotaRefreshFunc(func(context.Context, *Auth) (*Auth, error) { calls++; return nil, errors.New("unavailable") })
	_ = resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b})
	a.RegistrationEpoch++
	_ = resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b})
	if calls != 2 {
		t.Fatalf("new registration incorrectly suppressed: %d", calls)
	}
}

func TestResetAwareAuthoritativeWindowSupersedesOldModelObservation(t *testing.T) {
	selector := resetAwareFixtureSelector(nil)
	a := resetAwareFixtureAuth("a", "codex", resetAwareFixtureWindow("weekly", 27, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour))
	old := resetAwareFixtureWindow("weekly", 0, resetAwareFixtureNow.Add(-time.Minute), 7*24*time.Hour)
	old.ObservedAt = resetAwareFixtureNow.Add(-time.Hour)
	a.ModelStates = map[string]*ModelState{"sol": {Quota: QuotaState{ObservedAt: old.ObservedAt, Windows: []QuotaWindow{old}}}}
	b := resetAwareFixtureAuth("b", "codex", resetAwareFixtureWindow("weekly", 52, resetAwareFixtureNow.Add(2*time.Hour), 7*24*time.Hour))
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b}); got != "a" {
		t.Fatalf("stale model quota overrode authoritative snapshot: %s", got)
	}
}

func resetAwareFixtureSelector(fallback Selector) *ResetAwareSelector {
	selector := NewResetAwareSelector(ResetAwareSelectorConfig{
		LongestWindowFirst:             true,
		UseExpiringCapacityFirst:       true,
		MinLongWindowRemainingPercent:  10,
		MinShortWindowRemainingPercent: 5,
		ReservePolicy:                  "last-resort",
		StaleTelemetryPolicy:           "fallback",
		Fallback:                       fallback,
		TelemetryMaxAge:                time.Hour,
	})
	selector.now = func() time.Time { return resetAwareFixtureNow }
	return selector
}

func resetAwareFixtureAuth(id, provider string, windows ...QuotaWindow) *Auth {
	return &Auth{
		ID:       id,
		Provider: provider,
		Status:   StatusActive,
		Index:    "idx-" + id,
		Quota: QuotaState{
			ObservedAt: resetAwareFixtureNow,
			Windows:    windows,
		},
	}
}

func resetAwareFixtureWindow(name string, remaining float64, resetAt time.Time, duration time.Duration) QuotaWindow {
	return QuotaWindow{
		Name:             name,
		RemainingPercent: remaining,
		ResetAt:          resetAt,
		DurationSeconds:  int64(duration / time.Second),
		ObservedAt:       resetAwareFixtureNow,
	}
}

func resetAwareFixtureReserveWindow(name string, remaining float64, resetAt time.Time, duration time.Duration) QuotaWindow {
	window := resetAwareFixtureWindow(name, remaining, resetAt, duration)
	window.Reserve = true
	return window
}

func resetAwarePickedID(t *testing.T, selector *ResetAwareSelector, provider, model string, auths []*Auth) string {
	t.Helper()
	selected, errPick := selector.Pick(context.Background(), provider, model, cliproxyexecutor.Options{}, auths)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if selected == nil {
		t.Fatal("Pick() returned nil auth")
	}
	selector.OnResult(Result{AuthID: selected.ID})
	return selected.ID
}

func resetAwareExplanationIDs(explanations []ResetAwareRoutingCandidate) []string {
	ids := make([]string, 0, len(explanations))
	for _, explanation := range explanations {
		ids = append(ids, explanation.AuthID)
	}
	return ids
}

func TestResetAwareScreenshotFixtureOrdersExpiringNormalCapacityBeforeReserve(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	a := resetAwareFixtureAuth("fixture-a", "codex", resetAwareFixtureWindow("weekly", 27, resetAwareFixtureNow.Add(3*time.Hour+47*time.Minute), 7*24*time.Hour))
	b := resetAwareFixtureAuth("fixture-b", "codex",
		resetAwareFixtureWindow("weekly", 52, resetAwareFixtureNow.Add(11*time.Hour+52*time.Minute), 7*24*time.Hour),
		resetAwareFixtureWindow("5h", 92, resetAwareFixtureNow.Add(4*time.Hour), 5*time.Hour),
	)
	c := resetAwareFixtureAuth("fixture-c", "codex",
		resetAwareFixtureWindow("weekly", 0, resetAwareFixtureNow.Add(5*24*time.Hour+14*time.Hour+29*time.Minute), 7*24*time.Hour),
		resetAwareFixtureReserveWindow("gpt-reserve-weekly", 64, resetAwareFixtureNow.Add(6*24*time.Hour+4*time.Hour+46*time.Minute), 7*24*time.Hour),
	)

	explanations := selector.Explain("codex", "gpt-5-codex", []*Auth{c, b, a}, resetAwareFixtureNow)
	got := resetAwareExplanationIDs(explanations)
	want := []string{"fixture-a", "fixture-b", "fixture-c"}
	if len(got) != len(want) {
		t.Fatalf("explanation count = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank %d = %q, want %q; all=%#v", i+1, got[i], want[i], got)
		}
	}
	if !explanations[0].Selected || explanations[0].LongestWindowRemainingPercent == nil || *explanations[0].LongestWindowRemainingPercent != 27 {
		t.Fatalf("fixture A explanation = %+v", explanations[0])
	}
	if !explanations[2].Reserve || explanations[2].Normal {
		t.Fatalf("fixture C should be reserve-only: %+v", explanations[2])
	}
}

func TestResetAwareSingleCredentialBypassesScoring(t *testing.T) {
	selector := resetAwareFixtureSelector(&WeightedRoundRobinSelector{})
	auth := resetAwareFixtureAuth("only", "codex", resetAwareFixtureWindow("weekly", 12, resetAwareFixtureNow.Add(24*time.Hour), 7*24*time.Hour))
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{auth}); got != "only" {
		t.Fatalf("single credential = %q, want only", got)
	}
}

func TestResetAwareShortWindowResetIsSecondarySignal(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	longReset := resetAwareFixtureNow.Add(24 * time.Hour)
	a := resetAwareFixtureAuth("a", "codex",
		resetAwareFixtureWindow("weekly", 50, longReset, 7*24*time.Hour),
		resetAwareFixtureWindow("5h", 90, resetAwareFixtureNow.Add(30*time.Minute), 5*time.Hour),
	)
	b := resetAwareFixtureAuth("b", "codex",
		resetAwareFixtureWindow("weekly", 50, longReset, 7*24*time.Hour),
		resetAwareFixtureWindow("5h", 95, resetAwareFixtureNow.Add(4*time.Hour), 5*time.Hour),
	)
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{b, a}); got != "a" {
		t.Fatalf("short-window urgency winner = %q, want a", got)
	}
}

func TestResetAwareSafetyFloorProtectsLowLongWindow(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	a := resetAwareFixtureAuth("a", "codex", resetAwareFixtureWindow("weekly", 1, resetAwareFixtureNow.Add(2*time.Hour), 7*24*time.Hour))
	b := resetAwareFixtureAuth("b", "codex", resetAwareFixtureWindow("weekly", 70, resetAwareFixtureNow.Add(3*24*time.Hour), 7*24*time.Hour))
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b}); got != "b" {
		t.Fatalf("safety-floor winner = %q, want b", got)
	}
	explanations := selector.Explain("codex", "sol", []*Auth{a, b}, resetAwareFixtureNow)
	for _, explanation := range explanations {
		if explanation.AuthID == "a" && explanation.Eligible {
			t.Fatalf("low-capacity credential should be protected: %+v", explanation)
		}
	}
}

func TestResetAwareExhaustedCredentialExcluded(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	a := resetAwareFixtureAuth("a", "codex", QuotaWindow{
		Name:             "weekly",
		RemainingPercent: 0,
		ResetAt:          resetAwareFixtureNow.Add(time.Hour),
		DurationSeconds:  7 * 24 * 60 * 60,
		ObservedAt:       resetAwareFixtureNow,
		Exhausted:        true,
	})
	b := resetAwareFixtureAuth("b", "codex", resetAwareFixtureWindow("weekly", 50, resetAwareFixtureNow.Add(2*time.Hour), 7*24*time.Hour))
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b}); got != "b" {
		t.Fatalf("exhausted credential = %q, want b", got)
	}
}

func TestResetAwareNormalCapacityOutranksReserveAndReserveOnlyFallsBack(t *testing.T) {
	normal := resetAwareFixtureAuth("normal", "codex", resetAwareFixtureWindow("weekly", 15, resetAwareFixtureNow.Add(12*time.Hour), 7*24*time.Hour))
	reserve := resetAwareFixtureAuth("reserve", "codex", resetAwareFixtureReserveWindow("reserve-weekly", 90, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour))
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{reserve, normal}); got != "normal" {
		t.Fatalf("normal-vs-reserve winner = %q, want normal", got)
	}
	selector = resetAwareFixtureSelector(&RoundRobinSelector{})
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{reserve}); got != "reserve" {
		t.Fatalf("reserve-only winner = %q, want reserve", got)
	}
}

func TestResetAwareManualResetCreditsAreNeverSelected(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	manual := resetAwareFixtureWindow("manual-reset-credit", 100, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour)
	manual.ManualReset = true
	_, errPick := selector.Pick(context.Background(), "codex", "sol", cliproxyexecutor.Options{}, []*Auth{resetAwareFixtureAuth("manual", "codex", manual)})
	if errPick == nil {
		t.Fatal("manual reset credit was selected automatically")
	}
	manual.ResetAt = resetAwareFixtureNow.Add(-time.Minute)
	if _, errPick = selector.Pick(context.Background(), "codex", "sol", cliproxyexecutor.Options{}, []*Auth{resetAwareFixtureAuth("manual-due", "codex", manual)}); errPick == nil {
		t.Fatal("manual reset credit was selected automatically after its timestamp passed")
	}
}

func TestResetAwareNormalOnlyReservePolicyProtectsReserve(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	selector.config.ReservePolicy = "normal-only"
	reserve := resetAwareFixtureAuth("reserve", "codex", resetAwareFixtureReserveWindow("reserve-weekly", 90, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour))
	if _, errPick := selector.Pick(context.Background(), "codex", "sol", cliproxyexecutor.Options{}, []*Auth{reserve}); errPick == nil {
		t.Fatal("reserve capacity was selected under normal-only policy")
	}
}

func TestResetAwareSessionAffinityStaysStickyAndFailsOverWhenBoundCredentialUnavailable(t *testing.T) {
	base := resetAwareFixtureSelector(&RoundRobinSelector{})
	a := resetAwareFixtureAuth("a", "codex", resetAwareFixtureWindow("weekly", 70, resetAwareFixtureNow.Add(48*time.Hour), 7*24*time.Hour))
	b := resetAwareFixtureAuth("b", "codex", resetAwareFixtureWindow("weekly", 70, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour))
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: base, TTL: time.Hour})
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": []string{"sticky-session"}}}
	first, errFirst := affinity.Pick(context.Background(), "codex", "sol", opts, []*Auth{a, b})
	if errFirst != nil || first == nil || first.ID != "b" {
		t.Fatalf("initial sticky placement = %v, %v; want b", first, errFirst)
	}
	affinity.OnResult(Result{AuthID: first.ID, Provider: "codex", Model: "sol", Options: opts, Success: true})
	second, errSecond := affinity.Pick(context.Background(), "codex", "sol", opts, []*Auth{a, b})
	if errSecond != nil || second == nil || second.ID != "b" {
		t.Fatalf("sticky placement changed = %v, %v; want b", second, errSecond)
	}
	b.Unavailable = true
	b.NextRetryAfter = time.Now().UTC().Add(48 * time.Hour)
	third, errThird := affinity.Pick(context.Background(), "codex", "sol", opts, []*Auth{a, b})
	if errThird != nil || third == nil || third.ID != "a" {
		t.Fatalf("failover placement = %v, %v; want a", third, errThird)
	}
}

func TestResetAwareCooldownAndReadmission(t *testing.T) {
	now := resetAwareFixtureNow
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	selector.now = func() time.Time { return now }
	a := resetAwareFixtureAuth("a", "codex", resetAwareFixtureWindow("weekly", 80, now.Add(2*time.Hour), 7*24*time.Hour))
	a.Quota.Exceeded = true
	a.Quota.Reason = "credential_quota"
	a.Quota.NextRecoverAt = now.Add(time.Hour)
	b := resetAwareFixtureAuth("b", "codex", resetAwareFixtureWindow("weekly", 80, now.Add(2*time.Hour), 7*24*time.Hour))
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b}); got != "b" {
		t.Fatalf("cooldown winner = %q, want b", got)
	}
	now = now.Add(2 * time.Hour)
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b}); got != "a" {
		t.Fatalf("post-cooldown winner = %q, want a", got)
	}
}

func TestResetAwareResetDueRequiresFreshTelemetryWhenConfiguredToExclude(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	selector.config.StaleTelemetryPolicy = "exclude"
	a := resetAwareFixtureAuth("a", "codex", resetAwareFixtureWindow("weekly", 80, resetAwareFixtureNow.Add(-time.Minute), 7*24*time.Hour))
	b := resetAwareFixtureAuth("b", "codex", resetAwareFixtureWindow("weekly", 80, resetAwareFixtureNow.Add(2*time.Hour), 7*24*time.Hour))
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b}); got != "b" {
		t.Fatalf("fresh credential should remain selectable under exclude policy, got %q", got)
	}
	a.Quota.Windows[0].ResetAt = resetAwareFixtureNow.Add(30 * time.Minute)
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b}); got != "a" {
		t.Fatalf("freshly refreshed credential = %q, want a", got)
	}
}

func TestResetAwareResetDueRequestsAuthoritativeRefreshBeforeSelection(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	selector.config.RefreshAfterReset = true
	a := resetAwareFixtureAuth("a", "codex", resetAwareFixtureWindow("weekly", 27, resetAwareFixtureNow.Add(-time.Minute), 7*24*time.Hour))
	b := resetAwareFixtureAuth("b", "codex", resetAwareFixtureWindow("weekly", 70, resetAwareFixtureNow.Add(24*time.Hour), 7*24*time.Hour))
	refreshes := 0
	selector.SetQuotaRefreshFunc(func(_ context.Context, auth *Auth) (*Auth, error) {
		refreshes++
		updated := auth.Clone()
		updated.Quota.ObservedAt = resetAwareFixtureNow
		updated.Quota.Windows = []QuotaWindow{resetAwareFixtureWindow("weekly", 27, resetAwareFixtureNow.Add(30*time.Minute), 7*24*time.Hour)}
		return updated, nil
	})
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{a, b}); got != "a" {
		t.Fatalf("refreshed reset-due winner = %q, want a", got)
	}
	if refreshes != 1 {
		t.Fatalf("authoritative refresh calls = %d, want 1", refreshes)
	}
}

func TestResetAwareStaleAndMissingTelemetryUseConfiguredFallback(t *testing.T) {
	selector := resetAwareFixtureSelector(&FillFirstSelector{})
	stale := resetAwareFixtureAuth("stale", "codex", resetAwareFixtureWindow("weekly", 80, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour))
	stale.Quota.ObservedAt = resetAwareFixtureNow.Add(-2 * time.Hour)
	missing := &Auth{ID: "missing", Provider: "codex", Status: StatusActive}
	explanations := selector.Explain("codex", "sol", []*Auth{stale, missing}, resetAwareFixtureNow)
	if len(explanations) != 2 || explanations[0].TelemetryState == "fresh" || explanations[1].TelemetryState == "fresh" {
		t.Fatalf("stale/missing telemetry = %+v", explanations)
	}
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{stale, missing}); got != "missing" {
		t.Fatalf("fallback winner = %q, want missing by fill-first ID order", got)
	}
}

func TestResetAwareModelSpecificEligibilityAndProviderIsolation(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	sol := resetAwareFixtureAuth("sol-only", "codex", QuotaWindow{
		Name:             "weekly",
		RemainingPercent: 80,
		ResetAt:          resetAwareFixtureNow.Add(time.Hour),
		DurationSeconds:  7 * 24 * 60 * 60,
		ObservedAt:       resetAwareFixtureNow,
		Model:            "sol",
	})
	luna := resetAwareFixtureAuth("luna-only", "codex", QuotaWindow{
		Name:             "weekly",
		RemainingPercent: 80,
		ResetAt:          resetAwareFixtureNow.Add(2 * time.Hour),
		DurationSeconds:  7 * 24 * 60 * 60,
		ObservedAt:       resetAwareFixtureNow,
		Model:            "luna",
	})
	if got := resetAwarePickedID(t, selector, "codex", "luna", []*Auth{sol, luna}); got != "luna-only" {
		t.Fatalf("model-specific winner = %q, want luna-only", got)
	}
	claude := resetAwareFixtureAuth("claude", "claude", resetAwareFixtureWindow("weekly", 90, resetAwareFixtureNow.Add(48*time.Hour), 7*24*time.Hour))
	options := cliproxyexecutor.Options{Metadata: map[string]any{resetAwareProviderOrderMetadataKey: []string{"claude", "codex"}}}
	if got, errPick := selector.Pick(context.Background(), "mixed", "sol", options, []*Auth{sol, claude}); errPick != nil || got == nil || got.ID != "claude" {
		t.Fatalf("mixed provider isolation = %v, %v; want claude route first", got, errPick)
	}
}

func TestResetAwareDeterministicTieBreakerAndStateSerialization(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	reset := resetAwareFixtureNow.Add(time.Hour)
	a := resetAwareFixtureAuth("a", "codex", resetAwareFixtureWindow("weekly", 50, reset, 7*24*time.Hour))
	b := resetAwareFixtureAuth("b", "codex", resetAwareFixtureWindow("weekly", 50, reset, 7*24*time.Hour))
	if got := resetAwarePickedID(t, selector, "codex", "sol", []*Auth{b, a}); got != "a" {
		t.Fatalf("deterministic tie winner = %q, want a", got)
	}
	encoded, errMarshal := json.Marshal(a)
	if errMarshal != nil {
		t.Fatalf("marshal auth: %v", errMarshal)
	}
	var restored Auth
	if errUnmarshal := json.Unmarshal(encoded, &restored); errUnmarshal != nil {
		t.Fatalf("unmarshal auth: %v", errUnmarshal)
	}
	if len(restored.Quota.Windows) != 1 || restored.Quota.Windows[0].Name != "weekly" {
		t.Fatalf("normalized quota windows did not survive restart serialization: %+v", restored.Quota.Windows)
	}
}

func TestResetAwareConcurrentColdPlacementAvoidsCredentialStampede(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	a := resetAwareFixtureAuth("a", "codex", resetAwareFixtureWindow("weekly", 80, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour))
	b := resetAwareFixtureAuth("b", "codex", resetAwareFixtureWindow("weekly", 80, resetAwareFixtureNow.Add(2*time.Hour), 7*24*time.Hour))
	auths := []*Auth{a, b}
	ids := make(chan string, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			selected, errPick := selector.Pick(context.Background(), "codex", "sol", cliproxyexecutor.Options{}, auths)
			if errPick == nil && selected != nil {
				ids <- selected.ID
			}
		}()
	}
	wait.Wait()
	close(ids)
	seen := make(map[string]struct{})
	for id := range ids {
		seen[id] = struct{}{}
	}
	if len(seen) != 2 {
		t.Fatalf("concurrent cold placement selected %d unique credentials, want 2: %#v", len(seen), seen)
	}
}
