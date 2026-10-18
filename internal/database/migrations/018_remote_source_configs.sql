-- Restrict persisted configurations to remote sources and credentials bound to
-- precisely that source, including at the database boundary.
ALTER TABLE sources ADD CONSTRAINT sources_instance_id_kind_unique UNIQUE(instance_id,id,kind);
ALTER TABLE encrypted_secrets ADD CONSTRAINT encrypted_secrets_instance_id_purpose_unique UNIQUE(instance_id,id,purpose);
CREATE TABLE remote_source_configs (
 instance_id uuid NOT NULL,
 source_id uuid NOT NULL,
 source_kind text NOT NULL DEFAULT 'remote' CHECK(source_kind='remote'),
 credential_id uuid,
 credential_purpose text GENERATED ALWAYS AS ('remote-source:'||source_id::text) STORED,
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 config jsonb NOT NULL CHECK(jsonb_typeof(config)='object' AND octet_length(config::text)<=32768),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,source_id),
 FOREIGN KEY(instance_id,source_id,source_kind) REFERENCES sources(instance_id,id,kind),
 FOREIGN KEY(instance_id,credential_id,credential_purpose) REFERENCES encrypted_secrets(instance_id,id,purpose),
 CHECK(COALESCE(config->>'credential_id','')=COALESCE(credential_id::text,''))
);
