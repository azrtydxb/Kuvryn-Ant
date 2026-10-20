-- A browser session remembers the display profile of the identity it was issued
-- for (OIDC name/email/picture claims, SAML attributes) so the console can show
-- who is signed in without calling the IdP again. idp_id_token is the OIDC ID
-- token, kept only as the id_token_hint of RP-initiated logout; it is deleted
-- with the session and never returned by any API.
ALTER TABLE browser_sessions ADD COLUMN display_name text CHECK(length(display_name)<=256);
ALTER TABLE browser_sessions ADD COLUMN email text CHECK(length(email)<=320);
ALTER TABLE browser_sessions ADD COLUMN picture text CHECK(length(picture)<=2048);
ALTER TABLE browser_sessions ADD COLUMN idp_id_token text CHECK(length(idp_id_token)<=16384);
