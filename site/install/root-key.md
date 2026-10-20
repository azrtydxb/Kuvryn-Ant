# Root key

Every Kuvryn Ant instance needs a root key: exactly 32 random bytes in a file outside the database and outside blob storage. It is required at startup (`ANT_ROOT_KEY_FILE`); the chart mounts it from an existing Secret and never generates it.

## What it protects

| Use                          | Effect                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| ---------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Encrypted secrets keyring    | Secrets stored in PostgreSQL (`encrypted_secrets`) are envelope-encrypted with a per-record data key wrapped by the root key of a named version. This covers remote-source upstream credentials, identity-provider secrets managed in the console, webhook secrets, audit-sink credentials, APT and APK repository signing keys, local-user TOTP secrets, replication peer credentials and federation keys (CA, authority signing key, member key). |
| Pagination cursors           | Management API cursors are authenticated with a key derived from the root key, so clients cannot forge or tamper with them.                                                                                                                                                                                                                                                                                                                         |
| Signed URLs and fingerprints | Keys derived from the root key sign Terraform download URLs and fingerprint idempotent replication and remote-source requests.                                                                                                                                                                                                                                                                                                                      |

The root key is never written to PostgreSQL, to backups or to logs; `ant diagnostics` shows it as `[REDACTED]`.

## Generate

Generate it once, from a secure random source, as raw bytes (not hex, not base64):

```sh
umask 077
head -c 32 /dev/urandom > root.key
# or: openssl rand -out root.key 32
wc -c root.key   # must print 32
```

The server refuses to start unless the file is a regular file of exactly 32 bytes.

## Store in Kubernetes

```sh
kubectl -n ant create secret generic ant-root-key --from-file=root.key=./root.key
```

```yaml
rootKey:
  existingSecret: ant-root-key
  key: root.key
  activeVersion: v1
```

The whole Secret is mounted read-only at `/run/ant/root`, and `ANT_ROOT_KEY_FILE` points at `/run/ant/root/<rootKey.key>`. Changes require a pod restart.

For the binary or Compose, point `ANT_ROOT_KEY_FILE` at the file directly; `scripts/init-dev.sh` creates `.local/root.key` for local development.

## Back it up separately

- Store the key in your secret manager or an offline, access-controlled location, separately from database and blob backups. `ant backup` deliberately never includes it.
- Keep every previous key version that any stored secret or any retained backup still needs.
- Restoring a backup requires the keyring that was in use when it was taken: `ant restore` decrypts every restored secret before committing and aborts (rolling back) if a key is missing or wrong.
- Preserve the key across reinstalls. A reinstall onto an existing database with a new key behaves like a lost key.

## Rotation

The keyring is versioned. `ANT_ROOT_KEY_VERSION` names the active key; `ANT_PREVIOUS_ROOT_KEYS_FILE` points to a JSON manifest that maps up to 16 previous versions to key files. New secrets are encrypted with the active version, existing ones stay readable with the previous keys, and `ant rotate-secrets` re-encrypts them under the active key.

### Procedure with the chart

1. Generate the new key:

   ```sh
   umask 077
   head -c 32 /dev/urandom > root-v2.key
   ```

2. Write the manifest. Relative paths resolve against the manifest's directory, which is the Secret mount:

   ```json
   { "v1": "root.key" }
   ```

   Save it as `previous.json`.

3. Replace the Secret so it contains the old key, the new key and the manifest:

   ```sh
   kubectl -n ant create secret generic ant-root-key \
     --from-file=root.key=./root.key \
     --from-file=root-v2.key=./root-v2.key \
     --from-file=previous.json=./previous.json \
     --dry-run=client -o yaml | kubectl -n ant apply -f -
   ```

4. Point the release at the new key and upgrade:

   ```yaml
   rootKey:
     existingSecret: ant-root-key
     key: root-v2.key
     activeVersion: v2
     previousKeysManifestKey: previous.json
   ```

   ```sh
   helm upgrade ant deploy/chart -n ant -f my-values.yaml --wait
   ```

5. Re-encrypt stored secrets. Each run processes at most 100 records in one transaction and prints `{"rotated":N,"batch_limit":100}`. Repeat until `rotated` is `0`:

   ```sh
   kubectl -n ant exec deploy/ant-ant -- ant rotate-secrets
   ```

   The command is a local operation authorized by access to the database and key files; it never issues a login token. It is safe to interrupt and resume, and it can run while the server is serving (rows are locked with `SKIP LOCKED`).

6. Keep `root.key` (v1) in the Secret and the manifest until every backup that was taken under v1 has expired. Only then remove v1 from the manifest and the Secret.

Version labels match `^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`; the active version must not appear in the manifest.

::: warning Rotation side effects
Cursor authentication, Terraform URL signing and request fingerprints are derived from the active key only. After switching the active key, outstanding pagination cursors and previously issued signed download URLs no longer validate; clients must restart pagination or request a new URL. This follows from the code; the client-visible effect has not been exercised in a documented test.
:::

## If the key is lost

There is no recovery mechanism. Without the key (and the previous versions still in use):

- every encrypted secret is unreadable: upstream credentials, IdP secrets, webhook and audit-sink secrets, APT and APK signing keys, local-user TOTP enrolments, replication credentials and federation keys must be re-entered or re-created;
- `ant restore` aborts for any backup containing encrypted secrets, so existing backups cannot be restored;
- federation membership keys cannot be used; the member has to be re-identified or rejoined.

Artifacts, metadata and service tokens are not encrypted with the root key and stay intact.

::: warning Gap
Whether `ant serve` starts and serves normally with a different root key against an existing database (with failures only when an encrypted secret is used) is not documented or tested. Do not rely on it as a recovery path.
:::
