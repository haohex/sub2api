//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeRequestBodyOverrideCredentials(t *testing.T) {
	creds := map[string]any{
		credKeyRequestBodyOverrides: map[string]any{
			" alias-model ": map[string]any{
				"providerOptions": map[string]any{"gateway": map[string]any{"only": []any{"deepseek"}}},
				"provider":        map[string]any{"only": []any{"deepseek"}},
			},
		},
	}

	require.NoError(t, NormalizeRequestBodyOverrideCredentials(creds))
	normalized, ok := creds[credKeyRequestBodyOverrides].(map[string]any)
	require.True(t, ok)
	require.Contains(t, normalized, "alias-model")

	for _, field := range []string{"model", "messages", "input", "stream", "instructions", "tools", "tool_choice", "previous_response_id", "prompt_cache_key"} {
		err := NormalizeRequestBodyOverrideCredentials(map[string]any{
			credKeyRequestBodyOverrides: map[string]any{
				"alias": map[string]any{field: true},
			},
		})
		require.Error(t, err, "field %q must be protected", field)
	}
}

func TestNormalizeRequestBodyOverrideCredentialsRejectsInvalidShape(t *testing.T) {
	tests := []map[string]any{
		{credKeyRequestBodyOverrides: []any{"alias"}},
		{credKeyRequestBodyOverrides: map[string]any{"*": map[string]any{"provider": true}}},
		{credKeyRequestBodyOverrides: map[string]any{"alias*bad": map[string]any{"provider": true}}},
		{credKeyRequestBodyOverrides: map[string]any{"alias": []any{"provider"}}},
	}
	for _, creds := range tests {
		require.Error(t, NormalizeRequestBodyOverrideCredentials(creds))
	}
}

func TestApplyRequestBodyOverridesExactAndWildcardPriority(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			credKeyRequestBodyOverrides: map[string]any{
				"deepseek-*": map[string]any{
					"provider": "wildcard",
					"extra":    "kept-wildcard",
				},
				"deepseek-v4": map[string]any{
					"provider": "exact",
				},
			},
		},
	}

	body := []byte(`{"model":"deepseek-v4","messages":[{"role":"user","content":"hi"}],"provider":"client","keep":true}`)
	updated := account.ApplyRequestBodyOverrides(body, "deepseek-v4")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(updated, &payload))
	require.Equal(t, "exact", payload["provider"])
	require.NotContains(t, payload, "extra")
	require.Equal(t, "deepseek-v4", payload["model"])
	require.Equal(t, true, payload["keep"])

	wildcard := account.ApplyRequestBodyOverrides([]byte(`{"model":"deepseek-v5"}`), "deepseek-v5")
	require.NoError(t, json.Unmarshal(wildcard, &payload))
	require.Equal(t, "wildcard", payload["provider"])
	require.Equal(t, "kept-wildcard", payload["extra"])
}

func TestApplyRequestBodyOverridesNoOpForIneligibleAccount(t *testing.T) {
	body := []byte(`{"model":"alias","provider":"client"}`)
	for _, account := range []*Account{
		{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
			credKeyRequestBodyOverrides: map[string]any{"alias": map[string]any{"provider": "forced"}},
		}},
		{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{
			credKeyRequestBodyOverrides: map[string]any{"alias": map[string]any{"provider": "forced"}},
		}},
	} {
		require.Equal(t, body, account.ApplyRequestBodyOverrides(body, "alias"))
	}
}
