-- Policy-gated deletion (spec 6.1). A row scopes the delete policy to a feed
-- (namespace '') or to one namespace of a feed; a namespace row wins over the
-- feed row and no row means deletion is allowed. 'unlist_only' withholds byte
-- removal from clients (versions can only be hidden), 'denied' blocks both, and
-- immutable_releases additionally blocks deleting any non-snapshot version.
CREATE TABLE delete_policies (
 instance_id uuid NOT NULL,
 feed_id uuid NOT NULL,
 namespace text NOT NULL DEFAULT '' CHECK(octet_length(namespace)<=256),
 mode text NOT NULL DEFAULT 'allowed' CHECK(mode IN ('allowed','denied','unlist_only')),
 immutable_releases boolean NOT NULL DEFAULT false,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(feed_id,namespace),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id) ON DELETE CASCADE
);
