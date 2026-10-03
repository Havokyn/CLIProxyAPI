package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestClaudeStaleExhaustedNormalWindowDoesNotSpendReserveAheadOfFreshNormalCapacity(t *testing.T) {
	selector := resetAwareFixtureSelector(&RoundRobinSelector{})
	staleAt := resetAwareFixtureNow.Add(-2 * time.Hour)
	weekly := 7 * 24 * time.Hour

	normalExhausted := resetAwareFixtureWindow("weekly", 0, resetAwareFixtureNow.Add(weekly), weekly)
	normalExhausted.ObservedAt = staleAt
	reserve := resetAwareFixtureReserveWindow("reserve-weekly", 80, resetAwareFixtureNow.Add(weekly), weekly)
	reserve.ObservedAt = staleAt
	a := resetAwareFixtureAuth("a", "claude", normalExhausted, reserve)
	a.Quota.ObservedAt = staleAt
	b := resetAwareFixtureAuth("b", "claude", resetAwareFixtureWindow("weekly", 60, resetAwareFixtureNow.Add(weekly), weekly))

	got, errPick := selector.Pick(context.Background(), "claude", claudeAffinityQuotaModel, cliproxyexecutor.Options{}, []*Auth{a, b})
	if errPick != nil || got == nil || got.ID != b.ID {
		t.Fatalf("selection with stale exhausted normal quota and fresh normal capacity = %v, %v; want %q", got, errPick, b.ID)
	}
}
