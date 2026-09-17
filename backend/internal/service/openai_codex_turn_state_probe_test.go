package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestApplyConfiguredCodexTurnState_IsolatedByModelTokenAndTTL(t *testing.T) {
	now := time.Now()
	state := strings.Repeat("s", codexTurnStateLength)
	account := &Account{
		ID:       7,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "token-a",
			"model_mapping": map[string]any{"gpt-5.5": "gpt-5.5"},
		},
		Extra: map[string]any{
			CodexTurnStateProbeEnabledExtraKey: true,
			CodexTurnStateProbeProxyIDExtraKey: int64(99),
			CodexTurnStateProbeCacheExtraKey: map[string]any{
				"gpt-5.5": map[string]any{
					"state":           state,
					"obtained_at":     now.Add(-time.Minute).Format(time.RFC3339Nano),
					"expires_at":      now.Add(59 * time.Minute).Format(time.RFC3339Nano),
					"proxy_id":        int64(99),
					"credential_hash": codexTurnStateCredentialHash("token-a"),
				},
			},
		},
	}

	headers := http.Header{"X-Codex-Turn-State": []string{"client-state"}}
	ApplyConfiguredCodexTurnState(account, headers, "gpt-5.5", "token-a")
	require.Equal(t, state, headers.Get(openAICodexTurnStateHeader))

	headers.Set(openAICodexTurnStateHeader, "client-state")
	ApplyConfiguredCodexTurnState(account, headers, "gpt-5.4", "token-a")
	require.Equal(t, "client-state", headers.Get(openAICodexTurnStateHeader), "another model must not reuse the candidate")

	headers.Set(openAICodexTurnStateHeader, "client-state")
	ApplyConfiguredCodexTurnState(account, headers, "gpt-5.5", "token-b")
	require.Equal(t, "client-state", headers.Get(openAICodexTurnStateHeader), "another token must not reuse the candidate")

	account.Extra[CodexTurnStateProbeCacheExtraKey].(map[string]any)["gpt-5.5"].(map[string]any)["expires_at"] = now.Add(-time.Second).Format(time.RFC3339Nano)
	headers.Set(openAICodexTurnStateHeader, "client-state")
	ApplyConfiguredCodexTurnState(account, headers, "gpt-5.5", "token-a")
	require.Equal(t, "client-state", headers.Get(openAICodexTurnStateHeader), "expired candidates must not be injected")
}

func TestCodexTurnStateCacheNeedsRefreshFiveMinutesBeforeExpiry(t *testing.T) {
	now := time.Now()
	entry := codexTurnStateCacheEntry{
		ObtainedAt: now,
		ExpiresAt:  now.Add(codexTurnStateTTL),
	}
	require.False(t, codexTurnStateCacheNeedsRefresh(entry, now.Add(54*time.Minute)))
	require.True(t, codexTurnStateCacheNeedsRefresh(entry, now.Add(55*time.Minute)))
	require.True(t, codexTurnStateCacheNeedsRefresh(entry, now.Add(59*time.Minute)))
}

func TestCodexTurnStateProbe_ClosesSSEAfterResponseHeaders(t *testing.T) {
	state := strings.Repeat("s", codexTurnStateLength)
	body := &passthroughCloseTrackingReadCloser{Reader: strings.NewReader("data: keep-open\n")}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Codex-Turn-State": []string{state}},
		Body:       body,
	}}
	service := &CodexTurnStateProbeService{httpUpstream: upstream}
	account := &Account{
		ID:          11,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "token"},
	}
	proxy := &Proxy{ID: 23, Protocol: "http", Host: "proxy.example", Port: 8080, Status: StatusActive}

	got, _, err := service.probeOnce(context.Background(), account, proxy, "token", "gpt-5.5")
	require.NoError(t, err)
	require.Equal(t, state, got)
	require.True(t, body.closed, "probe must close the SSE body immediately after headers")
	require.Equal(t, proxy.URL(), upstream.lastProxyURL)
}

func TestConfiguredCodexTurnStateOverridesDeviceWireFrame(t *testing.T) {
	now := time.Now()
	state := strings.Repeat("s", codexTurnStateLength)
	account := wireProfileTestAccount(true)
	account.Credentials["model_mapping"] = map[string]any{"gpt-5.5": "gpt-5.5"}
	account.Extra[CodexTurnStateProbeEnabledExtraKey] = true
	account.Extra[CodexTurnStateProbeProxyIDExtraKey] = int64(23)
	account.Extra[CodexTurnStateProbeCacheExtraKey] = map[string]any{
		"gpt-5.5": map[string]any{
			"state":           state,
			"obtained_at":     now.Add(-time.Minute).Format(time.RFC3339Nano),
			"expires_at":      now.Add(59 * time.Minute).Format(time.RFC3339Nano),
			"proxy_id":        int64(23),
			"credential_hash": codexTurnStateCredentialHash("offline-token"),
		},
	}
	c := newConvTestContext(t, nil)
	frame := []byte(`{"type":"response.create","model":"gpt-5.5","client_metadata":{"x-codex-turn-state":"client-state"}}`)
	out := applyCodexWSFrameWireProfile(c, account, frame, "client-state", "offline-token")
	require.Equal(t, state, gjson.GetBytes(out, "client_metadata."+openAICodexTurnStateHeader).String())
}
