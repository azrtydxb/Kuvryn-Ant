-- External scanner results attached to assets (POST /assets/{id}/security-results),
-- and de-duplication state for replication lag alerts.
CREATE TABLE asset_security_results (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
 scanner text NOT NULL CHECK(octet_length(scanner) BETWEEN 1 AND 128),
 status text NOT NULL CHECK(status IN ('passed','failed','warning')),
 summary jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(summary)='object' AND octet_length(summary::text)<=16384),
 reported_by uuid,
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(instance_id,reported_by) REFERENCES principals(instance_id,id)
);
CREATE INDEX asset_security_results_asset_idx ON asset_security_results(asset_id,created_at DESC);
ALTER TABLE replication_jobs ADD COLUMN lag_alerted boolean NOT NULL DEFAULT false;
