CREATE TABLE service_tokens (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL,
 principal_id uuid NOT NULL,
 verifier bytea NOT NULL UNIQUE CHECK(octet_length(verifier)=32),
 scopes jsonb NOT NULL CHECK(jsonb_typeof(scopes)='array'),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz,
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id),
 CHECK(expires_at>created_at),
 UNIQUE(instance_id,id)
);
CREATE INDEX service_tokens_principal_idx ON service_tokens(instance_id,principal_id);
