// Generated from api/openapi.yaml by npm run generate. Do not edit.
export type ServiceStatus = { "name": "kuvryn-ant"; "api_version": "v1"; "readiness"?: ReadinessDetail };
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
export type PackageSearchResult = { "feed_id": string; "feed_slug": string; "format": "apk" | "ansible" | "apt" | "cocoapods" | "composer" | "conan" | "conda" | "oci" | "git-lfs" | "go" | "helm" | "huggingface" | "maven" | "npm" | "nuget" | "oci-artifacts" | "p2" | "pub" | "pypi" | "cran" | "raw" | "rubygems" | "cargo" | "swift" | "terraform" | "rpm"; "id": string; "coordinate": string; "namespace": string; "source_id": string; "origin": "local" | "remote" | "replica"; "metadata": {  } };
export type PackageSearchPage = { "items": Array<PackageSearchResult>; "page": PageInfo; "total": number };
export type ReorderSources = { "source_ids": Array<string> };
export type SourceOrderItem = { "source_id": string; "priority": number };
export type SourceOrder = { "feed_id": string; "feed_revision": number; "items": Array<SourceOrderItem> };
export type CreateFeedBatch = { "slug": string; "format": "apk" | "ansible" | "apt" | "cocoapods" | "composer" | "conan" | "conda" | "oci" | "git-lfs" | "go" | "helm" | "huggingface" | "maven" | "npm" | "nuget" | "oci-artifacts" | "p2" | "pub" | "pypi" | "cran" | "raw" | "rubygems" | "cargo" | "swift" | "terraform" | "rpm"; "anonymous_read"?: boolean; "source_ids"?: Array<string>; "publish_policy"?: PutPublishPolicy };
export type FeedBatchResult = { "feed": Feed; "sources": Array<SourceOrderItem>; "publish_policy"?: PublishPolicy };
export type ResolveSource = { "source_id": string; "name": string; "kind": "local" | "remote" | "replica"; "priority": number; "role": "serving" | "shadowed" | "absent" | "pull_through_candidate"; "reason": string; "versions": number };
export type ResolveCache = { "state": "not_applicable" | "not_cached" | "fresh" | "stale"; "last_cached_at"?: string; "last_accessed_at"?: string; "age_seconds"?: number; "metadata_ttl_seconds": number; "stale_if_error_seconds": number; "expires_at"?: string; "servable_stale_on_error": boolean };
export type ResolveExplanation = { "feed_id": string; "coordinate": string; "namespace": string; "status": "served" | "pull_through" | "not_found" | "namespace_mismatch"; "serving"?: ResolveSource; "sources": Array<ResolveSource>; "cache": ResolveCache; "hidden_sources": number };
export type PublishPolicyTestRequest = { "policy": PutPublishPolicy; "coordinate": string; "namespace"?: string; "path"?: string; "principal_id"?: string };
export type PublishPolicyTestTrace = { "rule_id": string; "priority": number; "matched": boolean; "selected": boolean; "reason": string };
export type PublishPolicyTestResult = { "allowed": boolean; "reason": string; "matched_rule_id"?: string; "target_source_id"?: string; "target_source_name"?: string; "denial"?: string; "principal_id": string; "principal_authorized"?: boolean; "trace": Array<PublishPolicyTestTrace> };
export type FeedDeleted = { "id": string; "deleted": boolean; "sources_detached": number; "replication_jobs_removed": number; "role_grants_removed": number; "retention_policies_removed": number; "upload_sessions_removed": number };
export type CachePurge = { "source_id": string; "components_removed": number; "versions_removed": number; "assets_removed": number; "bytes_released": number };
export type TokenRecord = { "id": string; "principal_id": string; "scopes": Array<{ "feed_id"?: string; "namespace"?: string; "actions": Array<"read" | "publish" | "delete" | "configure" | "replicate" | "admin"> }>; "status": "active" | "expired" | "revoked"; "created_at": string; "expires_at": string; "revoked_at"?: string; "last_used_at"?: string };
export type TokenPage = { "items": Array<TokenRecord>; "page": PageInfo };
export type IdentityBindingRecord = { "id": string; "issuer": string; "subject": string; "principal_id": string };
export type IdentityBindingPage = { "items": Array<IdentityBindingRecord>; "page": PageInfo };
export type IdentityBindingDeleted = { "id": string; "deleted": boolean };
export type AuditEvent = { "id": number; "created_at": string; "principal_id"?: string; "action": string; "resource_id"?: string; "result": "success" | "denied" | "failure"; "details": {  } };
export type AuditPage = { "items": Array<AuditEvent>; "page": PageInfo };
export type ComponentStatus = { "status": "ok" | "degraded" | "unavailable" | "unknown"; "latency_ms"?: number; "detail"?: string };
export type WriterLeaseStatus = { "state": "held" | "lost" | "superseded" | "expired" | "unavailable" | "not_serving"; "owner_id"?: string; "epoch"?: number; "expires_at"?: string; "remaining_ms"?: number };
export type ReadinessDetail = { "ready": boolean; "database": ComponentStatus; "blob_storage": ComponentStatus; "writer_lease": WriterLeaseStatus; "checked_at": string };
export type SourceHealth = { "source_id": string; "kind": string; "status": "healthy" | "degraded" | "failing" | "unknown" | "not_applicable"; "last_success_at"?: string; "last_error_at"?: string; "last_status_code"?: number; "last_error"?: string; "p95_latency_ms"?: number; "requests": number; "failures": number };
export type SourceHealthPage = { "items": Array<SourceHealth>; "page": PageInfo };
export type SourceTestResult = { "source_id": string; "reachable": boolean; "authenticated": boolean; "status_code"?: number; "latency_ms": number; "error"?: string };
export type FeedStats = { "feed_id": string; "slug": string; "format": string; "packages": number; "versions": number; "assets": number; "bytes": number; "pulls": number };
export type FeedStatsPage = { "items": Array<FeedStats>; "page": PageInfo };
export type SourceStats = { "source_id": string; "name": string; "kind": string; "format": string; "packages": number; "versions": number; "assets": number; "bytes": number; "attached_feeds": number; "cache_hits": number; "cache_misses": number };
export type SourceStatsPage = { "items": Array<SourceStats>; "page": PageInfo };
export type DomainUsage = { "domain_id": string; "name": string; "blob_count": number; "blob_bytes": number };
export type StorageUsage = { "backend": string; "blob_count": number; "blob_bytes": number; "logical_bytes": number; "dedup_saved_bytes": number; "unreferenced_bytes": number; "gc_candidates": number; "staging_bytes"?: number; "domains": Array<DomainUsage>; "measured_at": string };
export type TrafficPoint = { "bucket": string; "pulls": number; "publishes": number; "bytes_out": number; "bytes_in": number; "cache_hits": number; "cache_misses": number; "errors": number; "cache_hit_ratio"?: number };
export type TrafficSeries = { "scope": "instance" | "feed" | "source"; "ref"?: string; "from": string; "to": string; "step_seconds": number; "points": Array<TrafficPoint>; "totals": TrafficPoint };
export type Provenance = { "asset_id": string; "coordinate": string; "version": string; "asset_key": string; "source_id": string; "feed_id"?: string; "origin": "local" | "remote" | "replica"; "created_at": string; "uploader_id"?: string; "uploader_subject"?: string; "token_id"?: string; "matched_rule"?: string; "digest": string; "verification": "verified" | "integrity_failure"; "verification_detail": string; "downloads": number; "last_downloaded_at"?: string };
export type WebhookRate = { "delivered": number; "failed": number; "pending": number; "success_rate"?: number };
export type WebhookStatsItem = { "webhook_id": string; "endpoint": string; "delivered": number; "failed": number; "pending": number; "success_rate"?: number };
export type WebhookStatsPage = { "window_days": number; "overall": WebhookRate; "items": Array<WebhookStatsItem>; "next_after"?: string };
export type SettingItem = { "key": string; "value": string; "source": "env" | "stored" | "default"; "read_only": boolean; "enforced": boolean; "description"?: string };
export type InstanceSettings = { "revision": number; "items": Array<SettingItem>; "updated_at"?: string };
export type PutSettings = { "session_max_age_seconds"?: number; "session_idle_timeout_seconds"?: number; "default_anonymous_read"?: boolean };
export type RetentionPolicy = { "id": string; "feed_id"?: string; "source_id"?: string; "keep_last"?: number; "max_age_seconds"?: number; "max_idle_seconds"?: number; "min_keep": number; "include_tagged": boolean; "allow_release_delete": boolean; "enabled": boolean; "revision": number };
export type RetentionPolicyPage = { "items": Array<RetentionPolicy>; "next_after"?: string };
export type CreateRetentionPolicy = { "feed_id"?: string; "source_id"?: string; "keep_last"?: number; "max_age_seconds"?: number; "max_idle_seconds"?: number; "min_keep"?: number; "include_tagged"?: boolean; "allow_release_delete"?: boolean; "enabled"?: boolean };
export type PutRetentionPolicy = { "keep_last"?: number; "max_age_seconds"?: number; "max_idle_seconds"?: number; "min_keep"?: number; "include_tagged"?: boolean; "allow_release_delete"?: boolean; "enabled"?: boolean };
export type RunDeletion = { "source_id": string; "coordinate": string; "version": string; "reason": string };
export type RunBlob = { "domain_id": string; "digest": string; "kind": "row" | "object" };
export type RunFinding = { "domain_id": string; "digest": string; "kind": "missing" | "size" | "digest" };
export type RunReport = { "versions_deleted": number; "protected_release": number; "protected_tagged": number; "blobs_marked": number; "blobs_swept": number; "blobs_unmarked": number; "skipped_referenced": number; "gc_failed": number; "integrity_checked": number; "integrity_bytes": number; "integrity_errors": number; "integrity_mismatches": number; "integrity_wrapped": boolean; "deleted"?: Array<RunDeletion>; "marked"?: Array<RunBlob>; "swept"?: Array<RunBlob>; "mismatches"?: Array<RunFinding>; "errors"?: Array<string>; "truncated": boolean };
export type RetentionRun = { "id": string; "trigger": "manual" | "scheduled"; "dry_run": boolean; "state": "running" | "succeeded" | "failed"; "requested_by"?: string; "started_at": string; "finished_at"?: string; "report"?: RunReport };
export type RetentionRunPage = { "items": Array<RetentionRun>; "next_after"?: string };
export type StartRetentionRun = { "dry_run": boolean };
export type IntegrityFinding = { "id": number; "domain_id": string; "digest": string; "kind": "missing" | "size" | "digest"; "detected_at": string };
export type IntegrityFindingPage = { "items": Array<IntegrityFinding>; "next_after"?: string };
export type EffectiveAction = { "action": "read" | "publish" | "delete" | "configure" | "replicate" | "admin"; "allowed": boolean; "via_roles": Array<string>; "denied_by_roles": Array<string> };
export type EffectiveResource = { "kind": "instance" | "feed" | "source" | "artifact"; "feed_id"?: string; "source_id"?: string; "namespace"?: string };
export type EffectivePermissions = { "principal_id": string; "disabled": boolean; "resource": EffectiveResource; "actions": Array<EffectiveAction> };
export type AuditSink = { "configured": boolean; "kind"?: "syslog" | "http"; "endpoint"?: string; "enabled": boolean; "has_credential": boolean; "checkpoint": number; "last_forwarded_at"?: string; "last_error"?: string; "revision": number };
export type PutAuditSink = { "kind": "syslog" | "http"; "endpoint": string; "enabled"?: boolean; "credential"?: string };
export type ReplicationPeer = { "id": string; "name": string; "url": string; "ca_ref"?: string; "revision": number; "created_at": string; "updated_at": string };
export type ReplicationPeerList = { "items": Array<ReplicationPeer> };
export type ReplicationPeerInput = { "name": string; "url": string; "ca_ref"?: string; "token": string };
export type ReplicationPeerPatch = { "revision": number; "url"?: string; "ca_ref"?: string; "token"?: string };
export type ReplicationFilters = { "namespaces"?: Array<string>; "packages"?: Array<string>; "version_prefixes"?: Array<string> };
export type ReplicationStatus = { "phase": "new" | "seed_assets" | "seed_tags" | "incremental"; "running": boolean; "checkpoint": number; "remote_head"?: number; "changes_behind"?: number; "lag_seconds"?: number; "remote_checked_at"?: string; "seed_total"?: number; "seed_done": number; "last_attempt_at"?: string; "last_success_at"?: string; "next_run_at"?: string; "last_error"?: string; "consecutive_failures": number; "assets_applied": number; "blobs_transferred": number; "blobs_deduplicated": number; "bytes_transferred": number; "deletions_applied": number; "deletions_held": number; "conflicts": number; "failures_total": number; "reseeds": number; "reseeding": boolean; "last_reseed_at"?: string; "source_low_water"?: number; "backfills_pending": number; "backfills_completed": number; "resumed_bytes": number };
export type ReplicationJob = { "id": string; "name": string; "peer_id": string; "remote_feed_id": string; "dest_feed_id": string; "replica_source_id": string; "mode": "continuous" | "scheduled" | "manual"; "interval_seconds": number; "include_cached": boolean; "filters": ReplicationFilters; "deletion_policy": "ignore" | "propagate"; "enabled": boolean; "revision": number; "created_at": string; "status": ReplicationStatus };
export type ReplicationJobList = { "items": Array<ReplicationJob> };
export type ReplicationJobInput = { "name": string; "peer_id": string; "remote_feed_id": string; "dest_feed_id": string; "mode"?: "continuous" | "scheduled" | "manual"; "interval_seconds"?: number; "include_cached"?: boolean; "filters"?: ReplicationFilters; "deletion_policy"?: "ignore" | "propagate" };
export type ReplicationJobPatch = { "revision": number; "mode"?: "continuous" | "scheduled" | "manual"; "interval_seconds"?: number; "include_cached"?: boolean; "filters"?: ReplicationFilters; "deletion_policy"?: "ignore" | "propagate"; "enabled"?: boolean };
export type ReplicationStatusEntry = { "id": string; "name": string; "dest_feed_id": string; "enabled": boolean; "status": ReplicationStatus };
export type ReplicationStatusList = { "items": Array<ReplicationStatusEntry> };
export type ReplicationTrigger = { "job_id": string; "state": string };
export type ReplicationDeletionEntry = { "source": "held" | "pending"; "kind": string; "coordinate": string; "version"?: string; "key"?: string; "seq": number; "disposition": "would_delete" | "protected" | "absent" };
export type ReplicationDeletionPreview = { "policy": string; "applies": boolean; "entries": Array<ReplicationDeletionEntry>; "counts": { "would_delete"?: number; "protected"?: number; "absent"?: number }; "truncated": boolean; "reseed_required"?: boolean };
export type ReplicationOrigin = { "source_id": string; "name": string; "kind": string; "priority": number };
export type ReplicationSubject = { "digest": string; "size": number; "media_type": string; "artifact_type": string; "annotations"?: {  } };
export type ReplicationItem = { "origin": ReplicationOrigin; "namespace"?: string; "coordinate": string; "version": string; "asset_key": string; "media_type": string; "digest": string; "size": number; "component_metadata"?: {  }; "subject"?: ReplicationSubject };
export type ReplicationTagItem = { "origin": ReplicationOrigin; "namespace"?: string; "coordinate": string; "tag": string; "version": string };
export type ReplicationComponentItem = { "origin": ReplicationOrigin; "namespace"?: string; "coordinate": string; "metadata"?: {  } };
export type ReplicationHead = { "seq": number; "changed_at"?: string; "now": string; "format": string; "low_water": number };
export type ReplicationManifestPage = { "kind": "asset" | "tag"; "items"?: Array<ReplicationItem>; "tags"?: Array<ReplicationTagItem>; "next"?: string; "total"?: number };
export type ReplicationChange = { "seq": number; "at": string; "kind": string; "op": string; "source_id": string; "coordinate": string; "version"?: string; "key"?: string; "asset"?: ReplicationItem; "tag"?: ReplicationTagItem; "component"?: ReplicationComponentItem; "attachment"?: ReplicationAttachment };
export type ReplicationChangePage = { "changes": Array<ReplicationChange>; "next": number; "more": boolean; "head": number; "head_at"?: string; "low_water": number };
export type StorageJob = { "id": string; "kind": "migrate" | "cleanup"; "from": "disk" | "s3"; "to": "disk" | "s3"; "dry_run": boolean; "state": "running" | "paused" | "failed" | "completed"; "throttle_bytes_per_second": number; "total_blobs": number; "total_bytes": number; "processed_blobs": number; "processed_bytes": number; "copied_blobs": number; "copied_bytes": number; "skipped_blobs": number; "deleted_blobs": number; "deleted_bytes": number; "failed_blobs": number; "pass": number; "last_error"?: string; "requested_by"?: string; "started_at": string; "updated_at": string; "finished_at"?: string; "switched_at"?: string };
export type StorageJobPage = { "items": Array<StorageJob> };
export type StorageMigrationStatus = { "active_backend": "disk" | "s3"; "previous_backend"?: "disk" | "s3"; "cleanup_pending": boolean; "available_backends": Array<"disk" | "s3">; "current_job"?: StorageJob };
export type StartStorageJob = { "action": "migrate" | "cleanup"; "to"?: "disk" | "s3"; "dry_run": boolean; "throttle_bytes_per_second"?: number };
export type ReplicationAttachment = { "origin": ReplicationOrigin };
export type ReplicationTemplateRetention = { "keep_last"?: number; "max_age_seconds"?: number; "max_idle_seconds"?: number; "min_keep": number; "include_tagged": boolean; "enabled": boolean };
export type ReplicationTemplateCache = { "metadata_ttl_seconds": number; "negative_ttl_seconds": number; "stale_if_error_seconds": number; "max_entries": number; "max_bytes": number };
export type ReplicationTemplateRemote = { "name": string; "timeout_seconds": number; "cache": ReplicationTemplateCache };
export type ReplicationTemplateSource = { "name": string; "kind": string; "priority": number };
export type ReplicationTemplate = { "version": number; "format": string; "retention"?: ReplicationTemplateRetention; "remote_sources": Array<ReplicationTemplateRemote>; "sources": Array<ReplicationTemplateSource>; "excluded": Array<string> };
export type ReplicationTemplateDiff = { "section": string; "field": string; "action": "add" | "change" | "unchanged" | "preserved" | "blocked" | "info"; "current"?: unknown; "proposed"?: unknown; "reason"?: string };
export type ReplicationTemplateRequest = { "template"?: ReplicationTemplate };
export type ReplicationTemplatePreview = { "digest": string; "template": ReplicationTemplate; "diffs": Array<ReplicationTemplateDiff>; "applicable": number; "blocked": number };
export type ReplicationTemplateApplyRequest = { "digest": string; "template"?: ReplicationTemplate };
export type ReplicationTemplateApplyResult = { "digest": string; "applied": number; "diffs": Array<ReplicationTemplateDiff> };
export const formats = ["apk","ansible","apt","cocoapods","composer","conan","conda","oci","git-lfs","go","helm","huggingface","maven","npm","nuget","oci-artifacts","p2","pub","pypi","cran","raw","rubygems","cargo","swift","terraform","rpm"] as const;
export interface Operations {
getStatus: { body: undefined; response: ServiceStatus };
listFeeds: { body: undefined; response: FeedPage };
createFeed: { body: CreateFeed; response: Feed };
getFeed: { body: undefined; response: Feed };
updateFeed: { body: PatchFeed; response: Feed };
deleteFeed: { body: undefined; response: FeedDeleted };
listSources: { body: undefined; response: SourcePage };
createSource: { body: CreateSource; response: Source };
getSource: { body: undefined; response: Source };
updateSource: { body: PatchSource; response: Source };
listFeedSources: { body: undefined; response: SourceAttachmentPage };
reorderFeedSources: { body: ReorderSources; response: SourceOrder };
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
listIdentityBindings: { body: undefined; response: IdentityBindingPage };
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
searchPackages: { body: undefined; response: PackageSearchPage };
createFeedBatch: { body: CreateFeedBatch; response: FeedBatchResult };
resolveFeedCoordinate: { body: undefined; response: ResolveExplanation };
testPublishPolicy: { body: PublishPolicyTestRequest; response: PublishPolicyTestResult };
purgeSourceCache: { body: undefined; response: CachePurge };
listServiceTokens: { body: undefined; response: TokenPage };
deleteIdentityBinding: { body: undefined; response: IdentityBindingDeleted };
listAuditEvents: { body: undefined; response: AuditPage };
listSourceHealth: { body: undefined; response: SourceHealthPage };
getSourceHealth: { body: undefined; response: SourceHealth };
testSource: { body: undefined; response: SourceTestResult };
listFeedStats: { body: undefined; response: FeedStatsPage };
listSourceStats: { body: undefined; response: SourceStatsPage };
getStorageUsage: { body: undefined; response: StorageUsage };
getTrafficMetrics: { body: undefined; response: TrafficSeries };
getAssetProvenance: { body: undefined; response: Provenance };
getWebhookStats: { body: undefined; response: WebhookStatsPage };
redeliverWebhookDelivery: { body: undefined; response: WebhookDelivery };
getSettings: { body: undefined; response: InstanceSettings };
putSettings: { body: PutSettings; response: InstanceSettings };
listRetentionPolicies: { body: undefined; response: RetentionPolicyPage };
createRetentionPolicy: { body: CreateRetentionPolicy; response: RetentionPolicy };
putRetentionPolicy: { body: PutRetentionPolicy; response: RetentionPolicy };
deleteRetentionPolicy: { body: undefined; response: RetentionPolicy };
listRetentionRuns: { body: undefined; response: RetentionRunPage };
startRetentionRun: { body: StartRetentionRun; response: RetentionRun };
getRetentionRun: { body: undefined; response: RetentionRun };
listIntegrityFindings: { body: undefined; response: IntegrityFindingPage };
getEffectivePermissions: { body: undefined; response: EffectivePermissions };
getAuditSink: { body: undefined; response: AuditSink };
putAuditSink: { body: PutAuditSink; response: AuditSink };
deleteAuditSink: { body: undefined; response: AuditSink };
listReplicationPeers: { body: undefined; response: ReplicationPeerList };
createReplicationPeer: { body: ReplicationPeerInput; response: ReplicationPeer };
getReplicationPeer: { body: undefined; response: ReplicationPeer };
patchReplicationPeer: { body: ReplicationPeerPatch; response: ReplicationPeer };
deleteReplicationPeer: { body: undefined; response: undefined };
listReplicationJobs: { body: undefined; response: ReplicationJobList };
createReplicationJob: { body: ReplicationJobInput; response: ReplicationJob };
getReplicationJob: { body: undefined; response: ReplicationJob };
patchReplicationJob: { body: ReplicationJobPatch; response: ReplicationJob };
deleteReplicationJob: { body: undefined; response: undefined };
getReplicationJobStatus: { body: undefined; response: ReplicationStatus };
triggerReplicationJob: { body: undefined; response: ReplicationTrigger };
previewReplicationDeletions: { body: undefined; response: ReplicationDeletionPreview };
listReplicationStatus: { body: undefined; response: ReplicationStatusList };
getReplicationExportHead: { body: undefined; response: ReplicationHead };
getReplicationExportManifest: { body: undefined; response: ReplicationManifestPage };
getReplicationExportChanges: { body: undefined; response: ReplicationChangePage };
getReplicationExportBlob: { body: undefined; response: undefined };
getStorageMigration: { body: undefined; response: StorageMigrationStatus };
listStorageJobs: { body: undefined; response: StorageJobPage };
startStorageJob: { body: StartStorageJob; response: StorageJob };
getStorageJob: { body: undefined; response: StorageJob };
storageJobAction: { body: undefined; response: StorageJob };
getReplicationExportTemplate: { body: undefined; response: ReplicationTemplate };
previewReplicationTemplate: { body: ReplicationTemplateRequest; response: ReplicationTemplatePreview };
applyReplicationTemplate: { body: ReplicationTemplateApplyRequest; response: ReplicationTemplateApplyResult };
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
  "deleteFeed": {
    "path": "/api/v1/feeds/{id}",
    "method": "DELETE",
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
  "reorderFeedSources": {
    "path": "/api/v1/feeds/{id}/sources",
    "method": "PUT",
    "idempotent": true
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
  "listIdentityBindings": {
    "path": "/api/v1/identity-bindings",
    "method": "GET",
    "idempotent": false
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
  },
  "searchPackages": {
    "path": "/api/v1/packages",
    "method": "GET",
    "idempotent": false
  },
  "createFeedBatch": {
    "path": "/api/v1/feeds/batch",
    "method": "POST",
    "idempotent": true
  },
  "resolveFeedCoordinate": {
    "path": "/api/v1/feeds/{id}/resolve",
    "method": "GET",
    "idempotent": false
  },
  "testPublishPolicy": {
    "path": "/api/v1/feeds/{id}/publish-policy:test",
    "method": "POST",
    "idempotent": false
  },
  "purgeSourceCache": {
    "path": "/api/v1/sources/{id}/cache",
    "method": "DELETE",
    "idempotent": true
  },
  "listServiceTokens": {
    "path": "/api/v1/tokens",
    "method": "GET",
    "idempotent": false
  },
  "deleteIdentityBinding": {
    "path": "/api/v1/identity-bindings/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "listAuditEvents": {
    "path": "/api/v1/audit",
    "method": "GET",
    "idempotent": false
  },
  "listSourceHealth": {
    "path": "/api/v1/source-health",
    "method": "GET",
    "idempotent": false
  },
  "getSourceHealth": {
    "path": "/api/v1/sources/{id}/health",
    "method": "GET",
    "idempotent": false
  },
  "testSource": {
    "path": "/api/v1/sources/{id}:test",
    "method": "POST",
    "idempotent": false
  },
  "listFeedStats": {
    "path": "/api/v1/stats/feeds",
    "method": "GET",
    "idempotent": false
  },
  "listSourceStats": {
    "path": "/api/v1/stats/sources",
    "method": "GET",
    "idempotent": false
  },
  "getStorageUsage": {
    "path": "/api/v1/storage/usage",
    "method": "GET",
    "idempotent": false
  },
  "getTrafficMetrics": {
    "path": "/api/v1/metrics/traffic",
    "method": "GET",
    "idempotent": false
  },
  "getAssetProvenance": {
    "path": "/api/v1/assets/{id}/provenance",
    "method": "GET",
    "idempotent": false
  },
  "getWebhookStats": {
    "path": "/api/v1/webhooks/stats",
    "method": "GET",
    "idempotent": false
  },
  "redeliverWebhookDelivery": {
    "path": "/api/v1/webhooks/{id}/deliveries/{event_id}:redeliver",
    "method": "POST",
    "idempotent": true
  },
  "getSettings": {
    "path": "/api/v1/settings",
    "method": "GET",
    "idempotent": false
  },
  "putSettings": {
    "path": "/api/v1/settings",
    "method": "PUT",
    "idempotent": true
  },
  "listRetentionPolicies": {
    "path": "/api/v1/retention/policies",
    "method": "GET",
    "idempotent": false
  },
  "createRetentionPolicy": {
    "path": "/api/v1/retention/policies",
    "method": "POST",
    "idempotent": true
  },
  "putRetentionPolicy": {
    "path": "/api/v1/retention/policies/{id}",
    "method": "PUT",
    "idempotent": true
  },
  "deleteRetentionPolicy": {
    "path": "/api/v1/retention/policies/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "listRetentionRuns": {
    "path": "/api/v1/retention/runs",
    "method": "GET",
    "idempotent": false
  },
  "startRetentionRun": {
    "path": "/api/v1/retention/runs",
    "method": "POST",
    "idempotent": false
  },
  "getRetentionRun": {
    "path": "/api/v1/retention/runs/{id}",
    "method": "GET",
    "idempotent": false
  },
  "listIntegrityFindings": {
    "path": "/api/v1/retention/integrity-findings",
    "method": "GET",
    "idempotent": false
  },
  "getEffectivePermissions": {
    "path": "/api/v1/permissions/effective",
    "method": "GET",
    "idempotent": false
  },
  "getAuditSink": {
    "path": "/api/v1/audit-sink",
    "method": "GET",
    "idempotent": false
  },
  "putAuditSink": {
    "path": "/api/v1/audit-sink",
    "method": "PUT",
    "idempotent": true
  },
  "deleteAuditSink": {
    "path": "/api/v1/audit-sink",
    "method": "DELETE",
    "idempotent": true
  },
  "listReplicationPeers": {
    "path": "/api/v1/replication/peers",
    "method": "GET",
    "idempotent": false
  },
  "createReplicationPeer": {
    "path": "/api/v1/replication/peers",
    "method": "POST",
    "idempotent": true
  },
  "getReplicationPeer": {
    "path": "/api/v1/replication/peers/{id}",
    "method": "GET",
    "idempotent": false
  },
  "patchReplicationPeer": {
    "path": "/api/v1/replication/peers/{id}",
    "method": "PATCH",
    "idempotent": true
  },
  "deleteReplicationPeer": {
    "path": "/api/v1/replication/peers/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "listReplicationJobs": {
    "path": "/api/v1/replication/jobs",
    "method": "GET",
    "idempotent": false
  },
  "createReplicationJob": {
    "path": "/api/v1/replication/jobs",
    "method": "POST",
    "idempotent": true
  },
  "getReplicationJob": {
    "path": "/api/v1/replication/jobs/{id}",
    "method": "GET",
    "idempotent": false
  },
  "patchReplicationJob": {
    "path": "/api/v1/replication/jobs/{id}",
    "method": "PATCH",
    "idempotent": true
  },
  "deleteReplicationJob": {
    "path": "/api/v1/replication/jobs/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "getReplicationJobStatus": {
    "path": "/api/v1/replication/jobs/{id}/status",
    "method": "GET",
    "idempotent": false
  },
  "triggerReplicationJob": {
    "path": "/api/v1/replication/jobs/{id}/runs",
    "method": "POST",
    "idempotent": true
  },
  "previewReplicationDeletions": {
    "path": "/api/v1/replication/jobs/{id}/deletions",
    "method": "GET",
    "idempotent": false
  },
  "listReplicationStatus": {
    "path": "/api/v1/replication/status",
    "method": "GET",
    "idempotent": false
  },
  "getReplicationExportHead": {
    "path": "/api/v1/replication/export/feeds/{feed}/head",
    "method": "GET",
    "idempotent": false
  },
  "getReplicationExportManifest": {
    "path": "/api/v1/replication/export/feeds/{feed}/manifest",
    "method": "GET",
    "idempotent": false
  },
  "getReplicationExportChanges": {
    "path": "/api/v1/replication/export/feeds/{feed}/changes",
    "method": "GET",
    "idempotent": false
  },
  "getReplicationExportBlob": {
    "path": "/api/v1/replication/export/feeds/{feed}/blobs/{digest}",
    "method": "GET",
    "idempotent": false
  },
  "getStorageMigration": {
    "path": "/api/v1/storage/migration",
    "method": "GET",
    "idempotent": false
  },
  "listStorageJobs": {
    "path": "/api/v1/storage/migrations",
    "method": "GET",
    "idempotent": false
  },
  "startStorageJob": {
    "path": "/api/v1/storage/migrations",
    "method": "POST",
    "idempotent": false
  },
  "getStorageJob": {
    "path": "/api/v1/storage/migrations/{id}",
    "method": "GET",
    "idempotent": false
  },
  "storageJobAction": {
    "path": "/api/v1/storage/migrations/{id}/{action}",
    "method": "POST",
    "idempotent": false
  },
  "getReplicationExportTemplate": {
    "path": "/api/v1/replication/export/feeds/{feed}/template",
    "method": "GET",
    "idempotent": false
  },
  "previewReplicationTemplate": {
    "path": "/api/v1/replication/jobs/{id}/template/preview",
    "method": "POST",
    "idempotent": false
  },
  "applyReplicationTemplate": {
    "path": "/api/v1/replication/jobs/{id}/template/apply",
    "method": "POST",
    "idempotent": true
  }
} as const;
