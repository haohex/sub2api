//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexTurnStateProbeTakesPriorityOverMainAutoAndManual(t *testing.T) {
	account := codexTurnStateTestAccount()
	account.Extra[openAITurnStateAutoExtraKey] = true
	account.Extra[openAITurnStateOverrideExtraKey] = "legacy-manual"
	account.Extra[openAITurnStatePoolExtraKey] = []any{map[string]any{"blob": "legacy-auto", "minted_at": time.Now().UTC().Format(time.RFC3339Nano)}}
	repo := newTurnStateAutoRepo()
	svc := &OpenAIGatewayService{accountRepo: repo}
	c := turnStateAutoCtx("probe-wins")
	svc.setSessionTurnStateNeedsInjection(openAITurnStateSessionKey(c, account, "probe-wins"), true)
	state, source := svc.resolveOpenAITurnStateOverride(c, account)
	require.Empty(t, state)
	require.Empty(t, source)
	require.Equal(t, "client-state", svc.applyOpenAICodexTurnStateOverrideWSManualOnly(c, account, "client-state"))
	// A response from a probe-managed account cannot make the legacy pool retire
	// candidates or stop scheduling this account.
	markOpenAITurnStateInjected(c, "legacy-auto", turnStateSourceAuto)
	svc.observeOpenAITurnStateMint(c, account, strings.Repeat("x", 312))
	require.Empty(t, repo.extraWrites)
	require.Empty(t, repo.schedulable)
	require.Empty(t, repo.errors)
	// Priority applies while waiting for the first candidate as well.
	delete(account.Extra, CodexTurnStateProbeCacheExtraKey)
	state, source = svc.resolveOpenAITurnStateOverride(c, account)
	require.Empty(t, state)
	require.Empty(t, source)
	// Disabling our feature must not reactivate retired legacy settings.
	account.Extra[CodexTurnStateProbeEnabledExtraKey] = false
	fresh := turnStateAutoCtx("probe-wins")
	state, source = svc.resolveOpenAITurnStateOverride(fresh, account)
	require.Empty(t, state)
	require.Empty(t, source)
}

func TestCodexTurnStateProbePreservesMainUsageSource(t *testing.T) {
	account := codexTurnStateTestAccount()
	c := turnStateAutoCtx("probe-usage")
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	applyConfiguredCodexTurnStateToRequest(account, req, "gpt-5.4", "review-token")
	noteCodexTurnStateProbeUsage(c, req)
	require.Equal(t, "probe", OpenAITurnStateUsageSource(c))
	require.True(t, *usageCodexTurnStateOverriddenPtr(account, OpenAITurnStateUsageSource(c)))

	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	svc := &OpenAIGatewayService{accountRepo: repo}
	out, observer := svc.prepareCodexTurnStateWSFrame(context.Background(), c, account, []byte(`{"type":"response.create","model":"gpt-5.4"}`), "client-state", "review-token", nil)
	require.NotNil(t, observer)
	require.Equal(t, codexTestState("a", 292), gjson.GetBytes(out, "client_metadata."+openAICodexTurnStateHeader).String())
	require.Equal(t, "probe", OpenAITurnStateUsageSource(c))
	delete(account.Extra, CodexTurnStateProbeCacheExtraKey)
	_, observer = svc.prepareCodexTurnStateWSFrame(context.Background(), c, account, []byte(`{"type":"response.create","model":"gpt-5.4"}`), "client-state", "review-token", nil)
	require.Nil(t, observer.invalidate)
	require.Empty(t, OpenAITurnStateUsageSource(c), "a later WS turn without injection must not retain the previous usage marker")
}
