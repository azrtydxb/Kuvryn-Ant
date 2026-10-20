# Publish policies

A feed can combine several sources for reading, but every write must land in exactly one place. A feed's **publish policy** decides which local source receives a publish. Ant evaluates it before it accepts any upload bytes. A publish that resolves to no source, or to a denied route, is rejected up front.

Writes never go to remote or replica sources. Publishing is always local.

## Model

```json
{
  "default_source_id": "0b6e2f4a-9c1d-4e8b-a7f3-5d2c1b0a9e88",
  "rules": [
    {
      "id": "team-a",
      "priority": 100,
      "selector": { "namespace": "@team-a" },
      "target_source_id": "5a7c9e1b-2d4f-4a6b-8c0d-e1f2a3b4c5d6",
      "deny": false
    }
  ]
}
```

| Field                      | Meaning                                                                                                                                                           |
| -------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `default_source_id`        | Local source attached to the feed that receives every publish no rule matches. An empty string means no default. A publish that no rule matches is then rejected. |
| `rules`                    | Up to 256 rules (`PublishRule`)                                                                                                                                   |
| `rules[].id`               | Unique within the policy, `^[A-Za-z0-9_.:-]+$`, at most 128 characters. It appears in traces and audit records.                                                   |
| `rules[].priority`         | Integer. Higher wins.                                                                                                                                             |
| `rules[].selector`         | What the rule matches (`PublishSelector`)                                                                                                                         |
| `rules[].target_source_id` | Local source that an allow rule routes to. Must be omitted on deny rules.                                                                                         |
| `rules[].deny`             | `true` makes the rule refuse every publish it matches                                                                                                             |

### Selectors

A selector has four optional dimensions. All non-empty dimensions must match (AND). An empty dimension is unconstrained, so a selector with no fields matches every publish.

| Dimension      | Matched against                                                                                 | Rule                                                                                                                                                             |
| -------------- | ----------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `namespace`    | The package's [namespace](namespaces.md), for example `@myorg` for npm or the groupId for Maven | Exact, case-sensitive                                                                                                                                            |
| `package`      | The package coordinate, for example `@myorg/ui` or `org.example:lib`                            | Exact, case-sensitive                                                                                                                                            |
| `path_prefix`  | `/` followed by the asset key of the upload                                                     | Segment boundaries: `/team` matches `/team` and `/team/a`, not `/teammate`. `/` matches any path. Dot segments, backslashes and escaped separators are rejected. |
| `principal_id` | The publishing principal's ID                                                                   | Exact                                                                                                                                                            |

Selectors are exact identities by design. There are no wildcards or regular expressions.

## Evaluation

For each publish, Ant builds a request from the namespace, coordinate, asset path and principal, then evaluates the effective policy:

1. Rules are ordered by priority (highest first), then by `id`. The order the rules were written in does not matter.
2. If **any** matching rule is a deny rule, the publish is refused (`denied_by_rule`). Deny rules override every allow rule regardless of priority.
3. Otherwise the highest-priority matching allow rule wins (`selected_rule`), and its target receives the publish.
4. If no rule matches, the default source receives it (`default_destination`).
5. If there is no default either, the publish is refused (`no_destination`).

Routing selects a destination. Authorization is a separate check: the principal also needs `publish` on the target source and the package's namespace. See [roles and grants](../identity/roles.md). Before accepting the upload, Ant also refuses with `409` a coordinate that already exists in the target source under a different namespace.

Package clients see a refused route as an authorization failure. Most adapters answer `403`. On a feed fed by replication with `destination_read_only` (the default for new replication jobs), the default route is not enough: only an explicit allow rule that routes to a separate local source accepts a publish. Without one, the publish is answered with `409 replica feed — publish to <peer URL>`.

### Validation

Saving a policy (`PUT /api/v1/feeds/{id}/publish-policy`) compiles it against the feed's currently attached sources and rejects it with `400 invalid publish policy` if:

- the default or an allow-rule target is not a local source of the same format attached to the feed
- a deny rule has a target
- a rule ID is missing, malformed or duplicated
- a selector value has leading or trailing spaces or control characters, or a path prefix is not a valid absolute path
- two allow rules at the **same priority** have overlapping selectors and different targets. Give one of them a higher priority. Equal-priority overlapping rules that name the same target are allowed.
- more than 256 rules are given

The caller needs `configure` on the feed and on every source the policy names. `If-Match` is the feed revision, and the save increments it.

### Templates

Publish rules can be shared through policy templates (`POST /api/v1/policy-templates` with `kind: publish`, bound with `PUT /api/v1/feeds/{id}/policy-templates`). A template can extend another template of the same kind, up to a depth of 8, and cycles are rejected. The effective policy is merged root template first, then the feed's own policy:

- a rule with the same `id` in a later layer replaces the earlier rule in place
- a non-empty `default_source_id` in a later layer replaces the inherited default

`GET /api/v1/feeds/{id}/effective-policy` shows the merged result and where each value comes from.

## Dry run: publish-policy:test

```
POST /api/v1/feeds/{id}/publish-policy:test
```

The endpoint evaluates an **unsaved** policy against the feed's attached sources and persists nothing. It is the safe way to check a change before saving it. It requires `configure` on the feed and on every source the candidate policy references. Evaluating another principal (`principal_id`) requires instance administration. Without `principal_id`, the caller is evaluated.

Request (`PublishPolicyTestRequest`):

```sh
curl -fsS -X POST "$ANT/api/v1/feeds/$FEED/publish-policy:test" \
  -H "Authorization: Bearer $ANT_TOKEN" -H 'Content-Type: application/json' -d @- <<JSON
{
  "policy": {
    "default_source_id": "$LOCAL",
    "rules": [
      {"id": "team-a", "priority": 100, "selector": {"namespace": "@team-a"},
       "target_source_id": "$TEAM_A", "deny": false},
      {"id": "no-vendored", "priority": 0, "selector": {"namespace": "@vendor"}, "deny": true}
    ]
  },
  "coordinate": "@team-a/button",
  "namespace": "@team-a"
}
JSON
```

`path` is optional and is the absolute slash path the publish route would supply (`/` plus the asset key). Supply it when the policy uses `path_prefix`, because a path rule never matches an absent path.

Response (`PublishPolicyTestResult`):

```json
{
  "allowed": true,
  "reason": "selected_rule",
  "matched_rule_id": "team-a",
  "target_source_id": "5a7c9e1b-2d4f-4a6b-8c0d-e1f2a3b4c5d6",
  "target_source_name": "npm-team-a",
  "principal_id": "9d8c7b6a-5f4e-4d3c-2b1a-0f9e8d7c6b5a",
  "principal_authorized": true,
  "trace": [
    {
      "rule_id": "team-a",
      "priority": 100,
      "matched": true,
      "selected": true,
      "reason": "selected_rule"
    },
    {
      "rule_id": "no-vendored",
      "priority": 0,
      "matched": false,
      "selected": false,
      "reason": "namespace_mismatch"
    }
  ]
}
```

| Field                                    | Meaning                                                                                                                                                |
| ---------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `allowed`, `reason`                      | The routing decision: `selected_rule`, `default_destination`, `denied_by_rule` or `no_destination`                                                     |
| `matched_rule_id`                        | Winning allow rule, or the deny rule that refused the publish                                                                                          |
| `target_source_id`, `target_source_name` | Destination when allowed                                                                                                                               |
| `principal_authorized`                   | Whether the evaluated principal holds `publish` on the destination                                                                                     |
| `denial`                                 | Reason code when the publish would be refused. This is the routing reason, or `principal_not_permitted` when routing succeeds but authorization fails. |
| `trace[]`                                | Every rule in evaluation order, with `matched`, `selected` and a reason code                                                                           |

Trace reason codes:

| Code                                                                            | Meaning                                                                     |
| ------------------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| `namespace_mismatch`, `package_mismatch`, `principal_mismatch`, `path_mismatch` | That dimension did not match                                                |
| `path_missing`                                                                  | The rule has a `path_prefix` but the request has no path                    |
| `selected_rule`, `denied_by_rule`                                               | This rule decided the outcome                                               |
| `lower_priority`                                                                | Matched, but a higher-priority rule won                                     |
| `stable_tie_break`                                                              | Matched at the same priority as the winner and was ordered after it by `id` |
| `deny_override`                                                                 | An allow rule that matched but was overridden by a matching deny rule       |

## Examples

### One local source

The common case: everything goes to one local source.

```json
{ "default_source_id": "<npm-local id>", "rules": [] }
```

### Per-team destinations

Team scopes go to their own sources, and everything else goes to the shared default.

```json
{
  "default_source_id": "<npm-local id>",
  "rules": [
    {
      "id": "team-a",
      "priority": 100,
      "selector": { "namespace": "@team-a" },
      "target_source_id": "<npm-team-a id>",
      "deny": false
    },
    {
      "id": "team-b",
      "priority": 100,
      "selector": { "namespace": "@team-b" },
      "target_source_id": "<npm-team-b id>",
      "deny": false
    }
  ]
}
```

The two rules share a priority but cannot both match one publish (different namespaces), so they are not ambiguous.

### Block names you mirror

Refuse publishes into a namespace that only an upstream should provide, so that nobody can shadow it locally:

```json
{
  "default_source_id": "<npm-local id>",
  "rules": [
    {
      "id": "no-public-scope",
      "priority": 0,
      "selector": { "namespace": "@types" },
      "deny": true
    }
  ]
}
```

A deny rule wins even at priority 0. To exempt one principal from a deny, narrow the deny rule's selector instead of adding a higher-priority allow rule, because allow rules never override deny rules.

### Only CI may publish releases

No default, and one allow rule for the CI service principal. Every other principal gets `no_destination`.

```json
{
  "default_source_id": "",
  "rules": [
    {
      "id": "ci-only",
      "priority": 10,
      "selector": { "principal_id": "<ci principal id>" },
      "target_source_id": "<releases id>",
      "deny": false
    }
  ]
}
```

### Path-based routing (Raw)

On a Raw feed the asset key is the file path, so `path_prefix` can split a feed:

```json
{
  "default_source_id": "<raw-local id>",
  "rules": [
    {
      "id": "nightly",
      "priority": 50,
      "selector": { "path_prefix": "/nightly" },
      "target_source_id": "<raw-nightly id>",
      "deny": false
    }
  ]
}
```

`/nightly/tool-2026-10-20.tar.gz` goes to `raw-nightly`. `/nightly-old/x` does not match, because matching uses segment boundaries.

::: tip
The asset key differs per format. It is the protocol's file name or path inside a version, not the full URL. Use the dry run with the exact `path` before relying on `path_prefix` for formats other than Raw.
:::

## Related

- [Feeds and sources](feeds-and-sources.md)
- [Namespaces and scopes](namespaces.md)
- [Read order and resolve explain](resolution.md). Reads use separate rules.
