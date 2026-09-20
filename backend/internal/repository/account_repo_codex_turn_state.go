package repository

import (
	"context"
	"encoding/json"
	"errors"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.CodexTurnStateProbeRepository = (*accountRepository)(nil)

func (r *accountRepository) CompareAndSwapCodexTurnStateProbe(ctx context.Context, expected *service.Account, updates map[string]any) (bool, error) {
	if expected == nil {
		return false, service.ErrAccountNilInput
	}
	if len(updates) != 2 {
		return false, errors.New("codex turn-state runtime update requires both cache and failures")
	}
	for key := range updates {
		if key != service.CodexTurnStateProbeCacheExtraKey && key != service.CodexTurnStateProbeFailureExtraKey {
			return false, errors.New("invalid Codex turn-state runtime field")
		}
	}
	payload, err := json.Marshal(updates)
	if err != nil {
		return false, err
	}
	credentials, err := json.Marshal(normalizeJSONMap(expected.Credentials))
	if err != nil {
		return false, err
	}
	snapshot := make(map[string]any)
	for _, key := range []string{service.CodexTurnStateProbeEnabledExtraKey, service.CodexTurnStateProbeProxyIDExtraKey, service.CodexTurnStateProbeCacheExtraKey, service.CodexTurnStateProbeFailureExtraKey} {
		snapshot[key] = expected.Extra[key]
	}
	before, err := json.Marshal(snapshot)
	if err != nil {
		return false, err
	}
	// Only the two managed keys are stripped when cleared; unrelated JSON nulls
	// and concurrently updated account fields are preserved.
	result, err := clientFromContext(ctx, r.client).ExecContext(ctx, `UPDATE accounts SET extra =
  (COALESCE(extra, '{}'::jsonb) - 'codex_turn_state_probe_cache' - 'codex_turn_state_probe_failures') || jsonb_strip_nulls($1::jsonb), updated_at = NOW()
  WHERE id = $2 AND deleted_at IS NULL AND platform = $3 AND type = $4 AND credentials = $5::jsonb
  AND jsonb_build_object(
   'codex_turn_state_probe_enabled', extra -> 'codex_turn_state_probe_enabled',
   'codex_turn_state_probe_proxy_id', extra -> 'codex_turn_state_probe_proxy_id',
   'codex_turn_state_probe_cache', extra -> 'codex_turn_state_probe_cache',
   'codex_turn_state_probe_failures', extra -> 'codex_turn_state_probe_failures'
  ) = $6::jsonb`, string(payload), expected.ID, expected.Platform, expected.Type, string(credentials), string(before))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	if dbent.TxFromContext(ctx) == nil {
		r.syncSchedulerAccountSnapshot(ctx, expected.ID)
	}
	return true, nil
}

// Called within the account update transaction, after its row has been locked.
func preserveCodexTurnStateProbeRuntime(ctx context.Context, client *dbent.Client, account *service.Account, extra map[string]any) error {
	if !service.IsCodexTurnStateProbeAccount(account) {
		return nil
	}
	if _, exists := extra[service.CodexTurnStateProbeEnabledExtraKey]; !exists {
		return nil
	}
	rows, err := client.QueryContext(ctx, `SELECT platform, type, credentials, extra FROM accounts WHERE id = $1 AND deleted_at IS NULL FOR NO KEY UPDATE`, account.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return service.ErrAccountNotFound
	}
	current := &service.Account{ID: account.ID}
	var credentialsJSON, extraJSON []byte
	if err := rows.Scan(&current.Platform, &current.Type, &credentialsJSON, &extraJSON); err != nil {
		return err
	}
	if err := json.Unmarshal(credentialsJSON, &current.Credentials); err != nil {
		return err
	}
	if len(extraJSON) > 0 {
		if err := json.Unmarshal(extraJSON, &current.Extra); err != nil {
			return err
		}
	}
	updated := *account
	updated.Extra = extra
	service.PreserveCodexTurnStateProbeRuntimeForEdit(&updated, current)
	return rows.Err()
}
