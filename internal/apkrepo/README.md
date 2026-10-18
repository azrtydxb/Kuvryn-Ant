# APK (Alpine) adapter

Local Alpine APK v2 repositories (contract F01). Mount: `/feeds/{slug}/apk/`.

| Method | Path | Notes |
| --- | --- | --- |
| PUT | `{branch}/{repo}/{arch}/{name}-{ver}-r{N}.apk` | Feed Admin. Parses `.PKGINFO` from the control segment; name/version must match the filename, arch must equal the path arch or `noarch`. Package + index entry publish as one atomic bundle. Package bytes and signature segment are preserved. |
| GET/HEAD | same | Download. |
| DELETE | same | Feed Admin, local sources only; removes version, assets and tags in one transaction with audit and outbox events. Blobs are left for GC. |
| GET | `{branch}/{repo}/{arch}/APKINDEX.tar.gz` | Generated on request from committed entries, signed `.SIGN.RSA.ant-<feed8>.rsa.pub` (RSA-2048, SHA-1 over the compressed index segment). |
| GET | `keys/ant-<feed8>.rsa.pub` | PEM public key for `/etc/apk/keys`. |

Auth: Bearer or Basic (password = token), same as the other adapters. Index, key and package reads require Read.

Signing key: generated lazily per feed, stored as PKCS#8 via `internal/secrets` (purpose `apk-feed-signing-key:<feed id>`, no migration). Wire with `SetKeyring(*secrets.Keyring)`; without it, the index and key endpoints return 503.

Client setup:

    wget -O /etc/apk/keys/ant-XXXXXXXX.rsa.pub https://host/feeds/alpine/apk/keys/ant-XXXXXXXX.rsa.pub
    echo https://host/feeds/alpine/apk/v3.24/main >> /etc/apk/repositories

## Limitations

- No real `apk` client conformance run yet (apk-tools 3.24.2 fixture pending); tests verify the signature and format in Go.
- Uploaded package signatures are preserved but not verified (no trusted-key import yet).
- Local sources only; no remote/replica materialization. Index is regenerated per request (max 4096 packages per channel).
- v3 wire format is out of scope.

Run: `sh internal/apkrepo/test-postgres.sh`.
