ALTER TABLE event_subscriptions ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK(revision>0);
