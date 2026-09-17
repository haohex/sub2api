package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Model the repository CAS, including independent runtime fields and identity.
func (r *upstreamBillingProbeAccountRepo) CompareAndSwapCodexTurnStateProbe(_ context.Context, expected *Account, updates map[string]any) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.accounts[expected.ID]
	if current == nil {
		return false, ErrAccountNotFound
	}
	if current.Platform != expected.Platform || current.Type != expected.Type || !reflect.DeepEqual(current.Credentials, expected.Credentials) {
		return false, nil
	}
	for _, key := range []string{CodexTurnStateProbeEnabledExtraKey, CodexTurnStateProbeProxyIDExtraKey, CodexTurnStateProbeCacheExtraKey, CodexTurnStateProbeFailureExtraKey} {
		if !reflect.DeepEqual(current.Extra[key], expected.Extra[key]) {
			return false, nil
		}
	}
	extra := mergeMap(nil, current.Extra)
	for key, value := range updates {
		if value == nil {
			delete(extra, key)
		} else {
			extra[key] = value
		}
	}
	current.Extra = extra
	return true, nil
}

func TestCodexTurnStateEarlyInvalidationSignals(t *testing.T) {
	for _, tc := range []struct {
		name, payload, header string
		invalid               bool
	}{
		{name: "created downgraded model", payload: `{"type":"response.created","response":{"model":"gpt-5.6-luna"}}`, invalid: true},
		{name: "overloaded failed event", payload: `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`, invalid: true},
		{name: "overloaded error event", payload: `{"type":"error","code":"server_is_overloaded"}`, invalid: true},
		{name: "312 byte response header", header: strings.Repeat("x", 312), invalid: true},
		{name: "312 byte WS metadata", payload: `{"type":"response.metadata","headers":{"X-Codex-Turn-State":"` + strings.Repeat("x", 312) + `"}}`, invalid: true},
		{name: "normal completion", payload: `{"type":"response.completed","response":{"model":"gpt-6-astra"}}`},
		{name: "normal header", header: strings.Repeat("x", 292)},
		{name: "quoted error is not a signal", payload: `{"type":"response.output_text.delta","delta":"server_is_overloaded gpt-5.6-luna"}`},
		{name: "another error", payload: `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`},
		{name: "other model mismatch is not the specified signal", payload: `{"type":"response.created","response":{"model":"gpt-5.5"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := codexTurnStateTestAccount()
			cache := codexTurnStateCacheFromExtra(account.Extra)
			cache["gpt-5.5"] = cache["gpt-5.4"]
			account.Extra[CodexTurnStateProbeCacheExtraKey] = codexTurnStateCacheToExtra(cache)
			account.Extra[CodexTurnStateProbeFailureExtraKey] = codexTurnStateProbeFailuresToExtra(map[string]codexTurnStateProbeFailure{"gpt-5.4": {Attempts: 25, FailedAt: time.Now()}, "gpt-5.5": {Attempts: 25, FailedAt: time.Now()}})
			repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
			candidate := codexTurnStateCandidateSent(account, "gpt-5.4", cache["gpt-5.4"].State)
			wakes := 0
			observation := newCodexTurnStateObservation(context.Background(), repo, candidate, func() { wakes++ })
			observation.observeHeader(tc.header)
			observation.observeEvent([]byte(tc.payload), "")
			observation.observeEvent([]byte(tc.payload), "")
			stored := codexTurnStateCacheFromExtra(account.Extra)
			_, present := stored["gpt-5.4"]
			require.Equal(t, !tc.invalid, present)
			require.Contains(t, stored, "gpt-5.5", "another model must be retained")
			require.Contains(t, codexTurnStateProbeFailuresFromExtra(account.Extra), "gpt-5.5")
			if tc.invalid {
				require.Equal(t, 1, wakes)
				require.NotContains(t, codexTurnStateProbeFailuresFromExtra(account.Extra), "gpt-5.4")
			} else {
				require.Zero(t, wakes)
			}
		})
	}
}

func TestCodexTurnStateLateFailureKeepsRenewedCandidate(t *testing.T) {
	account := codexTurnStateTestAccount()
	cache := codexTurnStateCacheFromExtra(account.Extra)
	candidate := codexTurnStateCandidateSent(account, "gpt-5.4", cache["gpt-5.4"].State)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	observation := newCodexTurnStateObservation(context.Background(), repo, candidate, nil)
	replacement := cache["gpt-5.4"]
	replacement.State = strings.Repeat("n", 292)
	replacement.ObtainedAt = time.Now().UTC()
	replacement.ExpiresAt = replacement.ObtainedAt.Add(time.Hour)
	account.Extra[CodexTurnStateProbeCacheExtraKey] = codexTurnStateCacheToExtra(map[string]codexTurnStateCacheEntry{"gpt-5.4": replacement})
	observation.observeHeader(strings.Repeat("x", 312))
	require.Equal(t, replacement, codexTurnStateCacheFromExtra(account.Extra)["gpt-5.4"])
}

func TestCodexTurnStateHTTPObservationPreservesSplitSSE(t *testing.T) {
	account := codexTurnStateTestAccount()
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	applyConfiguredCodexTurnStateToRequest(account, req, "gpt-5.4", "review-token")
	stream := "event: response.created\r\ndata: {\"response\":{\"model\":\"gpt-5.6-luna\"}}\r\n\r\ndata: [DONE]\n\n"
	body := &passthroughCloseTrackingReadCloser{Reader: strings.NewReader(stream)}
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}
	observeCodexTurnStateHTTPResponse(req, resp, account, repo, nil)
	var out strings.Builder
	buf := make([]byte, 3)
	for {
		n, err := resp.Body.Read(buf)
		_, writeErr := out.Write(buf[:n])
		require.NoError(t, writeErr)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}
	require.Equal(t, stream, out.String())
	require.Empty(t, codexTurnStateCacheFromExtra(account.Extra))
	require.NoError(t, resp.Body.Close())
	require.True(t, body.closed)
}

func TestCodexTurnStateHTTPHeaderInvalidatesBeforeBodyRead(t *testing.T) {
	account := codexTurnStateTestAccount()
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": []string{strings.Repeat("x", 312)}}, Body: io.NopCloser(strings.NewReader(""))}}
	svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream}
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	applyConfiguredCodexTurnStateToRequest(account, req, "gpt-5.4", "review-token")
	resp, err := svc.doOpenAIUpstream(req, "http://proxy.example:8080", account)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NoError(t, resp.Body.Close())
	require.Empty(t, codexTurnStateCacheFromExtra(account.Extra))
}

func TestCodexTurnStateRetryPreservesUnexpiredState(t *testing.T) {
	account := codexTurnStateTestAccount()
	before := codexTurnStateCacheFromExtra(account.Extra)
	account.Extra[CodexTurnStateProbeFailureExtraKey] = codexTurnStateProbeFailuresToExtra(map[string]codexTurnStateProbeFailure{"gpt-5.4": {Attempts: 25, FailedAt: time.Now()}})
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	svc := &adminServiceImpl{accountRepo: repo}
	require.NoError(t, svc.UpdateAccountExtra(context.Background(), account.ID, map[string]any{CodexTurnStateProbeRetryExtraKey: true}))
	require.Equal(t, before, codexTurnStateCacheFromExtra(account.Extra))
	require.Empty(t, codexTurnStateProbeFailuresFromExtra(account.Extra))
}

func TestCodexTurnStateProbeBudgetAndTimeout(t *testing.T) {
	require.Equal(t, 10*time.Second, codexTurnStateProbeTimeout)
	require.Equal(t, 25, codexTurnStateProbeMaxAttempts)
	account := codexTurnStateTestAccount()
	delete(account.Extra, CodexTurnStateProbeCacheExtraKey)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	upstream := &httpUpstreamRecorder{err: errors.New("offline proxy")}
	svc := NewCodexTurnStateProbeService(repo, &codexTurnStateTestProxyRepo{}, nil, upstream)
	defer svc.Stop()
	require.NoError(t, svc.refreshAccount(context.Background(), account, CodexTurnStateProbeConfig{Enabled: true, ProxyID: 23}))
	require.Len(t, upstream.requests, 25)
	deadline, ok := upstream.lastReq.Context().Deadline()
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(10*time.Second), deadline, time.Second)
	failures := codexTurnStateProbeFailuresFromExtra(account.Extra)
	require.Equal(t, 25, failures["gpt-5.4"].Attempts)
	require.NoError(t, svc.refreshAccount(context.Background(), account, CodexTurnStateProbeConfig{Enabled: true, ProxyID: 23}))
	require.Len(t, upstream.requests, 25, "exhausted model stays paused")
}

func TestCodexTurnStateFailedRenewalKeepsCurrentState(t *testing.T) {
	account := codexTurnStateTestAccount()
	cache := codexTurnStateCacheFromExtra(account.Extra)
	entry := cache["gpt-5.4"]
	entry.ObtainedAt = time.Now().UTC().Add(-56 * time.Minute)
	entry.ExpiresAt = entry.ObtainedAt.Add(time.Hour)
	cache["gpt-5.4"] = entry
	account.Extra[CodexTurnStateProbeCacheExtraKey] = codexTurnStateCacheToExtra(cache)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	svc := NewCodexTurnStateProbeService(repo, &codexTurnStateTestProxyRepo{}, nil, &httpUpstreamRecorder{err: errors.New("offline proxy")})
	defer svc.Stop()
	require.NoError(t, svc.refreshAccount(context.Background(), account, CodexTurnStateProbeConfig{Enabled: true, ProxyID: 23}))
	require.Equal(t, entry, codexTurnStateCacheFromExtra(account.Extra)["gpt-5.4"])
}

func TestCodexTurnStateLegacyFailureBudgetStaysStopped(t *testing.T) {
	failures := codexTurnStateProbeFailuresFromExtra(map[string]any{CodexTurnStateProbeFailureExtraKey: map[string]any{"gpt-5.4": map[string]any{"attempts": 50, "failed_at": time.Now().Format(time.RFC3339Nano)}}})
	require.Equal(t, 25, failures["gpt-5.4"].Attempts)
}

func TestCodexTurnStateWSUsesFreshCandidateAndFinalModel(t *testing.T) {
	account := codexTurnStateTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"alias": "gpt-5.4", "gpt-5.4": "gpt-5.5"}
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	stale, err := repo.GetByID(context.Background(), account.ID)
	require.NoError(t, err)
	cache := codexTurnStateCacheFromExtra(account.Extra)
	entry := cache["gpt-5.4"]
	entry.State = strings.Repeat("n", 292)
	cache["gpt-5.4"] = entry
	account.Extra[CodexTurnStateProbeCacheExtraKey] = codexTurnStateCacheToExtra(cache)
	svc := &OpenAIGatewayService{accountRepo: repo}
	out, observer := svc.prepareCodexTurnStateWSFrame(context.Background(), nil, stale, []byte(`{"type":"response.create","model":"gpt-5.4"}`), "", "review-token", nil)
	require.Equal(t, entry.State, gjson.GetBytes(out, "client_metadata."+openAICodexTurnStateHeader).String())
	require.NotNil(t, observer)
	observer.observeEvent([]byte(`{"type":"error","error":{"code":"server_is_overloaded"}}`), "")
	require.Empty(t, codexTurnStateCacheFromExtra(account.Extra))
}

func TestCodexTurnStateEditUsesCurrentDatabaseRuntime(t *testing.T) {
	current := codexTurnStateTestAccount()
	encoded, err := json.Marshal(current)
	require.NoError(t, err)
	var updated Account
	require.NoError(t, json.Unmarshal(encoded, &updated))
	delete(current.Extra, CodexTurnStateProbeCacheExtraKey)
	PreserveCodexTurnStateProbeRuntimeForEdit(&updated, current)
	require.NotContains(t, updated.Extra, CodexTurnStateProbeCacheExtraKey, "editing must not resurrect an invalidated state from a stale read")
}

func TestCodexTurnStateCredentialOwnerChangeInvalidates(t *testing.T) {
	for _, field := range []string{"access_token", "chatgpt_account_id", "chatgpt_account_is_fedramp"} {
		t.Run(field, func(t *testing.T) {
			account := codexTurnStateTestAccount()
			repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
			credentials := mergeMap(nil, account.Credentials)
			if field == "chatgpt_account_is_fedramp" {
				credentials[field] = true
			} else {
				credentials[field] = "changed"
			}
			svc := &adminServiceImpl{accountRepo: repo}
			updated, err := svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{Credentials: credentials})
			require.NoError(t, err)
			require.NotContains(t, updated.Extra, CodexTurnStateProbeCacheExtraKey)
		})
	}
}

func TestCodexTurnStateHTTPObserverSkipsLargeEventAndFindsNext(t *testing.T) {
	reasons := 0
	observation := &codexTurnStateObservation{invalidate: func(string) { reasons++ }}
	prefix := "data: " + strings.Repeat("x", codexTurnStateObservationLimit+1) + "\n\n"
	stream := prefix + "data: {\"type\":\"error\",\"code\":\"server_is_overloaded\"}\n\n"
	body := &codexTurnStateObservedBody{ReadCloser: io.NopCloser(strings.NewReader(stream)), observer: observation, sse: true}
	output, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, stream, string(output))
	require.Equal(t, 1, reasons)
}

func TestCodexTurnStateHTTPRetryDoesNotReuseInvalidatedSnapshot(t *testing.T) {
	account := codexTurnStateTestAccount()
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	stale, err := repo.GetByID(context.Background(), account.ID)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	applyConfiguredCodexTurnStateToRequest(stale, req, "gpt-5.4", "review-token")
	require.Len(t, req.Header.Get(openAICodexTurnStateHeader), 292)
	delete(account.Extra, CodexTurnStateProbeCacheExtraKey)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: make(http.Header)}}
	svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream}
	_, err = svc.doOpenAIUpstream(req, "http://proxy.example:8080", stale)
	require.NoError(t, err)
	require.Empty(t, upstream.lastReq.Header.Get(openAICodexTurnStateHeader))
}

func TestCodexTurnStateHTTPDoesNotAttributeUnmanagedHeader(t *testing.T) {
	account := codexTurnStateTestAccount()
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req.Header.Set(openAICodexTurnStateHeader, strings.Repeat("a", 292))
	applyConfiguredCodexTurnStateToRequest(account, req, "gpt-5.4", "different-token")
	require.Nil(t, req.Context().Value(codexTurnStateRequestKey{}))
	require.Equal(t, strings.Repeat("a", 292), req.Header.Get(openAICodexTurnStateHeader))
}
