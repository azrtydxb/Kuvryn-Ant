# Maven and Gradle

The Maven adapter serves a Maven 2 repository layout for release artifacts: POMs,
JARs, classifier JARs, checksum sidecars and `maven-metadata.xml`. Maven, Gradle and
any other client that understands the Maven 2 layout can consume it. Publishing works
with native `mvn deploy` (one PUT per file) or with the adapter's atomic bundle
endpoint.

Endpoint:

```text
https://ant.example.com/feeds/<slug>/maven/
```

A file is addressed by its normal repository path, for example
`https://ant.example.com/feeds/maven/maven/org/example/demo/1.0/demo-1.0.jar`.

Implementation: [`internal/maven`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/maven/README.md).

## Support matrix

| Operation                           | Supported | Notes                                                                                                            |
| ----------------------------------- | --------- | ---------------------------------------------------------------------------------------------------------------- |
| Consume (Maven, Gradle)             | Yes       | GET/HEAD, conditional requests and byte ranges.                                                                  |
| Publish with `mvn deploy`           | Yes       | One PUT per file; the PUT of the artifact-level `maven-metadata.xml` commits the version.                        |
| Publish with Gradle `maven-publish` | Partly    | Sends the same PUT sequence, but see [Gradle publishing](#gradle-publishing) for what to disable.                |
| Publish with the bundle API         | Yes       | `POST <group path>/<artifact>/<version>/_bundle`, multipart, atomic.                                             |
| Delete                              | Yes       | `DELETE` of a file, or of a version directory (trailing slash). Gated by the feed's delete policy.               |
| Remote (pull-through)               | Yes       | Artifacts are cached on first download; `maven-metadata.xml` is served from upstream through the metadata cache. |
| Replica                             | Yes       | Through the format-agnostic replication core. No Maven-specific replica test exists.                             |
| SNAPSHOT versions                   | No        | Any path whose version contains `SNAPSHOT` returns 501.                                                          |

## Limits

These come from the adapter code and README.

- **Release versions only.** A version containing `SNAPSHOT` (in any case) returns
  `501 Not Implemented`, for reads as well as writes, local or remote.
- **File names.** Within a version directory only `<artifact>-<version>.pom`,
  `<artifact>-<version>.jar`, `<artifact>-<version>-<classifier>.jar` and their
  `.md5`, `.sha1`, `.sha256` and `.sha512` sidecars are accepted. Anything else is a
  `400`: `.war`, `.aar`, `.zip`, `.tar.gz`, `.asc` signatures and Gradle `.module`
  files included. This applies to pull-through reads too.
- **Path segments** must match `[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}`, so a version with
  `+` is rejected, and a path containing `%` or `\` is rejected.
- **Packaging** must be `jar` (the default) or `pom`. A `jar` version needs the POM and
  the main JAR; a `pom` version is the POM alone.
- **The POM must state its own coordinates.** The top-level `groupId`, `artifactId`,
  `version` and `modelVersion` 4.0.0 must be present and match the path. A `groupId`
  or `version` inherited from a `<parent>` is rejected. XML DTDs and entities are
  rejected.
- **Sizes.** 1 MiB per POM, 128 MiB per JAR, 256 MiB per bundle request. JARs are
  CRC-checked: at most 10,000 entries, 1 GiB total expansion.
- **Deploy staging.** At most 4 non-checksum files per version (for example the POM,
  the JAR, `-sources.jar` and `-javadoc.jar`) and 16 versions in flight per principal.
  Staged files expire after 15 minutes. They are stored in PostgreSQL, so the PUTs of
  one deployment can go to different replicas.
- **Immutability.** Deploying the same bytes again returns 204. Deploying different
  bytes to an existing version returns 409.
- **Metadata.** At most 10,000 versions per artifact. `latest` and `release` name the
  version added most recently. There is no group-level plugin-prefix metadata for
  local artifacts.
- **Concurrency.** Four uploads at a time per process.

::: warning Gap: authentication methods
The adapter needs a token-backed principal. Service tokens, personal access tokens
and tokens exchanged through GitHub OIDC all work. The code returns `404` for an
anonymous principal and for a principal that logged in with an LDAP directory
password (`basic-directory`), so **anonymous reads and directory passwords do not
work on Maven feeds**, even when the feed has anonymous reads turned on. This is
based on reading the code, not on a client test.
:::

## Create the feed

In the console:

1. **Sources**, then **Create source**: a `local` source with format `maven` (for
   example `maven-local`). For a proxy of Maven Central, also create a `remote` source
   (for example `maven-central`) and fill in its **Upstream** tab (see
   [Remote upstreams](#remote-upstreams)).
2. **Feeds**, then **Create feed**: slug `maven`, format `maven`.
3. On the feed, attach the sources under **Read order** with the local source first,
   then set the **Publish policy** default to the local source.

The same thing through the management API:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: maven-local-1' \
  -d '{"name":"maven-local","kind":"local","format":"maven"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: maven-central-1' \
  -d '{"name":"maven-central","kind":"remote","format":"maven"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: maven-central-cfg-1' -d '{
  "origin":"https://repo1.maven.org/maven2","redirect_origins":[],"allow_private":false,
  "timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'

curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: maven-feed-1' -d @- <<JSON
{"slug":"maven","format":"maven","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

The [read order](../concepts/resolution.md) decides which source owns a
`groupId:artifactId`. Once a local source owns an artifact, upstream is never asked
for it, so a private artifact cannot be replaced from Maven Central. See
[Feeds and sources](../concepts/feeds-and-sources.md) and
[Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The remote source origin is the root of the upstream Maven 2 repository. The layout
is the same on both sides, so a request for `<path>` on the feed fetches `<path>`
from the origin.

- **Artifacts** (POM, JAR, checksum sidecars) are cached into the store under the
  remote source on first download and served locally from then on, including during
  an upstream outage.
- **`maven-metadata.xml`** and its checksum files are mutable. They are passed
  through unchanged from upstream via the metadata cache (`metadata_ttl_seconds`,
  `negative_ttl_seconds`, `stale_if_error_seconds`). They are not merged with local
  versions. When upstream is unreachable, Ant builds the metadata from the versions
  it has cached locally and marks the response `X-Kuvryn-Cache: offline`.
- A `404` from upstream moves on to the next source in the read order. Any other
  upstream error stops the search with a `503`.

| Upstream                 | `origin`                                  | `redirect_origins`                 | Notes                                                                                                                                                                       |
| ------------------------ | ----------------------------------------- | ---------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Maven Central            | `https://repo1.maven.org/maven2`          | none                               | Pull-through of `commons-codec:commons-codec:1.16.1` is covered by the conformance suite. `https://repo.maven.apache.org/maven2` is the same repository under another name. |
| Google Maven             | `https://dl.google.com/dl/android/maven2` | unverified                         | Android libraries are published as `.aar`, which the adapter rejects (see [Limits](#limits)). Only `.jar`/`.pom` artifacts can come through.                                |
| Gradle Plugin Portal     | `https://plugins.gradle.org/m2`           | unverified                         | Not tested. The portal may redirect downloads to another host. If it does, add that host as a redirect origin.                                                              |
| Private Maven repository | its repository root URL                   | any host it redirects downloads to | Store the upstream credential on the source; see below.                                                                                                                     |

For an authenticated upstream (Artifactory, Nexus, GitHub Packages, and so on), set
the credential with `PUT /api/v1/sources/{id}/remote-credential`
(`{"authorization":"Basic <base64 user:password>"}` or
`{"authorization":"Bearer <token>"}`, with the source `If-Match` and an
`Idempotency-Key`). It is stored encrypted, never returned, and only sent to the
source origin, never to a redirect origin.

::: warning Gap
Pull-through has only been exercised against Maven Central. Google Maven and the
Gradle Plugin Portal have not been tested, and the redirect origins they need are
not documented in the repository.
:::

## Client configuration

Clients authenticate with HTTP Basic auth: any non-empty username, and a service
token or personal access token as the password. A bare `Authorization: Bearer <token>`
header works too. Requests without an `Authorization` header get
`401` with `WWW-Authenticate: Basic realm="kuvryn-ant"`, which Maven and Gradle answer
by sending their configured credentials. See
[Service accounts and tokens](../identity/tokens.md) and
[Package client auth](../identity/client-auth.md).

### Maven: consume through a mirror

`~/.m2/settings.xml` sends every repository (Maven Central included) through the
feed. The `<server>` `id` must match the mirror `id`.

```xml
<settings>
  <mirrors>
    <mirror>
      <id>ant</id>
      <mirrorOf>*</mirrorOf>
      <url>https://ant.example.com/feeds/maven/maven</url>
    </mirror>
  </mirrors>
  <servers>
    <server>
      <id>ant</id>
      <username>token</username>
      <password>${env.ANT_TOKEN}</password>
    </server>
  </servers>
</settings>
```

With `mirrorOf` set to `*`, a build that declares a snapshot repository also sends its
SNAPSHOT requests to Ant, which answers them with 501.

### Maven: publish with `mvn deploy`

Add `distributionManagement` to the POM. Its `id` selects the `<server>` credentials
from `settings.xml`. Leave out `snapshotRepository`, because snapshots are not
supported.

```xml
<distributionManagement>
  <repository>
    <id>ant</id>
    <url>https://ant.example.com/feeds/maven/maven</url>
  </repository>
</distributionManagement>
```

```sh
mvn -B deploy
```

A single file can be deployed with `deploy:deploy-file`:

```sh
mvn -B deploy:deploy-file -DrepositoryId=ant \
  -Durl=https://ant.example.com/feeds/maven/maven \
  -Dfile=demo-1.0.jar -DpomFile=demo-1.0.pom
```

Each file is PUT separately and staged privately. Nothing is visible until the PUT of
`<group>/<artifact>/maven-metadata.xml`, which publishes the version atomically. Ant
generates its own metadata document and discards the one the client uploaded. If the
POM is missing, or the JAR is missing for `jar` packaging, the deploy is rejected with
`400` and nothing is published.

### Bundle API (curl or scripts)

`POST .../<group path>/<artifact>/<version>/_bundle` with `multipart/form-data`. The
`filename` of each part must be a valid asset file name; field names are ignored.

```sh
curl --fail --user "token:$ANT_TOKEN" \
  -F 'asset=@demo-1.0.pom' -F 'asset=@demo-1.0.jar' -F 'asset=@demo-1.0-sources.jar' \
  https://ant.example.com/feeds/maven/maven/org/example/demo/1.0/_bundle
```

Returns `201` for a new version, `204` for an identical retry and `409` if the
version already exists with different content. A part may carry
`X-Checksum-Sha256`, `X-Checksum-Sha1` or `X-Checksum-Md5` headers, which must match.
Checksum files are not accepted as bundle parts.

### Gradle: consume

::: code-group

```kotlin [build.gradle.kts]
repositories {
    maven {
        name = "ant"
        url = uri("https://ant.example.com/feeds/maven/maven")
        credentials {
            username = "token"
            password = providers.environmentVariable("ANT_TOKEN").get()
        }
        // Optional, see the warning below.
        metadataSources {
            mavenPom()
            artifact()
        }
    }
}
```

```groovy [build.gradle]
repositories {
    maven {
        name = 'ant'
        url = uri('https://ant.example.com/feeds/maven/maven')
        credentials {
            username = 'token'
            password = System.getenv('ANT_TOKEN')
        }
        // Optional, see the warning below.
        metadataSources {
            mavenPom()
            artifact()
        }
    }
}
```

:::

Credentials can also come from `~/.gradle/gradle.properties` through Gradle's
`credentials(PasswordCredentials::class)` convention. With the repository named `ant`,
set `antUsername` and `antPassword`.

To resolve Gradle plugins through Ant as well, declare the repository in
`settings.gradle(.kts)` under `pluginManagement { repositories { ... } }`.

::: warning Gap: Gradle module metadata
The adapter does not accept `.module` files (Gradle module metadata). When a POM
says it was published with Gradle metadata, Gradle asks for the `.module` file and
gets a `400` rather than a `404`. Whether Gradle treats that as fatal has not been
tested. Restricting `metadataSources` to `mavenPom()` and `artifact()`, as above,
stops Gradle from asking for `.module` files. The repository README states that no
native Gradle fixture has been run.
:::

### Gradle: publish with `maven-publish`

::: code-group

```kotlin [build.gradle.kts]
plugins {
    `java-library`
    `maven-publish`
}

group = "org.example"
version = "1.0"

// The adapter rejects .module files; do not publish Gradle module metadata.
tasks.withType<GenerateModuleMetadata> {
    enabled = false
}

publishing {
    publications {
        create<MavenPublication>("maven") {
            from(components["java"])
        }
    }
    repositories {
        maven {
            name = "ant"
            url = uri("https://ant.example.com/feeds/maven/maven")
            credentials {
                username = "token"
                password = providers.environmentVariable("ANT_TOKEN").get()
            }
        }
    }
}
```

```groovy [build.gradle]
plugins {
    id 'java-library'
    id 'maven-publish'
}

group = 'org.example'
version = '1.0'

// The adapter rejects .module files; do not publish Gradle module metadata.
tasks.withType(GenerateModuleMetadata).configureEach {
    enabled = false
}

publishing {
    publications {
        maven(MavenPublication) {
            from components.java
        }
    }
    repositories {
        maven {
            name = 'ant'
            url = uri('https://ant.example.com/feeds/maven/maven')
            credentials {
                username = 'token'
                password = System.getenv('ANT_TOKEN')
            }
        }
    }
}
```

:::

```sh
./gradlew publishAllPublicationsToAntRepository
```

### Gradle publishing

Gradle `maven-publish` uses the same per-file PUT sequence as Maven, but by default
it also uploads a `.module` file, which the adapter rejects with `400`. Turn off
`GenerateModuleMetadata` as shown above. Also leave out signing (`.asc` files are
rejected) and keep each version to at most 4 non-checksum files.

::: warning Gap
The repository README says Gradle `maven-publish` "sends the same sequence but has not
been run". The Gradle configuration on this page has not been tested against Ant.
:::

### GitHub Actions

`actions/setup-java` can write `settings.xml` with a server entry. Get `ANT_TOKEN`
from a repository secret or from the GitHub OIDC exchange (see
[GitHub Actions](../ci/github-actions.md) and
[GitHub OIDC trust](../identity/github-oidc.md)):

```yaml
- uses: actions/setup-java@v4
  with:
    distribution: temurin
    java-version: 21
    server-id: ant
    server-username: ANT_USER
    server-password: ANT_TOKEN
  env:
    ANT_USER: ci
- run: mvn -B deploy
  env:
    ANT_USER: ci
    ANT_TOKEN: ${{ env.ANT_TOKEN }}
```

Declare the repository (mirror or `distributionManagement`) with id `ant` and URL
`https://ant.example.com/feeds/maven/maven`. Gradle only needs `ANT_TOKEN` in the
environment, as in the examples above.

## Troubleshooting

| Symptom                                       | Cause                                                                                                                                                                               |
| --------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401` with `WWW-Authenticate: Basic`          | No credentials were sent. Check that the `<server>` id matches the mirror or repository id.                                                                                         |
| `404` on everything, with credentials         | The feed slug is wrong, the token cannot read the feed, or the credential is an LDAP directory password or anonymous (see the gap under [Limits](#limits)).                         |
| `403`                                         | The token is valid but its role or scope does not allow the operation, or the feed's client-auth policy does not accept that kind of credential.                                    |
| `400` on a PUT or GET                         | Unsupported file name (`.module`, `.aar`, `.asc`, ...), a `%` or `+` in the path, a POM whose coordinates do not match the path or are inherited, or a deploy without a POM or JAR. |
| `501`                                         | The version contains `SNAPSHOT`.                                                                                                                                                    |
| `409` on deploy                               | The release already exists with different bytes. Releases are immutable; publish a new version.                                                                                     |
| `429` with `Retry-After`                      | Too many failed authentication attempts from this client.                                                                                                                           |
| `503`                                         | The upstream failed (it did not return a 404) and nothing was cached. A private outage never falls through to a public source.                                                      |
| `X-Kuvryn-Cache: stale` (with `Warning: 110`) | `maven-metadata.xml` came from the metadata cache after its TTL because upstream failed (`stale_if_error_seconds`).                                                                 |
| `X-Kuvryn-Cache: offline`                     | Upstream is unreachable, so the metadata lists only versions cached locally.                                                                                                        |

To see which source would answer for an artifact, call
`GET /api/v1/feeds/{id}/resolve?coordinate=<groupId>:<artifactId>&namespace=<groupId>`
(see [Read order and resolve explain](../concepts/resolution.md)).
