CREATE TABLE publish_policies (
 instance_id uuid NOT NULL,
 feed_id uuid PRIMARY KEY,
 default_source_id uuid,
 rules jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(rules)='array' AND jsonb_array_length(rules)<=256),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id),
 FOREIGN KEY(instance_id,default_source_id) REFERENCES sources(instance_id,id)
);
