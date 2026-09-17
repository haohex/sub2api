package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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

type statePoolWSConn struct {
	frames [][]byte
	writes []map[string]any
	closed bool
}

func (c *statePoolWSConn) WriteJSON(_ context.Context, v any) error {
	raw, _ := json.Marshal(v)
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	c.writes = append(c.writes, value)
	return nil
}
func (c *statePoolWSConn) ReadMessage(context.Context) ([]byte, error) {
	if len(c.frames) == 0 {
		return nil, errors.New("no more events")
	}
	value := c.frames[0]
	c.frames = c.frames[1:]
	return value, nil
}
func (c *statePoolWSConn) Ping(context.Context) error { return nil }
func (c *statePoolWSConn) Close() error               { c.closed = true; return nil }

type statePoolWSDialer struct {
	frames    [][]byte
	conns     []*statePoolWSConn
	headers   []http.Header
	proxies   []string
	deadlines []time.Time
}

func (d *statePoolWSDialer) Dial(ctx context.Context, _ string, h http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	conn := &statePoolWSConn{frames: append([][]byte(nil), d.frames...)}
	d.conns = append(d.conns, conn)
	d.headers = append(d.headers, h.Clone())
	d.proxies = append(d.proxies, proxy)
	deadline, _ := ctx.Deadline()
	d.deadlines = append(d.deadlines, deadline)
	return conn, 101, nil, nil
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

func TestCodexStateHTTPFallbackFreshWSUsesSharedBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		header  string
		frames  [][]byte
		wantWS  int
		success bool
	}{
		{name: "HTTP team state", header: strings.Repeat("s", 332), success: true},
		{name: "HTTP missing then WS metadata", frames: [][]byte{[]byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"` + strings.Repeat("s", 332) + `"}}`)}, wantWS: 1, success: true},
		{name: "unexpected HTTP length is failed acquisition", header: strings.Repeat("s", 356), wantWS: 0},
		{name: "shared total budget", frames: [][]byte{[]byte(`{"type":"codex.response.metadata","headers":{"x-codex-safety-buffering-faster-model":"gpt-5.6-luna"}}`), []byte(`{"type":"response.completed"}`)}, wantWS: 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := codexTurnStateTestAccount()
			a.Credentials["plan_type"] = "team"
			httpUp := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": []string{tc.header}}}}
			ws := &statePoolWSDialer{frames: tc.frames}
			svc := NewCodexTurnStateProbeService(nil, nil, nil, httpUp)
			svc.wsDialer = ws
			defer svc.Stop()
			proxy := &Proxy{ID: 8, Protocol: "http", Host: "dynamic.example", Port: 8080}
			state, _, err := svc.probeModel(context.Background(), a, proxy, "review-token", "gpt-6-astra", 25)
			if tc.success {
				require.NoError(t, err)
				require.Len(t, state, 332)
			} else {
				require.Error(t, err)
				require.Len(t, httpUp.requests, 25-tc.wantWS)
			}
			require.Len(t, ws.conns, tc.wantWS)
			for i, conn := range ws.conns {
				require.True(t, conn.closed)
				require.Len(t, conn.writes, 1)
				require.Equal(t, "response.create", conn.writes[0]["type"])
				require.NotContains(t, conn.writes[0], "headers")
				require.NotContains(t, conn.writes[0], "x-codex-turn-state")
				require.Empty(t, ws.headers[i].Get(openAICodexTurnStateHeader))
				require.Equal(t, proxy.URL(), ws.proxies[i])
				require.WithinDuration(t, time.Now().Add(10*time.Second), ws.deadlines[i], time.Second)
			}
		})
	}
}

func TestCodexStateTeamMetadataAndNoMintAreNotConfused(t *testing.T) {
	a := codexTurnStateTestAccount()
	a.Credentials["plan_type"] = "team"
	cache := codexTurnStateCacheFromExtra(a.Extra)
	entry := cache["gpt-5.4"]
	entry.State = strings.Repeat("t", 332)
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

func TestCodexStateGlobalProxyManualRefreshKeepsValidCandidate(t *testing.T) {
	a := codexTurnStateTestAccount()
	a.Status = StatusActive
	repo := &statePoolAccountRepo{&upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{a.ID: a}}}
	httpUp := &httpUpstreamRecorder{err: errors.New("offline proxy")}
	svc := NewCodexTurnStateProbeService(repo, &codexTurnStateTestProxyRepo{}, nil, httpUp)
	defer svc.Stop()
	svc.settingRepo = &statePoolSettingsRepo{value: "23"}
	before := codexTurnStateCacheFromExtra(a.Extra)
	_, err := svc.SetGlobalProxy(context.Background(), 24)
	require.NoError(t, err)
	require.Equal(t, before, codexTurnStateCacheFromExtra(a.Extra))
	require.Equal(t, strings.Repeat("a", 292), configuredCodexTurnState(a, "gpt-5.4", "review-token", time.Now()), "changing the acquisition proxy preserves usable states")
	require.NoError(t, svc.QueueRefresh(context.Background(), a.ID, "gpt-5.4"))
	require.NoError(t, svc.QueueRefresh(context.Background(), a.ID, "gpt-5.4"))
	pool, err := svc.Pool(context.Background())
	require.NoError(t, err)
	require.Equal(t, "queued", pool.Items[0].ProbeStatus)
	raw, err := json.Marshal(pool)
	require.NoError(t, err)
	require.NotContains(t, string(raw), strings.Repeat("a", 292), "pool API never returns raw state")
	svc.refreshOnce(context.Background())
	require.Len(t, httpUp.requests, 25, "manual requests coalesce and share the normal budget")
	require.Equal(t, before, codexTurnStateCacheFromExtra(a.Extra))
	pool, err = svc.Pool(context.Background())
	require.NoError(t, err)
	require.Equal(t, "valid", pool.Items[0].StateStatus)
	require.Equal(t, "stopped", pool.Items[0].ProbeStatus)
	httpUp.err = nil
	httpUp.resp = &http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": []string{strings.Repeat("n", 292)}}}
	require.NoError(t, svc.QueueRefresh(context.Background(), a.ID, "gpt-5.4"))
	svc.refreshOnce(context.Background())
	require.Equal(t, strings.Repeat("n", 292), codexTurnStateCacheFromExtra(a.Extra)["gpt-5.4"].State)
}

func TestCodexStateUnknownPlanDoesNotProbeOrInject(t *testing.T) {
	a := codexTurnStateTestAccount()
	delete(a.Credentials, "plan_type")
	up := &httpUpstreamRecorder{}
	svc := NewCodexTurnStateProbeService(nil, nil, nil, up)
	defer svc.Stop()
	require.NoError(t, svc.refreshAccount(context.Background(), a, CodexTurnStateProbeConfig{Enabled: true, ProxyID: 23}))
	require.Empty(t, up.requests)
	require.Empty(t, configuredCodexTurnState(a, "gpt-5.4", "review-token", time.Now()))
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

func TestCodexStateLegacyAccountProxyNeverReplacesGlobalSetting(t *testing.T) {
	account := codexTurnStateTestAccount()
	account.Status = StatusActive
	repo := &statePoolAccountRepo{&upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}}
	up := &httpUpstreamRecorder{}
	svc := NewCodexTurnStateProbeService(repo, &codexTurnStateTestProxyRepo{}, nil, up)
	defer svc.Stop()
	svc.settingRepo = &statePoolSettingsRepo{}
	before := codexTurnStateCacheFromExtra(account.Extra)
	svc.refreshOnce(context.Background())
	require.Empty(t, up.requests)
	require.Equal(t, before, codexTurnStateCacheFromExtra(account.Extra))
}
