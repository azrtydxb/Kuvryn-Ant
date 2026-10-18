-- Existing development/fixture sources predate format metadata; no deployed
-- artifact data exists. New API-created sources always supply their format.
ALTER TABLE sources ADD COLUMN format text NOT NULL DEFAULT 'raw'
 CHECK(format IN ('apk','ansible','apt','cocoapods','composer','conan','conda','oci','git-lfs','go','helm','huggingface','maven','npm','nuget','oci-artifacts','p2','pub','pypi','cran','raw','rubygems','cargo','swift','terraform','rpm'));
