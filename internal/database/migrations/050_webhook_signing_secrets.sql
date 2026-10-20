-- Webhook signing secrets become first-class, named, shared objects. The value
-- stays in encrypted_secrets (purpose 'webhook'); this table holds metadata and
-- the optional previous value that remains valid for a rotation grace window.
CREATE TABLE webhook_signing_secrets (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 name text NOT NULL CHECK(octet_length(name) BETWEEN 1 AND 200),
 description text NOT NULL DEFAULT '' CHECK(octet_length(description)<=1000),
 secret_id uuid NOT NULL,
 secret_purpose text NOT NULL DEFAULT 'webhook' CHECK(secret_purpose='webhook'),
 previous_secret_id uuid,
 previous_purpose text CHECK(previous_purpose IS NULL OR previous_purpose='webhook'),
 previous_expires_at timestamptz,
 created_by uuid,
 created_at timestamptz NOT NULL DEFAULT now(),
 rotated_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 UNIQUE(instance_id,id),
 UNIQUE(instance_id,name),
 UNIQUE(secret_id),
 CHECK((previous_secret_id IS NULL)=(previous_expires_at IS NULL) AND (previous_secret_id IS NULL)=(previous_purpose IS NULL)),
 FOREIGN KEY(instance_id,secret_id,secret_purpose) REFERENCES encrypted_secrets(instance_id,id,purpose),
 FOREIGN KEY(instance_id,previous_secret_id,previous_purpose) REFERENCES encrypted_secrets(instance_id,id,purpose)
);
-- A webhook either has a slug (name) under the instance webhook base URL, or is
-- a custom endpoint. endpoint always holds the resolved URL that is delivered to.
ALTER TABLE event_subscriptions
 ADD COLUMN name text CHECK(name IS NULL OR name ~ '^[a-z0-9-]{1,63}$'),
 ADD COLUMN custom_endpoint boolean NOT NULL DEFAULT true,
 ADD COLUMN signing_secret_id uuid,
 ALTER COLUMN secret_ref DROP NOT NULL;

-- Existing webhooks keep their free-form endpoint (marked custom) and their
-- encrypted value: the same ciphertext row is adopted by a signing secret named
-- after the webhook (and sharing its ID), so delivery signatures are unchanged.
INSERT INTO webhook_signing_secrets(id,instance_id,name,description,secret_id,created_at)
 SELECT s.id,s.instance_id,'webhook-'||left(s.id::text,8),'Migrated from the inline secret of an existing webhook.',e.id,s.created_at
 FROM event_subscriptions s JOIN encrypted_secrets e ON e.instance_id=s.instance_id AND e.id::text=s.secret_ref AND e.purpose='webhook'
 ON CONFLICT DO NOTHING;
UPDATE event_subscriptions s SET signing_secret_id=s.id,secret_ref=NULL WHERE EXISTS(SELECT 1 FROM webhook_signing_secrets w WHERE w.id=s.id);

ALTER TABLE event_subscriptions
 ADD CONSTRAINT event_subscriptions_secret_present CHECK(secret_ref IS NOT NULL OR signing_secret_id IS NOT NULL),
 ADD CONSTRAINT event_subscriptions_slug_or_custom CHECK(custom_endpoint OR name IS NOT NULL),
 ADD CONSTRAINT event_subscriptions_signing_secret_fk FOREIGN KEY(instance_id,signing_secret_id) REFERENCES webhook_signing_secrets(instance_id,id);
CREATE UNIQUE INDEX event_subscriptions_name_idx ON event_subscriptions(instance_id,name) WHERE name IS NOT NULL;
CREATE INDEX event_subscriptions_signing_secret_idx ON event_subscriptions(instance_id,signing_secret_id) WHERE signing_secret_id IS NOT NULL;

ALTER TABLE instance_settings ADD COLUMN webhook_base_url text CHECK(webhook_base_url IS NULL OR octet_length(webhook_base_url) BETWEEN 1 AND 1900);
