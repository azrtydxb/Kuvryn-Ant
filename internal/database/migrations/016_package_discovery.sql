-- Namespace is authoritative publication metadata, never inferred by readers.
-- Unsupported legacy formats remain NULL and fail closed until explicitly
-- reconciled; only implemented adapter normalization rules are backfilled.
ALTER TABLE components ADD COLUMN namespace text CHECK(octet_length(namespace)<=256);
UPDATE components c SET namespace=CASE
 WHEN s.format IN ('oci','raw') THEN CASE WHEN strpos(c.coordinate,'/')>0 THEN regexp_replace(c.coordinate,'/[^/]*$','') ELSE '' END
 WHEN s.format='go' THEN CASE WHEN strpos(c.coordinate,'/')>0 THEN regexp_replace(c.coordinate,'/[^/]*$','') ELSE '.' END
 WHEN s.format='npm' THEN CASE WHEN left(c.coordinate,1)='@' THEN split_part(c.coordinate,'/',1) ELSE '' END
 WHEN s.format='maven' AND strpos(c.coordinate,':')>0 THEN split_part(c.coordinate,':',1)
 WHEN s.format IN ('helm','cargo','pypi','nuget') THEN ''
 ELSE NULL END
FROM sources s WHERE s.instance_id=c.instance_id AND s.id=c.source_id;
ALTER TABLE components ALTER COLUMN namespace SET DEFAULT '';

ALTER TABLE assets ADD COLUMN created_at timestamptz;
WITH published AS (SELECT instance_id,resource_id,min(created_at) AS created_at FROM audit_events WHERE action='artifact.publish' GROUP BY instance_id,resource_id)
UPDATE assets a SET created_at=p.created_at FROM published p WHERE p.instance_id=a.instance_id AND p.resource_id=a.id;
UPDATE assets a SET created_at=v.created_at FROM versions v WHERE v.id=a.version_id AND a.created_at IS NULL;
ALTER TABLE assets ALTER COLUMN created_at SET DEFAULT clock_timestamp();
ALTER TABLE assets ALTER COLUMN created_at SET NOT NULL;
CREATE INDEX components_discovery_idx ON components(instance_id,coordinate text_pattern_ops,source_id);
CREATE INDEX versions_component_page_idx ON versions(component_id,id);
CREATE INDEX assets_version_page_idx ON assets(version_id,id);
CREATE INDEX blobs_checksum_idx ON blobs(instance_id,digest);
