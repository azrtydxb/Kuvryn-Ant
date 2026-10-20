# Generic callers and CI

Anything that can send an HTTP `POST` can call a hook. Pick the authentication mode that suits the caller.

## Signed with the generic HMAC format

Create the hook with format **Generic (X-Ant-Signature)** and sign the raw body with the signing secret:

```sh
export ANT=https://ant.example.com
BODY='{"repository":"acme/app","number":42}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$ANT_SIGNING_SECRET" -hex | sed 's/^.* //')
curl -fsS -X POST "$ANT/hook/clean-pr-cache" \
  -H 'Content-Type: application/json' \
  -H "X-Ant-Signature: sha256=$SIG" \
  -H "X-Ant-Delivery-Id: $(uuidgen)" \
  -d "$BODY"
```

`X-Ant-Delivery-Id` is optional; when present it enables 24-hour replay protection.

## Secret token in the URL

```sh
curl -fsS -X POST "$ANT/hook/ci-done/$ANT_HOOK_TOKEN" -H 'Content-Type: application/json' -d '{"job":"build"}'
```

## Bearer Ant token

A token that holds the hook's scope (for example `configure`) on the instance:

```sh
curl -fsS -X POST "$ANT/hook/purge-npm" \
  -H "Authorization: Bearer $ANT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"package":"left-pad"}'
```

In GitHub Actions the token can come from [GitHub OIDC trust](../identity/github-oidc) without a stored secret.

## Example: clean a pull request's build cache

Hook: slug `clean-pr-cache`, action `clean_cache` on a cache feed with prefix <code v-pre>{{ .repository }}:pr-{{ .number }}</code>. A workflow step on `pull_request` closed calls it with the body above, and Ant removes every tag of `acme/app` starting with `pr-42` from the cache feed.
