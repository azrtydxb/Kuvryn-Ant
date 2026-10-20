-- Incoming webhooks: <public URL>/hook/<slug> receives a call from an external
-- system (GitHub, GitLab, Docker Hub, Artifactory, Nexus, npm, CI) and runs one
-- configured action. The delivery table is both the delivery log and the work
-- queue: a worker under the instance writer lease claims pending rows.
CREATE TABLE incoming_hooks (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 slug text NOT NULL CHECK(slug ~ '^[a-z0-9-]{1,63}$'),
 description text NOT NULL DEFAULT '' CHECK(octet_length(description)<=1000),
 enabled boolean NOT NULL DEFAULT true,
 auth_mode text NOT NULL CHECK(auth_mode IN ('hmac','url_token','bearer')),
 hmac_format text CHECK(hmac_format IS NULL OR hmac_format IN ('github','gitlab','ant','npm','artifactory','nexus')),
 signing_secret_id uuid,
 -- sha256 of the random path suffix; the suffix itself is shown once.
 token_hash bytea CHECK(token_hash IS NULL OR octet_length(token_hash)=32),
 token_hint text CHECK(token_hint IS NULL OR octet_length(token_hint)<=8),
 bearer_scope text CHECK(bearer_scope IS NULL OR bearer_scope IN ('configure','replicate','admin')),
 action_type text NOT NULL CHECK(action_type IN ('prewarm','replication_run','purge_source_cache','clean_cache','upstream_notify','relay')),
 action jsonb NOT NULL CHECK(jsonb_typeof(action)='object' AND octet_length(action::text)<=16384),
 rate_per_minute integer NOT NULL DEFAULT 60 CHECK(rate_per_minute BETWEEN 1 AND 6000),
 created_by uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 UNIQUE(instance_id,slug),
 UNIQUE(instance_id,id),
 FOREIGN KEY(instance_id,signing_secret_id) REFERENCES webhook_signing_secrets(instance_id,id),
 CHECK((auth_mode='hmac')=(signing_secret_id IS NOT NULL AND hmac_format IS NOT NULL)),
 CHECK((auth_mode='url_token')=(token_hash IS NOT NULL)),
 CHECK((auth_mode='bearer')=(bearer_scope IS NOT NULL))
);
CREATE INDEX incoming_hooks_secret_idx ON incoming_hooks(instance_id,signing_secret_id) WHERE signing_secret_id IS NOT NULL;

CREATE TABLE incoming_hook_deliveries (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL,
 hook_id uuid NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 source_ip text NOT NULL DEFAULT '' CHECK(octet_length(source_ip)<=64),
 -- ok: authenticated; test: sent from the console; replay: redelivered from the log;
 -- invalid, missing, replay_rejected, too_large: rejected before any action.
 auth_result text NOT NULL CHECK(auth_result IN ('ok','test','redelivery','invalid','missing','duplicate','too_large')),
 state text NOT NULL CHECK(state IN ('pending','running','succeeded','failed','rejected')),
 attempts integer NOT NULL DEFAULT 0,
 available_at timestamptz NOT NULL DEFAULT now(),
 lease_until timestamptz,
 finished_at timestamptz,
 action_result text CHECK(action_result IS NULL OR octet_length(action_result)<=64),
 detail jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(detail)='object' AND octet_length(detail::text)<=8192),
 error text CHECK(error IS NULL OR octet_length(error)<=512),
 payload bytea,
 payload_size integer NOT NULL DEFAULT 0,
 headers jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(headers)='object' AND octet_length(headers::text)<=16384),
 preview text NOT NULL DEFAULT '' CHECK(octet_length(preview)<=4096),
 delivery_ref text CHECK(delivery_ref IS NULL OR octet_length(delivery_ref)<=200),
 redelivery_of uuid,
 FOREIGN KEY(instance_id,hook_id) REFERENCES incoming_hooks(instance_id,id) ON DELETE CASCADE
);
CREATE INDEX incoming_hook_deliveries_hook_idx ON incoming_hook_deliveries(hook_id,received_at DESC,id DESC);
CREATE INDEX incoming_hook_deliveries_queue_idx ON incoming_hook_deliveries(available_at) WHERE state IN ('pending','running');
CREATE INDEX incoming_hook_deliveries_age_idx ON incoming_hook_deliveries(received_at);

-- Provider delivery ids (X-GitHub-Delivery, X-Gitlab-Event-UUID) remembered for
-- 24 hours so a captured request cannot be replayed.
CREATE TABLE incoming_hook_replay (
 hook_id uuid NOT NULL REFERENCES incoming_hooks(id) ON DELETE CASCADE,
 delivery_ref text NOT NULL CHECK(octet_length(delivery_ref) BETWEEN 1 AND 200),
 seen_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(hook_id,delivery_ref)
);
CREATE INDEX incoming_hook_replay_age_idx ON incoming_hook_replay(seen_at);
