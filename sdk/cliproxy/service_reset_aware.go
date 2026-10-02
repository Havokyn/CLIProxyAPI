package cliproxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// configureResetAwareQuotaRefresh attaches the central plugin-backed quota
// refresh path to the active reset-aware selector. Header-only providers still
// use their passive response observations; providers with a registered quota
// capability can be refreshed when a recorded reset timestamp has passed.
func (s *Service) configureResetAwareQuotaRefresh() {
	if s == nil || s.coreManager == nil {
		return
	}
	selector := coreauth.ResetAwareSelectorFrom(s.coreManager.Selector())
	if selector == nil {
		return
	}
	selector.SetQuotaRefreshFunc(s.refreshResetAwareQuota)
}

func (s *Service) refreshResetAwareQuota(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if s == nil || s.coreManager == nil || auth == nil {
		return nil, fmt.Errorf("authoritative quota refresh is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	provider := strings.TrimSpace(auth.Provider)
	if strings.EqualFold(provider, "codex") {
		return s.coreManager.StartCredentialQuotaRefresh(ctx, auth)
	}
	if s.pluginHost == nil {
		return nil, fmt.Errorf("authoritative quota refresh is unavailable")
	}
	response, handled, errFetch := s.pluginHost.FetchQuota(ctx, pluginapi.QuotaFetchRequest{
		AuthIndex:  auth.Index,
		AuthID:     auth.ID,
		Provider:   provider,
		Metadata:   auth.Metadata,
		Attributes: auth.Attributes,
	})
	if errFetch != nil {
		return nil, fmt.Errorf("fetch quota for %s: %w", provider, errFetch)
	}
	if !handled {
		return nil, fmt.Errorf("no authoritative quota provider for %s", provider)
	}
	now := time.Now().UTC()
	windows := coreauth.QuotaWindowsFromNormalizedGroups(provider, response.Groups, now)
	if len(windows) == 0 {
		return nil, fmt.Errorf("authoritative quota provider returned no usable reset windows")
	}
	refreshed, errUpdate := s.coreManager.RecordQuotaObservation(auth, coreauth.QuotaState{ObservedAt: now, Windows: windows})
	if errUpdate != nil {
		return nil, fmt.Errorf("store refreshed quota: %w", errUpdate)
	}
	return refreshed, nil
}
