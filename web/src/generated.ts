// Generated from api/openapi.yaml by npm run generate. Do not edit.
export type ServiceStatus = { "name": "kuvryn-ant"; "api_version": "v1" };
export type Error = { "code": "invalid_request" | "unauthenticated" | "forbidden" | "not_found" | "conflict" | "precondition_failed" | "unavailable" | "internal_error"; "message": string; "request_id": string };
export type PageInfo = { "has_more": boolean; "next_cursor"?: string };
export type Feed = { "id": string; "slug": string; "format": "apk" | "ansible" | "apt" | "cocoapods" | "composer" | "conan" | "conda" | "oci" | "git-lfs" | "go" | "helm" | "huggingface" | "maven" | "npm" | "nuget" | "oci-artifacts" | "p2" | "pub" | "pypi" | "cran" | "raw" | "rubygems" | "cargo" | "swift" | "terraform" | "rpm"; "revision": number; "anonymous_read": boolean };
export type CreateFeed = { "slug": string; "format": "apk" | "ansible" | "apt" | "cocoapods" | "composer" | "conan" | "conda" | "oci" | "git-lfs" | "go" | "helm" | "huggingface" | "maven" | "npm" | "nuget" | "oci-artifacts" | "p2" | "pub" | "pypi" | "cran" | "raw" | "rubygems" | "cargo" | "swift" | "terraform" | "rpm"; "anonymous_read"?: boolean };
export type PatchFeed = { "slug"?: string; "anonymous_read"?: boolean };
export type FeedPage = { "items": Array<Feed>; "page": PageInfo };
export type Source = { "id": string; "name": string; "kind": "local" | "remote" | "replica"; "format": "apk" | "ansible" | "apt" | "cocoapods" | "composer" | "conan" | "conda" | "oci" | "git-lfs" | "go" | "helm" | "huggingface" | "maven" | "npm" | "nuget" | "oci-artifacts" | "p2" | "pub" | "pypi" | "cran" | "raw" | "rubygems" | "cargo" | "swift" | "terraform" | "rpm"; "revision": number };
export type CreateSource = { "name": string; "kind": "local" | "remote" | "replica"; "format": "apk" | "ansible" | "apt" | "cocoapods" | "composer" | "conan" | "conda" | "oci" | "git-lfs" | "go" | "helm" | "huggingface" | "maven" | "npm" | "nuget" | "oci-artifacts" | "p2" | "pub" | "pypi" | "cran" | "raw" | "rubygems" | "cargo" | "swift" | "terraform" | "rpm" };
export type PatchSource = { "name": string };
export type SourcePage = { "items": Array<Source>; "page": PageInfo };
export type SourceAttachment = { "source": Source; "priority": number };
export type SourceAttachmentPage = { "items": Array<SourceAttachment>; "page": PageInfo; "feed_revision": number };
export type AttachSource = { "priority": number };
export type SourceAttachmentMutation = { "feed_id": string; "source_id": string; "feed_revision": number; "priority"?: number };
export type PublishSelector = { "namespace"?: string; "package"?: string; "path_prefix"?: string; "principal_id"?: string };
export type PublishRule = { "id": string; "priority": number; "selector": PublishSelector; "target_source_id"?: string; "deny": boolean };
export type PutPublishPolicy = { "default_source_id"?: string; "rules": Array<PublishRule> };
export type PublishPolicy = { "default_source_id": string; "rules": Array<PublishRule>; "feed_id": string; "revision": number };
export type IdentityPrincipal = { "id": string; "subject": string; "kind": "user" | "service" | "emergency"; "disabled": boolean; "revision": number; "role_ids": Array<string> };
export type CreatePrincipal = { "subject": string; "kind": "user" | "service" };
export type IdentityRoleGrant = { "kind": "artifact" | "feed" | "source" | "instance"; "effect": "allow" | "deny"; "feed_id"?: string; "source_id"?: string; "namespace"?: string; "actions": Array<"read" | "publish" | "delete" | "configure" | "replicate" | "admin"> };
export type IdentityRole = { "id": string; "name": string; "revision": number; "grants": Array<IdentityRoleGrant> };
export type CreateRole = { "name": string };
export type SetRoleGrants = { "grants": Array<IdentityRoleGrant> };
export type SetPrincipalRoles = { "role_ids": Array<string> };
export type IdentityBinding = { "issuer": string; "subject": string; "principal_id": string };
export type IssueServiceToken = { "scopes": Array<{ "feed_id"?: string; "namespace"?: string; "actions": Array<"read" | "publish" | "delete" | "configure" | "replicate" | "admin"> }>; "expires_at": string };
export type IssuedServiceToken = { "id": string; "secret": string; "expires_at": string };
export type RevokedServiceToken = { "id": string; "revoked": true };
export type IdentityPrincipalPage = { "items": Array<IdentityPrincipal>; "next_after"?: string };
export type IdentityRolePage = { "items": Array<IdentityRole>; "next_after"?: string };
export type Webhook = { "id": string; "endpoint": string; "enabled": boolean; "revision": number; "created_at": string };
export type WebhookPage = { "items": Array<Webhook>; "next_after"?: string };
export type CreateWebhook = { "endpoint": string; "secret": string };
export type PatchWebhook = { "enabled": boolean };
export type RotateWebhookSecret = { "secret": string };
export type WebhookDelivery = { "event_id": number; "state": "pending" | "delivered" | "dead"; "attempts": number; "available_at": string; "last_failure"?: "invalid_destination" | "invalid_payload" | "secret_unavailable" | "transport_failure" | "http_failure" | "attempts_exhausted" | "delivery_failure"; "delivered_at"?: string };
export type WebhookDeliveryPage = { "items": Array<WebhookDelivery>; "next_after"?: number };
export type DiscoveredPackage = { "id": string; "coordinate": string; "namespace": string; "source_id": string; "metadata": {  } };
export type DiscoveredVersion = { "id": string; "component_id": string; "version": string; "revision": number; "created_at": string };
export type DiscoveredTag = { "name": string; "version_id": string; "revision": number };
export type DiscoveredAsset = { "id": string; "component_id": string; "version_id": string; "source_id": string; "key": string; "media_type": string; "digest": string; "size_bytes": number; "created_at": string; "uploader_id"?: string; "provenance": {  } };
export type DiscoveredPackagePage = { "items": Array<DiscoveredPackage>; "page": PageInfo };
export type DiscoveredVersionPage = { "items": Array<DiscoveredVersion>; "page": PageInfo };
export type DiscoveredTagPage = { "items": Array<DiscoveredTag>; "page": PageInfo };
export type DiscoveredAssetPage = { "items": Array<DiscoveredAsset>; "page": PageInfo };
export type RemoteCachePolicy = { "metadata_ttl_seconds": number; "negative_ttl_seconds": number; "stale_if_error_seconds": number; "max_entries": number; "max_bytes": number };
export type RemoteSourceConfig = { "origin": string; "redirect_origins": Array<string>; "credential_id"?: string; "ca_ref"?: string; "allow_private": boolean; "timeout_seconds": number; "cache": RemoteCachePolicy };
export type RemoteSourceRecord = { "source_id": string; "revision": number; "config": RemoteSourceConfig };
export type RemoteCredentialInput = { "authorization"?: string; "api_key"?: string };
export const formats = ["apk","ansible","apt","cocoapods","composer","conan","conda","oci","git-lfs","go","helm","huggingface","maven","npm","nuget","oci-artifacts","p2","pub","pypi","cran","raw","rubygems","cargo","swift","terraform","rpm"] as const;
export interface Operations {
getStatus: { body: undefined; response: ServiceStatus };
listFeeds: { body: undefined; response: FeedPage };
createFeed: { body: CreateFeed; response: Feed };
getFeed: { body: undefined; response: Feed };
updateFeed: { body: PatchFeed; response: Feed };
listSources: { body: undefined; response: SourcePage };
createSource: { body: CreateSource; response: Source };
getSource: { body: undefined; response: Source };
updateSource: { body: PatchSource; response: Source };
listFeedSources: { body: undefined; response: SourceAttachmentPage };
attachFeedSource: { body: AttachSource; response: SourceAttachmentMutation };
detachFeedSource: { body: undefined; response: SourceAttachmentMutation };
getPublishPolicy: { body: undefined; response: PublishPolicy };
setPublishPolicy: { body: PutPublishPolicy; response: PublishPolicy };
listPrincipals: { body: undefined; response: IdentityPrincipalPage };
createPrincipal: { body: CreatePrincipal; response: IdentityPrincipal };
listRoles: { body: undefined; response: IdentityRolePage };
createRole: { body: CreateRole; response: IdentityRole };
setRoleGrants: { body: SetRoleGrants; response: IdentityRole };
setPrincipalRoles: { body: SetPrincipalRoles; response: IdentityPrincipal };
issueServiceToken: { body: IssueServiceToken; response: IssuedServiceToken };
revokeServiceToken: { body: undefined; response: RevokedServiceToken };
createIdentityBinding: { body: IdentityBinding; response: IdentityBinding };
listWebhooks: { body: undefined; response: WebhookPage };
createWebhook: { body: CreateWebhook; response: Webhook };
patchWebhook: { body: PatchWebhook; response: Webhook };
rotateWebhookSecret: { body: RotateWebhookSecret; response: Webhook };
listWebhookDeliveries: { body: undefined; response: WebhookDeliveryPage };
listWebhookFailures: { body: undefined; response: WebhookDeliveryPage };
listPackages: { body: undefined; response: DiscoveredPackagePage };
listPackageVersions: { body: undefined; response: DiscoveredVersionPage };
listPackageTags: { body: undefined; response: DiscoveredTagPage };
listPackageAssets: { body: undefined; response: DiscoveredAssetPage };
getPackageAsset: { body: undefined; response: DiscoveredAsset };
getRemoteSourceConfig: { body: undefined; response: RemoteSourceRecord };
setRemoteSourceConfig: { body: RemoteSourceConfig; response: RemoteSourceRecord };
setRemoteSourceCredential: { body: RemoteCredentialInput; response: RemoteSourceRecord };
}
export const routes = {
  "getStatus": {
    "path": "/api/v1/status",
    "method": "GET",
    "idempotent": false
  },
  "listFeeds": {
    "path": "/api/v1/feeds",
    "method": "GET",
    "idempotent": false
  },
  "createFeed": {
    "path": "/api/v1/feeds",
    "method": "POST",
    "idempotent": true
  },
  "getFeed": {
    "path": "/api/v1/feeds/{id}",
    "method": "GET",
    "idempotent": false
  },
  "updateFeed": {
    "path": "/api/v1/feeds/{id}",
    "method": "PATCH",
    "idempotent": true
  },
  "listSources": {
    "path": "/api/v1/sources",
    "method": "GET",
    "idempotent": false
  },
  "createSource": {
    "path": "/api/v1/sources",
    "method": "POST",
    "idempotent": true
  },
  "getSource": {
    "path": "/api/v1/sources/{id}",
    "method": "GET",
    "idempotent": false
  },
  "updateSource": {
    "path": "/api/v1/sources/{id}",
    "method": "PATCH",
    "idempotent": true
  },
  "listFeedSources": {
    "path": "/api/v1/feeds/{id}/sources",
    "method": "GET",
    "idempotent": false
  },
  "attachFeedSource": {
    "path": "/api/v1/feeds/{id}/sources/{source_id}",
    "method": "PUT",
    "idempotent": true
  },
  "detachFeedSource": {
    "path": "/api/v1/feeds/{id}/sources/{source_id}",
    "method": "DELETE",
    "idempotent": true
  },
  "getPublishPolicy": {
    "path": "/api/v1/feeds/{id}/publish-policy",
    "method": "GET",
    "idempotent": false
  },
  "setPublishPolicy": {
    "path": "/api/v1/feeds/{id}/publish-policy",
    "method": "PUT",
    "idempotent": true
  },
  "listPrincipals": {
    "path": "/api/v1/principals",
    "method": "GET",
    "idempotent": false
  },
  "createPrincipal": {
    "path": "/api/v1/principals",
    "method": "POST",
    "idempotent": true
  },
  "listRoles": {
    "path": "/api/v1/roles",
    "method": "GET",
    "idempotent": false
  },
  "createRole": {
    "path": "/api/v1/roles",
    "method": "POST",
    "idempotent": true
  },
  "setRoleGrants": {
    "path": "/api/v1/roles/{id}/grants",
    "method": "PUT",
    "idempotent": true
  },
  "setPrincipalRoles": {
    "path": "/api/v1/principals/{id}/roles",
    "method": "PUT",
    "idempotent": true
  },
  "issueServiceToken": {
    "path": "/api/v1/principals/{id}/tokens",
    "method": "POST",
    "idempotent": false
  },
  "revokeServiceToken": {
    "path": "/api/v1/tokens/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "createIdentityBinding": {
    "path": "/api/v1/identity-bindings",
    "method": "POST",
    "idempotent": true
  },
  "listWebhooks": {
    "path": "/api/v1/webhooks",
    "method": "GET",
    "idempotent": false
  },
  "createWebhook": {
    "path": "/api/v1/webhooks",
    "method": "POST",
    "idempotent": true
  },
  "patchWebhook": {
    "path": "/api/v1/webhooks/{id}",
    "method": "PATCH",
    "idempotent": true
  },
  "rotateWebhookSecret": {
    "path": "/api/v1/webhooks/{id}/secret",
    "method": "PUT",
    "idempotent": true
  },
  "listWebhookDeliveries": {
    "path": "/api/v1/webhooks/{id}/deliveries",
    "method": "GET",
    "idempotent": false
  },
  "listWebhookFailures": {
    "path": "/api/v1/webhooks/{id}/failures",
    "method": "GET",
    "idempotent": false
  },
  "listPackages": {
    "path": "/api/v1/feeds/{id}/packages",
    "method": "GET",
    "idempotent": false
  },
  "listPackageVersions": {
    "path": "/api/v1/feeds/{id}/packages/{component}/versions",
    "method": "GET",
    "idempotent": false
  },
  "listPackageTags": {
    "path": "/api/v1/feeds/{id}/packages/{component}/tags",
    "method": "GET",
    "idempotent": false
  },
  "listPackageAssets": {
    "path": "/api/v1/feeds/{id}/packages/{component}/versions/{version}/assets",
    "method": "GET",
    "idempotent": false
  },
  "getPackageAsset": {
    "path": "/api/v1/feeds/{id}/assets/{asset}",
    "method": "GET",
    "idempotent": false
  },
  "getRemoteSourceConfig": {
    "path": "/api/v1/sources/{id}/remote-config",
    "method": "GET",
    "idempotent": false
  },
  "setRemoteSourceConfig": {
    "path": "/api/v1/sources/{id}/remote-config",
    "method": "PUT",
    "idempotent": true
  },
  "setRemoteSourceCredential": {
    "path": "/api/v1/sources/{id}/remote-credential",
    "method": "PUT",
    "idempotent": true
  }
} as const;
