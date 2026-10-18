-- Administrable sign-in providers, the emergency (break-glass) local account and
-- the SSO-mandatory policy. Secrets live in encrypted_secrets, never in config.

CREATE TABLE identity_providers (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 kind text NOT NULL CHECK(kind IN ('oidc','saml','ldap')),
 name text NOT NULL CHECK(char_length(name) BETWEEN 1 AND 64),
 enabled boolean NOT NULL DEFAULT false,
 config jsonb NOT NULL CHECK(octet_length(config::text)<=524288),
 secret_id uuid,
 secret_purpose text GENERATED ALWAYS AS ('identity-provider:'||id::text) STORED,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 last_error text CHECK(last_error IS NULL OR octet_length(last_error)<=256),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(instance_id,name),
 UNIQUE(instance_id,id),
 FOREIGN KEY(instance_id,secret_id,secret_purpose) REFERENCES encrypted_secrets(instance_id,id,purpose)
);
-- The login runtime serves one provider per protocol.
CREATE UNIQUE INDEX identity_providers_one_enabled ON identity_providers(instance_id,kind) WHERE enabled;

CREATE TABLE identity_policy (
 instance_id uuid PRIMARY KEY REFERENCES instances(id),
 sso_mandatory boolean NOT NULL DEFAULT false,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE local_accounts (
 instance_id uuid NOT NULL,
 principal_id uuid NOT NULL,
 username text NOT NULL CHECK(username ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
 password_hash text NOT NULL CHECK(octet_length(password_hash) BETWEEN 32 AND 512),
 break_glass boolean NOT NULL DEFAULT false,
 totp_secret_id uuid,
 totp_secret_purpose text GENERATED ALWAYS AS ('local-totp:'||principal_id::text) STORED,
 totp_enabled boolean NOT NULL DEFAULT false,
 totp_last_step bigint NOT NULL DEFAULT 0,
 failed_attempts integer NOT NULL DEFAULT 0 CHECK(failed_attempts>=0),
 locked_until timestamptz,
 last_login_at timestamptz,
 password_changed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,principal_id),
 UNIQUE(instance_id,username),
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id),
 FOREIGN KEY(instance_id,totp_secret_id,totp_secret_purpose) REFERENCES encrypted_secrets(instance_id,id,purpose),
 CHECK(NOT totp_enabled OR totp_secret_id IS NOT NULL)
);
CREATE UNIQUE INDEX local_accounts_one_break_glass ON local_accounts(instance_id) WHERE break_glass;
