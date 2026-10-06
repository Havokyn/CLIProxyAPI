package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CredentialQuotaFetcher is an optional read-only provider capability. It must
// return authoritative usage observations, never redeem manual reset credits.
type CredentialQuotaFetcher interface {
	FetchQuotaHeaders(context.Context, *Auth) (http.Header, error)
}

// ErrQuotaRefreshBusy means another probe owns the bounded refresh capacity.
// Callers must fall back without caching this as a provider failure.
var ErrQuotaRefreshBusy = fmt.Errorf("quota refresh already in progress")

type quotaProbeState struct {
	mu        sync.Mutex
	attempted time.Time
	err       error
	epoch     uint64
}

// RefreshCredentialQuota coalesces both management and routing probes. Success
// and failure share a 30-second minimum interval; all quota data lives in Auth.
func (m *Manager) RefreshCredentialQuota(ctx context.Context, base *Auth) (*Auth, error) {
	return m.refreshCredentialQuota(ctx, base, time.Now)
}

// StartCredentialQuotaRefresh starts a bounded probe without putting network
// latency on the inference path. It uses the caller's cancellation; until the
// observation arrives selection keeps the configured stale-data fallback.
func (m *Manager) StartCredentialQuotaRefresh(ctx context.Context, base *Auth) (*Auth, error) {
	return m.probeCredentialQuota(ctx, base, time.Now, true)
}

func (m *Manager) refreshCredentialQuota(ctx context.Context, base *Auth, nowFunc func() time.Time) (*Auth, error) {
	return m.probeCredentialQuota(ctx, base, nowFunc, false)
}

func (m *Manager) probeCredentialQuota(ctx context.Context, base *Auth, nowFunc func() time.Time, asynchronous bool) (*Auth, error) {
	if m == nil || base == nil || strings.TrimSpace(base.ID) == "" {
		return nil, fmt.Errorf("quota credential unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	value, _ := m.quotaProbeStates.LoadOrStore(base.ID, &quotaProbeState{})
	state := value.(*quotaProbeState)
	if !state.mu.TryLock() {
		return nil, ErrQuotaRefreshBusy
	}
	unlockHere := true
	defer func() {
		if unlockHere {
			state.mu.Unlock()
		}
	}()
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
	}
	current, ok := m.GetByID(base.ID)
	if !ok || current.RegistrationEpoch != base.RegistrationEpoch || CredentialsChanged(base, current) {
		return nil, fmt.Errorf("quota credential changed during refresh")
	}
	now := nowFunc().UTC()
	if state.epoch == current.RegistrationEpoch && !state.attempted.IsZero() && now.Sub(state.attempted) < 30*time.Second {
		if state.err != nil {
			return nil, state.err
		}
		return current, nil
	}
	fetcher, ok := m.executorFor(executorKeyFromAuth(current)).(CredentialQuotaFetcher)
	if !ok {
		return nil, fmt.Errorf("authoritative quota provider unavailable")
	}
	select {
	case m.quotaProbeSlots <- struct{}{}:
	default:
		return nil, ErrQuotaRefreshBusy
	}
	state.attempted, state.epoch = now, current.RegistrationEpoch
	probe := func() (*Auth, error) {
		defer func() { <-m.quotaProbeSlots }()
		headers, errFetch := fetcher.FetchQuotaHeaders(ctx, current.Clone())
		if errContext := ctx.Err(); errContext != nil {
			state.attempted = time.Time{}
			state.err = nil
			return nil, errContext
		}
		if errFetch != nil {
			state.err = fmt.Errorf("authoritative quota fetch failed: %w", errFetch)
			return nil, state.err
		}
		observation := QuotaState{}
		if !observation.ObserveResponseHeadersForProvider(current.Provider, headers, nowFunc().UTC()) || len(observation.Windows) == 0 {
			state.err = fmt.Errorf("authoritative quota response has no usable windows")
			return nil, state.err
		}
		updated, errRecord := m.RecordQuotaObservation(current, observation)
		state.err = errRecord
		return updated, errRecord
	}
	if asynchronous {
		unlockHere = false
		go func() { defer state.mu.Unlock(); _, _ = probe() }()
		return nil, ErrQuotaRefreshBusy
	}
	return probe()
}

// RecordQuotaObservation merges only telemetry into the latest credential under
// the manager lock. Concurrent cooldowns, credential edits and newer response
// observations are preserved; no auth file or OAuth token is rewritten.
func (m *Manager) RecordQuotaObservation(base *Auth, observation QuotaState) (*Auth, error) {
	if m == nil || base == nil || observation.ObservedAt.IsZero() || len(observation.Windows) == 0 {
		return nil, fmt.Errorf("invalid quota observation")
	}
	m.mu.Lock()
	current := m.auths[base.ID]
	if current == nil || current.RegistrationEpoch != base.RegistrationEpoch || CredentialsChanged(base, current) {
		m.mu.Unlock()
		return nil, fmt.Errorf("quota credential changed during refresh")
	}
	if !current.Quota.ObservedAt.After(observation.ObservedAt) {
		// Keep the published object stable while persistence temporarily releases m.mu.
		// Update merges these runtime deltas, and persistLocked retains save enrichment.
		copyObservation := observation.Clone()
		current.Quota.ObservedAt = copyObservation.ObservedAt
		current.Quota.Signals = copyObservation.Signals
		current.Quota.Windows = copyObservation.Windows
		current.Generation++
	}
	updated := current.Clone()
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(updated.Clone())
	}
	return updated, nil
}
