package service

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"net/http"
	"strings"
	"testing"
	"time"
)

func codexTurnStateTestAccount() *Account {
	now := time.Now()
	return &Account{ID: 4242, Platform: PlatformOpenAI, Type: AccountTypeSetupToken,
		Credentials: map[string]any{"access_token": "review-token", "plan_type": "plus", "model_mapping": map[string]any{"gpt-5.4": "gpt-5.4"}},
		Extra: map[string]any{CodexTurnStateProbeEnabledExtraKey: true, CodexTurnStateProbeProxyIDExtraKey: int64(23),
			CodexTurnStateProbeCacheExtraKey: codexTurnStateCacheToExtra(map[string]codexTurnStateCacheEntry{
				"gpt-5.4": {State: strings.Repeat("a", 292), ObtainedAt: now, ExpiresAt: now.Add(time.Hour), ProxyID: 23, CredentialHash: codexTurnStateCredentialHash("review-token")},
			})},
	}
}

func TestCodexTurnStateUnchangedCredentialsPreserveCandidate(t *testing.T) {
	account := codexTurnStateTestAccount()
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	svc := &adminServiceImpl{accountRepo: repo}
	updated, err := svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{Name: "renamed", Credentials: mergeMap(nil, account.Credentials)})
	require.NoError(t, err)
	require.Contains(t, updated.Extra, CodexTurnStateProbeCacheExtraKey, "UI always resubmits unchanged credentials even when only renaming")
}

func TestCodexTurnStateOutboundModelMustNotBeMappedTwice(t *testing.T) {
	account := codexTurnStateTestAccount()
	account.Credentials["plan_type"] = "plus"
	account.Credentials["model_mapping"] = map[string]any{"alias": "gpt-5.4", "gpt-5.4": "gpt-5.5"}
	cache := codexTurnStateCacheFromExtra(account.Extra)
	entry := cache["gpt-5.4"]
	entry.State = strings.Repeat("b", 292)
	cache["gpt-5.5"] = entry
	account.Extra[CodexTurnStateProbeCacheExtraKey] = codexTurnStateCacheToExtra(cache)
	outboundModel := account.GetMappedModel("alias")
	require.Equal(t, "gpt-5.4", outboundModel)
	headers := make(http.Header)
	ApplyConfiguredCodexTurnState(account, headers, outboundModel, "review-token")
	require.Equal(t, strings.Repeat("a", 292), headers.Get(openAICodexTurnStateHeader), "a gpt-5.4 outbound request must use gpt-5.4 state")
}

type codexTurnStateTestProxyRepo struct{ ProxyRepository }

func (*codexTurnStateTestProxyRepo) GetByID(context.Context, int64) (*Proxy, error) {
	return &Proxy{ID: 23, Protocol: "http", Host: "proxy.example", Port: 8080, Status: StatusActive}, nil
}

func TestCodexTurnStateProbeUsesEachMappedTarget(t *testing.T) {
	account := codexTurnStateTestAccount()
	account.Credentials["plan_type"] = "plus"
	account.Credentials["model_mapping"] = map[string]any{"alias": "gpt-5.4", "gpt-5.4": "gpt-5.5"}
	delete(account.Extra, CodexTurnStateProbeCacheExtraKey)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": []string{strings.Repeat("s", 292)}}}}
	svc := NewCodexTurnStateProbeService(repo, &codexTurnStateTestProxyRepo{}, nil, upstream)
	defer svc.Stop()
	require.NoError(t, svc.refreshAccount(context.Background(), account, CodexTurnStateProbeConfig{Enabled: true, ProxyID: 23}))
	var models []string
	for _, body := range upstream.bodies {
		models = append(models, gjson.GetBytes(body, "model").String())
	}
	require.ElementsMatch(t, []string{"gpt-5.4", "gpt-5.5"}, models)
}
