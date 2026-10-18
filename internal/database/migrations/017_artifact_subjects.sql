-- Subject links are digest associations: OCI permits a missing target manifest.
-- The referrer asset and component belong to the same independently scoped instance.
ALTER TABLE assets ADD CONSTRAINT assets_instance_id_id_key UNIQUE(instance_id,id);
CREATE TABLE artifact_subjects (
 asset_id uuid PRIMARY KEY,
 instance_id uuid NOT NULL,
 component_id uuid NOT NULL,
 subject_digest text NOT NULL CHECK(subject_digest ~ '^[0-9a-f]{64}$'),
 subject_size bigint NOT NULL CHECK(subject_size>=0),
 subject_media_type text NOT NULL,
 artifact_type text NOT NULL DEFAULT '',
 annotations jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(annotations)='object'),
 FOREIGN KEY(instance_id,asset_id) REFERENCES assets(instance_id,id) ON DELETE CASCADE,
 FOREIGN KEY(instance_id,component_id) REFERENCES components(instance_id,id)
);
CREATE INDEX artifact_subjects_lookup_idx ON artifact_subjects(component_id,subject_digest,asset_id);
