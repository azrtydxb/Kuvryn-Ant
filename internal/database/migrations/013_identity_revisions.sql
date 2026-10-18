ALTER TABLE principals ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK(revision>0);
ALTER TABLE roles ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK(revision>0);
