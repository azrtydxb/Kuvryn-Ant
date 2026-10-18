CREATE TABLE instance_writer_leases (
 instance_id uuid PRIMARY KEY REFERENCES instances(id),
 owner_id uuid NOT NULL,
 epoch bigint NOT NULL CHECK(epoch>0),
 expires_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
