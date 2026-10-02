package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type routingQuotaTestExecutor struct{ refreshRecordExecutor }

func (e *routingQuotaTestExecutor) FetchQuotaHeaders(context.Context, *coreauth.Auth) (http.Header, error) {
	return http.Header{"X-Codex-Primary-Used-Percent": {"73"}, "X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}}, nil
}

func TestRefreshRoutingQuotaWritesObservationWithoutOAuthRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &routingQuotaTestExecutor{refreshRecordExecutor: refreshRecordExecutor{provider: "codex"}}
	manager.RegisterExecutor(exec)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "quota-test", Provider: "codex", Status: coreauth.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{authManager: manager}
	engine := gin.New()
	engine.POST("/refresh", h.RefreshRoutingQuota)
	body, _ := json.Marshal(map[string]string{"auth_index": auth.Index})
	request := httptest.NewRequest(http.MethodPost, "/refresh", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	updated, _ := manager.GetByID(auth.ID)
	if len(updated.Quota.Windows) != 1 || updated.Quota.Windows[0].RemainingPercent != 27 || exec.refreshCnt.Load() != 0 {
		t.Fatalf("quota refresh mutated OAuth or lost observation: %+v", updated.Quota)
	}
}

func TestGetResetAwareRoutingReturnsNonsecretRanking(t *testing.T) {
	now := time.Now().UTC()
	selector := coreauth.NewResetAwareSelector(coreauth.ResetAwareSelectorConfig{
		LongestWindowFirst:             true,
		UseExpiringCapacityFirst:       true,
		MinLongWindowRemainingPercent:  10,
		MinShortWindowRemainingPercent: 5,
		ReservePolicy:                  "last-resort",
		TelemetryMaxAge:                time.Hour,
	})
	manager := coreauth.NewManager(nil, selector, nil)
	registerRoutingTestAuth(t, manager, &coreauth.Auth{
		ID:       "fixture-a",
		Index:    "fixture-index-a",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Quota: coreauth.QuotaState{
			ObservedAt: now,
			Windows: []coreauth.QuotaWindow{{
				Name:             "weekly",
				RemainingPercent: 27,
				ResetAt:          now.Add(time.Hour),
				DurationSeconds:  7 * 24 * 60 * 60,
				ObservedAt:       now,
			}},
		},
	})
	registerRoutingTestAuth(t, manager, &coreauth.Auth{
		ID:       "fixture-b",
		Index:    "fixture-index-b",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Quota: coreauth.QuotaState{
			ObservedAt: now,
			Windows: []coreauth.QuotaWindow{{
				Name:             "weekly",
				RemainingPercent: 52,
				ResetAt:          now.Add(2 * time.Hour),
				DurationSeconds:  7 * 24 * 60 * 60,
				ObservedAt:       now,
			}},
		},
	})

	h := NewHandlerWithoutConfigFilePath(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "reset-aware"},
	}, manager)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/routing/reset-aware?provider=codex&model=sol", nil)
	h.GetResetAwareRouting(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload struct {
		Enabled    bool                                  `json:"enabled"`
		Preview    bool                                  `json:"preview"`
		Candidates []coreauth.ResetAwareRoutingCandidate `json:"candidates"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if !payload.Enabled || payload.Preview || len(payload.Candidates) != 2 {
		t.Fatalf("unexpected routing response: %+v", payload)
	}
	if payload.Candidates[0].AuthID != "fixture-a" || !payload.Candidates[0].Selected {
		t.Fatalf("ranking = %+v, want fixture-a selected first", payload.Candidates)
	}
	if payload.Candidates[0].AuthIndex != "fixture-index-a" {
		t.Fatalf("auth index = %q, want fixture-index-a", payload.Candidates[0].AuthIndex)
	}
	if payload.Candidates[0].LongestWindowRemainingPercent == nil || *payload.Candidates[0].LongestWindowRemainingPercent != 27 {
		t.Fatalf("long-window diagnostics = %+v", payload.Candidates[0])
	}
}

func TestGetResetAwareRoutingProvidesReadOnlyPreviewForLegacyStrategy(t *testing.T) {
	now := time.Now().UTC()
	manager := coreauth.NewManager(nil, nil, nil)
	registerRoutingTestAuth(t, manager, &coreauth.Auth{
		ID:       "preview-auth",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Quota: coreauth.QuotaState{
			ObservedAt: now,
			Windows: []coreauth.QuotaWindow{{
				Name:             "weekly",
				RemainingPercent: 50,
				ResetAt:          now.Add(time.Hour),
				DurationSeconds:  7 * 24 * 60 * 60,
				ObservedAt:       now,
			}},
		},
	})
	h := NewHandlerWithoutConfigFilePath(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "round-robin"},
	}, manager)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/routing/reset-aware?provider=codex&model=sol", nil)
	h.GetResetAwareRouting(ctx)
	var payload struct {
		Enabled    bool                                  `json:"enabled"`
		Preview    bool                                  `json:"preview"`
		Candidates []coreauth.ResetAwareRoutingCandidate `json:"candidates"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload.Enabled || !payload.Preview || len(payload.Candidates) != 1 {
		t.Fatalf("unexpected preview response: %+v", payload)
	}
}

func registerRoutingTestAuth(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth) {
	t.Helper()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth %q: %v", auth.ID, errRegister)
	}
}
