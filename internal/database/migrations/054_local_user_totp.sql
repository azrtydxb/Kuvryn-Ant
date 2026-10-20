-- Instance setting: let ordinary local users enrol their own TOTP second factor.
-- The break-glass administrator can always manage its own factor.
ALTER TABLE identity_policy ADD COLUMN local_user_totp boolean NOT NULL DEFAULT false;
