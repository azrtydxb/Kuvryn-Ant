-- Console management API support.
-- audit_events.result lets the audit read API filter outcomes; existing rows and
-- writers keep the default because they only record committed (successful) effects.
ALTER TABLE audit_events ADD COLUMN result text NOT NULL DEFAULT 'success' CHECK(result IN ('success','denied','failure'));
CREATE INDEX audit_instance_principal_idx ON audit_events(instance_id,principal_id,id DESC);
CREATE INDEX audit_instance_action_idx ON audit_events(instance_id,action,id DESC);
-- Best-effort last-use timestamp, updated at most once a minute per token.
ALTER TABLE service_tokens ADD COLUMN last_used_at timestamptz;
