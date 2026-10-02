package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigV8ResetAwarePatchRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\nserver: {port: 8317}\nupstream: {codex: {response-steering: true}}\nrouting: {strategy: reset-aware}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.GET("/v8/management/config", h.ConfigV8)
	router.PATCH("/v8/management/config", h.ConfigV8)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPatch, "/v8/management/config", strings.NewReader(`{"routing":{"reset-aware":{"preserve-session-affinity":false,"min-long-window-remaining-percent":0,"min-short-window-remaining-percent":0}}}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("PATCH status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	reloaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := reloaded.Routing.ResetAware
	if reloaded.Routing.Strategy != "reset-aware" || !reloaded.Codex.ResponseSteering || policy.PreserveSessionAffinity == nil || *policy.PreserveSessionAffinity || policy.MinLongWindowRemainingPercent == nil || *policy.MinLongWindowRemainingPercent != 0 || policy.MinShortWindowRemainingPercent == nil || *policy.MinShortWindowRemainingPercent != 0 {
		t.Fatal("v8 PATCH lost reset-aware or shared upstream settings")
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v8/management/config", nil))
	var wire map[string]any
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &wire) != nil {
		t.Fatal("v8 GET failed")
	}
	routing, ok := wire["routing"].(map[string]any)
	if !ok {
		t.Fatal("v8 GET omitted routing")
	}
	settings, ok := routing["reset-aware"].(map[string]any)
	if !ok || routing["strategy"] != "reset-aware" || settings["preserve-session-affinity"] != false || settings["min-long-window-remaining-percent"] != float64(0) || settings["min-short-window-remaining-percent"] != float64(0) {
		t.Fatal("v8 GET lost explicit false/zero policy")
	}
}
