-- Maven deploy staging (internal/maven/deploy.go). A standard `mvn deploy`
-- uploads each file with its own PUT and the version commits on the final
-- maven-metadata.xml PUT. The staged files live here, not in process memory
-- or local disk, so the PUTs of one deploy may reach different replicas and
-- survive a restart. Rows older than the staging TTL are purged
-- opportunistically by the next deploy request.
CREATE TABLE maven_deploy_sets (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 feed_id uuid NOT NULL,
 owner text NOT NULL CHECK(octet_length(owner) BETWEEN 1 AND 128),
 group_id text NOT NULL CHECK(octet_length(group_id) BETWEEN 1 AND 256),
 artifact text NOT NULL CHECK(octet_length(artifact) BETWEEN 1 AND 256),
 version text NOT NULL CHECK(octet_length(version) BETWEEN 1 AND 256),
 packaging text NOT NULL DEFAULT '' CHECK(octet_length(packaging)<=64),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 claimed_at timestamptz,
 UNIQUE(instance_id,feed_id,owner,group_id,artifact,version),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id) ON DELETE CASCADE
);
CREATE INDEX maven_deploy_sets_updated ON maven_deploy_sets(instance_id,updated_at);
CREATE TABLE maven_deploy_files (
 set_id uuid NOT NULL REFERENCES maven_deploy_sets(id) ON DELETE CASCADE,
 file text NOT NULL CHECK(octet_length(file) BETWEEN 1 AND 512),
 size bigint NOT NULL CHECK(size>0),
 sha256 text NOT NULL,
 sha512 text NOT NULL,
 sha1 text NOT NULL,
 md5 text NOT NULL,
 media_type text NOT NULL,
 PRIMARY KEY(set_id,file)
);
CREATE TABLE maven_deploy_chunks (
 set_id uuid NOT NULL,
 file text NOT NULL,
 idx integer NOT NULL CHECK(idx>=0),
 data bytea NOT NULL,
 PRIMARY KEY(set_id,file,idx),
 FOREIGN KEY(set_id,file) REFERENCES maven_deploy_files(set_id,file) ON DELETE CASCADE
);
