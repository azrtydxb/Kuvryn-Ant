-- Rate limits and storage quotas (spec 6.5), reusable policy templates and
-- configuration-time loop protection (spec 6.3).

-- One limits document per instance. Rules are validated by the management API;
-- the document is small (at most 1000 rules each) and read as a whole.
CREATE TABLE limits_config (
 instance_id uuid PRIMARY KEY REFERENCES instances(id),
 rate_limits jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(rate_limits)='array' AND jsonb_array_length(rate_limits)<=1000),
 quotas jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(quotas)='array' AND jsonb_array_length(quotas)<=1000),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT now()
);

-- Reusable policy fragments. A publish template carries default_source_id and
-- rules; a read template carries the read-resolution flags. A template may
-- extend one template of the same kind; cycles are rejected by the API.
CREATE TABLE policy_templates (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 name text NOT NULL CHECK(name ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$'),
 description text NOT NULL DEFAULT '' CHECK(octet_length(description)<=1024),
 kind text NOT NULL CHECK(kind IN ('publish','read')),
 extends_id uuid,
 body jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(body)='object' AND octet_length(body::text)<=1048576),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(instance_id,name), UNIQUE(instance_id,id),
 CHECK(extends_id IS NULL OR extends_id<>id),
 FOREIGN KEY(instance_id,extends_id) REFERENCES policy_templates(instance_id,id)
);

-- The templates a feed references. The feed's own publish policy (migration 009)
-- is applied on top of the bound publish template.
CREATE TABLE feed_policy_bindings (
 instance_id uuid NOT NULL,
 feed_id uuid PRIMARY KEY,
 publish_template_id uuid,
 read_template_id uuid,
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id),
 FOREIGN KEY(instance_id,publish_template_id) REFERENCES policy_templates(instance_id,id),
 FOREIGN KEY(instance_id,read_template_id) REFERENCES policy_templates(instance_id,id)
);
CREATE INDEX feed_policy_bindings_publish_idx ON feed_policy_bindings(publish_template_id) WHERE publish_template_id IS NOT NULL;
CREATE INDEX feed_policy_bindings_read_idx ON feed_policy_bindings(read_template_id) WHERE read_template_id IS NOT NULL;
