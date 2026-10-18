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
