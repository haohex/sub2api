package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type statePoolSettingsRepo struct {
	SettingRepository
	mu    sync.Mutex
	value string
}

func (r *statePoolSettingsRepo) GetValue(context.Context, string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.value, nil
}
func (r *statePoolSettingsRepo) Set(_ context.Context, _ string, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.value = value
	return nil
}

type statePoolAccountRepo struct {
	*upstreamBillingProbeAccountRepo
}

func (r *statePoolAccountRepo) ListByPlatform(context.Context, string) ([]Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []Account
	for _, a := range r.accounts {
		copyAccount := *a
		copyAccount.Extra = mergeMap(nil, a.Extra)
		result = append(result, copyAccount)
	}
	return result, nil
}
func (r *statePoolAccountRepo) ListActive(ctx context.Context) ([]Account, error) {
	return r.ListByPlatform(ctx, PlatformOpenAI)
}

func TestCodexStatePlanLengthRules(t *testing.T) {
	for _, tc := range []struct {
		plan   string
		length int
	}{{"plus", 292}, {"pro", 292}, {"chatgpt_pro", 292}, {"prolite", 292}, {"pro_5x", 292}, {"self_serve_business_prolite", 0}, {"pro5x", 292}, {"pro20x", 292}, {"team", 332}, {"", 0}, {"free", 0}, {"enterprise", 0}} {
		t.Run(tc.plan, func(t *testing.T) {
			a := codexTurnStateTestAccount()
			a.Credentials["plan_type"] = tc.plan
			require.Equal(t, tc.length, CodexTurnStateHealthyLength(a))
		})
	}
}

func TestCodexStateTeamMetadataAndNoMintAreNotConfused(t *testing.T) {
	a := codexTurnStateTestAccount()
	a.Credentials["plan_type"] = "team"
	cache := codexTurnStateCacheFromExtra(a.Extra)
	entry := cache["gpt-5.4"]
	entry.State = codexTestState("t", 332)
	cache["gpt-5.4"] = entry
	a.Extra[CodexTurnStateProbeCacheExtraKey] = codexTurnStateCacheToExtra(cache)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{a.ID: a}}
	candidate := codexTurnStateCandidateSent(a, "gpt-5.4", entry.State)
	obs := newCodexTurnStateObservation(context.Background(), repo, candidate, nil)
	for range 2 {
		obs.observeEvent([]byte(`{"type":"codex.response.metadata","headers":{"x-models-etag":"etag","x-codex-safety-buffering-enabled":"true","x-codex-safety-buffering-faster-model":"gpt-5.6-luna"}}`), "")
		obs.observeEvent([]byte(`{"type":"response.completed","response":{"model":"gpt-6-astra"}}`), "")
	}
	require.Contains(t, codexTurnStateCacheFromExtra(a.Extra), "gpt-5.4")
	obs.observeEvent([]byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"`+strings.Repeat("b", 356)+`"}}`), "")
	require.Empty(t, codexTurnStateCacheFromExtra(a.Extra))
}

func TestCodexStateTwoTurnsReuseFrameCandidateWithoutMint(t *testing.T) {
	account := codexTurnStateTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"gpt-6-astra": "gpt-6-astra"}
	entry := codexTurnStateCacheFromExtra(account.Extra)["gpt-5.4"]
	account.Extra[CodexTurnStateProbeCacheExtraKey] = codexTurnStateCacheToExtra(map[string]codexTurnStateCacheEntry{"gpt-6-astra": entry})
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	svc := &OpenAIGatewayService{accountRepo: repo}
	for _, previous := range []string{"", "resp_1"} {
		payload := map[string]any{"type": "response.create", "model": "gpt-6-astra", "client_metadata": map[string]string{"session_id": "same-session", "x-codex-turn-state": "old-client-value"}}
		if previous != "" {
			payload["previous_response_id"] = previous
		}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		out, observer := svc.prepareCodexTurnStateWSFrame(context.Background(), nil, account, raw, "", "review-token", nil)
		var sent map[string]any
		require.NoError(t, json.Unmarshal(out, &sent))
		metadata, ok := sent["client_metadata"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, entry.State, metadata[openAICodexTurnStateHeader])
		require.NotContains(t, sent, "headers")
		require.NotContains(t, sent, openAICodexTurnStateHeader)
		if previous != "" {
			require.Equal(t, previous, sent["previous_response_id"])
		}
		observer.observeEvent([]byte(`{"type":"codex.response.metadata","headers":{"x-models-etag":"etag","x-codex-safety-buffering-enabled":"true","x-codex-safety-buffering-faster-model":"gpt-5.6-luna"}}`), "")
		observer.observeEvent([]byte(`{"type":"response.created","response":{"model":"gpt-6-astra"}}`), "")
		observer.observeEvent([]byte(`{"type":"response.completed","response":{"model":"gpt-6-astra"}}`), "")
		require.Contains(t, codexTurnStateCacheFromExtra(account.Extra), "gpt-6-astra")
	}
}
