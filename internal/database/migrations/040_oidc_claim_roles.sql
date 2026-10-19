-- OIDC claim -> role mapping: principals created just-in-time at login from a
-- provider's claim mapping are marked on their identity binding, and the
-- memberships derived from claims carry their own origin so a later login can
-- add and remove exactly those and never touch explicit (local) memberships.
ALTER TABLE oidc_identity_bindings ADD COLUMN claim_managed boolean NOT NULL DEFAULT false;
ALTER TABLE principal_roles DROP CONSTRAINT principal_roles_origin_check;
ALTER TABLE principal_roles ADD CONSTRAINT principal_roles_origin_check CHECK(origin IN ('local','federation','claim'));
