-- Federation operations (P2-M6): authority handover and forced recovery,
-- recovery-key drill, two-phase join.

-- pending_authority_*: an authority key generated here for the next epoch and
-- not yet certified (planned handover target, or forced recovery operator).
-- handover_*: a planned handover this authority started; publishing is
-- blocked until the new epoch is applied or the handover is aborted.
-- fenced_*: this instance was the authority and learned of a higher epoch.
-- recovery_*: the recovery-key possession drill (a stored one-time challenge
-- and the time a valid signature over it was last presented).
ALTER TABLE federation_state ADD COLUMN pending_authority_secret_id uuid;
ALTER TABLE federation_state ADD COLUMN pending_authority_epoch bigint CHECK(pending_authority_epoch IS NULL OR pending_authority_epoch>1);
ALTER TABLE federation_state ADD COLUMN handover_to uuid;
ALTER TABLE federation_state ADD COLUMN handover_base_generation bigint;
ALTER TABLE federation_state ADD COLUMN handover_started_at timestamptz;
ALTER TABLE federation_state ADD COLUMN fenced_at timestamptz;
ALTER TABLE federation_state ADD COLUMN fenced_by_epoch bigint;
ALTER TABLE federation_state ADD COLUMN recovery_challenge text NOT NULL DEFAULT '';
ALTER TABLE federation_state ADD COLUMN recovery_challenge_expires_at timestamptz;
ALTER TABLE federation_state ADD COLUMN recovery_verified_at timestamptz;

-- A join claims its invitation; the joiner's confirmation consumes it. An
-- unconfirmed claim (a relayed or interrupted join) is released after a
-- timeout and the provisional member removed, so the invitation survives.
ALTER TABLE federation_invitations ADD COLUMN claimed_at timestamptz;
ALTER TABLE federation_invitations ADD COLUMN claimed_by uuid;
