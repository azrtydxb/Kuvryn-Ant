# pub (Dart / Flutter)

The pub adapter implements the Hosted Pub Repository Specification v2. `dart pub get`, `flutter pub get` and `dart pub publish` work against it. A feed can hold packages you publish natively, and it can proxy and cache pub.dev.

Hosted URL: `https://ant.example.com/feeds/<slug>/pub` (feed format `pub`).

## Support matrix

| Publish                                              | Consume                                                                        | Remote (pull-through)                                   | Replica                       | Notes                                                   |
| ---------------------------------------------------- | ------------------------------------------------------------------------------ | ------------------------------------------------------- | ----------------------------- | ------------------------------------------------------- |
| Yes, native `dart pub publish` (upload and finalize) | Yes, `dart pub get` and `flutter pub get` (Dart 3.13.5 in the conformance run) | Yes: pub.dev, archives checked against `archive_sha256` | Yes, through core replication | Retract and restore through `PUT`/`DELETE .../retract`. |

Routes under `/feeds/<slug>/pub/`:

| Method          | Path                                             | Purpose                                                                               |
| --------------- | ------------------------------------------------ | ------------------------------------------------------------------------------------- |
| `GET`           | `api/packages/<name>`                            | Package document (versions, `latest`, `archive_url`, `archive_sha256`, `retracted`)   |
| `GET`           | `api/packages/<name>/versions/<version>`         | One version                                                                           |
| `GET`, `HEAD`   | `packages/<name>/versions/<version>.tar.gz`      | Archive download                                                                      |
| `GET`           | `api/packages/versions/new`                      | Start a publish session; returns the upload URL                                       |
| `POST`          | `uploads/<session>`                              | Multipart archive upload (authorized by the random session URL, as pub expects)       |
| `GET`           | `finalize/<session>`                             | Validate and commit the upload (same token as the session)                            |
| `PUT`, `DELETE` | `api/packages/<name>/versions/<version>/retract` | Retract or restore a version                                                          |
| `DELETE`        | `api/packages/<name>/versions/<version>`         | Delete a version. The artifact core's delete policy controls whether this is allowed. |

## Limits

- An archive can be at most 32 MiB compressed and 128 MiB expanded, with at most 10,000 members and a `pubspec.yaml` of at most 256 KiB. Links, path traversal, duplicate members, YAML aliases and excessive nesting are rejected. Nothing is extracted or run.
- A package document lists at most 4,096 versions and 8 MiB of accumulated pubspecs.
- At most 64 open publish sessions. A session expires after 10 minutes. Uploads beyond `ANT_UPLOAD_SLOTS` get 503.
- A version is immutable. A same-byte retry succeeds, different bytes conflict (409).
- The publish session starts before the package name is known, so it always targets the **default** source of the feed's publish policy. A per-package publish rule that routes to a different source fails closed.
- `ANT_PUBLIC_URL` must be set: `archive_url` and the upload URL are absolute and are built only from it, never from the request `Host`. Without it the adapter answers 503.

::: warning Gap
Publish sessions and their staged files live in memory in the replica that started the session. If several Ant replicas run behind a load balancer, `versions/new`, the `POST` upload and `finalize` must all reach the same replica, or the publish fails with 404. A restart drops uncommitted sessions. Sticky routing or a single replica for publishing avoids this. Neither has been tested.
:::

## Create the feed

In the console, open **Feeds** and click **Create feed** with format `pub`. On **Sources**, use **Create source** to create a `local` source, attach it with **Attach source** and make it the **Publish policy** default. To proxy pub.dev, add a `remote` source and set its origin and redirect origins on the **Upstream** tab.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: pub-local-1' \
  -d '{"name":"pub-local","kind":"local","format":"pub"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: pub-remote-1' \
  -d '{"name":"pub-dev","kind":"remote","format":"pub"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: pub-remote-cfg-1' -d '{
  "origin":"https://pub.dev",
  "redirect_origins":["https://storage.googleapis.com","https://pub.dev"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: pub-feed-1' -d @- <<JSON
{"slug":"pub","format":"pub","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

See [Feeds and sources](../concepts/feeds-and-sources.md) and [Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The origin is the pub.dev host. The adapter reads `api/packages/<name>` from it.

- The package document is validated (name, semver, pubspec identity, hex `archive_sha256`) and re-rendered with every `archive_url` pointing at the feed (`packages/<name>/versions/<v>.tar.gz`). Unknown fields such as `isDiscontinued`, `published` and `retracted` are kept, and `latest` follows upstream.
- Archives are fetched from the upstream `archive_url`. pub.dev redirects them to a storage host. Each archive is checked against `archive_sha256` before the first byte is served, then cached. The pubspec is cached beside it, so cached versions stay listable during an outage, with `X-Kuvryn-Cache: offline`.
- A package name owned by a local source is never answered from upstream.

| Upstream                  | `origin`            | `redirect_origins`                                        |
| ------------------------- | ------------------- | --------------------------------------------------------- |
| pub.dev                   | `https://pub.dev`   | `https://storage.googleapis.com`, `https://pub.dev`       |
| Another hosted pub server | its hosted URL root | the hosts its `archive_url` values and redirects point to |

The redirect origins never receive the source credential. The conformance run uses exactly the pub.dev values above (it resolves `meta 1.15.0` through the feed, then serves the cached archive while the upstream is unreachable).

::: warning Limit
A remote package document is held in memory and is capped by the metadata limit compiled into the binary (24 MiB). Very large package documents cannot be proxied.
:::

## Client configuration

The adapter accepts `Authorization: Bearer <token>`, which is what `dart pub token add` sends. HTTP Basic with the token as password is also accepted. An unauthenticated request gets `401`.

### Register the token

```sh
export ANT_TOKEN=<TOKEN>
dart pub token add https://ant.example.com/feeds/pub/pub --env-var ANT_TOKEN
```

`--env-var` stores only the variable name, so the secret stays out of pub's config file. `flutter pub token add` is equivalent.

### Consume

::: code-group

```yaml [pubspec.yaml (per dependency)]
dependencies:
  my_package:
    hosted: https://ant.example.com/feeds/pub/pub
    version: ^1.0.0
```

```sh [PUB_HOSTED_URL (all hosted deps)]
export PUB_HOSTED_URL=https://ant.example.com/feeds/pub/pub
dart pub get        # or: flutter pub get
```

:::

With `PUB_HOSTED_URL`, every dependency without an explicit `hosted:` resolves through the feed. With pub.dev attached as a remote source, that includes public packages.

### Publish

Set `publish_to` in the package's `pubspec.yaml`, then publish:

```yaml
name: my_package
version: 1.0.0
publish_to: https://ant.example.com/feeds/pub/pub
environment:
  sdk: ^3.0.0
```

```sh
dart pub publish --force
```

The token needs publish permission on the feed. To retract a version (the package document then marks it `retracted` and `latest` skips it), or to restore it:

```sh
curl -fsS -X PUT    -H "Authorization: Bearer $ANT_TOKEN" \
  https://ant.example.com/feeds/pub/pub/api/packages/my_package/versions/1.0.1/retract
curl -fsS -X DELETE -H "Authorization: Bearer $ANT_TOKEN" \
  https://ant.example.com/feeds/pub/pub/api/packages/my_package/versions/1.0.1/retract
```

### GitHub Actions

Exchange the OIDC token for a short-lived Ant token (see [GitHub Actions](../ci/github-actions.md)), then:

```yaml
- run: |
    dart pub token add https://ant.example.com/feeds/pub/pub --env-var ANT_TOKEN
    dart pub get
    dart pub publish --force
```

## Troubleshooting

| Symptom                                      | Cause                                                                                                                               |
| -------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `401` / pub asks to run `dart pub token add` | No token registered for exactly this hosted URL, or the variable named by `--env-var` is empty.                                     |
| `403`                                        | The token lacks read or publish on the feed, or `finalize` was called with a different token than the one that started the session. |
| `404` on `finalize`                          | The session expired (10 minutes), the server restarted, or the request reached another replica.                                     |
| `400` during publish                         | The archive or pubspec failed validation (name or version format, links, YAML aliases, size).                                       |
| `409`                                        | That version already exists with different bytes.                                                                                   |
| `503`                                        | `ANT_PUBLIC_URL` is not set, the 64-session or upload-slot limit is reached, or the upstream is down and nothing is cached.         |
| Public package missing from a mixed feed     | A local source owns that package name.                                                                                              |

Adapter internals: [internal/pubrepo/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/pubrepo/README.md).
