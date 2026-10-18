CREATE TABLE oci_uploads (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 upload_root text NOT NULL CHECK(upload_root ~ '^[0-9a-f]{64}$'),
 principal_id uuid NOT NULL,
 feed_id uuid NOT NULL,
 source_id uuid NOT NULL,
 repository text NOT NULL CHECK(length(repository) BETWEEN 1 AND 256),
 offset_bytes bigint NOT NULL DEFAULT 0 CHECK(offset_bytes>=0),
 state text NOT NULL DEFAULT 'active' CHECK(state IN ('active','complete','cancelled')),
 digest text,
 expires_at timestamptz NOT NULL DEFAULT now()+interval '24 hours',
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id),
 FOREIGN KEY(instance_id,source_id) REFERENCES sources(instance_id,id)
);
CREATE INDEX oci_uploads_owner_idx ON oci_uploads(instance_id,principal_id) WHERE state='active';
