# Console tour

The web console is a React application embedded in the `ant` binary and served at
`/console/`. It talks only to the [management API](../reference/api.md), so every
action in the console is authorized, audited and available through the API and the
[MCP endpoint](../reference/mcp.md) as well.

Sign in with a configured provider ([OIDC](../identity/oidc.md),
[SAML](../identity/saml.md), [LDAP](../identity/ldap.md)), a
[local account](../identity/local-users.md) or an API token. The theme toggle in
the header switches between light and dark mode.

::: tip About these screenshots
The screenshots come from a running instance. Hostnames, addresses, user names,
identifiers and token prefixes were replaced with placeholders such as
`ant.example.com`, `10.0.0.x` and `00000000-0000-4000-8000-...`. The Overview
screenshot shows the dashboard as it was before the one-screen dashboard layout
was released.
:::

## Overview

The landing page summarizes service reachability, package and request counts, the
remote cache hit ratio, a traffic chart, storage usage and recent activity.

![Overview dashboard](../screenshots/overview.png)

![Overview dashboard in dark mode](../screenshots/overview-dark.png)

## Feeds

Feeds are the stable client endpoints. The list shows each feed's format, access
mode (anonymous read or authenticated), package count and stored bytes, and can be
filtered by purpose. See [Feeds and sources](../concepts/feeds-and-sources.md).

![Feed list](../screenshots/feeds.png)

A feed's **Read order** tab lists its sources in priority order. The **Resolve a
package** box calls the [resolve explain](../concepts/resolution.md) endpoint and
shows which source would serve a coordinate, without fetching anything.

![Feed read order and resolve explain](../screenshots/feed-detail-resolve.png)

The **Publish policy** tab edits [publish rules](../concepts/publish-policies.md),
the default destination, and lets you dry-run a publish against unsaved rules.

![Publish policy editor](../screenshots/feed-publish-policy.png)

The **Connect** tab prints client commands for the feed's format.

![Connect tab with client commands](../screenshots/feed-connect.png)

## Build-cache feeds

Filtering by the **Build caches** purpose shows the `cache` feeds used for buildx
layer caches and sccache. Their settings hold the idle eviction and size limit; see
[Build caches](../build-caches/index.md).

![Cache-purpose feeds](../screenshots/feeds-cache.png)

![A buildx cache feed](../screenshots/buildcache-feed.png)

![Cache feed settings: idle eviction and size limit](../screenshots/buildcache-settings.png)

## Sources

Sources hold content: local storage, remote upstreams, or replicas created by
replication. The list shows kind, format, how many feeds use each source, stored
bytes and upstream health.

![Source list](../screenshots/sources.png)

A remote source's **Upstream** tab holds the HTTPS origin, the allowed redirect
origins, timeout, operator CA ID, private-address permission and cache bounds. See
the [remote upstream fields](../formats/index.md).

![Remote source upstream configuration](../screenshots/source-upstream.png)

## Packages

The package browser searches everything you can read across feeds, by name,
namespace or digest, and can switch to a federation-wide search.

![Package browser](../screenshots/packages.png)

![Package browser in dark mode](../screenshots/packages-dark.png)

The package drawer shows an install command for the feed, the versions with their
digests, tags (including tags last seen upstream for cached images), details and
provenance.

![Package drawer with install command and versions](../screenshots/package-drawer.png)

![Package tags](../screenshots/package-tags.png)

## Replication

Replication jobs pull feeds from a peer instance into a local replica source. See
[Replication and multi-site](../replication/index.md).

![Replication jobs](../screenshots/replication.png)

## Federation

The Federation page shows this member's state, role, membership generation,
endpoints and key fingerprints, with tabs for members, policy, replication,
authority and federated audit. See [Federation](../replication/federation.md).

![Federation status](../screenshots/federation.png)

![Federation members and invitations](../screenshots/federation-members.png)

![Signed federation policy generations](../screenshots/federation-policy.png)

![Federated feeds and read routing](../screenshots/federation-replication.png)

![Authority and recovery key](../screenshots/federation-authority.png)

## Access

Users, service principals, roles, tokens and sign-in providers. See
[Identity and access](../identity/index.md).

![Users](../screenshots/access.png)

![Roles and grants](../screenshots/access-roles.png)

![Tokens with scopes and expiry](../screenshots/access-tokens.png)

## Audit log

Every authorized and denied mutation, written in the same transaction as the
change. Filter by principal, action, result and time; export CSV or NDJSON, or
stream to a SIEM through the audit sink. See
[Observability](../operations/observability.md).

![Audit log](../screenshots/audit.png)

## Import and migrate

Export and import feed and source definitions, migrate from Nexus, and review the
history of runs. See [Import, export and Nexus migration](../operations/import-export.md).

![Import, export and migration](../screenshots/import-export.png)

## Settings

Instance settings show where each value comes from; values set by environment
variables are read-only.

![General settings](../screenshots/settings.png)

**Storage** shows the active blob backend, deduplication savings, unreferenced
bytes and the online [storage migration](../operations/storage-migration.md).

![Storage settings](../screenshots/settings-storage.png)

**Retention & cleanup** covers [storage pressure, retention policies, runs and
integrity findings](../operations/storage-maintenance.md).

![Storage pressure status](../screenshots/settings-retention.png)

![Default retention policy](../screenshots/settings-retention-defaults.png)
