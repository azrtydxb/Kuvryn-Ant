CREATE TABLE encrypted_secrets (
 instance_id uuid NOT NULL REFERENCES instances(id),
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 purpose text NOT NULL CHECK(octet_length(purpose) BETWEEN 1 AND 256),
 envelope jsonb NOT NULL CHECK(jsonb_typeof(envelope)='object') CHECK(octet_length(envelope::text)<=2097152),
 key_version text NOT NULL CHECK(key_version ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,id),
 CHECK((jsonb_typeof(envelope->'key_version')='string' AND envelope->>'key_version'=key_version) IS TRUE)
);
CREATE INDEX encrypted_secrets_rotation_idx ON encrypted_secrets(instance_id,key_version,id);
