package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type productionStateRepo struct {
	*statePoolAccountRepo
	eventsMu sync.Mutex
	events   []CodexStateEvent
}

func (r *productionStateRepo) AppendCodexStateEvent(_ context.Context, e CodexStateEvent) error {
	r.eventsMu.Lock()
	defer r.eventsMu.Unlock()
	r.events = append(r.events, e)
	return nil
}
func (r *productionStateRepo) ListCodexStateEvents(context.Context, int64, string, int64, int) ([]CodexStateEvent, error) {
	return nil, nil
}
func (r *productionStateRepo) CleanupCodexStateEvents(context.Context, time.Time) error { return nil }
func productionStateService() (*CodexTurnStateProbeService, *Account, *productionStateRepo) {
	a := codexTurnStateTestAccount()
	a.Status = StatusActive
	repo := &productionStateRepo{statePoolAccountRepo: &statePoolAccountRepo{&upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{a.ID: a}}}}
	svc := NewCodexTurnStateProbeService(repo, &codexTurnStateTestProxyRepo{}, nil, nil)
	svc.settingRepo = &statePoolSettingsRepo{value: "23"}
	return svc, a, repo
}
func TestCodexStateTimestampAndModeBoundaries(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		left time.Duration
		mode string
	}{{31 * time.Minute, "idle"}, {30 * time.Minute, "periodic"}, {11 * time.Minute, "periodic"}, {10 * time.Minute, "continuous"}, {time.Second, "continuous"}, {-time.Second, "continuous"}} {
		state := codexTestStateAt("a", 292, now.Add(tc.left-time.Hour))
		entry := codexTurnStateCacheEntry{State: state, ObtainedAt: now, ExpiresAt: now.Add(time.Hour), CredentialHash: codexTurnStateCredentialHash("token")}
		require.Equal(t, tc.mode, codexStateMode(entry, 292, "token", now))
	}
	_, _, err := codexStateTimes(strings.Repeat("x", 292), now, now)
	require.Error(t, err)
	_, _, err = codexStateTimes(codexTestStateAt("a", 292, now.Add(61*time.Second)), now, now)
	require.Error(t, err)
	_, expires, err := codexStateTimes(codexTestStateAt("a", 292, now.Add(-20*time.Minute)), now, now)
	require.NoError(t, err)
	require.Equal(t, now.Add(40*time.Minute), expires)
	_, expires, err = codexStateTimes(codexTestStateAt("a", 292, now), now.Add(-30*time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, now.Add(30*time.Minute), expires)
}
func TestCodexStateImportValidatesAndProtectsOldValue(t *testing.T) {
	for _, tc := range []struct{ name, event, reason string }{
		{"valid no mint", `{"type":"response.completed","response":{"model":"gpt-5.4"}}`, "replaced"},
		{"downgrade", `{"type":"response.created","response":{"model":"gpt-5.6-luna"}}`, "upstream_model_mismatch"},
		{"incomplete", `{"type":"response.created","response":{"model":"gpt-5.4"}}`, "validation_not_completed"},
		{"remint", `{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"` + codexTestState("b", 292) + `"}}`, "state_not_accepted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, a, repo := productionStateService()
			defer svc.Stop()
			before := codexTurnStateCacheFromExtra(a.Extra)
			supplied := codexTestStateAt("n", 292, time.Now().Truncate(time.Second))
			svc.requestDo = func(req *http.Request, proxy string) (*http.Response, error) {
				require.Equal(t, supplied, req.Header.Get(openAICodexTurnStateHeader))
				require.Equal(t, "http://proxy.example:8080", proxy)
				deadline, ok := req.Context().Deadline()
				require.True(t, ok)
				require.WithinDuration(t, time.Now().Add(10*time.Second), deadline, time.Second)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: " + tc.event + "\n\n"))}, nil
			}
			result, err := svc.ImportState(context.Background(), a.ID, "gpt-5.4", supplied)
			if tc.reason == "replaced" {
				require.NoError(t, err)
				require.Equal(t, "replaced", result)
				require.Equal(t, supplied, codexTurnStateCacheFromExtra(a.Extra)["gpt-5.4"].State)
			} else {
				require.ErrorContains(t, err, tc.reason)
				require.Equal(t, before, codexTurnStateCacheFromExtra(a.Extra))
			}
			require.NotEmpty(t, repo.events)
			require.Equal(t, tc.reason, repo.events[len(repo.events)-1].Reason)
		})
	}
}
func TestCodexStateImportRejectsBusyMalformedAndOlder(t *testing.T) {
	svc, a, _ := productionStateService()
	defer svc.Stop()
	_, err := svc.ImportState(context.Background(), a.ID, "gpt-5.4", strings.Repeat("x", 292))
	require.ErrorContains(t, err, "invalid_state_format")
	require.True(t, svc.reserveStateAccount(a.ID))
	_, err = svc.ImportState(context.Background(), a.ID, "gpt-5.4", codexTestState("a", 292))
	require.ErrorContains(t, err, "account_busy")
	svc.releaseStateAccount(a.ID)
	saved, reason := svc.replaceState(context.Background(), a, "gpt-5.4", "review-token", codexTestStateAt("n", 292, time.Now().Add(-10*time.Minute)), time.Now(), 23, "manual_import")
	require.False(t, saved)
	require.Equal(t, "not_newer", reason)
}
func TestCodexStateContinuousAttemptsCoolDownWhenNoCandidateExists(t *testing.T) {
	svc, a, repo := productionStateService()
	defer svc.Stop()
	delete(a.Extra, CodexTurnStateProbeCacheExtraKey)
	key := codexTurnStatePoolKey{a.ID, "gpt-5.4"}
	svc.schedules = map[codexTurnStatePoolKey]*codexStateSchedule{key: {Mode: "continuous", NoValid: true, Remaining: 25}}
	calls := 0
	svc.requestDo = func(*http.Request, string) (*http.Response, error) { calls++; return nil, errors.New("offline") }
	for range 25 {
		svc.executeStateAttempt(context.Background(), a.ID, "gpt-5.4")
	}
	require.Equal(t, 25, calls)
	require.Equal(t, 0, svc.schedules[key].Attempt)
	require.WithinDuration(t, time.Now().Add(5*time.Minute), svc.schedules[key].Next, time.Second)
	require.Len(t, repo.events, 50)
	svc.schedules[key] = &codexStateSchedule{Mode: "periodic", Remaining: 25}
	for range 25 {
		svc.executeStateAttempt(context.Background(), a.ID, "gpt-5.4")
	}
	require.WithinDuration(t, time.Now().Add(5*time.Minute), svc.schedules[key].Next, time.Second)
}
func TestCodexStateAcquisitionUsesSSEMetadataWithoutWS(t *testing.T) {
	svc, a, _ := productionStateService()
	defer svc.Stop()
	want := codexTestState("n", 292)
	svc.requestDo = func(req *http.Request, _ string) (*http.Response, error) {
		require.Empty(t, req.Header.Get(openAICodexTurnStateHeader))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"codex.response.metadata\",\"headers\":{\"x-codex-turn-state\":\"" + want + "\"}}\n\n"))}, nil
	}
	result := svc.acquireState(context.Background(), a, &Proxy{ID: 23}, "review-token", "gpt-5.4", "")
	require.NoError(t, result.err)
	require.Equal(t, want, result.state)
}

func TestCodexStateScheduleIntervalsAndAccountConcurrency(t *testing.T) {
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	state := &codexStateSchedule{Mode: "periodic", Remaining: 1}
	advanceCodexStateSchedule(state, false, now)
	require.Equal(t, now.Add(5*time.Minute), state.Next)
	state = &codexStateSchedule{Mode: "continuous", Remaining: 1}
	advanceCodexStateSchedule(state, false, now)
	require.Equal(t, now.Add(time.Second), state.Next)
	state.Manual = true
	advanceCodexStateSchedule(state, true, now)
	require.False(t, state.Manual)
	require.Equal(t, now.Add(time.Second), state.Next)
	svc, a, _ := productionStateService()
	defer svc.Stop()
	require.True(t, svc.reserveStateAccount(a.ID))
	require.False(t, svc.reserveStateAccount(a.ID))
	for _, id := range []int64{1, 2, 3} {
		require.True(t, svc.reserveStateAccount(id))
	}
	require.False(t, svc.reserveStateAccount(4))
	svc.releaseStateAccount(2)
	require.True(t, svc.reserveStateAccount(4))
}

func TestCodexStateBusinessLengthsAndInvalidationEvents(t *testing.T) {
	svc, a, repo := productionStateService()
	defer svc.Stop()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	gateway := &OpenAIGatewayService{accountRepo: repo}
	_, observer := gateway.prepareCodexTurnStateWSFrame(context.Background(), c, a, []byte(`{"type":"response.create","model":"gpt-5.4"}`), "", "review-token", nil)
	initial := OpenAICodexStateUsageLengths(c)
	require.Equal(t, 292, *initial.Sent)
	require.Equal(t, 0, *initial.Returned)
	observer.observeEvent([]byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"`+strings.Repeat("x", 312)+`"}}`), "")
	observed := OpenAICodexStateUsageLengths(c)
	require.Equal(t, 312, *observed.Returned)
	require.Empty(t, codexTurnStateCacheFromExtra(a.Extra))
	require.Len(t, repo.events, 2)
	require.Equal(t, "invalidation_observed", repo.events[0].Kind)
	require.Equal(t, "invalidated", repo.events[1].Kind)
	require.Equal(t, 292, repo.events[1].SentLength)
	require.Equal(t, 312, repo.events[1].ReturnedLength)
}

func TestCodexStateImportRechecksCredentialsAfterValidation(t *testing.T) {
	svc, a, _ := productionStateService()
	defer svc.Stop()
	before := codexTurnStateCacheFromExtra(a.Extra)
	svc.requestDo = func(*http.Request, string) (*http.Response, error) {
		a.Credentials["access_token"] = "changed-owner"
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.4\"}}\n\n"))}, nil
	}
	_, err := svc.ImportState(context.Background(), a.ID, "gpt-5.4", codexTestStateAt("n", 292, time.Now().Truncate(time.Second)))
	require.ErrorContains(t, err, "account_changed")
	require.Equal(t, before, codexTurnStateCacheFromExtra(a.Extra))
}

func TestCodexStateImportCancelledValidationKeepsCandidate(t *testing.T) {
	svc, a, _ := productionStateService()
	defer svc.Stop()
	before := codexTurnStateCacheFromExtra(a.Extra)
	ctx, cancel := context.WithCancel(context.Background())
	svc.requestDo = func(req *http.Request, _ string) (*http.Response, error) { cancel(); return nil, req.Context().Err() }
	_, err := svc.ImportState(ctx, a.ID, "gpt-5.4", codexTestStateAt("n", 292, time.Now().Truncate(time.Second)))
	require.ErrorContains(t, err, "probe_cancelled")
	require.Equal(t, before, codexTurnStateCacheFromExtra(a.Extra))
}

func TestCodexStateDispatcherDoesNotStarveFifthAccount(t *testing.T) {
	svc, a, repo := productionStateService()
	defer svc.Stop()
	repo.accounts = map[int64]*Account{}
	for id := int64(1); id <= 5; id++ {
		copyAccount := *a
		copyAccount.ID = id
		copyAccount.Extra = map[string]any{CodexTurnStateProbeEnabledExtraKey: true}
		copyAccount.Credentials = mergeMap(nil, a.Credentials)
		copyAccount.Credentials["access_token"] = fmt.Sprint(id)
		repo.accounts[id] = &copyAccount
	}
	entered := make(chan string, 12)
	release := make(chan struct{})
	svc.requestDo = func(req *http.Request, _ string) (*http.Response, error) {
		entered <- req.Header.Get("Authorization")
		select {
		case <-release:
		case <-req.Context().Done():
		}
		return nil, errors.New("offline")
	}
	svc.dispatchStateProbes(svc.ctx)
	seen := map[string]bool{}
	for range 4 {
		select {
		case id := <-entered:
			seen[id] = true
		case <-time.After(3 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	require.Len(t, seen, 4)
	close(release)
	svc.wg.Wait()
	svc.dispatchStateProbes(svc.ctx)
	svc.wg.Wait()
	for len(entered) > 0 {
		seen[<-entered] = true
	}
	require.Len(t, seen, 5, "a failing account cannot keep a worker indefinitely")
}

func TestCodexStatePoolShowsLatestCompletedAttempt(t *testing.T) {
	svc, a, _ := productionStateService()
	defer svc.Stop()
	svc.setProbeRuntime(a.ID, "gpt-5.4", "idle", 27, "probe_transport_failed")
	pool, err := svc.Pool(context.Background())
	require.NoError(t, err)
	require.Len(t, pool.Items, 1)
	require.Equal(t, 27, pool.Items[0].Attempts)
	require.Equal(t, "probe_transport_failed", pool.Items[0].Reason)
	require.Equal(t, "valid", pool.Items[0].StateStatus)
}
