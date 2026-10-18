-- Browser-bound, single-use SAML correlation and LDAP pre-login CSRF state.
CREATE TABLE enterprise_login_states (
 state_hash bytea PRIMARY KEY CHECK(octet_length(state_hash)=32),
 browser_hash bytea NOT NULL CHECK(octet_length(browser_hash)=32),
 instance_id uuid NOT NULL REFERENCES instances(id),
 provider text NOT NULL CHECK(length(provider) BETWEEN 1 AND 2048),
 kind text NOT NULL CHECK(kind IN ('saml','ldap')),
 request_id text NOT NULL CHECK(length(request_id)<=1024),
 expires_at timestamptz NOT NULL
);
CREATE INDEX enterprise_login_states_expiry ON enterprise_login_states(expires_at);
