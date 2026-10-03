package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestResetAwareTerminalAuthSurvivesTelemetryAndCooldownReset(t *testing.T) {
	ctx := context.Background()
	selector := resetAwareFixtureSelector(nil)
	manager := NewManager(nil, selector, nil)
	executor := &unauthorizedRefreshExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	a := resetAwareFixtureAuth("terminal", "codex", resetAwareFixtureWindow("weekly", 27, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour))
	a.Status, a.Unavailable = StatusError, true
	a.LastError = &Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized, Message: "test-only-revoked"}
	a.Metadata = map[string]any{"access_token": "test-only-old", "refresh_token": "test-only-refresh"}
	base, errRegister := manager.Register(ctx, a)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	b := resetAwareFixtureAuth("healthy", "codex", resetAwareFixtureWindow("weekly", 52, resetAwareFixtureNow.Add(2*time.Hour), 7*24*time.Hour))
	if _, errRegister := manager.Register(ctx, b); errRegister != nil {
		t.Fatal(errRegister)
	}
	observation := base.Quota.Clone()
	observation.ObservedAt = observation.ObservedAt.Add(time.Second)
	updated, errRecord := manager.RecordQuotaObservation(base, observation)
	if errRecord != nil || !hasUnauthorizedAuthFailure(updated) {
		t.Fatalf("telemetry revived terminal auth: %v", errRecord)
	}
	updated, _, errReset := manager.ResetQuota(WithSkipPersist(ctx), a.ID)
	if errReset != nil || !hasUnauthorizedAuthFailure(updated) {
		t.Fatalf("cooldown reset revived terminal auth: %v", errReset)
	}
	if _, errRefresh := manager.refreshAuthForRequest(ctx, a.ID, "test-only-old"); errRefresh == nil || executor.RefreshCalls() != 0 {
		t.Fatal("automatic refresh retried terminal auth")
	}
	if got := resetAwarePickedID(t, selector, "codex", "sol", manager.List()); got != b.ID {
		t.Fatalf("terminal auth selected: %s", got)
	}
	// Explicit force refresh is the recovery boundary, and rotates the token.
	updated, errRefresh := manager.ForceRefreshAuth(ctx, a.ID)
	if errRefresh != nil || hasUnauthorizedAuthFailure(updated) || updated.Unavailable || executor.RefreshCalls() != 1 {
		t.Fatalf("explicit recovery failed: %v", errRefresh)
	}
	selector = resetAwareFixtureSelector(nil)
	if got := resetAwarePickedID(t, selector, "codex", "sol", manager.List()); got != a.ID {
		t.Fatalf("recovered earlier-reset auth not admitted: %s", got)
	}
}
