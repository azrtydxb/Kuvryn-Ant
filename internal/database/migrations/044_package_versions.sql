-- Package discovery: tags observed through a remote source, and the grouping of
-- image-index children under their index.
--
-- tags.observed_at is NULL for every authoritative tag (published or replicated
-- locally). A non-NULL value marks a tag recorded from what a remote source
-- answered when a client resolved it; it is refreshed on each re-resolution and
-- is never used to resolve a client request (the remote seam stays the source
-- of truth for a mutable pointer), only to show which version the tag last
-- pointed at.
ALTER TABLE tags ADD COLUMN observed_at timestamptz;

-- version_children records that a manifest list / OCI index version names other
-- versions of the same component (its per-platform manifests). The child is
-- stored by version string so the link can exist before the child is cached.
CREATE TABLE version_children (
 component_id uuid NOT NULL,
 parent_version_id uuid NOT NULL,
 child_version text NOT NULL CHECK (child_version<>''),
 platform text NOT NULL DEFAULT '',
 PRIMARY KEY(parent_version_id,child_version),
 FOREIGN KEY(component_id,parent_version_id) REFERENCES versions(component_id,id) ON DELETE CASCADE
);
CREATE INDEX version_children_child_idx ON version_children(component_id,child_version);

-- Package listings read each package's newest versions and latest timestamp.
CREATE INDEX versions_component_created_idx ON versions(component_id,created_at DESC);
