-- Per-feed client authentication policy for package-protocol traffic, named
-- network groups for anonymous CIDR restriction, personal access tokens and
-- GitHub Actions OIDC trust rules.

CREATE TABLE network_groups (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 name text NOT NULL CHECK(name ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
 cidrs text[] NOT NULL CHECK(cardinality(cidrs) BETWEEN 1 AND 64),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(instance_id,name), UNIQUE(instance_id,id)
);

-- feed_id NULL is the instance default policy; a feed row overrides it.
CREATE TABLE client_auth_policies (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 feed_id uuid,
 methods text[] NOT NULL CHECK(cardinality(methods)<=5 AND methods <@ ARRAY['anonymous','token','basic-directory','pat','github-oidc']),
 anonymous_network_group_ids uuid[] NOT NULL DEFAULT '{}' CHECK(cardinality(anonymous_network_group_ids)<=32),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX client_auth_policies_feed_idx ON client_auth_policies(instance_id,feed_id) WHERE feed_id IS NOT NULL;
CREATE UNIQUE INDEX client_auth_policies_default_idx ON client_auth_policies(instance_id) WHERE feed_id IS NULL;

CREATE TABLE personal_access_tokens (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL,
 principal_id uuid NOT NULL,
 name text NOT NULL CHECK(name<>'' AND octet_length(name)<=128),
 verifier bytea NOT NULL UNIQUE CHECK(octet_length(verifier)=32),
 scopes jsonb NOT NULL CHECK(jsonb_typeof(scopes)='array'),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 last_used_at timestamptz,
 revoked_at timestamptz,
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id),
 CHECK(expires_at>created_at),
 UNIQUE(instance_id,id)
);
CREATE INDEX personal_access_tokens_principal_idx ON personal_access_tokens(instance_id,principal_id);

CREATE TABLE github_oidc_config (
 instance_id uuid PRIMARY KEY REFERENCES instances(id),
 audience text NOT NULL DEFAULT '' CHECK(octet_length(audience)<=256),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE github_oidc_trust_rules (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL,
 name text NOT NULL CHECK(name<>'' AND octet_length(name)<=128),
 org text NOT NULL CHECK(org<>'' AND octet_length(org)<=128),
 repo text NOT NULL DEFAULT '*' CHECK(repo<>'' AND octet_length(repo)<=256),
 ref text NOT NULL DEFAULT '*' CHECK(ref<>'' AND octet_length(ref)<=256),
 environment text NOT NULL DEFAULT '*' CHECK(environment<>'' AND octet_length(environment)<=256),
 workflow text NOT NULL DEFAULT '*' CHECK(workflow<>'' AND octet_length(workflow)<=256),
 principal_id uuid NOT NULL,
 scopes jsonb NOT NULL CHECK(jsonb_typeof(scopes)='array'),
 ttl_seconds integer NOT NULL DEFAULT 900 CHECK(ttl_seconds BETWEEN 60 AND 3600),
 enabled boolean NOT NULL DEFAULT true,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id),
 UNIQUE(instance_id,name), UNIQUE(instance_id,id)
);
