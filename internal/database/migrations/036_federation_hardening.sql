-- Federation security hardening (docs/planning/phase-2-federation.md §14).

-- The document log stays append-only, except for the snapshot resync of §6.4
-- rule 6: a member whose chain diverged from the authority's (for example a
-- relayed copy of a document with another signature spelling) replaces the
-- divergent segment with the authority's verified chain. That runs only in a
-- transaction that sets ant.federation_resync=on, and is audited
-- (federation.document.resynced). Updates are never allowed.
CREATE OR REPLACE FUNCTION federation_documents_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'DELETE' AND coalesce(current_setting('ant.federation_resync', true),'') = 'on' THEN
  RETURN OLD;
 END IF;
 RAISE EXCEPTION 'federation_documents is append-only';
END $$;

-- A federated artifact or feed allow without a feed key selects every local
-- feed; like a federated instance-admin allow it stays inert until the member
-- sets accept_admin_grants.
ALTER TABLE role_grants DROP CONSTRAINT role_grants_inert_check;
ALTER TABLE role_grants ADD CONSTRAINT role_grants_inert_check CHECK(inert IS NULL OR inert IN ('unresolved_feed','admin_not_accepted','wildcard_not_accepted'));

-- Per-issuer cap on assertion replay records (counted over unexpired rows).
CREATE INDEX federation_assertion_nonces_issuer_idx ON federation_assertion_nonces(instance_id,issuer,expires_at);
