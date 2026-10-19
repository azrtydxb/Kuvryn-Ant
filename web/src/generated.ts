// Generated from api/openapi.yaml by npm run generate. Do not edit.
export type ServiceStatus = { "name": "kuvryn-ant"; "api_version": "v1"; "readiness"?: ReadinessDetail };
export type Error = { "code": "invalid_request" | "unauthenticated" | "forbidden" | "not_found" | "conflict" | "precondition_failed" | "unavailable" | "internal_error" | "rate_limited" | "federation_not_configured" | "not_authority" | "member_unreachable" | "invitation_invalid" | "stale_generation" | "document_rejected" | "federation_managed" | "not_federated" | "feed_home_elsewhere" | "feed_transferring"; "message": string; "request_id": string };
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
export type IdentityPrincipal = { "id": string; "subject": string; "kind": "user" | "service" | "emergency"; "disabled": boolean; "revision": number; "role_ids": Array<string>; "origin": "local" | "federation" };
export type CreatePrincipal = { "subject": string; "kind": "user" | "service" };
export type IdentityRoleGrant = { "kind": "artifact" | "feed" | "source" | "instance"; "effect": "allow" | "deny"; "feed_id"?: string; "source_id"?: string; "namespace"?: string; "actions": Array<"read" | "publish" | "delete" | "configure" | "replicate" | "admin">; "feed_key"?: string; "inert"?: "unresolved_feed" | "admin_not_accepted" | "wildcard_not_accepted" };
export type IdentityRole = { "id": string; "name": string; "revision": number; "grants": Array<IdentityRoleGrant>; "origin": "local" | "federation" };
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
export type ResolveExplanation = { "feed_id": string; "coordinate": string; "namespace": string; "status": "served" | "pull_through" | "not_found" | "namespace_mismatch" | "federated"; "serving"?: ResolveSource; "sources": Array<ResolveSource>; "cache": ResolveCache; "hidden_sources": number; "federation"?: FederationRoutePlan };
export type PublishPolicyTestRequest = { "policy": PutPublishPolicy; "coordinate": string; "namespace"?: string; "path"?: string; "principal_id"?: string };
export type PublishPolicyTestTrace = { "rule_id": string; "priority": number; "matched": boolean; "selected": boolean; "reason": string };
export type PublishPolicyTestResult = { "allowed": boolean; "reason": string; "matched_rule_id"?: string; "target_source_id"?: string; "target_source_name"?: string; "denial"?: string; "principal_id": string; "principal_authorized"?: boolean; "trace": Array<PublishPolicyTestTrace> };
export type FeedDeleted = { "id": string; "deleted": boolean; "sources_detached": number; "replication_jobs_removed": number; "role_grants_removed": number; "retention_policies_removed": number; "upload_sessions_removed": number };
export type SourceDeleted = { "id": string; "deleted": boolean; "feeds_detached": number; "components_removed": number; "versions_removed": number; "assets_removed": number; "bytes_released": number };
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
export type EffectivePermissions = { "principal_id": string; "disabled": boolean; "resource": EffectiveResource; "actions": Array<EffectiveAction>; "origin": "local" | "federation"; "roles": Array<EffectiveRole>; "federation_policy_generation": number };
export type AuditSink = { "configured": boolean; "kind"?: "syslog" | "http"; "endpoint"?: string; "enabled": boolean; "has_credential": boolean; "checkpoint": number; "last_forwarded_at"?: string; "last_error"?: string; "revision": number };
export type PutAuditSink = { "kind": "syslog" | "http"; "endpoint": string; "enabled"?: boolean; "credential"?: string };
export type ReplicationPeer = { "id": string; "name": string; "url": string; "ca_ref"?: string; "revision": number; "created_at": string; "updated_at": string; "auth_mode": "token" | "federation"; "member_instance_id"?: string };
export type ReplicationPeerList = { "items": Array<ReplicationPeer> };
export type ReplicationPeerInput = { "name": string; "url": string; "ca_ref"?: string; "token": string };
export type ReplicationPeerPatch = { "revision"?: number; "url"?: string; "ca_ref"?: string; "token"?: string };
export type ReplicationFilters = { "namespaces"?: Array<string>; "packages"?: Array<string>; "version_prefixes"?: Array<string> };
export type ReplicationStatus = { "phase": "new" | "seed_assets" | "seed_tags" | "incremental"; "running": boolean; "checkpoint": number; "remote_head"?: number; "changes_behind"?: number; "lag_seconds"?: number; "remote_checked_at"?: string; "seed_total"?: number; "seed_done": number; "last_attempt_at"?: string; "last_success_at"?: string; "next_run_at"?: string; "last_error"?: string; "consecutive_failures": number; "assets_applied": number; "blobs_transferred": number; "blobs_deduplicated": number; "bytes_transferred": number; "deletions_applied": number; "deletions_held": number; "conflicts": number; "failures_total": number; "reseeds": number; "reseeding": boolean; "last_reseed_at"?: string; "source_low_water"?: number; "backfills_pending": number; "backfills_completed": number; "resumed_bytes": number };
export type ReplicationJob = { "id": string; "name": string; "peer_id": string; "remote_feed_id": string; "dest_feed_id": string; "replica_source_id": string; "mode": "continuous" | "scheduled" | "manual"; "interval_seconds": number; "include_cached": boolean; "filters": ReplicationFilters; "deletion_policy": "ignore" | "propagate"; "enabled": boolean; "revision": number; "created_at": string; "status": ReplicationStatus; "destination_read_only": boolean; "origin": "local" | "federation"; "federation_rule_id"?: string; "federation_feed_key"?: string; "adopted_at"?: string };
export type ReplicationJobList = { "items": Array<ReplicationJob> };
export type ReplicationJobInput = { "name": string; "peer_id": string; "remote_feed_id": string; "dest_feed_id": string; "mode"?: "continuous" | "scheduled" | "manual"; "interval_seconds"?: number; "include_cached"?: boolean; "filters"?: ReplicationFilters; "deletion_policy"?: "ignore" | "propagate"; "destination_read_only"?: boolean };
export type ReplicationJobPatch = { "revision"?: number; "mode"?: "continuous" | "scheduled" | "manual"; "interval_seconds"?: number; "include_cached"?: boolean; "filters"?: ReplicationFilters; "deletion_policy"?: "ignore" | "propagate"; "enabled"?: boolean; "destination_read_only"?: boolean };
export type ReplicationStatusEntry = { "id": string; "name": string; "dest_feed_id": string; "enabled": boolean; "status": ReplicationStatus };
export type ReplicationStatusList = { "items": Array<ReplicationStatusEntry> };
export type ReplicationTrigger = { "job_id": string; "state": string };
export type ReplicationDeletionEntry = { "source": "held" | "pending"; "kind": string; "coordinate": string; "version"?: string; "key"?: string; "seq": number; "disposition": "would_delete" | "protected" | "absent" };
export type ReplicationDeletionPreview = { "policy": string; "applies": boolean; "entries": Array<ReplicationDeletionEntry>; "counts": { "would_delete"?: number; "protected"?: number; "absent"?: number }; "truncated": boolean; "reseed_required"?: boolean };
export type ReplicationOrigin = { "source_id": string; "name": string; "kind": string; "priority": number };
export type ReplicationSubject = { "digest": string; "size": number; "media_type": string; "artifact_type": string; "annotations"?: Record<string, string> };
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
export type PackageMutation = { "action": "delete" | "unlist" | "relist"; "version_removed": boolean; "component_removed"?: boolean; "assets_removed": number; "tags_removed": number; "unchanged": boolean };
export type DeletePolicy = { "namespace"?: string; "mode": "allowed" | "denied" | "unlist_only"; "immutable_releases": boolean; "revision"?: number };
export type RateLimitRule = { "scope": "principal" | "feed" | "namespace" | "site"; "subject": string; "namespace"?: string; "cidrs"?: Array<string>; "requests_per_minute": number; "burst": number };
export type QuotaRule = { "scope": "feed" | "namespace"; "feed_id": string; "namespace"?: string; "max_bytes"?: number; "max_packages"?: number };
export type LimitsConfig = { "revision": number; "rate_limits": Array<RateLimitRule>; "quotas": Array<QuotaRule>; "updated_at"?: string };
export type PutLimits = { "rate_limits": Array<RateLimitRule>; "quotas": Array<QuotaRule> };
export type QuotaUsage = { "scope": "feed" | "namespace"; "feed_id": string; "namespace"?: string; "bytes_used": number; "packages_used": number; "max_bytes"?: number; "max_packages"?: number };
export type ThrottleCounts = { "principal": number; "feed": number; "namespace": number; "site": number };
export type LimitsUsage = { "quotas": Array<QuotaUsage>; "throttled": ThrottleCounts; "measured_at": string };
export type ReadPolicySettings = { "local_first"?: boolean; "fallback_on_not_found"?: boolean; "failover_on_upstream_error"?: boolean };
export type PublishTemplateBody = { "default_source_id": string; "rules": Array<PublishRule> };
export type PolicyTemplateInput = { "name": string; "description"?: string; "kind": "publish" | "read"; "extends_id"?: string; "publish"?: PublishTemplateBody; "read"?: ReadPolicySettings };
export type PolicyTemplate = { "id": string; "revision": number; "bound_feeds": number; "description": string; "name": string; "kind": "publish" | "read"; "extends_id"?: string; "publish"?: PublishTemplateBody; "read"?: ReadPolicySettings };
export type PolicyTemplateList = { "items": Array<PolicyTemplate> };
export type FeedPolicyBinding = { "feed_id": string; "revision": number; "publish_template_id"?: string; "read_template_id"?: string };
export type PutFeedPolicyBinding = { "publish_template_id"?: string; "read_template_id"?: string };
export type EffectivePublishRule = { "id": string; "priority": number; "selector": PublishSelector; "target_source_id"?: string; "deny": boolean; "origin": string };
export type EffectiveReadOrigins = { "local_first"?: string; "fallback_on_not_found"?: string; "failover_on_upstream_error"?: string };
export type EffectivePolicy = { "feed_id": string; "revision": number; "publish_template_chain": Array<string>; "read_template_chain": Array<string>; "default_source_id": string; "default_origin": string; "rules": Array<EffectivePublishRule>; "read": ReadPolicySettings; "read_origins": EffectiveReadOrigins };
export type CertificateStatus = { "subject": string; "use"?: string; "not_before": string; "not_after": string; "days_left": number; "status": "ok" | "expiring" | "expired" | "not_yet_valid"; "sha256": string };
export type GroupRoleMapping = { "group_id": string; "role_ids": Array<string> };
export type IdentityProviderConfig = { "issuer"?: string; "client_id"?: string; "groups_claim"?: string; "entity_id"?: string; "idp_entity_id"?: string; "idp_metadata_xml"?: string; "sp_certificate_pem"?: string; "groups_attribute"?: string; "email_attribute"?: string; "provider_id"?: string; "url"?: string; "bind_dn"?: string; "ca_pem"?: string; "user_base_dn"?: string; "group_base_dn"?: string; "login_attribute"?: string; "user_id_attribute"?: string; "group_id_attribute"?: string; "user_class"?: string; "group_class"?: string; "member_attribute"?: string; "allowed_endpoints"?: Array<string>; "group_roles"?: Array<GroupRoleMapping> };
export type IdentityProvider = { "id": string; "kind": "oidc" | "saml" | "ldap"; "name": string; "enabled": boolean; "config": IdentityProviderConfig; "has_secret": boolean; "revision": number; "last_error"?: string; "created_at": string; "updated_at": string; "active": boolean; "shadowed_by_file": boolean; "certificates": Array<CertificateStatus> };
export type IdentityProviderInput = { "kind": "oidc" | "saml" | "ldap"; "name": string; "enabled": boolean; "config": IdentityProviderConfig; "secret"?: string; "rotate_sp_certificate"?: boolean };
export type UpdateIdentityProvider = { "kind"?: "oidc" | "saml" | "ldap"; "name": string; "enabled": boolean; "config": IdentityProviderConfig; "secret"?: string; "rotate_sp_certificate"?: boolean };
export type FileIdentityProvider = { "kind": "oidc" | "saml" | "ldap"; "label": string; "detail": string };
export type ActiveIdentityProvider = { "kind": "oidc" | "saml" | "ldap"; "source": "file" | "database"; "provider_id"?: string };
export type LocalAccount = { "exists": boolean; "username"?: string; "totp_enabled": boolean; "locked_until"?: string; "last_login_at"?: string };
export type IdentityProviderList = { "items": Array<IdentityProvider>; "file_items": Array<FileIdentityProvider>; "active": Array<ActiveIdentityProvider>; "sso_mandatory": boolean; "origin": string; "local_account": LocalAccount };
export type IdentityProviderTestRequest = { "id"?: string; "kind": "oidc" | "saml" | "ldap"; "config": IdentityProviderConfig; "secret"?: string };
export type IdentityTestCheck = { "name": string; "ok": boolean; "detail": string };
export type IdentityProviderTest = { "kind": "oidc" | "saml" | "ldap"; "ok": boolean; "checks": Array<IdentityTestCheck>; "warnings": Array<string>; "clock_skew_seconds"?: number; "suggested_allowed_endpoints"?: Array<string>; "tls_mode"?: "ldaps" | "starttls"; "certificates": Array<CertificateStatus> };
export type SamlMetadataImportRequest = { "xml": string };
export type SamlEndpoint = { "binding": string; "location": string };
export type SamlMetadataImport = { "entity_id": string; "sso_endpoints": Array<SamlEndpoint>; "slo_endpoints": Array<SamlEndpoint>; "certificates": Array<CertificateStatus>; "valid_until"?: string; "errors": Array<string>; "warnings": Array<string>; "ok": boolean };
export type MappingPreviewRequest = { "sample"?: string; "login"?: string };
export type MappingSubject = { "issuer": string; "subject": string; "email"?: string; "groups": Array<string>; "audience"?: Array<string> };
export type MappingBinding = { "found": boolean; "principal_id"?: string; "disabled"?: boolean; "roles": Array<string> };
export type MappingPreview = { "identity": MappingSubject; "signature_verified": boolean; "binding": MappingBinding; "proposed_roles": Array<string>; "would_sign_in": boolean; "problems": Array<string>; "warnings": Array<string>; "clock_skew_seconds"?: number };
export type IdentityPolicy = { "sso_mandatory": boolean };
export type TotpEnrollment = { "otpauth_uri": string };
export type TotpConfirm = { "code": string };
export type SecurityResultInput = { "scanner": string; "status": "passed" | "failed" | "warning"; "summary"?: {  } };
export type SecurityResult = { "id": string; "asset_id": string; "scanner": string; "status": "passed" | "failed" | "warning"; "summary": {  }; "created_at": string };
export type ClientAuthMethod = "anonymous" | "token" | "basic-directory" | "pat" | "github-oidc";
export type ClientAuthPolicy = { "feed_id"?: string; "methods": Array<ClientAuthMethod>; "anonymous_network_group_ids": Array<string>; "inherited": boolean; "revision": number };
export type PutClientAuthPolicy = { "methods": Array<ClientAuthMethod>; "anonymous_network_group_ids": Array<string> };
export type NetworkGroup = { "id": string; "name": string; "cidrs": Array<string>; "revision": number };
export type NetworkGroupInput = { "name": string; "cidrs": Array<string> };
export type NetworkGroupList = { "items": Array<NetworkGroup> };
export type GitHubOIDCConfig = { "issuer": string; "audience": string; "revision": number };
export type PutGitHubOIDCConfig = { "audience": string };
export type TokenScope = { "feed_id"?: string; "namespace"?: string; "actions": Array<"read" | "publish" | "delete"> };
export type GitHubTrustRule = { "id": string; "name": string; "org": string; "repo": string; "ref": string; "environment": string; "workflow": string; "principal_id": string; "scopes": Array<TokenScope>; "ttl_seconds": number; "enabled": boolean; "revision": number };
export type GitHubTrustRuleInput = { "name": string; "org": string; "repo": string; "ref"?: string; "environment"?: string; "workflow"?: string; "principal_id": string; "scopes": Array<TokenScope>; "ttl_seconds"?: number; "enabled"?: boolean };
export type GitHubTrustRuleList = { "items": Array<GitHubTrustRule> };
export type GitHubExchangeRequest = { "token": string };
export type GitHubExchangeResponse = { "access_token": string; "token_type": "Bearer"; "expires_at": string; "trust_rule_id": string };
export type PersonalAccessToken = { "id": string; "name": string; "scopes": Array<TokenScope>; "created_at": string; "expires_at": string; "last_used_at"?: string; "revoked_at"?: string };
export type PersonalAccessTokenList = { "items": Array<PersonalAccessToken> };
export type CreatePersonalAccessToken = { "name": string; "scopes": Array<TokenScope>; "expires_in_days": number };
export type CreatedPersonalAccessToken = { "id": string; "name": string; "scopes": Array<TokenScope>; "created_at": string; "expires_at": string; "last_used_at"?: string; "revoked_at"?: string; "token": string };
export type FederationProtocolRange = { "min": number; "max": number };
export type FederationDocumentHead = { "generation": number; "hash": string; "applied_at": string; "age_seconds": number; "issued_at": string };
export type FederationSyncStatus = { "last_attempt_at"?: string; "last_success_at"?: string; "last_error"?: string; "source"?: string };
export type FederationCertificateStatus = { "serial": string; "expires_at"?: string };
export type FederationMemberCounts = { "active": number; "suspended": number; "removed": number };
export type FederationStatus = { "instance_id": string; "state": "none" | "joining" | "active" | "left" | "removed"; "role"?: "member" | "authority"; "health": "disabled" | "ok" | "degraded"; "federation_id"?: string; "federation_name"?: string; "authority_epoch"?: number; "authority_instance_id"?: string; "authority_url"?: string; "listener_configured": boolean; "listen_address"?: string; "federation_url"?: string; "ca_sha256"?: string; "recovery_key_fingerprint"?: string; "membership"?: FederationDocumentHead; "certificate"?: FederationCertificateStatus; "sync": FederationSyncStatus; "members": FederationMemberCounts; "software_version": string; "protocol": FederationProtocolRange; "policy"?: FederationPolicyStatus; "alerts": Array<FederationAlert>; "topology"?: FederationTopologyStatus };
export type FederationMember = { "instance_id": string; "name": string; "status": "active" | "suspended" | "removed"; "api_url"?: string; "federation_url": string; "cert_sha256"?: string; "cert_serial"?: string; "labels": Record<string, string>; "software_version"?: string; "protocol": FederationProtocolRange; "joined_at"?: string; "authority": boolean; "self": boolean; "last_seen_at"?: string; "rtt_ms"?: number; "skew_ms"?: number; "membership_generation": number };
export type FederationMemberList = { "items": Array<FederationMember> };
export type FoundFederation = { "name": string; "member_name"?: string; "labels"?: Record<string, string> };
export type FederationFoundResult = { "status": FederationStatus; "recovery_key": string; "recovery_public_key": string; "ca_sha256": string };
export type JoinFederation = { "authority_url": string; "ca_sha256": string; "authority_cert_sha256"?: string; "invitation": string; "labels"?: Record<string, string> };
export type LeaveFederation = { "confirm": boolean; "force"?: boolean };
export type FederationInvitation = { "id": string; "name": string; "labels": Record<string, string>; "expires_at": string; "created_at": string; "consumed_at"?: string; "consumed_by"?: string; "revoked_at"?: string; "state": "pending" | "claimed" | "consumed" | "expired" | "revoked"; "claimed_at"?: string };
export type FederationInvitationList = { "items": Array<FederationInvitation> };
export type CreateFederationInvitation = { "name": string; "labels"?: Record<string, string>; "ttl_seconds"?: number };
export type FederationIssuedInvitation = { "id": string; "name": string; "labels": Record<string, string>; "expires_at": string; "created_at": string; "consumed_at"?: string; "consumed_by"?: string; "revoked_at"?: string; "state": "pending" | "consumed" | "expired" | "revoked"; "secret": string; "ca_sha256": string; "authority_url": string; "authority_cert_sha256": string };
export type RotateFederationRecoveryKey = { "rotation": FederationRecoveryRotation; "authority_certificate": FederationAuthorityCertificate };
export type FederationRecoveryTemplate = { "federation_id": string; "generation": number; "previous_key": string; "authority": { "epoch": number; "instance_id": string; "public_key": string; "certified_by": "recovery"; "signature"?: string } };
export type FederationRecoveryRotationResult = { "status": FederationStatus; "recovery_public_key": string };
export type FederationDocumentMember = { "instance_id": string; "name": string; "status": "active" | "suspended" | "removed"; "api_url": string; "federation_url": string; "cert_sha256": string; "cert_serial": string; "previous_cert_sha256"?: string; "previous_cert_serial"?: string; "previous_cert_until"?: string; "signing_key": string; "labels": Record<string, string>; "software_version": string; "protocol": FederationProtocolRange; "joined_at": string };
export type FederationAuthorityCertificate = { "epoch": number; "instance_id": string; "public_key": string; "certified_by": "recovery" | "previous_epoch"; "signature": string; "acceptance"?: string };
export type FederationRecoveryRotation = { "previous_key": string; "new_key": string; "generation": number; "signature": string };
export type FederationSignature = { "key_id": string; "alg": string; "value": string };
export type FederationMembershipDocument = { "kind": "membership"; "federation_id": string; "federation_name": string; "generation": number; "authority_epoch": number; "prev_hash": string; "protocol": FederationProtocolRange; "authority": FederationAuthorityCertificate; "recovery_public_key": string; "recovery_rotation"?: FederationRecoveryRotation; "ca_certificates": Array<string>; "members": Array<FederationDocumentMember>; "revoked_cert_serials": Array<string>; "issued_at": string; "issued_by": string; "signature"?: FederationSignature };
export type FederationStoredDocument = { "kind": "membership" | "policy" | "topology"; "generation": number; "hash": string; "received_at": string; "applied": boolean; "document": unknown };
export type TransferExportRequest = { "feeds"?: Array<string>; "namespaces"?: Array<string>; "include_content"?: boolean };
export type TransferDocument = { "format_version": number; "created_at"?: string; "selection"?: {  }; "sources": Array<{  }>; "feeds": Array<{  }>; "components": Array<{  }>; "skipped_content"?: Array<string> };
export type TransferImportRequest = { "document": TransferDocument; "dry_run"?: boolean };
export type TransferItem = { "kind": string; "name": string; "action": "created" | "updated" | "unchanged" | "skipped" | "conflict"; "reason"?: string };
export type TransferUnmapped = { "kind": string; "name": string; "reason": string };
export type TransferFailure = { "digest": string; "reason": string };
export type TransferReport = { "dry_run": boolean; "summary": Record<string, Record<string, number>>; "items": Array<TransferItem>; "items_truncated"?: boolean; "unmapped": Array<TransferUnmapped>; "integrity_failures": Array<TransferFailure> };
export type MigrationRepository = { "name": string; "type": string; "format": string; "ant_format"?: string; "mapped_to": string; "assets"?: number; "bytes"?: number };
export type MigrationReport = { "dry_run": boolean; "summary": Record<string, Record<string, number>>; "items": Array<TransferItem>; "items_truncated"?: boolean; "unmapped": Array<TransferUnmapped>; "integrity_failures": Array<TransferFailure>; "manager": string; "repositories": Array<MigrationRepository> };
export type NexusMigrationRequest = { "url": string; "username"?: string; "password"?: string; "token"?: string; "repositories"?: Array<string>; "count_assets"?: boolean; "dry_run"?: boolean };
export type MigrationCompareItem = { "repository": string; "path": string; "feed"?: string };
export type MigrationCompareRequest = { "url": string; "username"?: string; "password"?: string; "token"?: string; "items": Array<MigrationCompareItem> };
export type MigrationCompareSide = { "found": boolean; "sha256"?: string; "size"?: number; "detail"?: string };
export type MigrationCompareResult = { "repository": string; "path": string; "feed": string; "status": "identical" | "differs" | "missing_in_ant" | "missing_in_source" | "missing_in_both" | "error"; "source": MigrationCompareSide; "ant": MigrationCompareSide };
export type MigrationCompareReport = { "manager": string; "counts": Record<string, number>; "results": Array<MigrationCompareResult> };
export type TransferRun = { "id": string; "kind": "export" | "import" | "migration" | "compare"; "dry_run": boolean; "summary": {  }; "created_at": string };
export type TransferRunPage = { "items": Array<TransferRun> };
export type FederationIdentityDomain = { "id": string; "type": "oidc" | "saml" | "ldap"; "issuer": string };
export type FederationPolicyGrant = { "kind": "artifact" | "feed" | "instance"; "effect": "allow" | "deny"; "feed_key"?: string; "namespace"?: string; "actions": Array<"read" | "publish" | "delete" | "configure" | "replicate" | "admin"> };
export type FederationPolicyRole = { "key": string; "revision": number; "grants": Array<FederationPolicyGrant> };
export type FederationPrincipalBinding = { "domain": string; "subject": string; "roles": Array<string> };
export type FederationGroupBinding = { "domain": string; "group_id": string; "roles": Array<string> };
export type FederationServiceAccount = { "name": string; "roles": Array<string> };
export type FederationRevocation = { "domain"?: string; "subject"?: string; "service_account"?: string; "effective_at"?: string; "reason"?: string };
export type FederationMemberOverride = { "accept_admin_grants": boolean };
export type FederationPolicyBody = { "identity_domains"?: Array<FederationIdentityDomain>; "roles"?: Array<FederationPolicyRole>; "principal_bindings"?: Array<FederationPrincipalBinding>; "group_bindings"?: Array<FederationGroupBinding>; "service_accounts"?: Array<FederationServiceAccount>; "revocations"?: Array<FederationRevocation>; "member_overrides"?: Record<string, FederationMemberOverride> };
export type FederationPolicyDocument = { "kind": "policy"; "federation_id": string; "federation_name": string; "generation": number; "authority_epoch": number; "prev_hash": string; "identity_domains": Array<FederationIdentityDomain>; "roles": Array<FederationPolicyRole>; "principal_bindings": Array<FederationPrincipalBinding>; "group_bindings": Array<FederationGroupBinding>; "service_accounts": Array<FederationServiceAccount>; "revocations": Array<FederationRevocation>; "member_overrides": Record<string, FederationMemberOverride>; "issued_at": string; "issued_by": string; "signature"?: FederationSignature };
export type FederationPrincipalConflict = { "domain": string; "subject": string; "principal_id": string; "reason": "conflict_local_principal" };
export type FederationPolicyReport = { "roles": number; "grants": number; "principals": number; "revoked": number; "unresolved_feed_keys": Array<string>; "inert_admin_grants": number; "conflicts": Array<FederationPrincipalConflict> };
export type FederationPolicyStatus = { "generation": number; "hash"?: string; "applied_at"?: string; "confirmed_at"?: string; "age_seconds": number; "staleness": "fresh" | "warning" | "critical"; "error"?: string; "error_at"?: string; "report": FederationPolicyReport };
export type FederationAlert = { "code": "policy_stale" | "policy_rejected" | "membership_stale" | "policy_conflicts" | "topology_rejected" | "feed_home_conflict" | "authority_fenced" | "handover_pending" | "recovery_key_unverified" | "member_unreachable"; "severity": "warning" | "critical"; "message": string };
export type FederationSettings = { "accept_admin_grants": boolean; "policy_max_age_seconds"?: number };
export type FederationPolicyHistory = { "generation": number; "hash": string; "issued_at": string; "issued_by": string; "received_at": string };
export type FederationPolicyView = { "status": FederationPolicyStatus; "document"?: FederationPolicyDocument; "has_draft": boolean; "settings": FederationSettings; "history": Array<FederationPolicyHistory> };
export type FederationSectionDiff = { "added": Array<string>; "removed": Array<string>; "changed": Array<string> };
export type FederationPolicyDiff = { "from_generation": number; "to_generation": number; "identity_domains": FederationSectionDiff; "roles": FederationSectionDiff; "principal_bindings": FederationSectionDiff; "group_bindings": FederationSectionDiff; "service_accounts": FederationSectionDiff; "revocations": FederationSectionDiff; "changes": number };
export type FederationPolicyDraft = { "policy": FederationPolicyBody; "base_generation": number; "revision": number; "updated_at": string; "diff": FederationPolicyDiff };
export type SaveFederationPolicyDraft = { "policy": FederationPolicyBody };
export type PublishFederationPolicy = { "base_generation": number; "confirm": boolean; "policy"?: FederationPolicyBody };
export type FederationPolicyPublishResult = { "generation": number; "hash": string; "report": FederationPolicyReport };
export type FederationFeedBinding = { "feed_key": string; "feed_id": string; "feed_slug": string; "created_at": string };
export type FederationFeedBindingList = { "items": Array<FederationFeedBinding> };
export type BindFederationFeed = { "feed_key": string; "feed_id": string };
export type FederatedPrincipal = { "id": string; "kind": string; "issuer": string; "subject": string; "origin": "local" | "federation"; "federated_id"?: string; "disabled": boolean; "roles": Array<string>; "local_roles": Array<string>; "conflict"?: string };
export type FederatedPrincipalList = { "items": Array<FederatedPrincipal> };
export type AdoptFederatedPrincipal = { "confirm": boolean };
export type FederationAccessCheck = { "member_id": string; "kind": "artifact" | "feed" | "instance"; "feed_key"?: string; "namespace"?: string; "action": "read" | "publish" | "delete" | "configure" | "replicate" | "admin" };
export type FederationSearchCopy = { "member": string; "member_name": string; "origin": "local" | "remote" | "replica"; "home": boolean; "api_url": string };
export type FederationSearchHit = { "member": string; "member_name": string; "feed_key": string; "feed_slug": string; "format": string; "coordinate": string; "namespace": string; "origin": "local" | "remote" | "replica"; "home": boolean; "api_url": string; "metadata"?: {  }; "copies": Array<FederationSearchCopy> };
export type FederationSearchMember = { "id": string; "name": string; "status": "ok" | "timeout" | "error" | "skipped"; "code"?: string; "took_ms": number; "hits": number; "cached"?: boolean };
export type FederationSearchPage = { "items": Array<FederationSearchHit>; "members": Array<FederationSearchMember>; "partial": boolean; "page": PageInfo };
export type FederationAccessCheckResult = { "member_id": string; "allowed": boolean; "reason"?: string; "policy_generation": number; "federated_id"?: string };
export type EffectiveRole = { "name": string; "origin": "local" | "federation" };
export type FederationFeedDescriptor = { "format": string; "anonymous_read": boolean };
export type FederationFeedTransfer = { "id": string; "from": string; "to": string; "mode": "planned" | "forced"; "state": "freeze" | "drain" | "promote" | "complete" | "aborted"; "head": number; "checkpoint": number; "baseline": number; "started_at": string; "deadline"?: string; "updated_at": string; "reason"?: string };
export type FederationFeedRouting = { "mode": "proxy" | "redirect" | "off"; "blob_urls": boolean; "blob_url_bind_client"?: boolean; "cache": boolean; "cache_ttl_seconds"?: number; "max_lag_seconds"?: number; "prefer"?: Array<string> };
export type FederationRouteCandidate = { "member_id": string; "name": string; "role": "home" | "replica"; "api_url": string; "replica_state"?: string; "lag_seconds"?: number; "circuit": "closed" | "open" | "half_open"; "consecutive_failures": number; "rtt_ms"?: number; "preference": number; "eligible": boolean; "order": number; "reason": string };
export type FederationRoutePlan = { "feed_key": string; "format": string; "home": string; "local_role": "home" | "replica" | "unbound"; "local_feed_id"?: string; "routing": FederationFeedRouting; "decision": "local" | "proxy" | "redirect" | "off" | "no_candidates" | "unavailable"; "reason": string; "candidates": Array<FederationRouteCandidate> };
export type FederationRuleTargets = { "labels"?: Record<string, string>; "members"?: Array<string> };
export type FederationRuleFilters = { "namespaces"?: Array<string>; "packages"?: Array<string>; "version_prefixes"?: Array<string> };
export type FederationReplicationRule = { "id": string; "feed_key": string; "targets": FederationRuleTargets; "mode": "continuous" | "scheduled" | "manual"; "interval_seconds": number; "include_cached": boolean; "filters": FederationRuleFilters; "deletion_policy": "ignore" | "propagate"; "destination_read_only": boolean; "create_destination_feed": boolean; "adopt_existing": boolean; "max_lag_seconds": number; "revision": number };
export type FederationReplicationRuleList = { "items": Array<FederationReplicationRule> };
export type FederationTopologyFeed = { "key": string; "home": string; "descriptor": FederationFeedDescriptor; "revision": number; "transfer"?: FederationFeedTransfer; "routing"?: FederationFeedRouting };
export type FederationTopologyFeedView = { "key": string; "home": string; "descriptor": FederationFeedDescriptor; "revision": number; "transfer"?: FederationFeedTransfer; "routing"?: FederationFeedRouting; "home_name": string; "local_feed_id"?: string; "local_role": "home" | "replica" | "unbound"; "frozen": boolean };
export type FederationTopologyView = { "generation": number; "hash"?: string; "applied_at"?: string; "error"?: string; "authority": boolean; "feeds": Array<FederationTopologyFeedView>; "rules": Array<FederationReplicationRule> };
export type FederationTopologyStatus = { "generation": number; "hash"?: string; "applied_at"?: string; "age_seconds": number; "error"?: string; "feeds": number; "rules": number; "home_feeds": number; "rules_targeting_self": number; "active_transfers": number; "unresolved_quarantines": number };
export type CreateFederationFeed = { "feed_key": string; "home_member_id": string; "descriptor"?: FederationFeedDescriptor; "routing"?: FederationFeedRouting };
export type CreateFederationRule = { "feed_key": string; "targets": FederationRuleTargets; "mode"?: "continuous" | "scheduled" | "manual"; "interval_seconds"?: number; "include_cached"?: boolean; "filters"?: FederationRuleFilters; "deletion_policy"?: "ignore" | "propagate"; "destination_read_only"?: boolean; "create_destination_feed"?: boolean; "adopt_existing"?: boolean; "max_lag_seconds"?: number };
export type PatchFederationRule = { "revision"?: number; "targets"?: FederationRuleTargets; "mode"?: "continuous" | "scheduled" | "manual"; "interval_seconds"?: number; "include_cached"?: boolean; "filters"?: FederationRuleFilters; "deletion_policy"?: "ignore" | "propagate"; "destination_read_only"?: boolean; "create_destination_feed"?: boolean; "adopt_existing"?: boolean; "max_lag_seconds"?: number };
export type StartFederationTransfer = { "to_member_id": string; "mode"?: "planned" | "forced"; "confirm": boolean; "accept_data_loss"?: boolean; "drain_timeout_seconds"?: number };
export type AbortFederationTransfer = { "confirm": boolean; "reason"?: string };
export type FederationRuleStatus = { "rule_id": string; "feed_key": string; "member_id": string; "member_name"?: string; "state": "in_sync" | "lagging" | "seeding" | "failed" | "pending" | "missing_feed" | "conflict" | "disabled" | "unknown" | "ready"; "detail"?: string; "job_id"?: string; "adopted": boolean; "phase"?: string; "checkpoint": number; "changes_behind"?: number; "lag_seconds"?: number; "last_success_at"?: string; "consecutive_failures": number; "updated_at"?: string };
export type FederationRuleCompliance = { "rule": FederationReplicationRule; "home": string; "members": Array<FederationRuleStatus>; "summary": Record<string, number> };
export type FederationQuarantine = { "id": string; "feed_key": string; "transfer_id"?: string; "feed_id"?: string; "source_id": string; "source_name": string; "divergent_changes"?: number; "created_at": string; "resolved_at"?: string };
export type FederationQuarantineList = { "items": Array<FederationQuarantine> };
export type FederationReplicationStatus = { "items": Array<FederationRuleStatus>; "quarantines": Array<FederationQuarantine> };
export type AdoptFederationReplicationJob = { "job_id": string; "rule_id": string };
export type FederationAdoptResult = { "job_id": string; "rule_id": string; "feed_key": string; "phase": string; "checkpoint": number; "previous_peer_id": string; "peer_id": string };
export type ResolveFederationQuarantine = { "confirm": boolean };
export type FederationEpochRecord = { "epoch": number; "instance_id": string; "name"?: string; "certified_by": "recovery" | "previous_epoch"; "from_generation": number; "key_fingerprint": string; "applied_at"?: string };
export type FederationHandoverStatus = { "to_member_id": string; "to_name"?: string; "started_at": string; "base_generation": number; "expires_at": string };
export type FederationFenceStatus = { "at": string; "by_epoch": number };
export type FederationRecoveryDrill = { "verified_at"?: string; "due": boolean };
export type FederationAuthorityStatus = { "role": "member" | "authority"; "epoch": number; "authority_instance_id": string; "authority_name"?: string; "key_fingerprint": string; "recovery_key_fingerprint": string; "epochs": Array<FederationEpochRecord>; "handover"?: FederationHandoverStatus; "fenced"?: FederationFenceStatus; "recovery": FederationRecoveryDrill; "pending_authority_key": boolean };
export type HandoverFederationAuthority = { "to_member_id": string; "confirm": boolean };
export type FederationHandoverResult = { "status": FederationStatus; "epoch": number; "applied": boolean };
export type RecoverFederationAuthority = { "authority_certificate": FederationAuthorityCertificate; "remove_previous_authority"?: boolean; "confirm": boolean };
export type FederationRecoverResult = { "status": FederationStatus; "epoch": number; "ca_sha256": string; "previous_authority_removed": boolean };
export type FederationRecoveryChallenge = { "federation_id": string; "challenge": string; "expires_at": string; "recovery_public_key": string };
export type VerifyFederationRecoveryKey = { "challenge": string; "signature": string };
export type FederationAuditEvent = { "member": string; "member_name": string; "id": number; "created_at": string; "principal_id"?: string; "action": string; "resource_id"?: string; "result": "success" | "denied" | "failure"; "details"?: {  } };
export type FederationAuditMember = { "id": string; "name": string; "status": "ok" | "forbidden" | "timeout" | "error" | "skipped"; "code"?: string; "took_ms": number; "events": number };
export type FederationAuditPage = { "items": Array<FederationAuditEvent>; "members": Array<FederationAuditMember>; "partial": boolean; "page": PageInfo };
export type LocalUser = { "username": string; "principal_id": string; "break_glass": boolean; "totp_enabled": boolean; "locked_until"?: string; "last_login_at"?: string };
export type LocalUserList = { "items": Array<LocalUser> };
export type LocalUserInput = { "username": string; "password": string };
export type LocalUserPassword = { "password": string };
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
deleteSource: { body: undefined; response: SourceDeleted };
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
deletePackageAsset: { body: undefined; response: PackageMutation };
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
getDiagnostics: { body: undefined; response: undefined };
getReplicationExportTemplate: { body: undefined; response: ReplicationTemplate };
previewReplicationTemplate: { body: ReplicationTemplateRequest; response: ReplicationTemplatePreview };
applyReplicationTemplate: { body: ReplicationTemplateApplyRequest; response: ReplicationTemplateApplyResult };
deletePackageVersion: { body: undefined; response: PackageMutation };
getDeletePolicy: { body: undefined; response: DeletePolicy };
setDeletePolicy: { body: DeletePolicy; response: DeletePolicy };
getLimits: { body: undefined; response: LimitsConfig };
putLimits: { body: PutLimits; response: LimitsConfig };
getLimitsUsage: { body: undefined; response: LimitsUsage };
listPolicyTemplates: { body: undefined; response: PolicyTemplateList };
createPolicyTemplate: { body: PolicyTemplateInput; response: PolicyTemplate };
getPolicyTemplate: { body: undefined; response: PolicyTemplate };
putPolicyTemplate: { body: PolicyTemplateInput; response: PolicyTemplate };
deletePolicyTemplate: { body: undefined; response: undefined };
getFeedPolicyTemplates: { body: undefined; response: FeedPolicyBinding };
putFeedPolicyTemplates: { body: PutFeedPolicyBinding; response: FeedPolicyBinding };
getEffectivePolicy: { body: undefined; response: EffectivePolicy };
listIdentityProviders: { body: undefined; response: IdentityProviderList };
createIdentityProvider: { body: IdentityProviderInput; response: IdentityProvider };
testIdentityProvider: { body: IdentityProviderTestRequest; response: IdentityProviderTest };
importSamlMetadata: { body: SamlMetadataImportRequest; response: SamlMetadataImport };
getIdentityProvider: { body: undefined; response: IdentityProvider };
updateIdentityProvider: { body: UpdateIdentityProvider; response: IdentityProvider };
deleteIdentityProvider: { body: undefined; response: undefined };
enableIdentityProvider: { body: undefined; response: IdentityProvider };
disableIdentityProvider: { body: undefined; response: IdentityProvider };
getSamlSpMetadata: { body: undefined; response: undefined };
previewIdentityMapping: { body: MappingPreviewRequest; response: MappingPreview };
getIdentityPolicy: { body: undefined; response: IdentityPolicy };
putIdentityPolicy: { body: IdentityPolicy; response: IdentityPolicy };
getLocalAccount: { body: undefined; response: LocalAccount };
unlockLocalAccount: { body: undefined; response: undefined };
beginLocalTotp: { body: undefined; response: TotpEnrollment };
disableLocalTotp: { body: undefined; response: undefined };
confirmLocalTotp: { body: TotpConfirm; response: undefined };
ingestSecurityResult: { body: SecurityResultInput; response: SecurityResult };
getClientAuthPolicy: { body: undefined; response: ClientAuthPolicy };
putClientAuthPolicy: { body: PutClientAuthPolicy; response: ClientAuthPolicy };
getFeedClientAuthPolicy: { body: undefined; response: ClientAuthPolicy };
putFeedClientAuthPolicy: { body: PutClientAuthPolicy; response: ClientAuthPolicy };
deleteFeedClientAuthPolicy: { body: undefined; response: undefined };
listNetworkGroups: { body: undefined; response: NetworkGroupList };
createNetworkGroup: { body: NetworkGroupInput; response: NetworkGroup };
putNetworkGroup: { body: NetworkGroupInput; response: NetworkGroup };
deleteNetworkGroup: { body: undefined; response: undefined };
getGitHubOIDCConfig: { body: undefined; response: GitHubOIDCConfig };
putGitHubOIDCConfig: { body: PutGitHubOIDCConfig; response: GitHubOIDCConfig };
listGitHubTrustRules: { body: undefined; response: GitHubTrustRuleList };
createGitHubTrustRule: { body: GitHubTrustRuleInput; response: GitHubTrustRule };
putGitHubTrustRule: { body: GitHubTrustRuleInput; response: GitHubTrustRule };
deleteGitHubTrustRule: { body: undefined; response: undefined };
exchangeGitHubOIDCToken: { body: GitHubExchangeRequest; response: GitHubExchangeResponse };
listPersonalAccessTokens: { body: undefined; response: PersonalAccessTokenList };
createPersonalAccessToken: { body: CreatePersonalAccessToken; response: CreatedPersonalAccessToken };
revokePersonalAccessToken: { body: undefined; response: undefined };
foundFederation: { body: FoundFederation; response: FederationFoundResult };
getFederationStatus: { body: undefined; response: FederationStatus };
joinFederation: { body: JoinFederation; response: FederationStatus };
leaveFederation: { body: LeaveFederation; response: FederationStatus };
listFederationMembers: { body: undefined; response: FederationMemberList };
getFederationMember: { body: undefined; response: FederationMember };
removeFederationMember: { body: undefined; response: FederationMember };
suspendFederationMember: { body: undefined; response: FederationMember };
resumeFederationMember: { body: undefined; response: FederationMember };
listFederationInvitations: { body: undefined; response: FederationInvitationList };
createFederationInvitation: { body: CreateFederationInvitation; response: FederationIssuedInvitation };
revokeFederationInvitation: { body: undefined; response: undefined };
getFederationDocument: { body: undefined; response: FederationStoredDocument };
getFederationDocumentGeneration: { body: undefined; response: FederationStoredDocument };
getFederationRecoveryTemplate: { body: undefined; response: FederationRecoveryTemplate };
rotateFederationRecoveryKey: { body: RotateFederationRecoveryKey; response: FederationRecoveryRotationResult };
exportTransfer: { body: TransferExportRequest; response: TransferDocument };
importTransfer: { body: TransferImportRequest; response: TransferReport };
listTransferRuns: { body: undefined; response: TransferRunPage };
migrateNexus: { body: NexusMigrationRequest; response: MigrationReport };
compareMigration: { body: MigrationCompareRequest; response: MigrationCompareReport };
getFederationPolicy: { body: undefined; response: FederationPolicyView };
getFederationPolicyDraft: { body: undefined; response: FederationPolicyDraft };
saveFederationPolicyDraft: { body: SaveFederationPolicyDraft; response: FederationPolicyDraft };
publishFederationPolicy: { body: PublishFederationPolicy; response: FederationPolicyPublishResult };
diffFederationPolicy: { body: undefined; response: FederationPolicyDiff };
listFederationFeedBindings: { body: undefined; response: FederationFeedBindingList };
bindFederationFeed: { body: BindFederationFeed; response: FederationFeedBinding };
unbindFederationFeed: { body: undefined; response: undefined };
listFederatedPrincipals: { body: undefined; response: FederatedPrincipalList };
adoptFederatedPrincipal: { body: AdoptFederatedPrincipal; response: FederationPolicyReport };
getFederationSettings: { body: undefined; response: FederationSettings };
setFederationSettings: { body: FederationSettings; response: FederationSettings };
federatedSearch: { body: undefined; response: FederationSearchPage };
checkFederationAccess: { body: FederationAccessCheck; response: FederationAccessCheckResult };
getFederationTopology: { body: undefined; response: FederationTopologyView };
createFederationFeed: { body: CreateFederationFeed; response: FederationTopologyFeed };
deleteFederationFeed: { body: undefined; response: undefined };
startFederationFeedTransfer: { body: StartFederationTransfer; response: FederationFeedTransfer };
abortFederationFeedTransfer: { body: AbortFederationTransfer; response: FederationFeedTransfer };
setFederationFeedRouting: { body: FederationFeedRouting; response: FederationTopologyFeed };
explainFederationRoute: { body: undefined; response: FederationRoutePlan };
listFederationReplicationRules: { body: undefined; response: FederationReplicationRuleList };
createFederationReplicationRule: { body: CreateFederationRule; response: FederationReplicationRule };
getFederationReplicationRule: { body: undefined; response: FederationReplicationRule };
updateFederationReplicationRule: { body: PatchFederationRule; response: FederationReplicationRule };
deleteFederationReplicationRule: { body: undefined; response: undefined };
getFederationReplicationRuleCompliance: { body: undefined; response: FederationRuleCompliance };
getFederationReplicationStatus: { body: undefined; response: FederationReplicationStatus };
adoptFederationReplicationJob: { body: AdoptFederationReplicationJob; response: FederationAdoptResult };
listFederationQuarantines: { body: undefined; response: FederationQuarantineList };
resolveFederationQuarantine: { body: ResolveFederationQuarantine; response: FederationQuarantine };
getFederationAuthority: { body: undefined; response: FederationAuthorityStatus };
handoverFederationAuthority: { body: HandoverFederationAuthority; response: FederationHandoverResult };
abortFederationAuthorityHandover: { body: undefined; response: undefined };
createFederationRecoveryEpochTemplate: { body: undefined; response: FederationRecoveryTemplate };
recoverFederationAuthority: { body: RecoverFederationAuthority; response: FederationRecoverResult };
createFederationRecoveryChallenge: { body: undefined; response: FederationRecoveryChallenge };
verifyFederationRecoveryKey: { body: VerifyFederationRecoveryKey; response: FederationAuthorityStatus };
federatedAudit: { body: undefined; response: FederationAuditPage };
listLocalUsers: { body: undefined; response: LocalUserList };
createLocalUser: { body: LocalUserInput; response: LocalUser };
setLocalUserPassword: { body: LocalUserPassword; response: undefined };
deleteLocalUser: { body: undefined; response: undefined };
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
  "deleteSource": {
    "path": "/api/v1/sources/{id}",
    "method": "DELETE",
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
  "deletePackageAsset": {
    "path": "/api/v1/feeds/{id}/assets/{asset}",
    "method": "DELETE",
    "idempotent": true
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
    "idempotent": true
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
    "idempotent": true
  },
  "getStorageJob": {
    "path": "/api/v1/storage/migrations/{id}",
    "method": "GET",
    "idempotent": false
  },
  "storageJobAction": {
    "path": "/api/v1/storage/migrations/{id}/{action}",
    "method": "POST",
    "idempotent": true
  },
  "getDiagnostics": {
    "path": "/api/v1/diagnostics",
    "method": "GET",
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
  },
  "deletePackageVersion": {
    "path": "/api/v1/feeds/{id}/packages/{component}/versions/{version}",
    "method": "DELETE",
    "idempotent": true
  },
  "getDeletePolicy": {
    "path": "/api/v1/feeds/{id}/delete-policy",
    "method": "GET",
    "idempotent": false
  },
  "setDeletePolicy": {
    "path": "/api/v1/feeds/{id}/delete-policy",
    "method": "PUT",
    "idempotent": true
  },
  "getLimits": {
    "path": "/api/v1/limits",
    "method": "GET",
    "idempotent": false
  },
  "putLimits": {
    "path": "/api/v1/limits",
    "method": "PUT",
    "idempotent": true
  },
  "getLimitsUsage": {
    "path": "/api/v1/limits/usage",
    "method": "GET",
    "idempotent": false
  },
  "listPolicyTemplates": {
    "path": "/api/v1/policy-templates",
    "method": "GET",
    "idempotent": false
  },
  "createPolicyTemplate": {
    "path": "/api/v1/policy-templates",
    "method": "POST",
    "idempotent": true
  },
  "getPolicyTemplate": {
    "path": "/api/v1/policy-templates/{id}",
    "method": "GET",
    "idempotent": false
  },
  "putPolicyTemplate": {
    "path": "/api/v1/policy-templates/{id}",
    "method": "PUT",
    "idempotent": true
  },
  "deletePolicyTemplate": {
    "path": "/api/v1/policy-templates/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "getFeedPolicyTemplates": {
    "path": "/api/v1/feeds/{id}/policy-templates",
    "method": "GET",
    "idempotent": false
  },
  "putFeedPolicyTemplates": {
    "path": "/api/v1/feeds/{id}/policy-templates",
    "method": "PUT",
    "idempotent": true
  },
  "getEffectivePolicy": {
    "path": "/api/v1/feeds/{id}/effective-policy",
    "method": "GET",
    "idempotent": false
  },
  "listIdentityProviders": {
    "path": "/api/v1/identity-providers",
    "method": "GET",
    "idempotent": false
  },
  "createIdentityProvider": {
    "path": "/api/v1/identity-providers",
    "method": "POST",
    "idempotent": true
  },
  "testIdentityProvider": {
    "path": "/api/v1/identity-providers/test",
    "method": "POST",
    "idempotent": false
  },
  "importSamlMetadata": {
    "path": "/api/v1/identity-providers/saml/import-metadata",
    "method": "POST",
    "idempotent": false
  },
  "getIdentityProvider": {
    "path": "/api/v1/identity-providers/{id}",
    "method": "GET",
    "idempotent": false
  },
  "updateIdentityProvider": {
    "path": "/api/v1/identity-providers/{id}",
    "method": "PUT",
    "idempotent": true
  },
  "deleteIdentityProvider": {
    "path": "/api/v1/identity-providers/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "enableIdentityProvider": {
    "path": "/api/v1/identity-providers/{id}/enable",
    "method": "POST",
    "idempotent": true
  },
  "disableIdentityProvider": {
    "path": "/api/v1/identity-providers/{id}/disable",
    "method": "POST",
    "idempotent": true
  },
  "getSamlSpMetadata": {
    "path": "/api/v1/identity-providers/{id}/metadata",
    "method": "GET",
    "idempotent": false
  },
  "previewIdentityMapping": {
    "path": "/api/v1/identity-providers/{id}/preview",
    "method": "POST",
    "idempotent": false
  },
  "getIdentityPolicy": {
    "path": "/api/v1/identity-policy",
    "method": "GET",
    "idempotent": false
  },
  "putIdentityPolicy": {
    "path": "/api/v1/identity-policy",
    "method": "PUT",
    "idempotent": true
  },
  "getLocalAccount": {
    "path": "/api/v1/local-account",
    "method": "GET",
    "idempotent": false
  },
  "unlockLocalAccount": {
    "path": "/api/v1/local-account/unlock",
    "method": "POST",
    "idempotent": true
  },
  "beginLocalTotp": {
    "path": "/api/v1/local-account/totp",
    "method": "POST",
    "idempotent": true
  },
  "disableLocalTotp": {
    "path": "/api/v1/local-account/totp",
    "method": "DELETE",
    "idempotent": true
  },
  "confirmLocalTotp": {
    "path": "/api/v1/local-account/totp/confirm",
    "method": "POST",
    "idempotent": true
  },
  "ingestSecurityResult": {
    "path": "/api/v1/assets/{id}/security-results",
    "method": "POST",
    "idempotent": true
  },
  "getClientAuthPolicy": {
    "path": "/api/v1/auth/policy",
    "method": "GET",
    "idempotent": false
  },
  "putClientAuthPolicy": {
    "path": "/api/v1/auth/policy",
    "method": "PUT",
    "idempotent": true
  },
  "getFeedClientAuthPolicy": {
    "path": "/api/v1/feeds/{id}/auth-policy",
    "method": "GET",
    "idempotent": false
  },
  "putFeedClientAuthPolicy": {
    "path": "/api/v1/feeds/{id}/auth-policy",
    "method": "PUT",
    "idempotent": true
  },
  "deleteFeedClientAuthPolicy": {
    "path": "/api/v1/feeds/{id}/auth-policy",
    "method": "DELETE",
    "idempotent": true
  },
  "listNetworkGroups": {
    "path": "/api/v1/auth/network-groups",
    "method": "GET",
    "idempotent": false
  },
  "createNetworkGroup": {
    "path": "/api/v1/auth/network-groups",
    "method": "POST",
    "idempotent": true
  },
  "putNetworkGroup": {
    "path": "/api/v1/auth/network-groups/{id}",
    "method": "PUT",
    "idempotent": true
  },
  "deleteNetworkGroup": {
    "path": "/api/v1/auth/network-groups/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "getGitHubOIDCConfig": {
    "path": "/api/v1/auth/github-oidc/config",
    "method": "GET",
    "idempotent": false
  },
  "putGitHubOIDCConfig": {
    "path": "/api/v1/auth/github-oidc/config",
    "method": "PUT",
    "idempotent": true
  },
  "listGitHubTrustRules": {
    "path": "/api/v1/auth/github-oidc/trust-rules",
    "method": "GET",
    "idempotent": false
  },
  "createGitHubTrustRule": {
    "path": "/api/v1/auth/github-oidc/trust-rules",
    "method": "POST",
    "idempotent": true
  },
  "putGitHubTrustRule": {
    "path": "/api/v1/auth/github-oidc/trust-rules/{id}",
    "method": "PUT",
    "idempotent": true
  },
  "deleteGitHubTrustRule": {
    "path": "/api/v1/auth/github-oidc/trust-rules/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "exchangeGitHubOIDCToken": {
    "path": "/api/v1/auth/github-oidc/exchange",
    "method": "POST",
    "idempotent": false
  },
  "listPersonalAccessTokens": {
    "path": "/api/v1/auth/personal-access-tokens",
    "method": "GET",
    "idempotent": false
  },
  "createPersonalAccessToken": {
    "path": "/api/v1/auth/personal-access-tokens",
    "method": "POST",
    "idempotent": true
  },
  "revokePersonalAccessToken": {
    "path": "/api/v1/auth/personal-access-tokens/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "foundFederation": {
    "path": "/api/v1/federation",
    "method": "POST",
    "idempotent": false
  },
  "getFederationStatus": {
    "path": "/api/v1/federation/status",
    "method": "GET",
    "idempotent": false
  },
  "joinFederation": {
    "path": "/api/v1/federation/join",
    "method": "POST",
    "idempotent": false
  },
  "leaveFederation": {
    "path": "/api/v1/federation/leave",
    "method": "POST",
    "idempotent": false
  },
  "listFederationMembers": {
    "path": "/api/v1/federation/members",
    "method": "GET",
    "idempotent": false
  },
  "getFederationMember": {
    "path": "/api/v1/federation/members/{id}",
    "method": "GET",
    "idempotent": false
  },
  "removeFederationMember": {
    "path": "/api/v1/federation/members/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "suspendFederationMember": {
    "path": "/api/v1/federation/members/{id}/suspend",
    "method": "POST",
    "idempotent": true
  },
  "resumeFederationMember": {
    "path": "/api/v1/federation/members/{id}/resume",
    "method": "POST",
    "idempotent": true
  },
  "listFederationInvitations": {
    "path": "/api/v1/federation/invitations",
    "method": "GET",
    "idempotent": false
  },
  "createFederationInvitation": {
    "path": "/api/v1/federation/invitations",
    "method": "POST",
    "idempotent": false
  },
  "revokeFederationInvitation": {
    "path": "/api/v1/federation/invitations/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "getFederationDocument": {
    "path": "/api/v1/federation/documents/{kind}",
    "method": "GET",
    "idempotent": false
  },
  "getFederationDocumentGeneration": {
    "path": "/api/v1/federation/documents/{kind}/{generation}",
    "method": "GET",
    "idempotent": false
  },
  "getFederationRecoveryTemplate": {
    "path": "/api/v1/federation/recovery-key/rotation",
    "method": "GET",
    "idempotent": false
  },
  "rotateFederationRecoveryKey": {
    "path": "/api/v1/federation/recovery-key/rotate",
    "method": "POST",
    "idempotent": false
  },
  "exportTransfer": {
    "path": "/api/v1/transfer/export",
    "method": "POST",
    "idempotent": false
  },
  "importTransfer": {
    "path": "/api/v1/transfer/import",
    "method": "POST",
    "idempotent": false
  },
  "listTransferRuns": {
    "path": "/api/v1/transfer/runs",
    "method": "GET",
    "idempotent": false
  },
  "migrateNexus": {
    "path": "/api/v1/migrations/nexus",
    "method": "POST",
    "idempotent": false
  },
  "compareMigration": {
    "path": "/api/v1/migrations/compare",
    "method": "POST",
    "idempotent": false
  },
  "getFederationPolicy": {
    "path": "/api/v1/federation/policy",
    "method": "GET",
    "idempotent": false
  },
  "getFederationPolicyDraft": {
    "path": "/api/v1/federation/policy/draft",
    "method": "GET",
    "idempotent": false
  },
  "saveFederationPolicyDraft": {
    "path": "/api/v1/federation/policy/draft",
    "method": "PUT",
    "idempotent": true
  },
  "publishFederationPolicy": {
    "path": "/api/v1/federation/policy/publish",
    "method": "POST",
    "idempotent": true
  },
  "diffFederationPolicy": {
    "path": "/api/v1/federation/policy/diff",
    "method": "GET",
    "idempotent": false
  },
  "listFederationFeedBindings": {
    "path": "/api/v1/federation/feed-bindings",
    "method": "GET",
    "idempotent": false
  },
  "bindFederationFeed": {
    "path": "/api/v1/federation/feed-bindings",
    "method": "POST",
    "idempotent": true
  },
  "unbindFederationFeed": {
    "path": "/api/v1/federation/feed-bindings/{fed}/{slug}",
    "method": "DELETE",
    "idempotent": true
  },
  "listFederatedPrincipals": {
    "path": "/api/v1/federation/principals",
    "method": "GET",
    "idempotent": false
  },
  "adoptFederatedPrincipal": {
    "path": "/api/v1/federation/principals/{id}/adopt",
    "method": "POST",
    "idempotent": true
  },
  "getFederationSettings": {
    "path": "/api/v1/federation/settings",
    "method": "GET",
    "idempotent": false
  },
  "setFederationSettings": {
    "path": "/api/v1/federation/settings",
    "method": "PUT",
    "idempotent": true
  },
  "federatedSearch": {
    "path": "/api/v1/federation/search",
    "method": "GET",
    "idempotent": false
  },
  "checkFederationAccess": {
    "path": "/api/v1/federation/access-check",
    "method": "POST",
    "idempotent": false
  },
  "getFederationTopology": {
    "path": "/api/v1/federation/topology",
    "method": "GET",
    "idempotent": false
  },
  "createFederationFeed": {
    "path": "/api/v1/federation/feeds",
    "method": "POST",
    "idempotent": true
  },
  "deleteFederationFeed": {
    "path": "/api/v1/federation/feeds/{fed}/{slug}",
    "method": "DELETE",
    "idempotent": true
  },
  "startFederationFeedTransfer": {
    "path": "/api/v1/federation/feeds/{fed}/{slug}/transfer",
    "method": "POST",
    "idempotent": true
  },
  "abortFederationFeedTransfer": {
    "path": "/api/v1/federation/feeds/{fed}/{slug}/transfer/abort",
    "method": "POST",
    "idempotent": true
  },
  "setFederationFeedRouting": {
    "path": "/api/v1/federation/feeds/{fed}/{slug}/routing",
    "method": "PUT",
    "idempotent": true
  },
  "explainFederationRoute": {
    "path": "/api/v1/federation/routes/{fed}/{slug}",
    "method": "GET",
    "idempotent": false
  },
  "listFederationReplicationRules": {
    "path": "/api/v1/federation/replication/rules",
    "method": "GET",
    "idempotent": false
  },
  "createFederationReplicationRule": {
    "path": "/api/v1/federation/replication/rules",
    "method": "POST",
    "idempotent": true
  },
  "getFederationReplicationRule": {
    "path": "/api/v1/federation/replication/rules/{id}",
    "method": "GET",
    "idempotent": false
  },
  "updateFederationReplicationRule": {
    "path": "/api/v1/federation/replication/rules/{id}",
    "method": "PATCH",
    "idempotent": true
  },
  "deleteFederationReplicationRule": {
    "path": "/api/v1/federation/replication/rules/{id}",
    "method": "DELETE",
    "idempotent": true
  },
  "getFederationReplicationRuleCompliance": {
    "path": "/api/v1/federation/replication/rules/{id}/compliance",
    "method": "GET",
    "idempotent": false
  },
  "getFederationReplicationStatus": {
    "path": "/api/v1/federation/replication/status",
    "method": "GET",
    "idempotent": false
  },
  "adoptFederationReplicationJob": {
    "path": "/api/v1/federation/replication/adopt",
    "method": "POST",
    "idempotent": true
  },
  "listFederationQuarantines": {
    "path": "/api/v1/federation/quarantines",
    "method": "GET",
    "idempotent": false
  },
  "resolveFederationQuarantine": {
    "path": "/api/v1/federation/quarantines/{id}/resolve",
    "method": "POST",
    "idempotent": true
  },
  "getFederationAuthority": {
    "path": "/api/v1/federation/authority",
    "method": "GET",
    "idempotent": false
  },
  "handoverFederationAuthority": {
    "path": "/api/v1/federation/authority/handover",
    "method": "POST",
    "idempotent": false
  },
  "abortFederationAuthorityHandover": {
    "path": "/api/v1/federation/authority/handover/abort",
    "method": "POST",
    "idempotent": false
  },
  "createFederationRecoveryEpochTemplate": {
    "path": "/api/v1/federation/authority/recovery-template",
    "method": "POST",
    "idempotent": false
  },
  "recoverFederationAuthority": {
    "path": "/api/v1/federation/authority/recover",
    "method": "POST",
    "idempotent": false
  },
  "createFederationRecoveryChallenge": {
    "path": "/api/v1/federation/recovery-key/challenge",
    "method": "POST",
    "idempotent": false
  },
  "verifyFederationRecoveryKey": {
    "path": "/api/v1/federation/recovery-key/verify",
    "method": "POST",
    "idempotent": false
  },
  "federatedAudit": {
    "path": "/api/v1/federation/audit",
    "method": "GET",
    "idempotent": false
  },
  "listLocalUsers": {
    "path": "/api/v1/local-accounts",
    "method": "GET",
    "idempotent": false
  },
  "createLocalUser": {
    "path": "/api/v1/local-accounts",
    "method": "POST",
    "idempotent": true
  },
  "setLocalUserPassword": {
    "path": "/api/v1/local-accounts/{username}/password",
    "method": "PUT",
    "idempotent": true
  },
  "deleteLocalUser": {
    "path": "/api/v1/local-accounts/{username}",
    "method": "DELETE",
    "idempotent": true
  }
} as const;
