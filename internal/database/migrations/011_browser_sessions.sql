-- Forward-only: dropping these tables invalidates all browser login state/sessions.
CREATE TABLE oidc_identity_bindings (
 instance_id uuid NOT NULL REFERENCES instances(id),
 issuer text NOT NULL CHECK (issuer <> ''),
 subject text NOT NULL CHECK (subject <> ''),
 principal_id uuid NOT NULL,
 PRIMARY KEY(instance_id,issuer,subject),
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id)
);
CREATE TABLE oidc_login_states (
 state_hash bytea PRIMARY KEY CHECK(octet_length(state_hash)=32),
 instance_id uuid NOT NULL REFERENCES instances(id),
 provider_key text NOT NULL,
 browser_hash bytea NOT NULL CHECK(octet_length(browser_hash)=32),
 nonce_hash bytea NOT NULL CHECK(octet_length(nonce_hash)=32),
 expires_at timestamptz NOT NULL DEFAULT (clock_timestamp()+interval '5 minutes')
);
CREATE INDEX oidc_login_states_expiry ON oidc_login_states(expires_at);
CREATE TABLE browser_sessions (
 verifier bytea PRIMARY KEY CHECK(octet_length(verifier)=32),
 csrf_hash bytea NOT NULL CHECK(octet_length(csrf_hash)=32),
 instance_id uuid NOT NULL,
 issuer text NOT NULL,
 subject text NOT NULL,
 principal_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 last_seen_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 idle_seconds integer NOT NULL CHECK(idle_seconds BETWEEN 1 AND 1800),
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id),
 FOREIGN KEY(instance_id,issuer,subject) REFERENCES oidc_identity_bindings(instance_id,issuer,subject) ON DELETE CASCADE,
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '8 hours')
);
CREATE INDEX browser_sessions_expiry ON browser_sessions(expires_at);
