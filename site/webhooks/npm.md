# npm

The npm registry (and npm Enterprise or registries that implement hooks) can call a webhook when a package, scope or owner changes. Ant uses `package:publish`, `package:unpublish`, `package:change` and the other package events; stars are ignored.

## 1. Create the hook

| Field          | Value                                          |
| -------------- | ---------------------------------------------- |
| Slug           | `npm-publish`                                  |
| Authentication | HMAC signature, format **npm**                 |
| Signing secret | a secret from Access                           |
| Action         | Upstream change notification, provider **npm** |

npm signs the body with HMAC-SHA256 and sends it as `x-npm-signature: sha256=<hex>`.

## 2. Add the hook with the npm CLI

```sh
# one package
npm hook add left-pad https://ant.example.com/hook/npm-publish "$ANT_SIGNING_SECRET"
# everything under a scope
npm hook add @acme https://ant.example.com/hook/npm-publish "$ANT_SIGNING_SECRET" --type scope
# list and remove
npm hook ls
npm hook rm <hook id>
```

## What Ant does

Remote sources whose origin is `registry.npmjs.org` are matched automatically (for another registry, set **Origins**). The package name, and for scoped packages also its `%2f`-encoded form, are used to drop the cached packument and negative-cache entries, so the new version is resolvable at once instead of after the metadata TTL.
