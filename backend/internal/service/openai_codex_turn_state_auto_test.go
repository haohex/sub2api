//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// turnStateAutoRepo 只实现自动接管真正会调到的三个方法，其余继承 accountRepoStub 的
// panic 实现——多调一个方法就会当场炸出来。
type turnStateAutoRepo struct {
	*accountRepoStub

	mu sync.Mutex
	// latest 模拟 DB 侧的账号当前态：候选池维护会在锁内重新读它。
	latest      *Account
	extraWrites []map[string]any
	schedulable []bool
	errors      []string
}

func (r *turnStateAutoRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latest == nil {
		return nil, errors.New("not found")
	}
	return r.latest, nil
}

func newTurnStateAutoRepo() *turnStateAutoRepo {
	return &turnStateAutoRepo{accountRepoStub: &accountRepoStub{}}
}

func (r *turnStateAutoRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.extraWrites = append(r.extraWrites, updates)
	return nil
}

func (r *turnStateAutoRepo) SetSchedulable(_ context.Context, _ int64, schedulable bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schedulable = append(r.schedulable, schedulable)
	return nil
}

func (r *turnStateAutoRepo) SetError(_ context.Context, _ int64, msg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, msg)
	return nil
}

func turnStateAutoCtx(sessionID string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if sessionID != "" {
		c.Request.Header.Set("Session-Id", sessionID)
	}
	return c
}

func turnStateAutoAccount() *Account {
	return &Account{
		ID:       7,
		Platform: PlatformOpenAI,
		Type:     AccountTypeCPR,
		Extra:    map[string]any{openAITurnStateAutoExtraKey: true},
	}
}

// healthy/degraded 只用长度说话——判定逻辑只看 len == 292。
func turnStateBlob(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(i%26)
	}
	return string(b)
}

func TestLegacyStateSettingsNeverInjectOrDisableAccounts(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		account := turnStateAutoAccount()
		account.Extra[CodexTurnStateProbeEnabledExtraKey] = enabled
		account.Extra[openAITurnStateOverrideExtraKey] = "legacy-secret"
		repo := newTurnStateAutoRepo()
		svc := &OpenAIGatewayService{accountRepo: repo}
		c := turnStateAutoCtx("legacy")
		require.False(t, account.IsOpenAITurnStateAutoEnabled())
		require.Empty(t, account.OpenAICodexTurnStateOverride())
		state, source := svc.resolveOpenAITurnStateOverride(c, account)
		require.Empty(t, state)
		require.Empty(t, source)
		svc.observeOpenAITurnStateMint(c, account, turnStateBlob(312))
		require.Empty(t, repo.extraWrites)
		require.Empty(t, repo.schedulable)
		require.Empty(t, repo.errors)
	}
}
