//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCodexTurnStateRepositoryConditionalWritesAndEdits(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	candidate := func(state string) map[string]any {
		now := time.Now().UTC()
		return map[string]any{"gpt-5.4": map[string]any{"state": strings.Repeat(state, 292), "obtained_at": now.Format(time.RFC3339Nano), "expires_at": now.Add(time.Hour).Format(time.RFC3339Nano), "proxy_id": 23, "credential_hash": "test-hash"}}
	}
	account := &service.Account{Name: "turn-state-cas", Platform: service.PlatformOpenAI, Type: service.AccountTypeSetupToken, Status: service.StatusActive, Concurrency: 1, Priority: 50,
		Credentials: map[string]any{"access_token": "test-token", "model_mapping": map[string]any{"gpt-5.4": "gpt-5.4"}},
		Extra:       map[string]any{service.CodexTurnStateProbeEnabledExtraKey: true, service.CodexTurnStateProbeProxyIDExtraKey: 23, service.CodexTurnStateProbeCacheExtraKey: candidate("a"), "unrelated_null": nil},
	}
	require.NoError(t, repo.Create(ctx, account))
	old, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	replacement := candidate("b")
	updates := map[string]any{service.CodexTurnStateProbeCacheExtraKey: replacement, service.CodexTurnStateProbeFailureExtraKey: nil}
	require.NoError(t, repo.UpdateExtra(ctx, account.ID, map[string]any{"quota_used": 3.5}))
	saved, err := repo.CompareAndSwapCodexTurnStateProbe(ctx, old, updates)
	require.NoError(t, err)
	require.True(t, saved)
	saved, err = repo.CompareAndSwapCodexTurnStateProbe(ctx, old, map[string]any{service.CodexTurnStateProbeCacheExtraKey: nil, service.CodexTurnStateProbeFailureExtraKey: nil})
	require.NoError(t, err)
	require.False(t, saved, "late failure cannot delete a renewed candidate")

	// An ordinary form save can have read the old candidate before the renewal.
	old.Name = "renamed"
	require.NoError(t, repo.Update(ctx, old))
	current, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	cache := current.Extra[service.CodexTurnStateProbeCacheExtraKey].(map[string]any)
	require.Equal(t, strings.Repeat("b", 292), cache["gpt-5.4"].(map[string]any)["state"])
	require.NotContains(t, current.Extra, service.CodexTurnStateProbeFailureExtraKey)
	require.Contains(t, current.Extra, "unrelated_null")

	saved, err = repo.CompareAndSwapCodexTurnStateProbe(ctx, current, map[string]any{service.CodexTurnStateProbeCacheExtraKey: nil, service.CodexTurnStateProbeFailureExtraKey: nil})
	require.NoError(t, err)
	require.True(t, saved)
	current.Name = "rename after invalidation"
	require.NoError(t, repo.Update(ctx, current))
	current, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.NotContains(t, current.Extra, service.CodexTurnStateProbeCacheExtraKey, "stale form cannot resurrect an invalidated candidate")

	stale := *current
	current.Credentials = map[string]any{"access_token": "different-owner-token", "model_mapping": map[string]any{"gpt-5.4": "gpt-5.4"}}
	require.NoError(t, repo.Update(ctx, current))
	saved, err = repo.CompareAndSwapCodexTurnStateProbe(ctx, &stale, updates)
	require.NoError(t, err)
	require.False(t, saved, "probe under previous credentials cannot publish")
}
