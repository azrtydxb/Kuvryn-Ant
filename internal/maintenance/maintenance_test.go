package maintenance

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	pool     *pgxpool.Pool
	instance string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open fixture")
	}
	schema := fmt.Sprintf("ant_maint_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid fixture DSN")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "Maintenance")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{pool, owner.InstanceID}
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%v: %s", err, sql)
	}
}

func (f *fixture) count(t *testing.T, sql string, args ...any) (n int) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// event inserts an audit event of the given age and returns its id.
func (f *fixture) event(t *testing.T, age time.Duration) (id int64) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(), `INSERT INTO audit_events(instance_id,action,created_at) VALUES($1,'test.event',now()-$2::interval) RETURNING id`, f.instance, fmt.Sprintf("%d seconds", int(age.Seconds()))).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

const day = 24 * time.Hour

func TestAuditPurgeHonorsRetention(t *testing.T) {
	f := newFixture(t)
	f.exec(t, `DELETE FROM outbox_events`)
	f.exec(t, `DELETE FROM audit_events`)
	old1, old2 := f.event(t, 500*day), f.event(t, 450*day)
	fresh := f.event(t, 10*day)
	// An old event with an undelivered outbox row is purged together with it.
	f.exec(t, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,'x','{}')`, old1)

	purge := AuditPurge{DB: f.pool, InstanceID: f.instance, Retain: 400 * day, BatchSize: 1}
	report, err := purge.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Deleted != 2 || report.SinkLag {
		t.Fatalf("report = %+v", report)
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE id=ANY($1)`, []int64{old1, old2}) != 0 || f.count(t, `SELECT count(*) FROM audit_events WHERE id=$1`, fresh) != 1 {
		t.Fatal("wrong events purged")
	}
	if f.count(t, `SELECT count(*) FROM outbox_events WHERE audit_id=$1`, old1) != 0 {
		t.Fatal("outbox row survived its audit event")
	}
	disabled, err := AuditPurge{DB: f.pool, InstanceID: f.instance, BatchSize: 10}.Run(context.Background())
	if err != nil || !disabled.Disabled {
		t.Fatalf("zero retention must disable purging: %+v %v", disabled, err)
	}
}

func TestAuditPurgeNeverDeletesUnexportedEvents(t *testing.T) {
	f := newFixture(t)
	f.exec(t, `DELETE FROM outbox_events`)
	f.exec(t, `DELETE FROM audit_events`)
	exported1, exported2 := f.event(t, 600*day), f.event(t, 590*day)
	lagging1, lagging2 := f.event(t, 580*day), f.event(t, 500*day)
	f.exec(t, `INSERT INTO audit_sinks(instance_id,kind,endpoint,checkpoint,enabled) VALUES($1,'http','https://siem.example/in',$2,false)`, f.instance, exported2)

	purge := AuditPurge{DB: f.pool, InstanceID: f.instance, Retain: 400 * day, BatchSize: 100}
	report, err := purge.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Deleted != 2 || report.HeldBySink != 2 || !report.SinkLag {
		t.Fatalf("report = %+v", report)
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE id=ANY($1)`, []int64{exported1, exported2}) != 0 {
		t.Fatal("exported events were not purged")
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE id=ANY($1)`, []int64{lagging1, lagging2}) != 2 {
		t.Fatal("unexported events were purged")
	}
	// Once the sink catches up, they become eligible.
	f.exec(t, `UPDATE audit_sinks SET checkpoint=$2 WHERE instance_id=$1`, f.instance, lagging2)
	report, err = purge.Run(context.Background())
	if err != nil || report.Deleted != 2 || report.SinkLag {
		t.Fatalf("after catch-up: %+v %v", report, err)
	}
}

func TestAuditPurgeIsFencedByLease(t *testing.T) {
	f := newFixture(t)
	f.event(t, 500*day)
	stale := func() ownership.Lease {
		return ownership.Lease{InstanceID: f.instance, OwnerID: "00000000-0000-0000-0000-000000000009", Epoch: 7, ExpiresAt: time.Now().Add(time.Minute)}
	}
	_, err := AuditPurge{DB: f.pool, InstanceID: f.instance, Retain: 400 * day, BatchSize: 10, Lease: stale}.Run(context.Background())
	if err == nil {
		t.Fatal("purge ran without holding the writer lease")
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE created_at<now()-interval '400 days'`) != 1 {
		t.Fatal("event deleted despite fencing failure")
	}
}

// orphans builds inconsistent metadata by bypassing foreign keys, as a partial
// restore or manual surgery could.
func (f *fixture) orphans(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	f.exec(t, `INSERT INTO sources(id,instance_id,name,kind) VALUES('00000000-0000-0000-0000-0000000000a1',$1,'s','local')`, f.instance)
	f.exec(t, `INSERT INTO dedup_domains(id,instance_id,name) VALUES('00000000-0000-0000-0000-0000000000d1',$1,'d')`, f.instance)
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	run := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v: %s", err, sql)
		}
	}
	run(`SET session_replication_role=replica`)
	defer func() { _, _ = conn.Exec(ctx, `SET session_replication_role=DEFAULT`) }()
	const (
		healthyComp, healthyVer, healthyBlob, healthyAsset = "00000000-0000-0000-0000-000000000c01", "00000000-0000-0000-0000-000000000e01", "00000000-0000-0000-0000-000000000b01", "00000000-0000-0000-0000-000000000f01"
		emptyComp, bareVer, bareComp                       = "00000000-0000-0000-0000-000000000c02", "00000000-0000-0000-0000-000000000e02", "00000000-0000-0000-0000-000000000c03"
		ghostComp, ghostVer, noBlobAsset, noVerAsset       = "00000000-0000-0000-0000-000000000c99", "00000000-0000-0000-0000-000000000e99", "00000000-0000-0000-0000-000000000f02", "00000000-0000-0000-0000-000000000f03"
		ghostBlob                                          = "00000000-0000-0000-0000-000000000b99"
	)
	src, dom := "00000000-0000-0000-0000-0000000000a1", "00000000-0000-0000-0000-0000000000d1"
	digest := func(c string) string { return fmt.Sprintf("%064s", c) }
	// Healthy chain that must survive untouched.
	run(`INSERT INTO components(id,instance_id,source_id,coordinate) VALUES($1,$2,$3,'ok')`, healthyComp, f.instance, src)
	run(`INSERT INTO versions(id,instance_id,component_id,version) VALUES($1,$2,$3,'1')`, healthyVer, f.instance, healthyComp)
	run(`INSERT INTO blobs(id,instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,1,'k')`, healthyBlob, f.instance, dom, digest("1"))
	run(`INSERT INTO assets(id,instance_id,version_id,asset_key,blob_id) VALUES($1,$2,$3,'a',$4)`, healthyAsset, f.instance, healthyVer, healthyBlob)
	run(`INSERT INTO tags(component_id,name,version_id) VALUES($1,'latest',$2)`, healthyComp, healthyVer)
	// Component without versions.
	run(`INSERT INTO components(id,instance_id,source_id,coordinate) VALUES($1,$2,$3,'empty')`, emptyComp, f.instance, src)
	// Old version without assets, tagged, under a component that then becomes empty.
	run(`INSERT INTO components(id,instance_id,source_id,coordinate) VALUES($1,$2,$3,'bare')`, bareComp, f.instance, src)
	run(`INSERT INTO versions(id,instance_id,component_id,version,created_at) VALUES($1,$2,$3,'1',now()-interval '2 days')`, bareVer, f.instance, bareComp)
	run(`INSERT INTO tags(component_id,name,version_id) VALUES($1,'t',$2)`, bareComp, bareVer)
	// A fresh assetless version (in-flight publish) must not be touched.
	run(`INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'2')`, f.instance, healthyComp)
	// Asset whose blob row is missing; asset whose version is missing; version whose component is missing.
	run(`INSERT INTO assets(id,instance_id,version_id,asset_key,blob_id) VALUES($1,$2,$3,'nb',$4)`, noBlobAsset, f.instance, healthyVer, ghostBlob)
	run(`INSERT INTO assets(id,instance_id,version_id,asset_key,blob_id) VALUES($1,$2,$3,'nv',$4)`, noVerAsset, f.instance, ghostVer, healthyBlob)
	run(`INSERT INTO versions(id,instance_id,component_id,version) VALUES('00000000-0000-0000-0000-000000000e77',$1,$2,'9')`, f.instance, ghostComp)
	run(`INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,'00000000-0000-0000-0000-000000000e77','x',$2)`, f.instance, healthyBlob)
	// Dangling attachments: subject without asset, subject with wrong component, tag to nowhere.
	run(`INSERT INTO artifact_subjects(asset_id,instance_id,component_id,subject_digest,subject_size,subject_media_type) VALUES('00000000-0000-0000-0000-000000000f77',$1,$2,$3,1,'m')`, f.instance, healthyComp, digest("5"))
	run(`INSERT INTO artifact_subjects(asset_id,instance_id,component_id,subject_digest,subject_size,subject_media_type) VALUES($1,$2,$3,$4,1,'m')`, healthyAsset, f.instance, emptyComp, digest("6"))
	run(`INSERT INTO tags(component_id,name,version_id) VALUES($1,'ghost',$2)`, healthyComp, ghostVer)
	// Stale open integrity finding for a blob row that no longer exists.
	run(`INSERT INTO blob_integrity_findings(instance_id,dedup_domain_id,digest,kind) VALUES($1,$2,$3,'size')`, f.instance, dom, digest("7"))
	// Report-only: a healthy asset's blob object is missing from storage.
	run(`INSERT INTO blob_integrity_findings(instance_id,dedup_domain_id,digest,kind) VALUES($1,$2,$3,'missing')`, f.instance, dom, digest("1"))
}

func TestRepairMetadataDryRunThenApply(t *testing.T) {
	f := newFixture(t)
	f.orphans(t)
	ctx := context.Background()
	snapshot := func() [8]int {
		return [8]int{
			f.count(t, `SELECT count(*) FROM components`), f.count(t, `SELECT count(*) FROM versions`), f.count(t, `SELECT count(*) FROM assets`),
			f.count(t, `SELECT count(*) FROM tags`), f.count(t, `SELECT count(*) FROM artifact_subjects`),
			f.count(t, `SELECT count(*) FROM blob_integrity_findings WHERE resolved_at IS NULL`), f.count(t, `SELECT count(*) FROM blobs`),
			f.count(t, `SELECT count(*) FROM artifact_subjects WHERE component_id='00000000-0000-0000-0000-000000000c01'`),
		}
	}
	before := snapshot()

	dry, err := RepairMetadata(ctx, f.pool, true)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.Total == 0 || dry.Fixed != 0 {
		t.Fatalf("dry run report = %+v", dry)
	}
	if snapshot() != before {
		t.Fatal("dry run changed data")
	}
	found := map[string]int64{}
	for _, fi := range dry.Findings {
		found[fi.Check] = fi.Found
	}
	for check, want := range map[string]int64{
		"tags_dangling": 1, "subjects_without_asset": 1, "subjects_component_mismatch": 1, "assets_without_blob": 1, "assets_without_version": 1,
		"assets_without_component": 1, "versions_without_component": 1, "versions_without_assets": 1, "components_without_versions": 1,
		"stale_integrity_findings": 1, "assets_with_missing_blob_object": 1,
	} {
		if found[check] != want {
			t.Errorf("dry run %s = %d, want %d (all: %v)", check, found[check], want, found)
		}
	}

	applied, err := RepairMetadata(ctx, f.pool, false)
	if err != nil {
		t.Fatal(err)
	}
	if applied.DryRun || applied.Fixed == 0 {
		t.Fatalf("apply report = %+v", applied)
	}
	// Survivors: healthy component with its version, asset, tag, subject (moved to the right
	// component) and the fresh assetless version; the missing-object finding is untouched.
	if got := f.count(t, `SELECT count(*) FROM components`); got != 1 {
		t.Errorf("components = %d", got)
	}
	if got := f.count(t, `SELECT count(*) FROM versions`); got != 2 {
		t.Errorf("versions = %d (healthy + in-flight)", got)
	}
	if f.count(t, `SELECT count(*) FROM assets`) != 1 || f.count(t, `SELECT count(*) FROM tags`) != 1 || f.count(t, `SELECT count(*) FROM blobs`) != 1 {
		t.Error("healthy rows damaged")
	}
	if f.count(t, `SELECT count(*) FROM artifact_subjects WHERE component_id='00000000-0000-0000-0000-000000000c01'`) != 1 || f.count(t, `SELECT count(*) FROM artifact_subjects`) != 1 {
		t.Error("subjects not repaired")
	}
	if f.count(t, `SELECT count(*) FROM blob_integrity_findings WHERE resolved_at IS NULL`) != 1 {
		t.Error("only the report-only finding should remain open")
	}

	again, err := RepairMetadata(ctx, f.pool, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, fi := range again.Findings {
		if fi.Found != 0 && !fi.ReportOnly {
			t.Errorf("not converged: %+v", fi)
		}
	}
}
