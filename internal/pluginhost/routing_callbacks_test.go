package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestHostRoutingResetCooldownPreservesResetAwareObservations(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "exhausted", true: "terminal"}[terminal], func(t *testing.T) {
			now := time.Now()
			auth := &coreauth.Auth{ID: "test-only-reset-observation", Provider: "codex", Status: coreauth.StatusError, Unavailable: true,
				Quota: coreauth.QuotaState{ObservedAt: now, Windows: []coreauth.QuotaWindow{{Name: "weekly", RemainingPercent: 0, ResetAt: now.Add(time.Hour), DurationSeconds: int64((7 * 24 * time.Hour).Seconds())}}}}
			if terminal {
				auth.LastError = &coreauth.Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized, Message: "test-only-revoked"}
			} else {
				auth.Quota.Exceeded = true
				auth.Quota.Reason = "credential_quota"
				auth.Quota.NextRecoverAt = now.Add(time.Hour)
				auth.NextRetryAfter = now.Add(time.Hour)
			}
			selector := coreauth.NewResetAwareSelector(coreauth.ResetAwareSelectorConfig{LongestWindowFirst: true, UseExpiringCapacityFirst: true, ReservePolicy: "last-resort", TelemetryMaxAge: time.Hour})
			manager := coreauth.NewManager(nil, selector, nil)
			registered, errRegister := manager.Register(context.Background(), auth)
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			host := New()
			host.SetAuthManager(manager)
			request, errMarshal := json.Marshal(pluginapi.HostRoutingResetCooldownRequest{AuthIndex: registered.Index})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			if _, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostRoutingResetCooldown, request); errCall != nil {
				t.Fatal(errCall)
			}
			updated, _ := manager.GetByID(auth.ID)
			if !updated.Quota.ObservedAt.Equal(now) || len(updated.Quota.Windows) != 1 || updated.Quota.Windows[0].RemainingPercent != 0 {
				t.Fatal("internal reset erased authoritative exhaustion")
			}
			if terminal && (!updated.Unavailable || updated.LastError == nil || updated.LastError.HTTPStatus != http.StatusUnauthorized) {
				t.Fatal("plugin reset revived terminal auth")
			}
			if !terminal && (updated.Quota.Exceeded || !updated.NextRetryAfter.IsZero()) {
				t.Fatal("internal cooldown not cleared")
			}
			for _, candidate := range selector.Explain("codex", "sol", manager.List(), now) {
				if candidate.Eligible {
					t.Fatal("internal reset admitted terminal or observed-exhausted credential")
				}
			}
		})
	}
}

type recordingTokenStore struct {
	saveCalls int
}

func (s *recordingTokenStore) List(ctx context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (s *recordingTokenStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	s.saveCalls++
	return auth.ID, nil
}
func (s *recordingTokenStore) Delete(ctx context.Context, id string) error { return nil }

func TestHostRoutingResetCooldownClearsCredentialCooldown(t *testing.T) {
	const model = "claude-sonnet-5-5"
	next := time.Now().Add(65 * time.Hour)
	auth := &coreauth.Auth{
		ID:             "claude-a.json",
		Provider:       "claude",
		FileName:       "claude-a.json",
		Status:         coreauth.StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: next, BackoffLevel: 1},
		ModelStates: map[string]*coreauth.ModelState{
			model: {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota:          coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: next, BackoffLevel: 1},
				UpdatedAt:      time.Now(),
			},
		},
	}
	auth.EnsureIndex()

	host := New()
	manager := coreauth.NewManager(nil, nil, nil)
	host.SetAuthManager(manager)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	if held, _ := manager.GetByID(auth.ID); held == nil || !held.Quota.Exceeded || !held.Unavailable {
		t.Fatalf("registered auth = %+v, want a held credential_quota cooldown", held)
	}

	request, errMarshal := json.Marshal(pluginapi.HostRoutingResetCooldownRequest{AuthIndex: auth.Index})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostRoutingResetCooldown, request)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostRoutingResetCooldownResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.AuthIndex != auth.Index {
		t.Fatalf("auth_index = %q, want %q", resp.AuthIndex, auth.Index)
	}
	if len(resp.Models) != 1 || resp.Models[0] != model {
		t.Fatalf("models = %v, want [%s]", resp.Models, model)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("auth %s missing after reset", auth.ID)
	}
	if updated.Status != coreauth.StatusActive || updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("auth state = status %q unavailable %v next %v, want active and available", updated.Status, updated.Unavailable, updated.NextRetryAfter)
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" || !updated.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("auth quota = %+v, want cleared", updated.Quota)
	}
	state := updated.ModelStates[model]
	if state == nil || state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded {
		t.Fatalf("model state = %+v, want cleared", state)
	}
}

func TestHostRoutingResetCooldownLeavesTokenFilesAlone(t *testing.T) {
	const model = "claude-sonnet-5-5"
	next := time.Now().Add(48 * time.Hour)
	auth := &coreauth.Auth{
		ID:             "claude-token-test.json",
		Provider:       "claude",
		FileName:       "claude-token-test.json",
		Status:         coreauth.StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: next, BackoffLevel: 1},
		Metadata: map[string]any{
			"account_id": "claude-seat-123",
		},
		ModelStates: map[string]*coreauth.ModelState{
			model: {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota:          coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: next, BackoffLevel: 1},
				UpdatedAt:      time.Now(),
			},
		},
	}
	auth.EnsureIndex()

	store := &recordingTokenStore{}
	manager := coreauth.NewManager(store, nil, nil)
	host := New()
	host.SetAuthManager(manager)

	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	// Reset recorded save calls from registration
	store.saveCalls = 0

	request, errMarshal := json.Marshal(pluginapi.HostRoutingResetCooldownRequest{AuthIndex: auth.Index})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostRoutingResetCooldown, request)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostRoutingResetCooldownResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.AuthIndex != auth.Index {
		t.Fatalf("auth_index = %q, want %q", resp.AuthIndex, auth.Index)
	}

	// Verify token store Save was NOT called (token files left alone)
	if store.saveCalls != 0 {
		t.Fatalf("token store Save was called %d times during cooldown reset, want 0", store.saveCalls)
	}

	// Verify cooldown was still reset in memory
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("auth %s missing after reset", auth.ID)
	}
	if updated.Unavailable || updated.Quota.Exceeded {
		t.Fatalf("auth quota state = %+v, unavailable=%v; want cooldown cleared", updated.Quota, updated.Unavailable)
	}
}

func TestHostRoutingResetCooldownRejectsUnknownAuthIndex(t *testing.T) {
	host := New()
	host.SetAuthManager(coreauth.NewManager(nil, nil, nil))

	for _, authIndex := range []string{"", "missing"} {
		request, errMarshal := json.Marshal(pluginapi.HostRoutingResetCooldownRequest{AuthIndex: authIndex})
		if errMarshal != nil {
			t.Fatalf("marshal request: %v", errMarshal)
		}
		if _, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostRoutingResetCooldown, request); errCall == nil {
			t.Fatalf("callFromPlugin(%q) error = nil, want error", authIndex)
		}
	}
}

func TestHostRoutingResetCooldownRejectsInvalidJSON(t *testing.T) {
	host := New()
	host.SetAuthManager(coreauth.NewManager(nil, nil, nil))

	if _, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostRoutingResetCooldown, []byte("{invalid-json")); errCall == nil {
		t.Fatal("expected error on invalid JSON request, got nil")
	}
}

func TestHostRoutingResetCooldownRequiresAuthManager(t *testing.T) {
	host := New()
	host.SetAuthManager(nil)

	request, errMarshal := json.Marshal(pluginapi.HostRoutingResetCooldownRequest{AuthIndex: "any"})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	if _, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostRoutingResetCooldown, request); errCall == nil {
		t.Fatal("expected error when auth manager is unavailable, got nil")
	}
}

func TestHostRoutingResetCooldownLogsPluginID(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()

	auth := &coreauth.Auth{
		ID:          "claude-log-test.json",
		Provider:    "claude",
		FileName:    "claude-log-test.json",
		Status:      coreauth.StatusActive,
		ModelStates: map[string]*coreauth.ModelState{},
	}
	auth.EnsureIndex()

	host := New()
	manager := coreauth.NewManager(nil, nil, nil)
	host.SetAuthManager(manager)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	request, errMarshal := json.Marshal(pluginapi.HostRoutingResetCooldownRequest{AuthIndex: auth.Index})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}

	pluginCtx := withHostCallbackPluginID(context.Background(), "claude-seat-pacer")
	if _, errCall := host.callFromPlugin(pluginCtx, pluginabi.MethodHostRoutingResetCooldown, request); errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}

	found := false
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.InfoLevel && strings.Contains(entry.Message, "plugin reset credential cooldown") {
			if entry.Data["plugin_id"] == "claude-seat-pacer" && entry.Data["auth_index"] == auth.Index {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("expected info log entry with plugin_id=claude-seat-pacer and auth_index=%s, got entries: %+v", auth.Index, hook.AllEntries())
	}
}
