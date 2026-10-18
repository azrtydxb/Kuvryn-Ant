CREATE TABLE saml_replay_claims (
 instance_id uuid NOT NULL REFERENCES instances(id),
 provider_hash bytea NOT NULL CHECK(octet_length(provider_hash)=32),
 key_hash bytea NOT NULL CHECK(octet_length(key_hash)=32),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(instance_id,provider_hash,key_hash),
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '24 hours')
);
CREATE INDEX saml_replay_expiry_idx ON saml_replay_claims(expires_at);
