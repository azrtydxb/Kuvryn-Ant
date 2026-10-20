-- The OIDC ID token kept for RP-initiated logout (migration 051) was stored as
-- plaintext. It is now an encrypted envelope bound to the session row. Existing
-- plaintext values are discarded: those sessions simply sign out at the
-- provider without an id_token_hint.
UPDATE browser_sessions SET idp_id_token=NULL WHERE idp_id_token IS NOT NULL;
