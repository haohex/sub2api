//go:build integration

package repository

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestCodexStateEventPaginationAndRetention(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	for i := range 4 {
		e := service.CodexStateEvent{AccountID: 42, Model: "gpt-6-astra", Kind: "probe_result", ReturnedLength: 292, CreatedAt: time.Now()}
		if i == 0 {
			e.CreatedAt = time.Now().Add(-8 * 24 * time.Hour)
		}
		require.NoError(t, repo.AppendCodexStateEvent(ctx, e))
	}
	first, err := repo.ListCodexStateEvents(ctx, 42, "gpt-6-astra", 0, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, 292, first[0].ReturnedLength)
	next, err := repo.ListCodexStateEvents(ctx, 42, "gpt-6-astra", first[1].ID, 2)
	require.NoError(t, err)
	require.Len(t, next, 1)
	other, err := repo.ListCodexStateEvents(ctx, 42, "other", 0, 2)
	require.NoError(t, err)
	require.Empty(t, other)
	require.NoError(t, repo.CleanupCodexStateEvents(ctx, time.Now().Add(-7*24*time.Hour)))
	rows, err := tx.QueryContext(ctx, `SELECT COUNT(*) FROM codex_state_events WHERE account_id=42`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var count int
	require.NoError(t, rows.Scan(&count))
	require.Equal(t, 3, count)
}
func (s *UsageLogRepoSuite) TestStateLengthsRoundTrip() {
	user := mustCreateUser(s.T(), s.client, &service.User{Email: "state-length@test.com"})
	key := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID, Key: "sk-state-length", Name: "state"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "state-length"})
	sent, returned := 292, 0
	entry := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, Model: "gpt-6-astra", TurnStateSentLength: &sent, TurnStateReturnedLength: &returned}
	_, err := s.repo.Create(s.ctx, entry)
	s.Require().NoError(err)
	got, err := s.repo.GetByID(s.ctx, entry.ID)
	s.Require().NoError(err)
	s.Require().Equal(&sent, got.TurnStateSentLength)
	s.Require().Equal(&returned, got.TurnStateReturnedLength)
}
