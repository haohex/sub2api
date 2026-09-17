CREATE TABLE IF NOT EXISTS codex_state_events (
 id BIGSERIAL PRIMARY KEY,
 account_id BIGINT NOT NULL,
 model TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 payload JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS codex_state_events_account_model_id ON codex_state_events(account_id, model, id DESC);
CREATE INDEX IF NOT EXISTS codex_state_events_created_at ON codex_state_events(created_at);
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS turn_state_sent_length INTEGER;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS turn_state_returned_length INTEGER;
