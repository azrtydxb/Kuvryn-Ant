# Roles and grants

All authorization in Kuvryn Ant comes from persisted roles. A role is a named list of grants; principals are members of roles. Tokens and trust rules can only narrow what a principal's roles allow. This page describes grants precisely and gives ready-to-use role recipes.

All endpoints on this page require instance `admin` and are under `/api/v1`. Mutations need an `Idempotency-Key` header; updates of an existing role or principal need `If-Match: "<revision>"` (the revision number in double quotes).

## Grants

```json
{
  "kind": "artifact",
  "effect": "allow",
  "feed_id": "<feed UUID>",
  "namespace": "team-a",
  "actions": ["read", "publish"]
}
```

| Field       | Values                                                                   | Notes                                                                                                     |
| ----------- | ------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------- |
| `kind`      | `artifact`, `feed`, `source`, `instance`                                 | Required. Matched exactly: kinds never imply each other                                                   |
| `effect`    | `allow`, `deny`                                                          | Required. Any matching deny wins over every allow, across all roles                                       |
| `actions`   | 1 to 6 of `read`, `publish`, `delete`, `configure`, `replicate`, `admin` | Required, no duplicates. `admin` is its own action, not a wildcard                                        |
| `feed_id`   | feed UUID                                                                | Only on `artifact` and `feed` grants. Omitted means every feed                                            |
| `source_id` | source UUID                                                              | Only on `artifact` and `source` grants, and not together with `feed_id`. Omitted means every source       |
| `namespace` | 1 to 200 characters                                                      | Only on `artifact` grants. Exact match: `team` does not match `team/child`. Omitted means every namespace |

A role holds at most 256 grants. `PUT /api/v1/roles/{id}/grants` replaces the whole list.

### What each kind and action controls

| Resource kind | Checked for                                                                                                                                                                                                                                                                                                           |
| ------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `artifact`    | Package protocol and API access to content: `read` (download, resolve, list versions), `publish` (upload, tag), `delete` (delete or unlist versions)                                                                                                                                                                  |
| `feed`        | Feed metadata: `read` (see the feed), `configure` (create feeds, change a feed, its sources, publish policy, delete policy, client auth policy, package deletion through the API), `replicate` (export a feed to a replication destination, and create or run replication jobs into a feed together with `configure`) |
| `source`      | Sources: `read` (see a source), `configure` (create, change, delete a source, remote configuration and credentials; attach or target it in a publish policy)                                                                                                                                                          |
| `instance`    | Instance-wide administration: `admin` (identity, roles, tokens, providers, client auth, webhooks, settings and most operations), `read` (some instance-level status views)                                                                                                                                            |

Creating a feed or a source checks `configure` with no specific ID, so it needs a `feed` (or `source`) grant without `feed_id` (or `source_id`). A principal that can read or publish artifacts in a feed can also see that feed in listings, even without a `feed` grant.

::: tip
A replication peer's token needs `replicate` on the source feed (a `feed` grant) on the exporting instance. See [Replication](../replication/index.md).
:::

## Manage roles

| Operation               | Request                                                                     |
| ----------------------- | --------------------------------------------------------------------------- |
| List roles              | `GET /api/v1/roles?limit=&after=`                                           |
| Create a role           | `POST /api/v1/roles` with `{"name":"..."}` (1 to 128 characters)            |
| Replace grants          | `PUT /api/v1/roles/{id}/grants` with `{"grants":[...]}`                     |
| List principals         | `GET /api/v1/principals?limit=&after=`                                      |
| Create a principal      | `POST /api/v1/principals` with `{"kind":"user"\|"service","subject":"..."}` |
| Set a principal's roles | `PUT /api/v1/principals/{id}/roles` with `{"role_ids":[...]}`               |

Names starting with `fed:` are reserved for federation-managed roles. Roles and principals with `origin: "federation"` come from a signed federation policy and cannot be changed locally.

::: warning Gap
The API contract has no endpoint to delete a role or a principal. To retire a role, replace its grants with an empty list and remove it from every principal.
:::

### Recipe helpers

The examples below use these variables:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

# UUID of a feed by slug
feed_id() { curl -fsS "$ANT/api/v1/feeds?prefix=$1&limit=200" -H "$H" \
  | jq -r --arg s "$1" '.items[] | select(.slug==$s) | .id'; }

# create a role and set its grants; prints the role ID
make_role() { # name grants-json
  local role; role=$(curl -fsS -X POST $ANT/api/v1/roles -H "$H" -H "$J" \
    -H "Idempotency-Key: role-$1-create" -d "{\"name\":\"$1\"}")
  curl -fsS -X PUT "$ANT/api/v1/roles/$(echo "$role" | jq -r .id)/grants" -H "$H" -H "$J" \
    -H "If-Match: \"$(echo "$role" | jq -r .revision)\"" -H "Idempotency-Key: role-$1-grants" \
    -d "{\"grants\":$2}" | jq -r .id; }
```

## Example: read-only developer

Read every package in every feed, see feeds and sources in the console, change nothing.

```sh
DEV_ROLE=$(make_role developers-read '[
  {"kind":"artifact","effect":"allow","actions":["read"]},
  {"kind":"feed","effect":"allow","actions":["read"]},
  {"kind":"source","effect":"allow","actions":["read"]}]')
```

To keep one feed private from this role, add a deny; it overrides the broad allow:

```json
{
  "kind": "artifact",
  "effect": "deny",
  "feed_id": "<private feed UUID>",
  "actions": ["read"]
}
```

Map it to SSO users with a provider mapping (`claim_roles` / `group_roles`, see [OIDC](./oidc.md#role-mapping)) or assign it to individual principals.

## Example: CI publisher for one feed

A service principal that can download from and publish to the `npm` feed, and nothing else.

```sh
NPM=$(feed_id npm)
CI_ROLE=$(make_role ci-npm-publish "[
  {\"kind\":\"artifact\",\"effect\":\"allow\",\"feed_id\":\"$NPM\",\"actions\":[\"read\",\"publish\"]},
  {\"kind\":\"feed\",\"effect\":\"allow\",\"feed_id\":\"$NPM\",\"actions\":[\"read\"]}]")

SVC=$(curl -fsS -X POST $ANT/api/v1/principals -H "$H" -H "$J" \
  -H 'Idempotency-Key: principal-ci-npm' -d '{"kind":"service","subject":"ci-npm"}')
curl -fsS -X PUT "$ANT/api/v1/principals/$(echo "$SVC" | jq -r .id)/roles" -H "$H" -H "$J" \
  -H "If-Match: \"$(echo "$SVC" | jq -r .revision)\"" -H 'Idempotency-Key: principal-ci-npm-roles' \
  -d "{\"role_ids\":[\"$CI_ROLE\"]}"
```

Then issue it a token scoped to the same feed (see [Tokens](./tokens.md#service-tokens)), or let GitHub Actions obtain short-lived tokens through a [trust rule](./github-oidc.md). The token scope and the role must both allow an action for it to succeed.

## Example: feed administrator

A team lead who owns the `team-maven` feed: read, publish and delete its packages, change the feed and its publish policy, and manage its local source.

```sh
FEED=$(feed_id team-maven)
SRC=<UUID of the feed's local source>
ADMIN_ROLE=$(make_role team-maven-admin "[
  {\"kind\":\"artifact\",\"effect\":\"allow\",\"feed_id\":\"$FEED\",\"actions\":[\"read\",\"publish\",\"delete\"]},
  {\"kind\":\"feed\",\"effect\":\"allow\",\"feed_id\":\"$FEED\",\"actions\":[\"read\",\"configure\"]},
  {\"kind\":\"source\",\"effect\":\"allow\",\"source_id\":\"$SRC\",\"actions\":[\"read\",\"configure\"]}]")
```

This role cannot create new feeds or sources (that needs grants without an ID), cannot touch other feeds, and has no instance `admin`, so it cannot manage users, roles or tokens. Changing a feed's publish policy also needs `configure` on every source the policy targets, and attaching a source needs `configure` on both the feed and the source.

## Example: instance administrator

Equivalent to the `local-administrator` role that `ant admin local-user --admin` creates:

```json
[
  {
    "kind": "artifact",
    "effect": "allow",
    "actions": ["read", "publish", "delete", "configure", "replicate", "admin"]
  },
  {
    "kind": "feed",
    "effect": "allow",
    "actions": ["read", "publish", "delete", "configure", "replicate", "admin"]
  },
  {
    "kind": "source",
    "effect": "allow",
    "actions": ["read", "publish", "delete", "configure", "replicate", "admin"]
  },
  {
    "kind": "instance",
    "effect": "allow",
    "actions": ["read", "publish", "delete", "configure", "replicate", "admin"]
  }
]
```

## Assign roles to a principal

`PUT /api/v1/principals/{id}/roles` replaces the principal's role list (up to 256, unique). Get the current revision from `GET /api/v1/principals` (`items[].revision`):

```sh
REV=$(curl -fsS "$ANT/api/v1/principals?limit=200" -H "$H" \
  | jq -r --arg id "$PID" '.items[] | select(.id==$id) | .revision')
curl -fsS -X PUT $ANT/api/v1/principals/$PID/roles -H "$H" -H "$J" \
  -H "If-Match: \"$REV\"" -H 'Idempotency-Key: roles-alice-2' \
  -d "{\"role_ids\":[\"$DEV_ROLE\"]}"
```

For SSO users provisioned by a role mapping, the mapped memberships are reconciled at each sign-in; assign extra roles through an explicit binding instead (see [Identity bindings](./index.md#identity-bindings)).

## Effective permissions

`GET /api/v1/permissions/effective?principal=<id>&resource=<resource>[&source=<source id>]` shows what a principal can do on one resource, from its role grants. Instance admins can inspect any principal; others only themselves.

| `resource`                         | Meaning                            |
| ---------------------------------- | ---------------------------------- |
| `instance`                         | Instance-wide actions              |
| `feed:<feed UUID>`                 | Feed metadata                      |
| `source:<source UUID>`             | A source                           |
| `artifact:<feed UUID>`             | Content of a feed, any namespace   |
| `artifact:<feed UUID>:<namespace>` | Content of one namespace in a feed |

```sh
curl -fsS "$ANT/api/v1/permissions/effective?principal=$PID&resource=artifact:$NPM" -H "$H" | jq
```

```json
{
  "principal_id": "<UUID>",
  "disabled": false,
  "resource": { "kind": "artifact", "feed_id": "<UUID>" },
  "origin": "local",
  "roles": [{ "name": "ci-npm-publish", "origin": "local" }],
  "actions": [
    {
      "action": "read",
      "allowed": true,
      "via_roles": ["ci-npm-publish"],
      "denied_by_roles": []
    },
    {
      "action": "publish",
      "allowed": true,
      "via_roles": ["ci-npm-publish"],
      "denied_by_roles": []
    },
    {
      "action": "delete",
      "allowed": false,
      "via_roles": [],
      "denied_by_roles": []
    }
  ],
  "federation_policy_generation": 0
}
```

The answer reflects role grants only: the scopes of a particular token narrow it further and are not applied here.
