CREATE TABLE event_subscriptions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 endpoint text NOT NULL CHECK(octet_length(endpoint) BETWEEN 1 AND 2048),
 secret_ref text NOT NULL CHECK(octet_length(secret_ref) BETWEEN 1 AND 256),
 enabled boolean NOT NULL DEFAULT true,
 checkpoint bigint NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(instance_id,id)
);
CREATE TABLE event_deliveries (
 subscription_id uuid NOT NULL REFERENCES event_subscriptions(id),
 event_id bigint NOT NULL REFERENCES outbox_events(id),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','delivered','dead')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 available_at timestamptz NOT NULL DEFAULT now(),
 lease_token uuid,
 lease_until timestamptz,
 last_failure text,
 delivered_at timestamptz,
 PRIMARY KEY(subscription_id,event_id),
 CHECK((lease_token IS NULL)=(lease_until IS NULL))
);
CREATE INDEX event_delivery_pending_idx ON event_deliveries(available_at,event_id) WHERE state='pending';
