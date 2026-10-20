-- Expired and revoked token cleanup.
--
-- token_cleanup_after_days: NULL means the built-in default (30); 0 means never
-- remove; otherwise rows expired or revoked longer than this many days ago are
-- deleted by the maintenance worker. audit_events reference tokens through an
-- opaque resource_id with no foreign key, so removing a token row never
-- affects audit history.
ALTER TABLE instance_settings ADD COLUMN token_cleanup_after_days integer
 CHECK(token_cleanup_after_days IS NULL OR token_cleanup_after_days BETWEEN 0 AND 3650);

CREATE INDEX service_tokens_expiry_idx ON service_tokens(instance_id,expires_at);
CREATE INDEX personal_access_tokens_expiry_idx ON personal_access_tokens(instance_id,expires_at);
