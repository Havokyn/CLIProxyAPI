package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// FetchQuotaHeaders reads the same OAuth usage source as CPAMC. It uses the
// executor transport/proxy and credentials; it never consumes reset credits.
func (e *CodexExecutor) FetchQuotaHeaders(ctx context.Context, auth *coreauth.Auth) (http.Header, error) {
	if auth == nil || auth.Metadata == nil || auth.Attributes["api_key"] != "" {
		return nil, fmt.Errorf("Codex OAuth quota is unavailable")
	}
	token, _ := auth.Metadata["access_token"].(string)
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("Codex OAuth quota is unavailable")
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if errRequest != nil {
		return nil, fmt.Errorf("cannot create Codex quota request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "codex_cli_rs/0.101.0")
	if account, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(account) != "" {
		request.Header.Set("Chatgpt-Account-Id", strings.TrimSpace(account))
	}
	response, errResponse := e.HttpRequest(ctx, auth, request)
	if errResponse != nil {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		return nil, fmt.Errorf("Codex quota transport failed")
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.Debug("Codex quota response close failed")
		}
	}()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Codex quota HTTP %d", response.StatusCode)
	}
	const maxQuotaBody = 1024 * 1024
	payload, errRead := io.ReadAll(io.LimitReader(response.Body, maxQuotaBody+1))
	if errRead != nil || len(payload) > maxQuotaBody {
		return nil, fmt.Errorf("Codex quota response unreadable or oversized")
	}
	return helps.ParseCodexUsageHeaders(payload)
}

func (e *CodexAutoExecutor) FetchQuotaHeaders(ctx context.Context, auth *coreauth.Auth) (http.Header, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("Codex quota executor unavailable")
	}
	return e.httpExec.FetchQuotaHeaders(ctx, auth)
}
