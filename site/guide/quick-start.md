# Quick start

This page gets a Kuvryn Ant instance running and serving its first pull-through feed. Pick one of three ways to run it: Docker Compose on a workstation, Helm on Kubernetes, or the plain binary. All three need PostgreSQL 17 or 18 and a 32-byte root key.

::: warning No published release yet
There are no GitHub releases, no published container image and no Helm chart repository yet. Every path below starts from a git checkout and builds the image or binary locally.
:::

```sh
git clone https://github.com/azrtydxb/Kuvryn-Ant.git
cd Kuvryn-Ant
```

## Option A: Docker Compose

For evaluation on one machine. Requires Docker with Compose and OpenSSL. The Compose file is meant for local development, not production.

```sh
# 1. Generate .local/root.key (32 random bytes) and .local/postgres-password once.
scripts/init-dev.sh

# 2. Build the image and start PostgreSQL 18 plus the application.
docker compose --profile app up --build -d

# 3. Create the instance and the first administrator credential.
#    This prints a JSON credential once; keep it private.
docker compose --profile app exec app ant-entrypoint bootstrap

# 4. Restart so the serving topology is loaded (the server also reloads by itself
#    within a few seconds of the bootstrap).
docker compose --profile app restart app
```

::: warning Use 127.0.0.1, not localhost
Several package adapters accept plain HTTP only for a literal loopback IP. `compose.yaml` therefore defaults `ANT_PUBLIC_URL` to `http://127.0.0.1:8080`. If you override it with `http://localhost:8080`, the server exits after the bootstrap with `invalid artifact public origin`. Behind a real hostname, use HTTPS.
:::

The bootstrap output looks like this:

```json
{
  "instance_id": "<uuid>",
  "principal_id": "<uuid>",
  "token_id": "<uuid>",
  "token": "<TOKEN>",
  "expires_at": "<timestamp>"
}
```

`token` is an administrative service token valid for 24 hours. Open `http://127.0.0.1:8080/console/`, choose **Use an API token** and paste it. If it expires before you create other administrators, run `docker compose --profile app exec app ant-entrypoint bootstrap-token` for a new one.

What the Compose setup does:

- PostgreSQL 18 listens on `127.0.0.1:15432`; the application on `127.0.0.1:8080`.
- A one-shot `app-secrets` container copies the generated secrets into a private volume owned by UID 10001; the application runs non-root with a read-only root filesystem.
- Data lives in the `app_data` and `postgres_data` volumes. `docker compose down -v` deletes them.

Browser single sign-on needs HTTPS. For OIDC or SAML testing put a TLS reverse proxy in front and set `ANT_PUBLIC_URL` to its HTTPS origin.

## Option B: Helm on Kubernetes

For a real deployment. You need Kubernetes 1.30 or newer, Helm, a PostgreSQL 17 or 18 database, a block-storage `StorageClass` and a registry you can push to.

1. Build and push the image:

   ```sh
   docker buildx build --platform linux/amd64,linux/arm64 \
     -t registry.example.com/myorg/kuvryn-ant:0.1.0 --push .
   ```

2. Create the two required Secrets from protected files:

   ```sh
   kubectl create namespace ant
   umask 077
   printf '%s' 'postgres://ant:<password>@db.example.com:5432/ant?sslmode=verify-full' > dsn
   head -c 32 /dev/urandom > root.key
   kubectl -n ant create secret generic ant-postgres --from-file=dsn=./dsn
   kubectl -n ant create secret generic ant-root-key --from-file=root.key=./root.key
   rm -f dsn
   ```

   Back up `root.key` separately before you delete it locally; without it encrypted secrets and backups cannot be read. See [Root key](../install/root-key.md).

3. Write `my-values.yaml`:

   ```yaml
   image:
     repository: registry.example.com/myorg/kuvryn-ant
     tag: "0.1.0"
   publicURL: https://ant.example.com
   ingress:
     enabled: true
     className: nginx
     host: ant.example.com
     tlsSecret: ant-tls
     annotations:
       nginx.ingress.kubernetes.io/proxy-body-size: "0"
       nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
       nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
       nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
   ```

4. Install from the chart directory of the checkout:

   ```sh
   helm upgrade --install ant deploy/chart -n ant -f my-values.yaml --wait
   ```

5. Bootstrap once and keep the printed credential private:

   ```sh
   kubectl -n ant exec deploy/ant-ant -- ant bootstrap
   kubectl -n ant rollout restart deploy/ant-ant   # optional; the server reloads by itself
   ```

Open `https://ant.example.com/console/` and sign in with the token. The full chart documentation, every value and the storage, TLS and sizing options are in [Helm chart](../install/helm.md).

## Option C: the binary

For development or a VM. Requires Go 1.27.2 or newer in the 1.27 series. The console is already embedded from `internal/console/dist`.

```sh
go build -o ant ./cmd/ant
```

Start PostgreSQL. The repository's Compose file can provide it (`docker compose up -d --wait postgres`, port `15432`), or use any PostgreSQL 17 or 18 server. Then:

```sh
umask 077
head -c 32 /dev/urandom > root.key

export ANT_DATABASE_URL='postgres://ant:<password>@127.0.0.1:5432/ant?sslmode=disable'
export ANT_ROOT_KEY_FILE=$PWD/root.key
export ANT_DATA_DIR=$PWD/data           # default .local/data

./ant bootstrap                         # prints the credential once
./ant                                   # serve on 127.0.0.1:8080
```

With the repository's Compose PostgreSQL, `scripts/run-dev.sh` sets `ANT_DATABASE_URL` and `ANT_ROOT_KEY_FILE` for you: `scripts/init-dev.sh`, then `scripts/run-dev.sh bootstrap local`, then `scripts/run-dev.sh`.

On a loopback listener `ANT_PUBLIC_URL` defaults to `http://127.0.0.1:8080`. To listen on another interface set `ANT_LISTEN=0.0.0.0:8080` and `ANT_PUBLIC_URL` to the HTTPS origin your clients use; without it the server refuses to start serving packages on a non-loopback listener. Use `sslmode=verify-full` for any database that is not on the same host. All settings are listed in the [configuration reference](../reference/configuration.md).

## First feed in 2 minutes

This creates an OCI feed called `dockerhub` that pulls images through from Docker Hub and caches them. It uses only the management API; the console offers the same steps (Sources, then Feeds).

Set the base URL and the bootstrap token:

```sh
export ANT=http://127.0.0.1:8080          # or https://ant.example.com
export ANT_TOKEN='<TOKEN>'
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'
```

1. Create a remote source:

   ```sh
   REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: dockerhub-src-1' \
     -d '{"name":"docker-hub","kind":"remote","format":"oci"}' | jq -r .id)
   ```

2. Point it at Docker Hub. Updates need the current revision in `If-Match`. Docker Hub sends clients to a token service and redirects blob downloads to a CDN; both hosts must be listed in `redirect_origins` (they receive no credential):

   ```sh
   REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
   curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
     -H 'Idempotency-Key: dockerhub-cfg-1' -d '{
     "origin":"https://registry-1.docker.io",
     "redirect_origins":["https://auth.docker.io","https://production.cloudfront.docker.com",
                       "https://production.cloudflare.docker.com"],
     "allow_private":false,"timeout_seconds":60,
     "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
              "max_entries":10000,"max_bytes":268435456}}'
   ```

3. Create the feed with that source:

   ```sh
   curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: dockerhub-feed-1' \
     -d "{\"slug\":\"dockerhub\",\"format\":\"oci\",\"source_ids\":[\"$REM\"]}"
   ```

4. Pull through it. Image references are `<host>/<feed>/<repository>:<tag>`; Docker Hub official images live under `library/`:

   ::: code-group

   ```sh [docker]
   echo "$ANT_TOKEN" | docker login 127.0.0.1:8080 -u x --password-stdin
   docker pull 127.0.0.1:8080/dockerhub/library/alpine:3.20
   ```

   ```sh [crane]
   crane auth login 127.0.0.1:8080 -u x -p "$ANT_TOKEN"
   crane pull --insecure 127.0.0.1:8080/dockerhub/library/alpine:3.20 alpine.tar
   ```

   :::

   Docker treats registries on `127.0.0.0/8` as insecure, so plain HTTP works locally; crane needs `--insecure`. With a real deployment use `ant.example.com/dockerhub/library/alpine:3.20`. Any username works; the token is the password.

The first pull fetches from Docker Hub and stores the blobs; later pulls of the same digests are served from Ant. To check the source, call `GET $ANT/api/v1/sources/$REM/health`.

::: tip If blobs fail with a policy error
The manifest resolves but layer downloads fail (`registry unavailable`) and the source health shows `"last_error":"policy"`: the upstream redirected to a host that is not in `redirect_origins`. CDN host names change over time. In October 2026 Docker Hub redirected blobs to `production.cloudfront.docker.com`. Find the current host with `curl -sI` against the upstream blob URL and add it to the remote config.
:::

This recipe was run end to end against a local instance (binary, PostgreSQL 17, crane) while writing this page.

Where to go next:

- [Feeds and sources](../concepts/feeds-and-sources.md) and [resolution order](../concepts/resolution.md): combine local and remote sources in one feed.
- [Publish policies](../concepts/publish-policies.md): add a local source and publish your own artifacts.
- [OCI](../formats/oci.md) and [host caching](../host-caching/index.md): use the feed as a registry mirror for containerd, CRI-O, Docker and BuildKit.
- [Formats](../formats/index.md): npm, PyPI, Maven, Go and the other supported formats.
- [Identity](../identity/index.md): local users, SSO and scoped tokens instead of the bootstrap token.
