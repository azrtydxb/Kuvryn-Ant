# Other CI systems

GitLab CI, Jenkins, Buildkite and other CI systems authenticate to Kuvryn Ant with a **service token** stored as a masked secret. This page covers how to issue tightly scoped tokens, how to expose them to pipelines, the environment variables and config files each ecosystem needs, and how to rotate tokens.

GitHub Actions can avoid stored tokens entirely through OIDC; see [GitHub Actions](github-actions.md).

## Service tokens

A service token belongs to a **service principal** and carries **scopes**: a list of `{feed_id, namespace, actions}` entries. The token can do only what both its scopes and the principal's role grants allow (the intersection). Tokens expire after at most one year. Ant stores only a verifier; the secret is shown once at issue time.

Clients send the token either as a Bearer token or as the password of HTTP Basic authentication with any username (`ci`, `x`, `token`). Details per protocol are in [Package client auth](../identity/client-auth.md).

### One principal per pipeline (or team)

Give each pipeline, or each group of pipelines with the same needs, its own principal and token. That keeps the blast radius of a leaked token small and makes the audit log say which pipeline did what.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_ADMIN_TOKEN"; J='Content-Type: application/json'

# 1. Service principal
P=$(curl -fsS -X POST $ANT/api/v1/principals -H "$H" -H "$J" \
  -H 'Idempotency-Key: ci-myrepo-principal' -d '{"subject":"ci-myrepo","kind":"service"}')
PID=$(jq -r .id <<<"$P")

# 2. Role with exactly the grants the pipeline needs
R=$(curl -fsS -X POST $ANT/api/v1/roles -H "$H" -H "$J" \
  -H 'Idempotency-Key: ci-myrepo-role' -d '{"name":"ci-myrepo"}')
RID=$(jq -r .id <<<"$R")
curl -fsS -X PUT $ANT/api/v1/roles/$RID/grants -H "$H" -H "$J" \
  -H 'Idempotency-Key: ci-myrepo-grants' -H "If-Match: \"$(jq -r .revision <<<"$R")\"" -d @- <<JSON
{"grants":[
 {"kind":"artifact","effect":"allow","feed_id":"$NPM_FEED","actions":["read"]},
 {"kind":"artifact","effect":"allow","feed_id":"$PYPI_FEED","actions":["read"]},
 {"kind":"artifact","effect":"allow","feed_id":"$OCI_HOSTED_FEED","namespace":"myorg","actions":["read","publish"]}]}
JSON
curl -fsS -X PUT $ANT/api/v1/principals/$PID/roles -H "$H" -H "$J" \
  -H 'Idempotency-Key: ci-myrepo-assign' -H "If-Match: \"$(jq -r .revision <<<"$P")\"" \
  -d "{\"role_ids\":[\"$RID\"]}"

# 3. Token, scoped and with an expiry (at most one year)
curl -fsS -X POST $ANT/api/v1/principals/$PID/tokens -H "$H" -H "$J" -d @- <<JSON | jq -r .secret
{"scopes":[
  {"feed_id":"$NPM_FEED","actions":["read"]},
  {"feed_id":"$PYPI_FEED","actions":["read"]},
  {"feed_id":"$OCI_HOSTED_FEED","namespace":"myorg","actions":["read","publish"]}],
 "expires_at":"2027-06-30T00:00:00Z"}
JSON
```

The answer contains `id` (keep it for revocation), `secret` (store it in the CI system) and `expires_at`. The same steps are available in the console under identity and tokens; see [Service accounts and tokens](../identity/tokens.md) and [Roles and grants](../identity/roles.md).

Scoping guidelines:

- Pipelines that only build: `read` on the feeds they consume.
- Release pipelines: `publish` only on the feed (and OCI namespace) they release to, ideally a separate token used only on protected branches or tags.
- Build caches: `read`, `publish` and, for branch cleanup, `delete` on the [cache feeds](../build-caches/index.md), and nothing else.
- For OCI, a scope's `namespace` is the repository name up to its last slash (`myorg` for `oci-hosted/myorg/app`) and matches exactly.

## GitLab CI

Store the token as a CI/CD variable (Settings, CI/CD, Variables) named `ANT_TOKEN`, with **Masked** and, for publish tokens, **Protected** set so only protected branches and tags receive it.

```yaml
variables:
  ANT_HOST: ant.example.com

default:
  before_script:
    # npm / pnpm
    - printf 'registry=https://%s/feeds/npm/npm/\n//%s/feeds/npm/npm/:_authToken=%s\n' "$ANT_HOST" "$ANT_HOST" "$ANT_TOKEN" > ~/.npmrc
    # pip / uv
    - export PIP_INDEX_URL="https://ci:${ANT_TOKEN}@${ANT_HOST}/feeds/pypi/pypi/simple/"
    - export UV_DEFAULT_INDEX="$PIP_INDEX_URL"
    # Go
    - printf 'machine %s login ci password %s\n' "$ANT_HOST" "$ANT_TOKEN" > ~/.netrc && chmod 600 ~/.netrc
    - export GOPROXY="https://${ANT_HOST}/feeds/go/go|https://proxy.golang.org,direct"

test:
  image: node:22
  script:
    - npm ci
    - npm test

image:
  image: docker:27
  services: [docker:27-dind]
  script:
    - echo "$ANT_TOKEN" | docker login "$ANT_HOST" -u ci --password-stdin
    - docker build -t "$ANT_HOST/oci-hosted/myorg/myrepo:$CI_COMMIT_SHORT_SHA" .
    - docker push "$ANT_HOST/oci-hosted/myorg/myrepo:$CI_COMMIT_SHORT_SHA"
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
```

GitLab masks the variable in job logs only when its value meets GitLab's masking requirements; Ant service tokens are a single line without spaces.

## Jenkins

Store the token as a **Secret text** credential (for example ID `ant-token`) and bind it only for the steps that need it:

```groovy
pipeline {
  agent any
  environment {
    ANT_HOST = 'ant.example.com'
  }
  stages {
    stage('Build') {
      steps {
        withCredentials([string(credentialsId: 'ant-token', variable: 'ANT_TOKEN')]) {
          sh '''
            printf 'registry=https://%s/feeds/npm/npm/\n//%s/feeds/npm/npm/:_authToken=%s\n' \
              "$ANT_HOST" "$ANT_HOST" "$ANT_TOKEN" > .npmrc
            npm ci
            PIP_INDEX_URL="https://ci:${ANT_TOKEN}@${ANT_HOST}/feeds/pypi/pypi/simple/" \
              pip install -r requirements.txt
            echo "$ANT_TOKEN" | docker login "$ANT_HOST" -u ci --password-stdin
          '''
        }
      }
    }
  }
}
```

Use single-quoted `sh '''...'''` blocks so Groovy does not interpolate the secret into the command line. Write `.npmrc` into the workspace (as above) or a per-build `HOME` so agents shared between jobs do not keep another job's token.

## Buildkite and others

The pattern is the same for every CI system:

1. Keep the token in the system's secret store (Buildkite secrets or an agent `environment` hook that reads from your secret manager, CircleCI contexts, Azure DevOps secret variables, Drone/Woodpecker secrets).
2. Export it as `ANT_TOKEN` only for the steps that need it.
3. Write the client configuration below at the start of the step, into the job's own `HOME` or workspace.

Buildkite example with an `environment` agent hook:

```sh
# hooks/environment (on the agent)
if [[ "$BUILDKITE_PIPELINE_SLUG" == "myrepo" ]]; then
  export ANT_TOKEN="$(buildkite-agent secret get ANT_TOKEN)"
fi
```

## Environment variables and files per ecosystem

`<host>` is `ant.example.com`, feed slugs are examples. Exact client syntax and more options are on each format page.

| Ecosystem                 | Configuration                                                                                                                                                                                                                                                                               | Format page                                       |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------- |
| npm / pnpm / Yarn classic | `.npmrc`: `registry=https://<host>/feeds/npm/npm/` and `//<host>/feeds/npm/npm/:_authToken=${ANT_TOKEN}`                                                                                                                                                                                    | [npm](../formats/npm.md)                          |
| pip / uv                  | `PIP_INDEX_URL` / `UV_DEFAULT_INDEX` = `https://ci:${ANT_TOKEN}@<host>/feeds/pypi/pypi/simple/`                                                                                                                                                                                             | [PyPI](../formats/pypi.md)                        |
| Go                        | `GOPROXY=https://<host>/feeds/go/go\|https://proxy.golang.org,direct` plus `~/.netrc` `machine <host> login ci password ${ANT_TOKEN}`                                                                                                                                                       | [Go](../formats/go.md)                            |
| Cargo                     | `config.toml`: `[source.crates-io] replace-with = "ant"`, `[source.ant] registry = "sparse+https://<host>/feeds/cargo/cargo/"`, `[registries.ant] index = "sparse+https://<host>/feeds/cargo/cargo/"`, `credential-provider = "cargo:token"`; env `CARGO_REGISTRIES_ANT_TOKEN=${ANT_TOKEN}` | [Cargo](../formats/cargo.md)                      |
| Maven                     | `~/.m2/settings.xml` mirror of `central` to `https://<host>/feeds/maven/maven/` with a matching `<server>` (username `ci`, password the token)                                                                                                                                              | [Maven](../formats/maven.md)                      |
| NuGet                     | `NuGet.Config` source `https://<host>/feeds/nuget/nuget/v3/index.json` with `Username` / `ClearTextPassword`                                                                                                                                                                                | [NuGet](../formats/nuget.md)                      |
| Composer                  | `auth.json` `http-basic` for `<host>`, repository `https://<host>/feeds/composer/composer`                                                                                                                                                                                                  | [Composer](../formats/composer.md)                |
| Docker / OCI              | `docker login <host> -u ci --password-stdin`; image refs `<host>/<feed>/<repository>:<tag>`                                                                                                                                                                                                 | [OCI](../formats/oci.md)                          |
| Helm                      | `helm repo add` with `--username ci --password-stdin`, or `helm registry login <host>` for OCI charts                                                                                                                                                                                       | [Helm](../formats/helm.md)                        |
| sccache                   | `SCCACHE_WEBDAV_ENDPOINT=https://<host>/feeds/sccache/raw`, `SCCACHE_WEBDAV_TOKEN=${ANT_TOKEN}`                                                                                                                                                                                             | [sccache over WebDAV](../build-caches/sccache.md) |

Prefer config files written at job start over committing credentials into repository config: reference the token through an environment variable (npm expands `${ANT_TOKEN}` in `.npmrc`) and keep the file out of the repository.

## Token rotation

Service tokens expire (at most one year after issue). Rotate before expiry, or immediately if a token may have leaked:

1. **Issue a new token** for the same principal with the same scopes:
   `POST /api/v1/principals/{id}/tokens` with `scopes` and a new `expires_at`.
2. **Update the secret** in the CI system (GitLab variable, Jenkins credential, Buildkite secret).
3. **Verify** with a pipeline run (or `curl -fsS -u ci:$NEW https://ant.example.com/feeds/npm/npm/<package>`).
4. **Revoke the old token**: `DELETE /api/v1/tokens/{id}`. Revocation takes effect immediately, including for uploads in progress.

`GET /api/v1/tokens` lists tokens with their expiry, so a scheduled job can warn about tokens that expire soon. A leaked token is revoked first and replaced second.

::: warning Gap
Ant verifies OIDC tokens only from GitHub Actions (the issuer is fixed to `https://token.actions.githubusercontent.com`). GitLab CI `id_tokens`, Buildkite OIDC and other CI identity tokens cannot be exchanged for Ant tokens; those systems need stored service tokens.
:::
