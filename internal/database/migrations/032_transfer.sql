-- History of import, export and migration runs for the management console.
-- Only counts and a bounded, non-secret summary are stored.
CREATE TABLE transfer_runs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 principal_id uuid,
 kind text NOT NULL CHECK(kind IN ('export','import','migration','compare')),
 dry_run boolean NOT NULL DEFAULT false,
 summary jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(summary)='object' AND octet_length(summary::text)<=65536),
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id)
);
CREATE INDEX transfer_runs_created_idx ON transfer_runs(instance_id,created_at DESC,id);
