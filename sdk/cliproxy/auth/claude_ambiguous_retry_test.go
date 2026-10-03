package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	claudeAmbiguousRouteModel  = "public-claude-pooled-model"
	claudeAmbiguousFirstModel  = "claude-upstream-a"
	claudeAmbiguousSecondModel = "claude-upstream-b"
	claudeAmbiguousExecutorKey = "openai-compatible-claude"
)

type claudeAmbiguousAttempt struct {
	authID string
	model  string
}

type claudeAmbiguousRefreshExecutor struct {
	mu sync.Mutex

	executeAttempts []claudeAmbiguousAttempt
	streamAttempts  []claudeAmbiguousAttempt
	refreshCalls    int
}

func (*claudeAmbiguousRefreshExecutor) Identifier() string { return "openai-compatible-claude" }

func (e *claudeAmbiguousRefreshExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeAttempts = append(e.executeAttempts, claudeAmbiguousAttempt{authID: auth.ID, model: req.Model})
	callNumber := len(e.executeAttempts)
	e.mu.Unlock()
	if callNumber == 1 {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "401 unauthorized: expired access token"}
	}
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	return cliproxyexecutor.Response{}, errors.New("connection reset after dispatch")
}

func (e *claudeAmbiguousRefreshExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamAttempts = append(e.streamAttempts, claudeAmbiguousAttempt{authID: auth.ID, model: req.Model})
	callNumber := len(e.streamAttempts)
	e.mu.Unlock()
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	if callNumber == 1 {
		chunks <- cliproxyexecutor.StreamChunk{Err: &Error{HTTPStatus: http.StatusUnauthorized, Message: "401 unauthorized: expired access token"}}
	} else {
		chunks <- cliproxyexecutor.StreamChunk{Err: errors.New("connection reset before first payload")}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *claudeAmbiguousRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.refreshCalls++
	e.mu.Unlock()
	refreshed := auth.Clone()
	if refreshed.Metadata == nil {
		refreshed.Metadata = make(map[string]any)
	}
	refreshed.Metadata["access_token"] = "test-only-fresh-token"
	return refreshed, nil
}

func (*claudeAmbiguousRefreshExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (*claudeAmbiguousRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *claudeAmbiguousRefreshExecutor) attempts() ([]claudeAmbiguousAttempt, []claudeAmbiguousAttempt, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	execute := append([]claudeAmbiguousAttempt(nil), e.executeAttempts...)
	stream := append([]claudeAmbiguousAttempt(nil), e.streamAttempts...)
	return execute, stream, e.refreshCalls
}

func TestClaudeUnauthorizedRefreshStopsOnAmbiguousRetryBeforePooledAliasRotation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		streaming    bool
		reservations int
	}{
		{"execute", false, 1},
		{"stream-bootstrap", true, 1},
		{"execute-concurrent", false, 2},
		{"stream-bootstrap-concurrent", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streaming := tc.streaming
			executor := &claudeAmbiguousRefreshExecutor{}
			resetAware := NewResetAwareSelector(ResetAwareSelectorConfig{
				Fallback:                       &RoundRobinSelector{},
				MinLongWindowRemainingPercent:  10,
				MinShortWindowRemainingPercent: 5,
				StaleTelemetryPolicy:           "fallback",
				TelemetryMaxAge:                time.Hour,
			})
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: resetAware})
			manager := NewManager(nil, selector, nil)
			manager.SetRetryConfig(0, 0, 2)
			manager.SetConfig(&internalconfig.Config{OpenAICompatibility: []internalconfig.OpenAICompatibility{{
				Name: "claude-pool",
				Models: []internalconfig.OpenAICompatibilityModel{
					{Name: claudeAmbiguousFirstModel, Alias: claudeAmbiguousRouteModel},
					{Name: claudeAmbiguousSecondModel, Alias: claudeAmbiguousRouteModel},
				},
			}}})
			manager.RegisterExecutor(executor)

			primary := claudeAmbiguousAuth("primary")
			now := time.Now()
			primary.Quota = QuotaState{ObservedAt: now, Windows: []QuotaWindow{{Name: "weekly", Provider: claudeAmbiguousExecutorKey, RemainingPercent: 100, ResetAt: now.Add(7 * 24 * time.Hour), ObservedAt: now, DurationSeconds: int64((7 * 24 * time.Hour) / time.Second)}}}
			backup := claudeAmbiguousAuth("backup")
			registerClaudeAmbiguousAuth(t, manager, primary)
			registerClaudeAmbiguousAuth(t, manager, backup)
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": []string{"ambiguous-refresh-session"}}, Stream: streaming}
			bind, errBind := selector.Pick(context.Background(), "mixed", claudeAmbiguousRouteModel, opts, []*Auth{primary})
			if errBind != nil || bind == nil || bind.ID != primary.ID {
				t.Fatalf("initial affinity pick = %v, %v; want primary", bind, errBind)
			}
			if tc.reservations == 2 {
				if _, errPick := resetAware.Pick(context.Background(), "mixed", claudeAmbiguousRouteModel, cliproxyexecutor.Options{}, []*Auth{primary}); errPick != nil {
					t.Fatal(errPick)
				}
			}
			assertClaudeAmbiguousReservationCount(t, resetAware, primary, tc.reservations)

			request := cliproxyexecutor.Request{Model: claudeAmbiguousRouteModel, Payload: []byte(`{"model":"public-claude-pooled-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)}
			var errExecute error
			if streaming {
				stream, errStream := manager.ExecuteStream(context.Background(), []string{claudeAmbiguousExecutorKey}, request, opts)
				errExecute = errStream
				if stream != nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil && errExecute == nil {
							errExecute = chunk.Err
						}
					}
				}
			} else {
				_, errExecute = manager.Execute(context.Background(), []string{claudeAmbiguousExecutorKey}, request, opts)
			}
			if errExecute == nil {
				t.Fatal("ambiguous post-refresh retry returned success")
			}
			assertClaudeAmbiguousReservationCount(t, resetAware, primary, tc.reservations-1)

			executeAttempts, streamAttempts, refreshCalls := executor.attempts()
			if refreshCalls != 1 {
				t.Fatalf("OAuth refresh calls = %d, want 1; execute=%+v stream=%+v", refreshCalls, executeAttempts, streamAttempts)
			}
			if streaming {
				if len(executeAttempts) != 0 {
					t.Fatalf("Execute() unexpectedly ran during stream test: %+v", executeAttempts)
				}
				if len(streamAttempts) != 2 {
					t.Fatalf("stream attempts = %+v, want initial 401 and one refreshed retry", streamAttempts)
				}
				if streamAttempts[0].authID != primary.ID || streamAttempts[1].authID != primary.ID || streamAttempts[0].model != streamAttempts[1].model {
					t.Fatalf("ambiguous retry rotated account or pooled model: %+v", streamAttempts)
				}
				if streamAttempts[0].model != claudeAmbiguousFirstModel && streamAttempts[0].model != claudeAmbiguousSecondModel {
					t.Fatalf("unexpected pooled model attempts: %+v", streamAttempts)
				}
			} else {
				if len(streamAttempts) != 0 {
					t.Fatalf("ExecuteStream() unexpectedly ran during nonstream test: %+v", streamAttempts)
				}
				if len(executeAttempts) != 2 {
					t.Fatalf("execute attempts = %+v, want initial 401 and one refreshed retry", executeAttempts)
				}
				if executeAttempts[0].authID != primary.ID || executeAttempts[1].authID != primary.ID || executeAttempts[0].model != executeAttempts[1].model {
					t.Fatalf("ambiguous retry rotated account or pooled model: %+v", executeAttempts)
				}
				if executeAttempts[0].model != claudeAmbiguousFirstModel && executeAttempts[0].model != claudeAmbiguousSecondModel {
					t.Fatalf("unexpected pooled model attempts: %+v", executeAttempts)
				}
			}
		})
	}
}

func assertClaudeAmbiguousReservationCount(t *testing.T, selector *ResetAwareSelector, auth *Auth, want int) {
	t.Helper()
	candidates := selector.Explain("mixed", claudeAmbiguousRouteModel, []*Auth{auth}, time.Now())
	if len(candidates) != 1 || candidates[0].ActiveReservationCount != want {
		t.Fatalf("reset-aware reservations = %+v, want count %d", candidates, want)
	}
}

func claudeAmbiguousAuth(label string) *Auth {
	return &Auth{
		ID:       "claude-ambiguous-" + label + "-" + uuid.NewString(),
		Provider: "claude",
		Status:   StatusActive,
		Attributes: map[string]string{
			"auth_kind":    AuthKindOAuth,
			"compat_name":  "claude-pool",
			"provider_key": "claude",
			"source":       AuthSourceConfig,
		},
		Metadata: map[string]any{
			"access_token":  "test-only-stale-token",
			"refresh_token": "test-only-refresh-token",
		},
	}
}

func registerClaudeAmbiguousAuth(t *testing.T, manager *Manager, auth *Auth) {
	t.Helper()
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: claudeAmbiguousRouteModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register Claude auth: %v", errRegister)
	}
}
