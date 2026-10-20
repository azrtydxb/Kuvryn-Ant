-- Record which GitHub OIDC trust rule issued a service token so that deleting,
-- disabling or narrowing the rule can revoke the tokens it already handed out.
-- No foreign key: the rule row is deleted while its token rows stay for audit.
ALTER TABLE service_tokens ADD COLUMN trust_rule_id uuid;
CREATE INDEX service_tokens_trust_rule_idx ON service_tokens(instance_id,trust_rule_id) WHERE trust_rule_id IS NOT NULL AND revoked_at IS NULL;
