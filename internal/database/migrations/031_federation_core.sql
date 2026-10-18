-- Phase 2 federation foundation (docs/planning/phase-2-federation.md §11).
-- Additive only: nothing here is used until an administrator founds or joins a
-- federation. Key material lives in encrypted_secrets (purposes federation-ca,
-- federation-authority:<epoch>, federation-member-key); these tables hold only
-- public material and signed documents.

-- One row per instance once federation is initialized. state none means the
-- instance never founded or joined; left and removed keep the history.
CREATE TABLE federation_state (
 instance_id uuid PRIMARY KEY REFERENCES instances(id),
 federation_id uuid,
 name text CHECK(name IS NULL OR name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
 role text CHECK(role IS NULL OR role IN ('member','authority')),
 state text NOT NULL DEFAULT 'none' CHECK(state IN ('none','joining','active','left','removed')),
 authority_url text CHECK(authority_url IS NULL OR octet_length(authority_url)<=2048),
 federation_url text CHECK(federation_url IS NULL OR octet_length(federation_url)<=2048),
 ca_sha256 text CHECK(ca_sha256 IS NULL OR ca_sha256 ~ '^[0-9a-f]{64}$'),
 ca_secret_id uuid,
 authority_secret_id uuid,
 member_secret_id uuid,
 member_cert text CHECK(member_cert IS NULL OR octet_length(member_cert)<=16384),
 member_cert_serial text,
 member_cert_expires_at timestamptz,
 applied_generation bigint NOT NULL DEFAULT 0 CHECK(applied_generation>=0),
 applied_hash text NOT NULL DEFAULT '',
 applied_at timestamptz,
 last_sync_attempt_at timestamptz,
 last_sync_success_at timestamptz,
 last_sync_error text NOT NULL DEFAULT '' CHECK(octet_length(last_sync_error)<=1024),
 last_sync_source text NOT NULL DEFAULT '',
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK(state IN ('none') OR federation_id IS NOT NULL)
);

-- Membership as applied from the latest verified membership document. Document
-- columns are rewritten on every apply; the runtime columns (last_seen_at,
-- rtt_ms, skew_ms, health) are local observations kept across applies.
CREATE TABLE federation_members (
 instance_id uuid NOT NULL REFERENCES instances(id),
 federation_id uuid NOT NULL,
 member_instance_id uuid NOT NULL,
 name text NOT NULL,
 api_url text NOT NULL DEFAULT '',
 federation_url text NOT NULL,
 cert_sha256 text NOT NULL DEFAULT '',
 cert_serial text NOT NULL DEFAULT '',
 signing_key text NOT NULL,
 labels jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(labels)='object'),
 status text NOT NULL CHECK(status IN ('active','suspended','removed')),
 software_version text NOT NULL DEFAULT '',
 protocol_min integer NOT NULL DEFAULT 1,
 protocol_max integer NOT NULL DEFAULT 1,
 joined_at timestamptz,
 membership_generation bigint NOT NULL,
 last_seen_at timestamptz,
 rtt_ms integer,
 skew_ms integer,
 health jsonb NOT NULL DEFAULT '{}'::jsonb,
 PRIMARY KEY(instance_id,federation_id,member_instance_id)
);

-- Append-only log of every verified document. Every member keeps the same log,
-- so the signature chain can be audited anywhere. Rejected documents are
-- recorded in audit_events (federation.document.rejected), not here.
CREATE TABLE federation_documents (
 instance_id uuid NOT NULL REFERENCES instances(id),
 federation_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN ('membership','policy','topology')),
 generation bigint NOT NULL CHECK(generation>0),
 authority_epoch bigint NOT NULL CHECK(authority_epoch>0),
 hash text NOT NULL CHECK(hash ~ '^sha256:[0-9a-f]{64}$'),
 prev_hash text NOT NULL DEFAULT '',
 body jsonb NOT NULL CHECK(jsonb_typeof(body)='object' AND octet_length(body::text)<=4194304),
 received_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,federation_id,kind,generation)
);
CREATE FUNCTION federation_documents_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'federation_documents is append-only';
END $$;
CREATE TRIGGER federation_documents_immutable BEFORE UPDATE OR DELETE ON federation_documents FOR EACH ROW EXECUTE FUNCTION federation_documents_append_only();

-- One-time join invitations, held by the authority. Only a SHA-256 verifier of
-- the secret is stored; the secret is returned once at creation.
CREATE TABLE federation_invitations (
 instance_id uuid NOT NULL REFERENCES instances(id),
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 federation_id uuid NOT NULL,
 verifier bytea NOT NULL CHECK(octet_length(verifier)=32),
 name text NOT NULL CHECK(name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
 labels jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(labels)='object'),
 expires_at timestamptz NOT NULL,
 created_by uuid,
 created_at timestamptz NOT NULL DEFAULT now(),
 consumed_at timestamptz,
 consumed_by uuid,
 revoked_at timestamptz,
 PRIMARY KEY(instance_id,id)
);

-- Replay cache for signed principal assertions (P2-M2) and peer request nonces.
CREATE TABLE federation_assertion_nonces (
 instance_id uuid NOT NULL REFERENCES instances(id),
 jti text NOT NULL CHECK(octet_length(jti) BETWEEN 1 AND 128),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(instance_id,jti)
);
CREATE INDEX federation_assertion_nonces_expiry_idx ON federation_assertion_nonces(expires_at);
