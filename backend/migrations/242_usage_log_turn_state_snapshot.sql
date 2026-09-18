-- Preserve the actual candidate sent on this request and the normal length
-- known at that time. Both are nullable for historical/non-Codex rows.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS turn_state_sent TEXT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS turn_state_expected_length INTEGER;

COMMENT ON COLUMN usage_logs.turn_state_sent IS
    'The x-codex-turn-state value sent upstream when no new value was returned.';
COMMENT ON COLUMN usage_logs.turn_state_expected_length IS
    'The account plan normal turn-state length captured at request time.';
