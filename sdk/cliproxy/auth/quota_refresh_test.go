package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type quotaFetchTestExecutor struct {
	schedulerTestExecutor
	calls        atomic.Int32
	fetch        func() (http.Header, error)
	fetchContext func(context.Context) (http.Header, error)
}

func TestQuotaCredentialCapacityFailureRefreshesWithoutClearingCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, errRegister := manager.Register(context.Background(), &Auth{ID: "capacity", Provider: "codex", Status: StatusActive})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	exec := &quotaFetchTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "codex"}, fetch: func() (http.Header, error) {
		close(started)
		<-release
		return http.Header{"X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"120"}}, nil
	}}
	manager.RegisterExecutor(exec)
	retry := 2 * time.Minute
	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: "model", CredentialScope: true, Error: &Error{HTTPStatus: 429, Code: "usage_limit_reached"}, RetryAfter: &retry})
	<-started
	cooled, _ := manager.GetByID(auth.ID)
	if !cooled.Quota.Exceeded || cooled.Quota.Reason != "credential_quota" || !cooled.Unavailable {
		t.Fatal("capacity failure did not cool credential")
	}
	close(release)
	value, _ := manager.quotaProbeStates.Load(auth.ID)
	state := value.(*quotaProbeState)
	state.mu.Lock()
	state.mu.Unlock()
	observed, _ := manager.GetByID(auth.ID)
	if len(observed.Quota.Windows) == 0 || !observed.Quota.Exceeded || !observed.Quota.NextRecoverAt.Equal(cooled.Quota.NextRecoverAt) {
		t.Fatal("refresh lost capacity observation or active cooldown")
	}
}

func (e *quotaFetchTestExecutor) FetchQuotaHeaders(ctx context.Context, _ *Auth) (http.Header, error) {
	e.calls.Add(1)
	if e.fetchContext != nil {
		return e.fetchContext(ctx)
	}
	return e.fetch()
}

func TestQuotaCancelledRoutingProbeDoesNotPoisonNextRequest(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, _ := manager.Register(context.Background(), &Auth{ID: "a", Provider: "codex", Status: StatusActive})
	started := make(chan struct{})
	exec := &quotaFetchTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "codex"}, fetchContext: func(ctx context.Context) (http.Header, error) {
		if execCall := ctx.Value("cancel-test"); execCall != nil {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return http.Header{"X-Codex-Primary-Used-Percent": {"73"}, "X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}}, nil
	}}
	manager.RegisterExecutor(exec)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), "cancel-test", true))
	_, _ = manager.StartCredentialQuotaRefresh(ctx, auth)
	<-started
	cancel()
	value, _ := manager.quotaProbeStates.Load(auth.ID)
	state := value.(*quotaProbeState)
	state.mu.Lock()
	state.mu.Unlock()
	if _, err := manager.RefreshCredentialQuota(context.Background(), auth); err != nil {
		t.Fatalf("caller cancellation poisoned immediate retry: %v", err)
	}
	if exec.calls.Load() != 2 {
		t.Fatalf("calls=%d", exec.calls.Load())
	}
}

func TestQuotaRefreshCoalescesConcurrentSuccessAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			now := resetAwareFixtureNow
			manager := NewManager(nil, nil, nil)
			auth, err := manager.Register(context.Background(), &Auth{ID: "quota-probe", Provider: "codex", Status: StatusActive})
			if err != nil {
				t.Fatal(err)
			}
			exec := &quotaFetchTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "codex"}, fetch: func() (http.Header, error) {
				if fail {
					return nil, errors.New("test probe failed")
				}
				return http.Header{"X-Codex-Primary-Used-Percent": {"73"}, "X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}}, nil
			}}
			manager.RegisterExecutor(exec)
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, errRefresh := manager.refreshCredentialQuota(context.Background(), auth, func() time.Time { return now })
					if !errors.Is(errRefresh, ErrQuotaRefreshBusy) && (errRefresh != nil) != fail {
						t.Errorf("refresh error=%v", errRefresh)
					}
				}()
			}
			wg.Wait()
			if got := exec.calls.Load(); got != 1 {
				t.Fatalf("probe calls=%d", got)
			}
			now = now.Add(31 * time.Second)
			_, _ = manager.refreshCredentialQuota(context.Background(), auth, func() time.Time { return now })
			if got := exec.calls.Load(); got != 2 {
				t.Fatalf("retry calls=%d", got)
			}
		})
	}
}

func TestQuotaRefreshBusyDoesNotWaitOrPoisonRetry(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auths := make([]*Auth, 3)
	for i, id := range []string{"a", "b", "c"} {
		auths[i], _ = manager.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive})
	}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	exec := &quotaFetchTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "codex"}, fetch: func() (http.Header, error) {
		started <- struct{}{}
		<-release
		return http.Header{"X-Codex-Primary-Used-Percent": {"73"}, "X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}}, nil
	}}
	manager.RegisterExecutor(exec)
	var wg sync.WaitGroup
	for _, auth := range auths[:2] {
		wg.Add(1)
		go func(auth *Auth) { defer wg.Done(); _, _ = manager.RefreshCredentialQuota(context.Background(), auth) }(auth)
	}
	<-started
	<-started
	if _, err := manager.RefreshCredentialQuota(context.Background(), auths[2]); !errors.Is(err, ErrQuotaRefreshBusy) {
		t.Fatalf("third probe error=%v", err)
	}
	if _, err := manager.RefreshCredentialQuota(context.Background(), auths[0]); !errors.Is(err, ErrQuotaRefreshBusy) {
		t.Fatalf("same auth error=%v", err)
	}
	close(release)
	wg.Wait()
	if _, err := manager.RefreshCredentialQuota(context.Background(), auths[2]); err != nil {
		t.Fatalf("busy poisoned retry: %v", err)
	}
	if exec.calls.Load() != 3 {
		t.Fatalf("calls=%d", exec.calls.Load())
	}
}

func TestQuotaRoutingRefreshDoesNotWaitForProvider(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, _ := manager.Register(context.Background(), &Auth{ID: "a", Provider: "codex", Status: StatusActive})
	started := make(chan struct{})
	release := make(chan struct{})
	exec := &quotaFetchTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "codex"}, fetch: func() (http.Header, error) {
		close(started)
		<-release
		return http.Header{"X-Codex-Primary-Used-Percent": {"73"}, "X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}}, nil
	}}
	manager.RegisterExecutor(exec)
	if _, err := manager.StartCredentialQuotaRefresh(context.Background(), auth); !errors.Is(err, ErrQuotaRefreshBusy) {
		t.Fatalf("async refresh error=%v", err)
	}
	<-started
	if _, err := manager.StartCredentialQuotaRefresh(context.Background(), auth); !errors.Is(err, ErrQuotaRefreshBusy) {
		t.Fatalf("duplicate async refresh error=%v", err)
	}
	close(release)
	value, _ := manager.quotaProbeStates.Load(auth.ID)
	state := value.(*quotaProbeState)
	state.mu.Lock()
	state.mu.Unlock()
	updated, _ := manager.GetByID(auth.ID)
	if len(updated.Quota.Windows) != 1 || exec.calls.Load() != 1 {
		t.Fatalf("async observation lost or duplicated: calls=%d quota=%+v", exec.calls.Load(), updated.Quota)
	}
}

func TestQuotaRefreshCredentialChangeInvalidatesProbeBackoff(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, _ := manager.Register(context.Background(), &Auth{ID: "a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "test-only-old"}})
	exec := &quotaFetchTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "codex"}, fetch: func() (http.Header, error) {
		return http.Header{"X-Codex-Primary-Used-Percent": {"73"}, "X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}}, nil
	}}
	manager.RegisterExecutor(exec)
	_, _ = manager.RefreshCredentialQuota(context.Background(), auth)
	updated, _ := manager.GetByID(auth.ID)
	updated.Metadata["access_token"] = "synthetic-new"
	updated, err := manager.Update(context.Background(), updated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.RefreshCredentialQuota(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	if exec.calls.Load() != 2 {
		t.Fatalf("rotated credential reused old probe: calls=%d", exec.calls.Load())
	}
}

func TestQuotaObservationPreservesCooldownAndNewerTelemetry(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	base, err := manager.Register(context.Background(), &Auth{ID: "atomic-quota", Provider: "codex", Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Hour)
	manager.mu.Lock()
	manager.auths[base.ID].Quota.Exceeded = true
	manager.auths[base.ID].Quota.Reason = "credential_quota"
	manager.auths[base.ID].Quota.NextRecoverAt = deadline
	manager.auths[base.ID].Unavailable = true
	manager.mu.Unlock()
	observation := QuotaState{ObservedAt: resetAwareFixtureNow, Windows: []QuotaWindow{resetAwareFixtureWindow("weekly", 27, resetAwareFixtureNow.Add(time.Hour), 7*24*time.Hour)}}
	updated, err := manager.RecordQuotaObservation(base, observation)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Unavailable || !updated.Quota.Exceeded || !updated.Quota.NextRecoverAt.Equal(deadline) || len(updated.Quota.Windows) != 1 {
		t.Fatalf("cooldown or observation lost: %+v", updated.Quota)
	}
	observation.ObservedAt = observation.ObservedAt.Add(-time.Minute)
	observation.Windows[0].RemainingPercent = 99
	updated, err = manager.RecordQuotaObservation(base, observation)
	if err != nil || updated.Quota.Windows[0].RemainingPercent != 27 {
		t.Fatalf("older observation replaced newer: %v %+v", err, updated)
	}
	manager.Remove(context.Background(), base.ID)
	_, _ = manager.Register(context.Background(), &Auth{ID: base.ID, Provider: "codex", Status: StatusActive})
	if _, err = manager.RecordQuotaObservation(base, observation); err == nil {
		t.Fatal("stale registration accepted")
	}
}
