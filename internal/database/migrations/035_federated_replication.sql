-- Phase 2 P2-M3: coordinated replication (docs/planning/phase-2-federation.md
-- §9, §11). Additive: every new column defaults to Phase 1 behaviour, and
-- nothing here is used until a signed topology document is applied.

-- Federation-mode peers authenticate over the federation mTLS listener with
-- the member certificate and request signature instead of a bearer token.
-- url is informational for them (the member's API URL); the transport uses
-- the federation URL of the applied membership.
ALTER TABLE replication_peers ADD COLUMN auth_mode text NOT NULL DEFAULT 'token' CHECK(auth_mode IN ('token','federation'));
ALTER TABLE replication_peers ADD COLUMN member_instance_id uuid;
ALTER TABLE replication_peers ALTER COLUMN token_secret_id DROP NOT NULL;
ALTER TABLE replication_peers ADD CONSTRAINT replication_peers_auth_mode CHECK(
 ((auth_mode='token') = (token_secret_id IS NOT NULL)) AND ((auth_mode='federation') = (member_instance_id IS NOT NULL)));
CREATE UNIQUE INDEX replication_peers_member_idx ON replication_peers(instance_id,member_instance_id) WHERE member_instance_id IS NOT NULL;

-- Jobs created, adopted or re-pointed by the federation replication
-- reconciler. Federation-managed jobs are read-only through the local job API
-- (409 federation_managed); a removed rule releases its job back to origin
-- local (disabled), keeping content and the replica source.
ALTER TABLE replication_jobs ADD COLUMN origin text NOT NULL DEFAULT 'local' CHECK(origin IN ('local','federation'));
ALTER TABLE replication_jobs ADD COLUMN federation_rule_id uuid;
ALTER TABLE replication_jobs ADD COLUMN federation_feed_key text CHECK(federation_feed_key IS NULL OR federation_feed_key ~ '^[a-z0-9][a-z0-9-]{0,62}/[a-z0-9][a-z0-9_-]{0,62}$');
ALTER TABLE replication_jobs ADD COLUMN federation_rule_revision bigint;
ALTER TABLE replication_jobs ADD COLUMN adopted_at timestamptz;
ALTER TABLE replication_jobs ADD CONSTRAINT replication_jobs_federation_origin CHECK(origin='local' OR federation_feed_key IS NOT NULL);
ALTER TABLE replication_jobs ADD CONSTRAINT replication_jobs_federation_rule CHECK(federation_rule_id IS NULL OR origin='federation');
CREATE UNIQUE INDEX replication_jobs_rule_idx ON replication_jobs(instance_id,federation_rule_id) WHERE federation_rule_id IS NOT NULL;

-- Topology document head (same bookkeeping as the policy head).
ALTER TABLE federation_state ADD COLUMN topology_generation bigint NOT NULL DEFAULT 0 CHECK(topology_generation>=0);
ALTER TABLE federation_state ADD COLUMN topology_hash text NOT NULL DEFAULT '';
ALTER TABLE federation_state ADD COLUMN topology_applied_at timestamptz;
ALTER TABLE federation_state ADD COLUMN topology_confirmed_at timestamptz;
ALTER TABLE federation_state ADD COLUMN topology_error text NOT NULL DEFAULT '' CHECK(octet_length(topology_error)<=1024);
ALTER TABLE federation_state ADD COLUMN topology_error_at timestamptz;

-- Feeds this instance currently holds as the federation home. A topology
-- generation that names another home freezes the feed in the same
-- transaction (demotion happens before any further write is accepted).
CREATE TABLE federation_home_feeds (
 instance_id uuid NOT NULL REFERENCES instances(id),
 feed_key text NOT NULL,
 feed_id uuid NOT NULL,
 since_generation bigint NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,feed_key),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id) ON DELETE CASCADE
);

-- A frozen feed rejects publishes and deletes with 503 feed_transferring
-- (planned transfer freeze, or a demotion in progress).
CREATE TABLE federation_feed_freezes (
 instance_id uuid NOT NULL REFERENCES instances(id),
 feed_id uuid NOT NULL,
 feed_key text NOT NULL,
 transfer_id uuid,
 reason text NOT NULL CHECK(reason IN ('transfer','demote')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,feed_id),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id) ON DELETE CASCADE
);

-- Local, idempotent record of every transfer step this member executed.
CREATE TABLE federation_transfer_steps (
 instance_id uuid NOT NULL REFERENCES instances(id),
 transfer_id uuid NOT NULL,
 feed_key text NOT NULL,
 step text NOT NULL CHECK(step IN ('freeze','promote','demote')),
 detail jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(detail)='object'),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,transfer_id,step)
);

-- Content an old home accepted that the new home may not have (forced
-- transfer): the old local sources are detached and kept for manual
-- reconciliation. Nothing is merged automatically.
CREATE TABLE federation_quarantines (
 instance_id uuid NOT NULL REFERENCES instances(id),
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 feed_key text NOT NULL,
 transfer_id uuid,
 feed_id uuid,
 source_id uuid NOT NULL,
 divergent_changes bigint,
 created_at timestamptz NOT NULL DEFAULT now(),
 resolved_at timestamptz,
 resolved_by uuid,
 PRIMARY KEY(instance_id,id)
);

-- Last reconcile outcome per rule that targets this member.
CREATE TABLE federation_rule_status (
 instance_id uuid NOT NULL REFERENCES instances(id),
 rule_id uuid NOT NULL,
 feed_key text NOT NULL,
 state text NOT NULL,
 detail text NOT NULL DEFAULT '' CHECK(octet_length(detail)<=1024),
 job_id uuid,
 home_feed_id uuid,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,rule_id)
);
