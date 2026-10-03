package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const claudeAffinityQuotaModel = "claude-haiku-4-5-20251001"

func TestClaudeAffinityFailsOverWhenAuthoritativeWeeklyQuotaIsExhausted(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	weekly := 7 * 24 * time.Hour
	a := resetAwareFixtureAuth("a", "claude", resetAwareFixtureWindow("weekly", 100, resetAwareFixtureNow.Add(weekly), weekly))
	b := resetAwareFixtureAuth("b", "claude", resetAwareFixtureWindow("weekly", 100, resetAwareFixtureNow.Add(weekly/2), weekly))
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, TTL: time.Hour})
	opts := claudeAffinityOptions("weekly-exhausted")

	first, errFirst := affinity.Pick(context.Background(), "claude", claudeAffinityQuotaModel, opts, []*Auth{a})
	if errFirst != nil || first == nil || first.ID != "a" {
		t.Fatalf("initial affinity selection = %v, %v; want a", first, errFirst)
	}
	affinity.OnResult(Result{AuthID: first.ID, Provider: "claude", Model: claudeAffinityQuotaModel, Options: opts, Success: true})

	// This is a fresh credential-wide authoritative observation. There is no
	// cooldown: the affinity decision itself must honor the exhausted window.
	a.Quota = QuotaState{
		ObservedAt: resetAwareFixtureNow,
		Windows:    []QuotaWindow{resetAwareFixtureWindow("weekly", 0, resetAwareFixtureNow.Add(weekly), weekly)},
	}
	second, errSecond := affinity.Pick(context.Background(), "claude", claudeAffinityQuotaModel, opts, []*Auth{a, b})
	if errSecond != nil || second == nil || second.ID != "b" {
		t.Fatalf("selection after authoritative weekly exhaustion = %v, %v; want b", second, errSecond)
	}
	affinity.OnResult(Result{AuthID: second.ID, Provider: "claude", Model: claudeAffinityQuotaModel, Options: opts, Success: true})
	third, errThird := affinity.Pick(context.Background(), "claude", claudeAffinityQuotaModel, opts, []*Auth{a, b})
	if errThird != nil || third == nil || third.ID != "b" {
		t.Fatalf("selection after failover = %v, %v; want sticky b", third, errThird)
	}
}

func TestClaudeColdSelectionSkipsExhaustedWeeklyWindow(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	weekly := 7 * 24 * time.Hour
	fiveHours := 5 * time.Hour
	a := resetAwareFixtureAuth("a", "claude",
		resetAwareFixtureWindow("5h", 80, resetAwareFixtureNow.Add(fiveHours), fiveHours),
		resetAwareFixtureWindow("weekly", 0, resetAwareFixtureNow.Add(weekly), weekly),
	)
	b := resetAwareFixtureAuth("b", "claude",
		resetAwareFixtureWindow("5h", 98, resetAwareFixtureNow.Add(fiveHours), fiveHours),
		resetAwareFixtureWindow("weekly", 100, resetAwareFixtureNow.Add(weekly), weekly),
	)

	got, errPick := selector.Pick(context.Background(), "claude", claudeAffinityQuotaModel, cliproxyexecutor.Options{}, []*Auth{a, b})
	if errPick != nil || got == nil || got.ID != "b" {
		t.Fatalf("selection with exhausted weekly and short-window comparison = %v, %v; want b", got, errPick)
	}
}

func TestClaudeAffinityKeepsHealthyBindingBelowColdPlacementFloor(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	weekly := 7 * 24 * time.Hour
	a := resetAwareFixtureAuth("a", "claude", resetAwareFixtureWindow("weekly", 50, resetAwareFixtureNow.Add(weekly), weekly))
	b := resetAwareFixtureAuth("b", "claude", resetAwareFixtureWindow("weekly", 100, resetAwareFixtureNow.Add(weekly/2), weekly))
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, TTL: time.Hour})
	opts := claudeAffinityOptions("healthy-low-capacity")

	first, errFirst := affinity.Pick(context.Background(), "claude", claudeAffinityQuotaModel, opts, []*Auth{a})
	if errFirst != nil || first == nil || first.ID != "a" {
		t.Fatalf("initial affinity selection = %v, %v; want a", first, errFirst)
	}
	affinity.OnResult(Result{AuthID: first.ID, Provider: "claude", Model: claudeAffinityQuotaModel, Options: opts, Success: true})
	a.Quota.Windows[0].RemainingPercent = 5

	// B wins cold placement, but A still has usable quota and must retain this
	// established session binding.
	coldPick, errCold := selector.Pick(context.Background(), "claude", claudeAffinityQuotaModel, cliproxyexecutor.Options{}, []*Auth{a, b})
	if errCold != nil || coldPick == nil || coldPick.ID != "b" {
		t.Fatalf("cold placement = %v, %v; want b", coldPick, errCold)
	}
	got, errPick := affinity.Pick(context.Background(), "claude", claudeAffinityQuotaModel, opts, []*Auth{a, b})
	if errPick != nil || got == nil || got.ID != "a" {
		t.Fatalf("established healthy affinity = %v, %v; want a", got, errPick)
	}
}

func TestClaudeAffinityQuotaWindowsDoNotBlockOtherProviders(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	weekly := 7 * 24 * time.Hour
	claudeExhausted := resetAwareFixtureWindow("weekly", 0, resetAwareFixtureNow.Add(weekly), weekly)
	claudeExhausted.Provider = "claude"

	for _, provider := range []string{"codex", "antigravity"} {
		t.Run(provider, func(t *testing.T) {
			a := resetAwareFixtureAuth("a", provider, claudeExhausted)
			b := resetAwareFixtureAuth("b", provider, resetAwareFixtureWindow("weekly", 50, resetAwareFixtureNow.Add(weekly/2), weekly))
			got, errPick := selector.Pick(context.Background(), provider, "model", cliproxyexecutor.Options{}, []*Auth{a, b})
			if errPick != nil || got == nil || got.ID != "a" {
				t.Fatalf("provider selection = %v, %v; Claude-only exhaustion must not block this provider", got, errPick)
			}
		})
	}
}

func TestClaudeFreshCredentialWindowsSupersedeStaleSharedModelWindows(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	weekly := 7 * 24 * time.Hour
	fiveHours := 5 * time.Hour
	a := resetAwareFixtureAuth("a", "claude",
		resetAwareFixtureWindow("weekly", 0, resetAwareFixtureNow.Add(weekly), weekly),
		resetAwareFixtureWindow("5h", 100, resetAwareFixtureNow.Add(fiveHours), fiveHours),
	)
	staleAt := resetAwareFixtureNow.Add(-30 * time.Minute)
	staleWeekly := resetAwareFixtureWindow("7d", 100, resetAwareFixtureNow.Add(weekly), weekly)
	staleWeekly.ObservedAt = staleAt
	staleFiveHour := resetAwareFixtureWindow("5h", 100, resetAwareFixtureNow.Add(fiveHours), fiveHours)
	staleFiveHour.ObservedAt = staleAt
	a.ModelStates = map[string]*ModelState{
		claudeAffinityQuotaModel: {Quota: QuotaState{ObservedAt: staleAt, Windows: []QuotaWindow{staleWeekly, staleFiveHour}}},
	}
	b := resetAwareFixtureAuth("b", "claude",
		resetAwareFixtureWindow("weekly", 60, resetAwareFixtureNow.Add(weekly), weekly),
		resetAwareFixtureWindow("5h", 60, resetAwareFixtureNow.Add(fiveHours), fiveHours),
	)

	explanation := selector.Explain("claude", claudeAffinityQuotaModel, []*Auth{a, b}, resetAwareFixtureNow)
	if len(explanation) != 2 || explanation[0].AuthID != "b" || explanation[0].Rank != 1 || !explanation[0].Eligible || !explanation[0].TelemetryFresh || explanation[1].AuthID != "a" || explanation[1].Eligible || !explanation[1].TelemetryFresh {
		t.Fatalf("fresh credential observation ranking = %+v; want B rank 1 and A ineligible", explanation)
	}
}

func TestClaudeStaleExhaustedWindowCannotReplaceFreshCredentialCapacity(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	weekly := 7 * 24 * time.Hour
	a := resetAwareFixtureAuth("a", "claude", resetAwareFixtureWindow("weekly", 0, resetAwareFixtureNow.Add(weekly), weekly))
	stale := resetAwareFixtureWindow("7d", 100, resetAwareFixtureNow.Add(weekly), weekly)
	stale.ObservedAt = resetAwareFixtureNow.Add(-30 * time.Minute)
	a.ModelStates = map[string]*ModelState{
		claudeAffinityQuotaModel: {Quota: QuotaState{ObservedAt: stale.ObservedAt, Windows: []QuotaWindow{stale}}},
	}
	b := resetAwareFixtureAuth("b", "claude", resetAwareFixtureWindow("weekly", 50, resetAwareFixtureNow.Add(weekly), weekly))

	got, errPick := selector.Pick(context.Background(), "claude", claudeAffinityQuotaModel, cliproxyexecutor.Options{}, []*Auth{a, b})
	if errPick != nil || got == nil || got.ID != "b" {
		t.Fatalf("selection with stale positive alias and fresh exhausted window = %v, %v; want b", got, errPick)
	}
}

func TestClaudeAffinityClearsBindingWhenEveryCredentialIsExhausted(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	weekly := 7 * 24 * time.Hour
	a := resetAwareFixtureAuth("a", "claude", resetAwareFixtureWindow("weekly", 100, resetAwareFixtureNow.Add(weekly), weekly))
	b := resetAwareFixtureAuth("b", "claude", resetAwareFixtureWindow("weekly", 100, resetAwareFixtureNow.Add(weekly), weekly))
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, TTL: time.Hour})
	opts := claudeAffinityOptions("all-exhausted")
	first, errFirst := affinity.Pick(context.Background(), "claude", claudeAffinityQuotaModel, opts, []*Auth{a})
	if errFirst != nil || first == nil || first.ID != "a" {
		t.Fatalf("initial affinity selection = %v, %v; want a", first, errFirst)
	}
	affinity.OnResult(Result{AuthID: first.ID, Provider: "claude", Model: claudeAffinityQuotaModel, Options: opts, Success: true})
	a.Quota.Windows[0].RemainingPercent = 0
	b.Quota.Windows[0].RemainingPercent = 0
	if _, errPick := affinity.Pick(context.Background(), "claude", claudeAffinityQuotaModel, opts, []*Auth{a, b}); errPick == nil {
		t.Fatal("selection with all Claude credentials exhausted succeeded")
	}
	if authID, status := affinity.LookupAffinity("claude", claudeAffinityQuotaModel, "all-exhausted"); authID != "" || status != "unbound" {
		t.Fatalf("LookupAffinity after total exhaustion = (%q, %q), want unbound", authID, status)
	}
}

func TestClaudeDistinctModelWindowStillLimitsCredential(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	weekly := 7 * 24 * time.Hour
	a := resetAwareFixtureAuth("a", "claude", resetAwareFixtureWindow("weekly", 90, resetAwareFixtureNow.Add(weekly), weekly))
	modelWindow := resetAwareFixtureWindow("model-specific", 0, resetAwareFixtureNow.Add(weekly), weekly)
	modelWindow.Model = claudeAffinityQuotaModel
	a.ModelStates = map[string]*ModelState{
		claudeAffinityQuotaModel: {Quota: QuotaState{ObservedAt: resetAwareFixtureNow, Windows: []QuotaWindow{modelWindow}}},
	}
	b := resetAwareFixtureAuth("b", "claude", resetAwareFixtureWindow("weekly", 60, resetAwareFixtureNow.Add(weekly/2), weekly))

	got, errPick := selector.Pick(context.Background(), "claude", claudeAffinityQuotaModel, cliproxyexecutor.Options{}, []*Auth{a, b})
	if errPick != nil || got == nil || got.ID != "b" {
		t.Fatalf("selection with exhausted distinct model window = %v, %v; want b", got, errPick)
	}
}

func claudeAffinityOptions(session string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": []string{session}}}
}

func TestClaudeStaleExhaustionCannotEnterTelemetryFallback(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	window := resetAwareFixtureWindow("weekly", 0, resetAwareFixtureNow.Add(48*time.Hour), 7*24*time.Hour)
	window.ObservedAt = resetAwareFixtureNow.Add(-time.Hour)
	a := resetAwareFixtureAuth("a", "claude", window)
	a.Quota.ObservedAt = window.ObservedAt
	b := resetAwareFixtureAuth("b", "claude")
	if got := resetAwarePickedID(t, selector, "claude", claudeAffinityQuotaModel, []*Auth{a, b}); got != "b" {
		t.Fatalf("stale fallback selected exhausted credential %q", got)
	}
}

func TestClaudeAffinityShortExhaustionPreservesOtherModelBinding(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	a := resetAwareFixtureAuth("a", "claude", resetAwareFixtureWindow("weekly", 80, resetAwareFixtureNow.Add(48*time.Hour), 7*24*time.Hour))
	b := resetAwareFixtureAuth("b", "claude", resetAwareFixtureWindow("weekly", 100, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour))
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, TTL: time.Hour})
	defer affinity.Stop()
	opts := claudeAffinityOptions("model-isolation")
	otherModel := "claude-opus-5-5"
	for _, model := range []string{claudeAffinityQuotaModel, otherModel} {
		if _, err := affinity.Pick(context.Background(), "claude", model, opts, []*Auth{a}); err != nil {
			t.Fatal(err)
		}
		affinity.OnResult(Result{AuthID: a.ID, Provider: "claude", Model: model, Options: opts, Success: true})
	}
	window := resetAwareFixtureWindow("5h-haiku", 0, resetAwareFixtureNow.Add(time.Hour), 5*time.Hour)
	window.Model = claudeAffinityQuotaModel
	a.ModelStates = map[string]*ModelState{claudeAffinityQuotaModel: {Quota: QuotaState{ObservedAt: resetAwareFixtureNow, Windows: []QuotaWindow{window}}}}
	for model, want := range map[string]string{claudeAffinityQuotaModel: "b", otherModel: "a"} {
		got, err := affinity.Pick(context.Background(), "claude", model, opts, []*Auth{a, b})
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("model %s selected %v, %v; want %s", model, got, err, want)
		}
	}
}

func TestClaudeLCPExhaustionReleasesMatchedPrefix(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	a := resetAwareFixtureAuth("a", "claude", resetAwareFixtureWindow("weekly", 80, resetAwareFixtureNow.Add(48*time.Hour), 7*24*time.Hour))
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, TTL: time.Hour})
	defer affinity.Stop()
	options := func(body string) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(body), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "claude-lcp-caller"}}
	}
	first := options(`{"messages":[{"role":"system","content":"stable"},{"role":"user","content":"first"}]}`)
	if _, err := affinity.Pick(context.Background(), "claude", claudeAffinityQuotaModel, first, []*Auth{a}); err != nil {
		t.Fatal(err)
	}
	affinity.OnResult(Result{AuthID: a.ID, Provider: "claude", Model: claudeAffinityQuotaModel, Options: first, Success: true})
	a.Quota.Windows[0].RemainingPercent = 0
	grown := options(`{"messages":[{"role":"system","content":"stable"},{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"continue"}]}`)
	if _, err := affinity.Pick(context.Background(), "claude", claudeAffinityQuotaModel, grown, []*Auth{a}); err == nil {
		t.Fatal("exhausted LCP continuation was selected")
	}
	// Checking the original prefix catches removal of only the attempted grown
	// sequence, which would leave the actual matched binding behind.
	namespace := lcpAffinityNamespace("claude", claudeAffinityQuotaModel, first.Metadata)
	fingerprints, minPrefix := lcpFingerprintsFromMetadata(first.Metadata)
	if _, ok := affinity.matcher.MatchFingerprints(namespace, fingerprints, minPrefix); ok {
		t.Fatal("unusable matched prefix remained bound after failed fallback")
	}
}
