package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/replication"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
)

// replRuntime gives a harness instance artifact storage, the writer lease,
// the replication service (attached to federation) and a publisher.
type replRuntime struct {
	stores    *storage.Registry
	mgr       *ownership.Manager
	svc       *replication.Service
	publisher *artifact.Service
	admin     identity.Principal
}

// enableReplication wires replication into an instance and remounts its
// federation listener so the export API is served over mTLS.
func (in *instance) enableReplication() {
	in.t.Helper()
	ctx := context.Background()
	stores, err := storage.New(storage.Config{Directory: in.t.TempDir()})
	if err != nil {
		in.t.Fatal(err)
	}
	in.t.Cleanup(stores.Close)
	var owner string
	if err := in.pool.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&owner); err != nil {
		in.t.Fatal(err)
	}
	mgr, err := ownership.Start(context.WithoutCancel(ctx), in.pool, in.id, owner, 30*time.Second)
	if err != nil {
		in.t.Fatal(err)
	}
	in.t.Cleanup(func() { _ = mgr.Close() })
	svc, err := replication.New(replication.Config{DB: in.pool, Keys: in.keys, InstanceID: in.id, Lease: mgr.Lease, Storage: stores.Resolve, PageSize: 50})
	if err != nil {
		in.t.Fatal(err)
	}
	exporter := replication.Exporter{DB: in.pool, Storage: stores.Resolve, Lease: mgr.Lease}
	svc.AttachFederation(in.svc, exporter)
	replication.RegisterExport(in.mux, in.pool, exporter)
	svc.Register(in.mux, identity.Tokens{DB: in.pool})
	admin, err := (identity.Tokens{DB: in.pool}).Authenticate(ctx, in.token)
	if err != nil {
		in.t.Fatal(err)
	}
	pol := artifact.Policies{DB: in.pool, InstanceID: in.id}
	in.repl = &replRuntime{stores: stores, mgr: mgr, svc: svc, admin: admin,
		publisher: &artifact.Service{DB: in.pool, Lease: mgr.Lease, Router: pol.Load, Storage: stores.Resolve}}
	in.stop()
	in.restart()
}

// localFeed creates a raw feed with one local source that is its default
// publish target.
func (in *instance) localFeed(slug string) (feed, source string) {
	in.t.Helper()
	ctx := context.Background()
	must := func(err error) {
		in.t.Helper()
		if err != nil {
			in.t.Fatal(err)
		}
	}
	must(in.pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,$2,'raw') RETURNING id::text`, in.id, slug).Scan(&feed))
	must(in.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,'local','raw') RETURNING id::text`, in.id, slug+"-local").Scan(&source))
	_, err := in.pool.Exec(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2)`, in.id, "source:"+source)
	must(err)
	_, err = in.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, in.id, feed, source)
	must(err)
	_, err = in.pool.Exec(ctx, `INSERT INTO publish_policies(instance_id,feed_id,default_source_id) VALUES($1,$2,$3)`, in.id, feed, source)
	must(err)
	return feed, source
}

func (in *instance) publish(feed, coord, version string) error {
	data := []byte(fmt.Sprintf("%s %s %s %s", in.name, feed, coord, version))
	sum := sha256.Sum256(data)
	_, err := in.repl.publisher.Publish(context.Background(), in.repl.admin, artifact.Publication{FeedID: feed, Coordinate: coord, Version: version,
		AssetKey: coord + "/" + version + "/pkg.bin", MediaType: "application/octet-stream", Info: blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(data))}}, bytes.NewReader(data))
	return err
}

func (in *instance) mustPublish(feed, coord, version string) {
	in.t.Helper()
	if err := in.publish(feed, coord, version); err != nil {
		in.t.Fatalf("%s publish %s@%s: %v", in.name, coord, version, err)
	}
}

// hasVersion reports whether a feed (any attached source) serves an asset.
func (in *instance) hasVersion(feed, coord, version string) bool {
	in.t.Helper()
	var n int
	if err := in.pool.QueryRow(context.Background(), `SELECT count(*) FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id JOIN versions v ON v.component_id=c.id JOIN assets a ON a.version_id=v.id
WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 AND v.version=$4`, in.id, feed, coord, version).Scan(&n); err != nil {
		in.t.Fatal(err)
	}
	return n > 0
}

func (in *instance) boundFeed(key string) string {
	in.t.Helper()
	var feed string
	_ = in.pool.QueryRow(context.Background(), `SELECT feed_id::text FROM federated_feed_bindings WHERE instance_id=$1 AND feed_key=$2`, in.id, key).Scan(&feed)
	return feed
}

func (in *instance) reconcile() {
	in.t.Helper()
	if err := in.svc.ReconcileOnce(context.Background()); err != nil {
		in.t.Logf("%s reconcile: %v", in.name, err)
	}
}

func (in *instance) sync() {
	in.t.Helper()
	if err := in.svc.SyncOnce(context.Background()); err != nil {
		in.t.Fatalf("%s sync: %v", in.name, err)
	}
}

func (in *instance) jobs() []replication.Job {
	in.t.Helper()
	var list struct {
		Items []replication.Job `json:"items"`
	}
	if code := in.call("GET", "/api/v1/replication/jobs", nil, &list); code != 200 {
		in.t.Fatalf("%s jobs: %d", in.name, code)
	}
	return list.Items
}

func (in *instance) jobFor(key string) replication.Job {
	in.t.Helper()
	for _, j := range in.jobs() {
		if j.FederationFeedKey == key {
			return j
		}
	}
	in.t.Fatalf("%s: no federation job for %s in %+v", in.name, key, in.jobs())
	return replication.Job{}
}

func (in *instance) job(id string) replication.Job {
	in.t.Helper()
	var j replication.Job
	if code := in.call("GET", "/api/v1/replication/jobs/"+id, nil, &j); code != 200 {
		in.t.Fatalf("%s job %s: %d", in.name, id, code)
	}
	return j
}

func (in *instance) runJob(id string) error {
	return in.repl.svc.RunNow(context.Background(), id)
}

func (in *instance) mustRun(id string) {
	in.t.Helper()
	if err := in.runJob(id); err != nil {
		in.t.Fatalf("%s run job %s: %v", in.name, id, err)
	}
}

func (in *instance) ruleState(rule string) federation.RuleStatus {
	in.t.Helper()
	var st struct {
		Items       []federation.RuleStatus `json:"items"`
		Quarantines []federation.Quarantine `json:"quarantines"`
	}
	if code := in.call("GET", "/api/v1/federation/replication/status", nil, &st); code != 200 {
		in.t.Fatalf("%s replication status: %d", in.name, code)
	}
	for _, s := range st.Items {
		if s.RuleID == rule {
			return s
		}
	}
	return federation.RuleStatus{State: "absent"}
}

func (in *instance) topology() federation.TopologyView {
	in.t.Helper()
	var v federation.TopologyView
	if code := in.call("GET", "/api/v1/federation/topology", nil, &v); code != 200 {
		in.t.Fatalf("%s topology: %d", in.name, code)
	}
	return v
}

func feedOf(v federation.TopologyView, key string) federation.TopologyFeedView {
	for _, f := range v.Feeds {
		if f.Key == key {
			return f
		}
	}
	return federation.TopologyFeedView{}
}

func TestFederationReplicationScenarios(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma := newInstance(t, "alpha"), newInstance(t, "beta"), newInstance(t, "gamma")
	for _, in := range []*instance{alpha, beta, gamma} {
		in.enableReplication()
	}
	var founded federation.FoundResult
	if code := alpha.call("POST", "/api/v1/federation", map[string]any{"name": "lab", "member_name": "alpha", "labels": map[string]string{"site": "alpha"}}, &founded); code != 201 {
		t.Fatalf("found: %d", code)
	}
	for _, in := range []*instance{beta, gamma} {
		if code := in.join(alpha, alpha.invite(in.name)); code != 200 {
			t.Fatalf("join %s: %d", in.name, code)
		}
	}
	beta.sync()
	gamma.sync()

	// --- feed key with its single home ---
	release, _ := alpha.localFeed("release")
	alpha.mustPublish(release, "app", "1.0")
	alpha.bind("lab/release", release)
	var created federation.TopologyFeed
	if code := alpha.call("POST", "/api/v1/federation/feeds", map[string]any{"feed_key": "lab/release", "home_member_id": alpha.id}, &created); code != 201 ||
		created.Home != alpha.id || created.Descriptor.Format != "raw" {
		t.Fatalf("create feed key: %d %+v", code, created)
	}
	// One home per feed key: a second claim is rejected.
	if code, ec := alpha.errorCode("POST", "/api/v1/federation/feeds", map[string]any{"feed_key": "lab/release", "home_member_id": beta.id, "descriptor": map[string]any{"format": "raw", "anonymous_read": false}}); code != 409 || ec != "feed_home_elsewhere" {
		t.Fatalf("second home claim: %d %s", code, ec)
	}
	// Topology is authority-only.
	if code, ec := beta.errorCode("POST", "/api/v1/federation/feeds", map[string]any{"feed_key": "lab/other", "home_member_id": beta.id, "descriptor": map[string]any{"format": "raw", "anonymous_read": false}}); code != 409 || ec != "not_authority" {
		t.Fatalf("member creates feed key: %d %s", code, ec)
	}

	// --- rule → job creation on labelled targets over mTLS ---
	var rule1 federation.ReplicationRule
	if code := alpha.call("POST", "/api/v1/federation/replication/rules", map[string]any{"feed_key": "lab/release", "targets": map[string]any{"labels": map[string]string{"site": "beta"}}}, &rule1); code != 201 {
		t.Fatalf("create rule: %d", code)
	}
	if code, _ := alpha.errorCode("POST", "/api/v1/federation/replication/rules", map[string]any{"feed_key": "lab/release", "targets": map[string]any{"members": []string{alpha.id}}}); code != 400 {
		t.Fatalf("rule targeting the home: %d", code)
	}
	beta.sync()
	gamma.sync()
	if tv := beta.topology(); feedOf(tv, "lab/release").Home != alpha.id || len(tv.Rules) != 1 {
		t.Fatalf("beta topology: %+v", tv)
	}
	beta.reconcile()
	gamma.reconcile()
	betaRelease := beta.boundFeed("lab/release")
	if betaRelease == "" {
		t.Fatal("beta did not create the destination feed from the descriptor")
	}
	if len(gamma.jobs()) != 0 || gamma.boundFeed("lab/release") != "" {
		t.Fatal("gamma is not a target but got a job")
	}
	bj := beta.jobFor("lab/release")
	if bj.Origin != "federation" || bj.FederationRuleID != rule1.ID || !bj.DestinationReadOnly || bj.DeletionPolicy != "ignore" || bj.Mode != "continuous" {
		t.Fatalf("beta job: %+v", bj)
	}
	var peers struct {
		Items []replication.Peer `json:"items"`
	}
	beta.call("GET", "/api/v1/replication/peers", nil, &peers)
	if len(peers.Items) != 1 || peers.Items[0].AuthMode != "federation" || peers.Items[0].MemberInstanceID != alpha.id {
		t.Fatalf("beta peers: %+v", peers.Items)
	}
	// Federation-managed jobs and peers are read-only locally.
	if code, ec := beta.errorCode("PATCH", "/api/v1/replication/jobs/"+bj.ID, map[string]any{"revision": bj.Revision, "enabled": false}); code != 409 || ec != "federation_managed" {
		t.Fatalf("local patch of managed job: %d %s", code, ec)
	}
	if code, ec := beta.errorCode("DELETE", "/api/v1/replication/peers/"+peers.Items[0].ID, nil); code != 409 || ec != "federation_managed" {
		t.Fatalf("local delete of managed peer: %d %s", code, ec)
	}
	beta.mustRun(bj.ID)
	if !beta.hasVersion(betaRelease, "app", "1.0") {
		t.Fatal("content not replicated over the federation listener")
	}
	if st := beta.ruleState(rule1.ID); st.State != "in_sync" || st.JobID != bj.ID {
		t.Fatalf("beta rule status: %+v", st)
	}
	var comp federation.RuleCompliance
	if code := alpha.call("GET", "/api/v1/federation/replication/rules/"+rule1.ID+"/compliance", nil, &comp); code != 200 || len(comp.Members) != 1 || comp.Members[0].State != "in_sync" || comp.Members[0].MemberName != "beta" {
		t.Fatalf("compliance: %d %+v", code, comp)
	}
	// A member that is not a rule target cannot read the home feed.
	gid, gbase, err := gamma.svc.ReplicationIdentity(alpha.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (peer.Client{Timeout: 5 * time.Second}).Do(ctx, gid, gbase, "GET", replication.FederationExportPrefix+release+"/head", nil, alpha.id); peerCode(err) != "forbidden" {
		t.Fatalf("non-target export: %v", err)
	}

	// --- rule edits propagate ---
	if code := alpha.call("PATCH", "/api/v1/federation/replication/rules/"+rule1.ID, map[string]any{"revision": 1, "interval_seconds": 60, "deletion_policy": "propagate"}, &rule1); code != 200 || rule1.Revision != 2 {
		t.Fatalf("patch rule: %d %+v", code, rule1)
	}
	if code, _ := alpha.errorCode("PATCH", "/api/v1/federation/replication/rules/"+rule1.ID, map[string]any{"revision": 1, "interval_seconds": 30}); code != 412 {
		t.Fatalf("stale rule revision: %d", code)
	}
	beta.sync()
	beta.reconcile()
	if j := beta.job(bj.ID); j.IntervalSeconds != 60 || j.DeletionPolicy != "propagate" {
		t.Fatalf("rule edit not applied: %+v", j)
	}
	alpha.mustPublish(release, "app", "1.1")
	beta.mustRun(bj.ID)
	if !beta.hasVersion(betaRelease, "app", "1.1") {
		t.Fatal("incremental change not replicated")
	}

	// --- partition: serving and publishing continue, replication resumes ---
	alpha.proxy.set(faultDrop)
	beta.proxy.set(faultDrop)
	alpha.mustPublish(release, "app", "1.2") // the home publishes while partitioned
	if err := beta.runJob(bj.ID); err == nil {
		t.Fatal("job ran across a partition")
	}
	if !beta.hasVersion(betaRelease, "app", "1.1") {
		t.Fatal("replica stopped serving during the partition")
	}
	if st := beta.ruleState(rule1.ID); st.State != "failed" {
		t.Fatalf("rule state during partition: %+v", st)
	}
	if code := alpha.call("GET", "/api/v1/federation/replication/rules/"+rule1.ID+"/compliance", nil, &comp); code != 200 || comp.Members[0].State != "unknown" {
		t.Fatalf("compliance during partition: %+v", comp)
	}
	alpha.proxy.set(faultPass)
	beta.proxy.set(faultPass)
	beta.mustRun(bj.ID)
	if !beta.hasVersion(betaRelease, "app", "1.2") || beta.ruleState(rule1.ID).State != "in_sync" {
		t.Fatal("replication did not resume from its checkpoint after the partition")
	}

	// --- adoption of an existing Phase 1 job without reseed ---
	tools, _ := alpha.localFeed("tools")
	alpha.mustPublish(tools, "cli", "1")
	alpha.bind("lab/tools", tools)
	if code := alpha.call("POST", "/api/v1/federation/feeds", map[string]any{"feed_key": "lab/tools", "home_member_id": alpha.id}, nil); code != 201 {
		t.Fatalf("create lab/tools: %d", code)
	}
	gammaTools := gamma.createFeed("tools")
	var tokenPeer replication.Peer
	if code := gamma.call("POST", "/api/v1/replication/peers", map[string]any{"name": "oke-prod", "url": alpha.api.URL, "token": alpha.token}, &tokenPeer); code != 201 {
		t.Fatalf("token peer: %d", code)
	}
	var phase1 replication.Job
	if code := gamma.call("POST", "/api/v1/replication/jobs", map[string]any{"name": "oke-tools", "peer_id": tokenPeer.ID, "remote_feed_id": tools, "dest_feed_id": gammaTools,
		"include_cached": false, "deletion_policy": "propagate", "mode": "continuous", "interval_seconds": 30}, &phase1); code != 201 {
		t.Fatalf("phase 1 job: %d", code)
	}
	gamma.mustRun(phase1.ID)
	alpha.mustPublish(tools, "cli", "2")
	gamma.mustRun(phase1.ID)
	before := gamma.job(phase1.ID)
	if before.Status.Phase != "incremental" || !gamma.hasVersion(gammaTools, "cli", "2") {
		t.Fatalf("phase 1 job not seeded: %+v", before.Status)
	}
	var rule2 federation.ReplicationRule
	if code := alpha.call("POST", "/api/v1/federation/replication/rules", map[string]any{"feed_key": "lab/tools", "targets": map[string]any{"members": []string{gamma.id}},
		"include_cached": false, "deletion_policy": "propagate", "interval_seconds": 30}, &rule2); code != 201 {
		t.Fatalf("rule2: %d", code)
	}
	gamma.sync()
	gamma.reconcile()
	// The existing feed is never taken over implicitly: it must be bound.
	if st := gamma.ruleState(rule2.ID); st.State != "conflict" {
		t.Fatalf("unbound existing feed: %+v", st)
	}
	gamma.bind("lab/tools", gammaTools)
	// A job that does not replicate the home feed is refused.
	otherFeed := gamma.createFeed("other")
	var stray replication.Job
	if code := gamma.call("POST", "/api/v1/replication/jobs", map[string]any{"name": "stray", "peer_id": tokenPeer.ID, "remote_feed_id": release, "dest_feed_id": otherFeed, "mode": "manual"}, &stray); code != 201 {
		t.Fatalf("stray job: %d", code)
	}
	if code, _ := gamma.errorCode("POST", "/api/v1/federation/replication/adopt", map[string]any{"job_id": stray.ID, "rule_id": rule2.ID}); code != 400 {
		t.Fatalf("adopting a mismatched job: %d", code)
	}
	gamma.reconcile() // adopt_existing (default) adopts the matching job in place
	after := gamma.job(phase1.ID)
	if after.Origin != "federation" || after.FederationRuleID != rule2.ID || after.AdoptedAt == nil || after.ReplicaSourceID != before.ReplicaSourceID ||
		after.Status.Checkpoint != before.Status.Checkpoint || after.Status.Phase != "incremental" || after.Status.Reseeds != 0 || after.PeerID == tokenPeer.ID {
		t.Fatalf("adoption changed the job: before %+v after %+v", before, after)
	}
	if st := gamma.ruleState(rule2.ID); st.State != "in_sync" || !st.Adopted {
		t.Fatalf("adopted rule status: %+v", st)
	}
	alpha.mustPublish(tools, "cli", "3")
	gamma.mustRun(phase1.ID)
	final := gamma.job(phase1.ID)
	if !gamma.hasVersion(gammaTools, "cli", "3") || final.Status.Reseeds != 0 || final.Status.BlobsTransferred != before.Status.BlobsTransferred+1 {
		t.Fatalf("adopted job reseeded or did not continue: %+v", final.Status)
	}

	// --- planned home transfer: alpha → beta ---
	if code, _ := alpha.errorCode("POST", "/api/v1/federation/feeds/lab/release/transfer", map[string]any{"to_member_id": gamma.id, "confirm": true}); code != 400 {
		t.Fatalf("transfer to a member without a replica: %d", code)
	}
	var tr federation.FeedTransfer
	if code := alpha.call("POST", "/api/v1/federation/feeds/lab/release/transfer", map[string]any{"to_member_id": beta.id, "mode": "planned", "confirm": true}, &tr); code != 202 || tr.State != "freeze" {
		t.Fatalf("start transfer: %d %+v", code, tr)
	}
	if code, ec := alpha.errorCode("POST", "/api/v1/federation/feeds/lab/release/transfer", map[string]any{"to_member_id": beta.id, "confirm": true}); code != 409 || ec != "feed_transferring" {
		t.Fatalf("second transfer: %d %s", code, ec)
	}
	var frozen *artifact.FeedTransferringError
	if err := alpha.publish(release, "app", "1.3"); !errors.As(err, &frozen) {
		t.Fatalf("publish on a frozen home: %v", err)
	}
	alpha.reconcile() // freeze confirmed → drain
	if f := feedOf(alpha.topology(), "lab/release"); f.Transfer == nil || f.Transfer.State != "drain" || f.Transfer.Head == 0 || !f.Frozen {
		t.Fatalf("after freeze: %+v", f)
	}
	beta.sync()
	beta.reconcile()
	beta.mustRun(bj.ID)
	alpha.reconcile() // drained → promote
	if f := feedOf(alpha.topology(), "lab/release"); f.Transfer.State != "promote" {
		t.Fatalf("after drain: %+v", f.Transfer)
	}
	beta.sync()
	beta.reconcile()  // promote: replica source becomes local
	alpha.reconcile() // promoted → complete, home = beta
	if f := feedOf(alpha.topology(), "lab/release"); f.Home != beta.id || f.Transfer.State != "complete" {
		t.Fatalf("after promote: %+v", f)
	}
	beta.sync()
	alpha.reconcile() // the old home demotes
	beta.mustPublish(betaRelease, "app", "2.0")
	if f := feedOf(beta.topology(), "lab/release"); f.LocalRole != "home" || f.Frozen {
		t.Fatalf("beta after promotion: %+v", f)
	}
	// The demoted home is a read-only replica: its publish target was the
	// converted source, so publishes are refused.
	var replica *artifact.ReplicaFeedError
	if err := alpha.publish(release, "app", "1.4"); !errors.As(err, &replica) && !errors.Is(err, artifact.ErrDenied) {
		t.Fatalf("publish on the demoted home: %v", err)
	}
	aj := alpha.jobFor("lab/release")
	if aj.Origin != "federation" || aj.FederationRuleID != "" {
		t.Fatalf("demote job: %+v", aj)
	}
	alpha.mustRun(aj.ID)
	aj = alpha.job(aj.ID)
	if !alpha.hasVersion(release, "app", "2.0") || !alpha.hasVersion(release, "app", "1.2") || aj.Status.BlobsTransferred != 1 || aj.Status.DeletionsHeld != 0 {
		t.Fatalf("old home did not follow the new home without re-transfer: %+v", aj.Status)
	}

	// --- forced home transfer with quarantine: beta → gamma ---
	var rule3 federation.ReplicationRule
	if code := alpha.call("POST", "/api/v1/federation/replication/rules", map[string]any{"feed_key": "lab/release", "targets": map[string]any{"members": []string{alpha.id, gamma.id}}}, &rule3); code != 201 {
		t.Fatalf("rule3: %d", code)
	}
	alpha.reconcile() // alpha adopts its transfer job into rule3
	if j := alpha.job(aj.ID); j.FederationRuleID != rule3.ID {
		t.Fatalf("transfer job not adopted by rule3: %+v", j)
	}
	gamma.sync()
	gamma.reconcile()
	gj := gamma.jobFor("lab/release")
	gamma.mustRun(gj.ID)
	gammaRelease := gamma.boundFeed("lab/release")
	if !gamma.hasVersion(gammaRelease, "app", "2.0") {
		t.Fatal("gamma did not replicate from the new home")
	}
	beta.proxy.set(faultDrop) // the home is lost
	beta.mustPublish(betaRelease, "app", "3.0")
	if code, _ := alpha.errorCode("POST", "/api/v1/federation/feeds/lab/release/transfer", map[string]any{"to_member_id": gamma.id, "mode": "forced", "confirm": true}); code != 400 {
		t.Fatalf("forced transfer without accept_data_loss: %d", code)
	}
	if code := alpha.call("POST", "/api/v1/federation/feeds/lab/release/transfer", map[string]any{"to_member_id": gamma.id, "mode": "forced", "confirm": true, "accept_data_loss": true}, &tr); code != 202 || tr.State != "promote" {
		t.Fatalf("forced transfer: %d %+v", code, tr)
	}
	gamma.sync()
	gamma.reconcile()
	alpha.reconcile()
	if f := feedOf(alpha.topology(), "lab/release"); f.Home != gamma.id || f.Transfer.State != "complete" || f.Transfer.Mode != "forced" {
		t.Fatalf("forced transfer did not complete: %+v", f)
	}
	gamma.sync()
	gamma.mustPublish(gammaRelease, "app", "4.0")
	alpha.reconcile() // re-point alpha's job to gamma
	aj = alpha.job(aj.ID)
	alpha.mustRun(aj.ID)
	if !alpha.hasVersion(release, "app", "4.0") || alpha.hasVersion(release, "app", "3.0") {
		t.Fatal("alpha did not follow the forced new home")
	}
	// The old home returns: it fences itself before accepting writes.
	beta.proxy.set(faultPass)
	beta.sync()
	if err := beta.publish(betaRelease, "app", "3.1"); !errors.As(err, &frozen) {
		t.Fatalf("returning old home accepted a write: %v", err)
	}
	beta.reconcile()
	var qs struct {
		Items []federation.Quarantine `json:"items"`
	}
	if code := beta.call("GET", "/api/v1/federation/quarantines", nil, &qs); code != 200 || len(qs.Items) != 1 || qs.Items[0].DivergentChanges == nil || *qs.Items[0].DivergentChanges < 1 {
		t.Fatalf("quarantine: %d %+v", code, qs.Items)
	}
	if st := beta.status(); !hasAlert(st, "feed_home_conflict") {
		t.Fatalf("no feed_home_conflict alert: %+v", st.Alerts)
	}
	var qsrc string
	_ = beta.pool.QueryRow(ctx, `SELECT source_id::text FROM federation_quarantines WHERE instance_id=$1`, beta.id).Scan(&qsrc)
	var divergent int
	_ = beta.pool.QueryRow(ctx, `SELECT count(*) FROM components c JOIN versions v ON v.component_id=c.id WHERE c.source_id=$1 AND v.version='3.0'`, qsrc).Scan(&divergent)
	if divergent != 1 || beta.hasVersion(betaRelease, "app", "3.0") {
		t.Fatal("divergent write not quarantined out of the feed")
	}
	nj := beta.jobFor("lab/release") // adopted by rule1 (beta is a target again)
	if nj.FederationRuleID != rule1.ID {
		t.Fatalf("returning home job: %+v", nj)
	}
	beta.mustRun(nj.ID)
	if !beta.hasVersion(betaRelease, "app", "4.0") {
		t.Fatal("old home did not replicate from the forced new home")
	}
	if code := beta.call("POST", "/api/v1/federation/quarantines/"+qs.Items[0].ID+"/resolve", map[string]any{"confirm": true}, nil); code != 200 {
		t.Fatalf("resolve quarantine: %d", code)
	}
	if st := beta.status(); hasAlert(st, "feed_home_conflict") {
		t.Fatal("alert not cleared after resolve")
	}

	// --- rule removal keeps content ---
	if code := alpha.call("DELETE", "/api/v1/federation/replication/rules/"+rule2.ID, nil, nil); code != 204 {
		t.Fatalf("delete rule2: %d", code)
	}
	gamma.sync()
	gamma.reconcile()
	released := gamma.job(phase1.ID)
	if released.Origin != "local" || released.Enabled || released.FederationRuleID != "" || !gamma.hasVersion(gammaTools, "cli", "3") {
		t.Fatalf("rule removal: %+v", released)
	}
	if st := gamma.ruleState(rule2.ID); st.State != "absent" {
		t.Fatalf("removed rule still reported: %+v", st)
	}
	// Released jobs are local again.
	if code := gamma.call("PATCH", "/api/v1/replication/jobs/"+phase1.ID, map[string]any{"revision": released.Revision, "enabled": false}, nil); code != 200 {
		t.Fatalf("patch released job: %d", code)
	}
	// Topology documents are verified and relayed like policy documents.
	var doc federation.StoredDocument
	if code := gamma.call("GET", "/api/v1/federation/documents/topology", nil, &doc); code != 200 || !doc.Applied {
		t.Fatalf("topology document: %d", code)
	}
	if st := gamma.status(); st.Topology == nil || st.Topology.Generation != alpha.status().Topology.Generation {
		t.Fatalf("topology status: %+v", st.Topology)
	}
}
