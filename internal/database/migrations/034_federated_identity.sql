-- Phase 2 P2-M2: federated identity and policy (docs/planning/phase-2-federation.md
-- §6, §11). Additive: every new column defaults to local behaviour, and no row
-- is federation-managed until a signed policy document is applied.

-- Role names starting with "fed:" are reserved for federation-managed roles.
-- A local role that already uses the prefix is renamed so the reservation can
-- be enforced (role IDs, grants and memberships are unchanged).
UPDATE roles SET name='local-'||name WHERE name LIKE 'fed:%';

ALTER TABLE principals ADD COLUMN origin text NOT NULL DEFAULT 'local' CHECK(origin IN ('local','federation'));
ALTER TABLE principals ADD COLUMN federated_id uuid;
ALTER TABLE principals ADD CONSTRAINT principals_federated_id_unique UNIQUE(instance_id,federated_id);
ALTER TABLE principals ADD CONSTRAINT principals_federation_kind CHECK(origin='local' OR kind IN ('user','service'));

ALTER TABLE roles ADD COLUMN origin text NOT NULL DEFAULT 'local' CHECK(origin IN ('local','federation'));
ALTER TABLE roles ADD COLUMN federation_key text;
ALTER TABLE roles ADD COLUMN federation_generation bigint;
ALTER TABLE roles ADD CONSTRAINT roles_federation_prefix CHECK((name LIKE 'fed:%') = (origin='federation'));
ALTER TABLE roles ADD CONSTRAINT roles_federation_key CHECK((origin='federation') = (federation_key IS NOT NULL));

ALTER TABLE principal_roles ADD COLUMN origin text NOT NULL DEFAULT 'local' CHECK(origin IN ('local','federation'));

-- Federated grants reference portable feed keys. A key that is not bound to a
-- local feed is stored inert (feed_id NULL, inert set): it is never matched,
-- because an empty feed selector would otherwise mean "every feed".
ALTER TABLE role_grants ADD COLUMN origin text NOT NULL DEFAULT 'local' CHECK(origin IN ('local','federation'));
ALTER TABLE role_grants ADD COLUMN feed_key text CHECK(feed_key IS NULL OR octet_length(feed_key)<=200);
ALTER TABLE role_grants ADD COLUMN inert text CHECK(inert IS NULL OR inert IN ('unresolved_feed','admin_not_accepted'));
ALTER TABLE role_grants ADD CONSTRAINT role_grants_inert_federation CHECK(inert IS NULL OR origin='federation');
ALTER TABLE role_grants ADD CONSTRAINT role_grants_unresolved CHECK(inert IS DISTINCT FROM 'unresolved_feed' OR (feed_id IS NULL AND feed_key IS NOT NULL));
ALTER TABLE role_grants ADD CONSTRAINT role_grants_federation_no_source CHECK(origin='local' OR source_id IS NULL);

-- Local binding of a federation feed key to a local feed. Policy grants for the
-- key resolve to this feed; P2-M3 extends bindings with roles and descriptors.
CREATE TABLE federated_feed_bindings (
 instance_id uuid NOT NULL REFERENCES instances(id),
 feed_key text NOT NULL CHECK(feed_key ~ '^[a-z0-9][a-z0-9-]{0,62}/[a-z0-9][a-z0-9_-]{0,62}$'),
 feed_id uuid NOT NULL,
 created_by uuid,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,feed_key),
 UNIQUE(instance_id,feed_id),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id) ON DELETE CASCADE
);

-- Group bindings materialized from the applied policy (IdP group -> role).
CREATE TABLE federation_group_bindings (
 instance_id uuid NOT NULL,
 domain text NOT NULL,
 group_id text NOT NULL CHECK(octet_length(group_id) BETWEEN 1 AND 512),
 role_id uuid NOT NULL,
 PRIMARY KEY(instance_id,domain,group_id,role_id),
 FOREIGN KEY(instance_id,role_id) REFERENCES roles(instance_id,id) ON DELETE CASCADE
);

-- Groups observed for a federated principal at its last sign-in. Group
-- bindings are evaluated against these, so a policy change applies to
-- principals that already signed in.
CREATE TABLE federation_principal_groups (
 instance_id uuid NOT NULL,
 principal_id uuid NOT NULL,
 domain text NOT NULL,
 group_id text NOT NULL CHECK(octet_length(group_id) BETWEEN 1 AND 512),
 observed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,principal_id,domain,group_id),
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id) ON DELETE CASCADE
);

-- Policy document head, last-good bookkeeping and the authority's draft.
ALTER TABLE federation_state ADD COLUMN policy_generation bigint NOT NULL DEFAULT 0 CHECK(policy_generation>=0);
ALTER TABLE federation_state ADD COLUMN policy_hash text NOT NULL DEFAULT '';
ALTER TABLE federation_state ADD COLUMN policy_applied_at timestamptz;
ALTER TABLE federation_state ADD COLUMN policy_confirmed_at timestamptz;
ALTER TABLE federation_state ADD COLUMN policy_error text NOT NULL DEFAULT '' CHECK(octet_length(policy_error)<=1024);
ALTER TABLE federation_state ADD COLUMN policy_error_at timestamptz;
ALTER TABLE federation_state ADD COLUMN policy_report jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(policy_report)='object');
ALTER TABLE federation_state ADD COLUMN alert_state jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(alert_state)='object');

CREATE TABLE federation_policy_drafts (
 instance_id uuid NOT NULL REFERENCES instances(id),
 federation_id uuid NOT NULL,
 body jsonb NOT NULL CHECK(jsonb_typeof(body)='object' AND octet_length(body::text)<=1048576),
 base_generation bigint NOT NULL CHECK(base_generation>=0),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_by uuid,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,federation_id)
);

-- Local federation settings (never set by the federation): whether federated
-- instance-admin grants are honoured, and the optional hard staleness limit
-- after which federated principals are denied (D4, default off).
ALTER TABLE instance_settings ADD COLUMN federation_accept_admin_grants boolean NOT NULL DEFAULT false;
ALTER TABLE instance_settings ADD COLUMN federation_policy_max_age_seconds integer CHECK(federation_policy_max_age_seconds IS NULL OR federation_policy_max_age_seconds BETWEEN 300 AND 31536000);

ALTER TABLE federation_assertion_nonces ADD COLUMN issuer uuid;

-- Federation-managed rows change only inside a policy apply, which sets the
-- transaction-local flag ant.federation_apply=on. Everything else (management
-- API, ad-hoc SQL) gets SQLSTATE AFM01, reported as 409 federation_managed.
CREATE FUNCTION federation_apply_active() RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT coalesce(current_setting('ant.federation_apply', true),'')='on'
$$;

CREATE FUNCTION federation_guard_origin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF federation_apply_active() THEN
  RETURN coalesce(NEW, OLD);
 END IF;
 IF (TG_OP IN ('UPDATE','DELETE') AND OLD.origin='federation') OR (TG_OP IN ('INSERT','UPDATE') AND NEW.origin='federation') THEN
  RAISE EXCEPTION 'federation-managed row in %', TG_TABLE_NAME USING ERRCODE='AFM01';
 END IF;
 RETURN coalesce(NEW, OLD);
END $$;
CREATE TRIGGER roles_federation_guard BEFORE INSERT OR UPDATE OR DELETE ON roles FOR EACH ROW EXECUTE FUNCTION federation_guard_origin();
CREATE TRIGGER role_grants_federation_guard BEFORE INSERT OR UPDATE OR DELETE ON role_grants FOR EACH ROW EXECUTE FUNCTION federation_guard_origin();

-- Federated principals: identity columns are federation-managed, the local
-- revision counter is not (local role memberships bump it).
CREATE FUNCTION federation_guard_principal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF federation_apply_active() THEN
  RETURN coalesce(NEW, OLD);
 END IF;
 IF TG_OP='INSERT' AND NEW.origin='federation' THEN
  RAISE EXCEPTION 'federation-managed principal' USING ERRCODE='AFM01';
 END IF;
 IF TG_OP='DELETE' AND OLD.origin='federation' THEN
  RAISE EXCEPTION 'federation-managed principal' USING ERRCODE='AFM01';
 END IF;
 IF TG_OP='UPDATE' AND (OLD.origin='federation' OR NEW.origin='federation') AND
    (NEW.origin IS DISTINCT FROM OLD.origin OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.subject IS DISTINCT FROM OLD.subject
     OR NEW.issuer IS DISTINCT FROM OLD.issuer OR NEW.disabled IS DISTINCT FROM OLD.disabled OR NEW.federated_id IS DISTINCT FROM OLD.federated_id) THEN
  RAISE EXCEPTION 'federation-managed principal' USING ERRCODE='AFM01';
 END IF;
 RETURN coalesce(NEW, OLD);
END $$;
CREATE TRIGGER principals_federation_guard BEFORE INSERT OR UPDATE OR DELETE ON principals FOR EACH ROW EXECUTE FUNCTION federation_guard_principal();

-- Memberships: federation rows are managed; a local membership may not
-- reference a federation-managed role (federated roles are assigned only by
-- policy bindings).
CREATE FUNCTION federation_guard_membership() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF federation_apply_active() THEN
  RETURN coalesce(NEW, OLD);
 END IF;
 IF (TG_OP IN ('UPDATE','DELETE') AND OLD.origin='federation') OR (TG_OP IN ('INSERT','UPDATE') AND NEW.origin='federation') THEN
  RAISE EXCEPTION 'federation-managed membership' USING ERRCODE='AFM01';
 END IF;
 IF TG_OP IN ('INSERT','UPDATE') AND EXISTS(SELECT 1 FROM roles r WHERE r.instance_id=NEW.instance_id AND r.id=NEW.role_id AND r.origin='federation') THEN
  RAISE EXCEPTION 'federation-managed role' USING ERRCODE='AFM01';
 END IF;
 RETURN coalesce(NEW, OLD);
END $$;
CREATE TRIGGER principal_roles_federation_guard BEFORE INSERT OR UPDATE OR DELETE ON principal_roles FOR EACH ROW EXECUTE FUNCTION federation_guard_membership();
