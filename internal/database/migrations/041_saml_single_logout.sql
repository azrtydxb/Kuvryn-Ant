-- SAML single logout. A browser session issued from a SAML assertion remembers
-- the IdP session index and NameID format so an SP-initiated LogoutRequest can
-- be built, and an IdP-initiated LogoutRequest can end the matching sessions.
ALTER TABLE browser_sessions ADD COLUMN idp_session_index text CHECK(length(idp_session_index)<=1024);
ALTER TABLE browser_sessions ADD COLUMN idp_name_id_format text CHECK(length(idp_name_id_format)<=256);
CREATE INDEX browser_sessions_external ON browser_sessions(instance_id,issuer,subject);
-- SP-initiated logout stages the signed request here, bound to the browser, until
-- the navigation that posts it to the IdP; the single-use state row then also
-- correlates the IdP's LogoutResponse.
ALTER TABLE enterprise_login_states ADD COLUMN payload text CHECK(length(payload)<=65536);
ALTER TABLE enterprise_login_states DROP CONSTRAINT IF EXISTS enterprise_login_states_kind_check;
ALTER TABLE enterprise_login_states ADD CONSTRAINT enterprise_login_states_kind_check CHECK(kind IN ('saml','ldap','saml-logout'));
