-- Outgoing webhooks are configured in the console with the full target URL. The
-- slug becomes a unique display name and the instance webhook base URL is gone.
-- Every existing subscription keeps its endpoint, signing secret and history.
ALTER TABLE event_subscriptions DROP CONSTRAINT IF EXISTS event_subscriptions_slug_or_custom;
ALTER TABLE event_subscriptions DROP CONSTRAINT IF EXISTS event_subscriptions_name_check;
DROP INDEX IF EXISTS event_subscriptions_name_idx;
-- Legacy custom-endpoint webhooks had no name; label them after their ID.
UPDATE event_subscriptions SET name='webhook-'||left(id::text,8) WHERE name IS NULL;
ALTER TABLE event_subscriptions
 ALTER COLUMN name SET DEFAULT 'webhook-'||left(gen_random_uuid()::text,8),
 ALTER COLUMN name SET NOT NULL,
 ADD CONSTRAINT event_subscriptions_name_check CHECK(octet_length(name) BETWEEN 1 AND 100 AND name !~ '[[:cntrl:]]'),
 DROP COLUMN custom_endpoint;
CREATE UNIQUE INDEX event_subscriptions_name_idx ON event_subscriptions(instance_id,lower(name));
ALTER TABLE instance_settings DROP COLUMN webhook_base_url;
