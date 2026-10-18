-- Replay records are committed in the same transaction as their database effect.
-- Secret/token issuance must never use this response cache.
CREATE TABLE mutation_results (
 instance_id uuid NOT NULL,
 principal_id uuid NOT NULL,
 operation text NOT NULL CHECK (length(operation) BETWEEN 1 AND 128),
 resource text NOT NULL CHECK (length(resource) BETWEEN 1 AND 1024),
 key_hash bytea NOT NULL CHECK (octet_length(key_hash)=32),
 fingerprint bytea NOT NULL CHECK (octet_length(fingerprint)=32),
 response jsonb NOT NULL,
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(instance_id,principal_id,operation,resource,key_hash),
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id)
);
CREATE INDEX mutation_results_expiry_idx ON mutation_results(expires_at);
