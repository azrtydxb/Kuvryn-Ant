CREATE TABLE instances (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 name text NOT NULL CHECK (name <> ''),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE principals (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 kind text NOT NULL CHECK (kind IN ('user','service','emergency')),
 subject text NOT NULL CHECK (subject <> ''),
 issuer text NOT NULL DEFAULT 'local',
 disabled boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(instance_id, issuer, subject), UNIQUE(instance_id,id)
);
CREATE TABLE feeds (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 slug text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
 format text NOT NULL CHECK (format <> ''),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 anonymous_read boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(instance_id,slug), UNIQUE(instance_id,id)
);
CREATE TABLE sources (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 name text NOT NULL CHECK (name <> ''),
 kind text NOT NULL CHECK (kind IN ('local','remote','replica')),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(instance_id,name), UNIQUE(instance_id,id)
);
CREATE TABLE feed_sources (
 instance_id uuid NOT NULL,
 feed_id uuid NOT NULL,
 source_id uuid NOT NULL,
 priority integer NOT NULL,
 PRIMARY KEY(feed_id,source_id),
 UNIQUE(feed_id,priority),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id),
 FOREIGN KEY(instance_id,source_id) REFERENCES sources(instance_id,id)
);
CREATE TABLE components (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL,
 source_id uuid NOT NULL,
 coordinate text NOT NULL CHECK (coordinate <> ''),
 metadata jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
 FOREIGN KEY(instance_id,source_id) REFERENCES sources(instance_id,id),
 UNIQUE(source_id,coordinate), UNIQUE(instance_id,id)
);
CREATE TABLE versions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL,
 component_id uuid NOT NULL,
 version text NOT NULL CHECK (version <> ''),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(instance_id,component_id) REFERENCES components(instance_id,id),
 UNIQUE(component_id,version), UNIQUE(instance_id,id), UNIQUE(component_id,id)
);
CREATE TABLE dedup_domains (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 name text NOT NULL,
 UNIQUE(instance_id,name), UNIQUE(instance_id,id)
);
CREATE TABLE blobs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL,
 dedup_domain_id uuid NOT NULL,
 digest_algorithm text NOT NULL DEFAULT 'sha256' CHECK (digest_algorithm='sha256'),
 digest text NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
 size_bytes bigint NOT NULL CHECK (size_bytes>=0),
 storage_key text NOT NULL CHECK (storage_key<>''),
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(instance_id,dedup_domain_id) REFERENCES dedup_domains(instance_id,id),
 UNIQUE(dedup_domain_id,digest_algorithm,digest), UNIQUE(instance_id,id)
);
CREATE TABLE assets (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL,
 version_id uuid NOT NULL,
 asset_key text NOT NULL CHECK (asset_key<>''),
 blob_id uuid NOT NULL,
 media_type text NOT NULL DEFAULT 'application/octet-stream',
 provenance jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(provenance)='object'),
 FOREIGN KEY(instance_id,version_id) REFERENCES versions(instance_id,id),
 FOREIGN KEY(instance_id,blob_id) REFERENCES blobs(instance_id,id),
 UNIQUE(version_id,asset_key)
);
CREATE INDEX assets_blob_id_idx ON assets(blob_id);
CREATE TABLE tags (
 component_id uuid NOT NULL REFERENCES components(id),
 name text NOT NULL CHECK (name<>''),
 version_id uuid NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
 PRIMARY KEY(component_id,name),
 FOREIGN KEY(component_id,version_id) REFERENCES versions(component_id,id)
);
CREATE TABLE audit_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 instance_id uuid NOT NULL REFERENCES instances(id),
 principal_id uuid,
 action text NOT NULL,
 resource_id uuid,
 details jsonb NOT NULL DEFAULT '{}'::jsonb,
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id)
);
CREATE INDEX audit_instance_created_idx ON audit_events(instance_id,created_at,id);
CREATE TABLE outbox_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 audit_id bigint NOT NULL REFERENCES audit_events(id),
 topic text NOT NULL,
 payload jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 delivered_at timestamptz
);
CREATE INDEX outbox_pending_idx ON outbox_events(id) WHERE delivered_at IS NULL;
