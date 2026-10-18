CREATE TABLE roles (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 128),
 UNIQUE(instance_id,id), UNIQUE(instance_id,name)
);
CREATE TABLE principal_roles (
 instance_id uuid NOT NULL,
 principal_id uuid NOT NULL,
 role_id uuid NOT NULL,
 PRIMARY KEY(instance_id,principal_id,role_id),
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id),
 FOREIGN KEY(instance_id,role_id) REFERENCES roles(instance_id,id)
);
CREATE TABLE role_grants (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL,
 role_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN ('artifact','feed','source','instance')),
 effect text NOT NULL CHECK(effect IN ('allow','deny')),
 feed_id uuid,
 source_id uuid,
 namespace text NOT NULL DEFAULT '' CHECK(length(namespace)<=200),
 actions text[] NOT NULL CHECK(cardinality(actions)>0 AND cardinality(actions)<=6 AND actions <@ ARRAY['read','publish','delete','configure','replicate','admin']::text[]),
 FOREIGN KEY(instance_id,role_id) REFERENCES roles(instance_id,id),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id),
 FOREIGN KEY(instance_id,source_id) REFERENCES sources(instance_id,id),
 CHECK(feed_id IS NULL OR kind IN ('artifact','feed')),
 CHECK(source_id IS NULL OR kind IN ('artifact','source')),
 CHECK(feed_id IS NULL OR source_id IS NULL),
 CHECK(namespace='' OR kind='artifact')
);
CREATE INDEX role_grants_role_idx ON role_grants(instance_id,role_id);
-- Only a local, explicitly authorized bootstrap/recovery operation reads this.
-- Tokens are issued separately and their secrets are never retained here.
CREATE TABLE bootstrap_state (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 instance_id uuid NOT NULL,
 principal_id uuid NOT NULL,
 FOREIGN KEY(instance_id,principal_id) REFERENCES principals(instance_id,id)
);
