package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const claudeAffinityIntegrationModel = "claude-haiku-4-5-20251001"

type claudeAffinityQuotaExecutor struct {
	*ClaudeExecutor
	quotaHeaders http.Header
	quotaCalls   chan string
}

func (e *claudeAffinityQuotaExecutor) FetchQuotaHeaders(_ context.Context, auth *cliproxyauth.Auth) (http.Header, error) {
	if e.quotaCalls != nil && auth != nil {
		select {
		case e.quotaCalls <- auth.ID:
		default:
		}
	}
	return e.quotaHeaders.Clone(), nil
}

func TestClaudeManagerWeeklyCapacityRotatesAffinityAndRefreshesQuota(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[streaming], func(t *testing.T) {
			var authACalls, authBCalls atomic.Int32
			resetAt := time.Now().Add(7 * 24 * time.Hour).Unix()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch claudeTestCredential(r) {
				case "test-only-a-key":
					authACalls.Add(1)
					writeClaudeWeekly429(w, resetAt)
				case "test-only-b-key":
					authBCalls.Add(1)
					if streaming {
						writeClaudeSuccessStream(w)
					} else {
						writeClaudeSuccessJSON(w)
					}
				default:
					http.Error(w, "unexpected credential", http.StatusUnauthorized)
				}
			}))
			defer server.Close()

			quotaCalls := make(chan string, 2)
			quotaHeaders := http.Header{
				"Anthropic-Ratelimit-Unified-5h-Status":          []string{"allowed"},
				"Anthropic-Ratelimit-Unified-5h-Utilization":     []string{"0.2"},
				"Anthropic-Ratelimit-Unified-5h-Reset":           []string{strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
				"Anthropic-Ratelimit-Unified-weekly-Status":      []string{"rejected"},
				"Anthropic-Ratelimit-Unified-weekly-Utilization": []string{"1.0"},
				"Anthropic-Ratelimit-Unified-weekly-Reset":       []string{strconv.FormatInt(resetAt, 10)},
			}
			executor := &claudeAffinityQuotaExecutor{
				ClaudeExecutor: NewClaudeExecutor(&config.Config{}),
				quotaHeaders:   quotaHeaders,
				quotaCalls:     quotaCalls,
			}
			manager, selector, authA, authB := newClaudeAffinityIntegrationManager(t, executor, server.URL)
			codexAuth := &cliproxyauth.Auth{ID: "codex-isolation-" + uuid.NewString(), Provider: "codex", Status: cliproxyauth.StatusActive}
			antigravityAuth := &cliproxyauth.Auth{ID: "antigravity-isolation-" + uuid.NewString(), Provider: "antigravity", Status: cliproxyauth.StatusActive}
			if _, errRegister := manager.Register(context.Background(), codexAuth); errRegister != nil {
				t.Fatalf("register Codex isolation auth: %v", errRegister)
			}
			if _, errRegister := manager.Register(context.Background(), antigravityAuth); errRegister != nil {
				t.Fatalf("register Antigravity isolation auth: %v", errRegister)
			}

			opts := cliproxyexecutor.Options{
				Headers:      http.Header{"X-Session-ID": []string{"claude-weekly-session"}},
				SourceFormat: sdktranslator.FormatClaude,
				Stream:       streaming,
			}
			bindClaudeAffinityToA(t, selector, authA, opts)
			request := cliproxyexecutor.Request{
				Model:   claudeAffinityIntegrationModel,
				Payload: []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`),
			}
			if streaming {
				stream, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, request, opts)
				if errStream != nil {
					t.Fatalf("ExecuteStream() error = %v", errStream)
				}
				if stream == nil {
					t.Fatal("ExecuteStream() returned nil stream")
				}
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatalf("successful fallback stream returned error chunk: %v", chunk.Err)
					}
				}
			} else {
				if _, errExecute := manager.Execute(context.Background(), []string{"claude"}, request, opts); errExecute != nil {
					t.Fatalf("Execute() error = %v", errExecute)
				}
			}

			if got := authACalls.Load(); got != 1 {
				t.Fatalf("credential A request count = %d, want 1", got)
			}
			if got := authBCalls.Load(); got != 1 {
				t.Fatalf("credential B request count = %d, want 1", got)
			}
			select {
			case id := <-quotaCalls:
				if id != authA.ID {
					t.Fatalf("quota refresh auth ID = %q, want A (%q)", id, authA.ID)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Claude weekly rejection did not request authoritative quota refresh")
			}

			updatedA, ok := manager.GetByID(authA.ID)
			if !ok || updatedA == nil {
				t.Fatal("credential A disappeared after capacity rejection")
			}
			if !updatedA.Quota.Exceeded || updatedA.Quota.Reason != "credential_quota" || !updatedA.Quota.NextRecoverAt.After(time.Now()) {
				t.Fatalf("credential A cooldown = %+v, want active temporary credential quota cooldown", updatedA.Quota)
			}
			if updatedA.Status == cliproxyauth.StatusDisabled || updatedA.Disabled {
				t.Fatalf("credential A was disabled after capacity rejection: status=%q disabled=%t", updatedA.Status, updatedA.Disabled)
			}
			if authID, status := selector.LookupAffinity("mixed", claudeAffinityIntegrationModel, "claude-weekly-session"); authID != authB.ID || status != "bound" {
				t.Fatalf("mixed-route affinity after successful fallback = (%q, %q), want B bound", authID, status)
			}
			for _, isolated := range []*cliproxyauth.Auth{codexAuth, antigravityAuth} {
				current, ok := manager.GetByID(isolated.ID)
				if !ok || current == nil || current.Unavailable || current.Quota.Exceeded || current.Disabled {
					t.Fatalf("unrelated %s credential changed after Claude exhaustion: %+v", isolated.Provider, current)
				}
			}
		})
	}
}

func TestClaudeManagerDoesNotReplayAmbiguousAcceptedRequests(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[streaming], func(t *testing.T) {
			var authACalls, authBCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch claudeTestCredential(r) {
				case "test-only-a-key":
					authACalls.Add(1)
				case "test-only-b-key":
					authBCalls.Add(1)
				default:
					http.Error(w, "unexpected credential", http.StatusUnauthorized)
					return
				}
				conn, _, errHijack := w.(http.Hijacker).Hijack()
				if errHijack != nil {
					t.Errorf("Hijack() error = %v", errHijack)
					return
				}
				_ = conn.Close()
			}))
			defer server.Close()

			quotaCalls := make(chan string, 2)
			executor := &claudeAffinityQuotaExecutor{
				ClaudeExecutor: NewClaudeExecutor(&config.Config{}),
				quotaHeaders:   make(http.Header),
				quotaCalls:     quotaCalls,
			}
			manager, selector, authA, _ := newClaudeAffinityIntegrationManager(t, executor, server.URL)
			opts := cliproxyexecutor.Options{
				Headers:      http.Header{"X-Session-ID": []string{"claude-ambiguous-session"}},
				SourceFormat: sdktranslator.FormatClaude,
				Stream:       streaming,
			}
			bindClaudeAffinityToA(t, selector, authA, opts)
			request := cliproxyexecutor.Request{
				Model:   claudeAffinityIntegrationModel,
				Payload: []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`),
			}
			if streaming {
				stream, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, request, opts)
				if errStream == nil {
					if stream == nil {
						t.Fatal("ExecuteStream() returned neither a stream nor an error")
					}
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							errStream = chunk.Err
							break
						}
					}
				}
				if errStream == nil {
					t.Fatal("ambiguous stream disconnect returned success")
				}
			} else {
				if _, errExecute := manager.Execute(context.Background(), []string{"claude"}, request, opts); errExecute == nil {
					t.Fatal("ambiguous nonstream disconnect returned success")
				}
			}
			if got := authACalls.Load(); got != 1 {
				t.Fatalf("credential A request count = %d, want 1", got)
			}
			if got := authBCalls.Load(); got != 0 {
				t.Fatalf("ambiguous accepted request replayed to B %d times, want 0", got)
			}
			select {
			case id := <-quotaCalls:
				t.Fatalf("ambiguous transport failure triggered quota refresh for %q", id)
			default:
			}
		})
	}
}

func TestClaudeStructuredWeeklyRateLimitWithoutHeadersIsCredentialScoped(t *testing.T) {
	err := classifyClaudeUpstreamErrorWithCooling(http.StatusTooManyRequests, make(http.Header), []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"You've hit your weekly limit."}}`), false)
	var scoped interface{ IsCredentialScoped() bool }
	if !errors.As(err, &scoped) || scoped == nil || !scoped.IsCredentialScoped() {
		t.Fatalf("structured weekly rate limit without headers = %T %v, want credential-scoped error", err, err)
	}
}

func TestClaudeFastDirectWeeklyRateLimitWithoutHeadersIsCredentialScoped(t *testing.T) {
	err := newClaudeFastDirectResponseError(&http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"You've hit your weekly limit."}}`))
	var scoped interface{ IsCredentialScoped() bool }
	if !errors.As(err, &scoped) || scoped == nil || !scoped.IsCredentialScoped() {
		t.Fatalf("Fast direct weekly rate limit without headers = %T %v, want credential-scoped error", err, err)
	}
}

func newClaudeAffinityIntegrationManager(t *testing.T, executor *claudeAffinityQuotaExecutor, baseURL string) (*cliproxyauth.Manager, *cliproxyauth.SessionAffinitySelector, *cliproxyauth.Auth, *cliproxyauth.Auth) {
	t.Helper()
	model := claudeAffinityIntegrationModel
	fallback := cliproxyauth.NewResetAwareSelector(cliproxyauth.ResetAwareSelectorConfig{
		Fallback:                       &cliproxyauth.RoundRobinSelector{},
		MinLongWindowRemainingPercent:  10,
		MinShortWindowRemainingPercent: 5,
		ReservePolicy:                  "last-resort",
		StaleTelemetryPolicy:           "fallback",
		TelemetryMaxAge:                time.Hour,
	})
	selector := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{Fallback: fallback, TTL: time.Hour})
	manager := cliproxyauth.NewManager(nil, selector, nil)
	manager.SetRetryConfig(0, 0, 2)
	manager.RegisterExecutor(executor)

	var auths []*cliproxyauth.Auth
	for _, suffix := range []string{"a", "b"} {
		auth := &cliproxyauth.Auth{
			ID:       "claude-integration-" + suffix + "-" + uuid.NewString(),
			Provider: "claude",
			Status:   cliproxyauth.StatusActive,
			Attributes: map[string]string{
				"api_key":  "test-only-" + suffix + "-key",
				"base_url": baseURL,
			},
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", suffix, errRegister)
		}
		auths = append(auths, auth)
	}
	return manager, selector, auths[0], auths[1]
}

func bindClaudeAffinityToA(t *testing.T, selector *cliproxyauth.SessionAffinitySelector, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) {
	t.Helper()
	selected, errPick := selector.Pick(context.Background(), "claude", claudeAffinityIntegrationModel, opts, []*cliproxyauth.Auth{auth})
	if errPick != nil || selected == nil || selected.ID != auth.ID {
		t.Fatalf("initial affinity pick = %v, %v; want %q", selected, errPick, auth.ID)
	}
	selector.OnResult(cliproxyauth.Result{AuthID: auth.ID, Provider: "claude", Model: claudeAffinityIntegrationModel, Options: opts, Success: true})
}

func claudeTestCredential(r *http.Request) string {
	if key := r.Header.Get("x-api-key"); key != "" {
		return key
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func writeClaudeWeekly429(w http.ResponseWriter, resetAt int64) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.2")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "1.0")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(resetAt, 10))
	w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")
	w.Header().Set("Retry-After", "126615")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."},"request_id":"req_test"}`))
}

func writeClaudeSuccessJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"msg_test","type":"message","model":%q,"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, claudeAffinityIntegrationModel)))
}

func writeClaudeSuccessStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_test","type":"message","model":"claude-haiku-4-5-20251001","role":"assistant","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1,"output_tokens":1}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
