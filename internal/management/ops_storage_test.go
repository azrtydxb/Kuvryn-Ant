package management

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
)

func TestStorageMigrationAPI(t *testing.T) {
	o := newOpsFixture(t)
	ctx := context.Background()
	// Without a migrator the endpoints are unavailable, not 404.
	expectSourceStatus(t, o.get("/api/v1/storage/migration"), 503)

	var domain string
	if err := o.db.QueryRow(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,'mig') RETURNING id::text`, o.instance).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	reg, err := storage.New(storage.Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	stores := map[string]*blob.Mem{"disk": blob.NewMem(), "s3": blob.NewMem()}
	for kind, mem := range stores {
		reg.SetFactory(kind, func(string) (blob.Store, error) { return mem, nil })
	}
	router := &storage.DBRouter{DB: o.db, InstanceID: o.instance, Default: "disk", TTL: time.Millisecond}
	reg.SetRouter(router)
	for i := range 5 {
		info := stores["disk"].Put([]byte{byte(i), 1, 2, 3})
		if _, err := o.db.Exec(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,'k')`, o.instance, domain, string(info.Digest), info.Size); err != nil {
			t.Fatal(err)
		}
	}
	m := &storage.Migrator{DB: o.db, InstanceID: o.instance, Backends: reg, Default: "disk", OnSwitch: router.Invalidate, Owner: "api-test"}
	deps := o.deps
	deps.StorageMigrator = func(string) *storage.Migrator { return m }
	mux := http.NewServeMux()
	Register(mux)
	RegisterOperations(mux, deps)
	o.handler = WithRequestID(mux)

	st := decodeJSON[StorageMigrationStatus](t, o.get("/api/v1/storage/migration"))
	if st.ActiveBackend != "disk" || st.CleanupPending || len(st.AvailableBackends) != 2 || st.CurrentJob != nil {
		t.Fatalf("%+v", st)
	}
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/storage/migration", "", "", "", o.limited), 403)
	expectSourceStatus(t, o.requestAs("POST", "/api/v1/storage/migrations", `{"action":"migrate","to":"s3","dry_run":true}`, idk(), "", o.limited), 403)
	expectSourceStatus(t, o.requestAs("POST", "/api/v1/storage/migrations", `{"action":"migrate","to":"disk","dry_run":false}`, "", "", ""), 401)
	expectSourceStatus(t, o.request("POST", "/api/v1/storage/migrations", `{"action":"migrate","to":"disk","dry_run":false}`, idk(), ""), 400)
	expectSourceStatus(t, o.request("POST", "/api/v1/storage/migrations", `{"action":"cleanup","dry_run":false}`, idk(), ""), 409)

	wait := func(id string) storage.Job {
		t.Helper()
		for range 200 {
			j := decodeJSON[storage.Job](t, o.get("/api/v1/storage/migrations/"+id))
			if j.State != storage.StateRunning {
				return j
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("job did not finish")
		return storage.Job{}
	}
	w := o.request("POST", "/api/v1/storage/migrations", `{"action":"migrate","to":"s3","dry_run":true}`, idk(), "")
	expectSourceStatus(t, w, 202)
	dry := wait(decodeJSON[storage.Job](t, w).ID)
	// A retry with the same key replays the job instead of starting another.
	jobs := o.count(`SELECT count(*) FROM storage_migrations`)
	first := o.request("POST", "/api/v1/storage/migrations", `{"action":"migrate","to":"s3","dry_run":true}`, "start-replay", "")
	expectSourceStatus(t, first, 202)
	replay := o.request("POST", "/api/v1/storage/migrations", `{"action":"migrate","to":"s3","dry_run":true}`, "start-replay", "")
	expectSourceStatus(t, replay, 202)
	if decodeJSON[storage.Job](t, first).ID != decodeJSON[storage.Job](t, replay).ID || o.count(`SELECT count(*) FROM storage_migrations`) != jobs+1 {
		t.Fatal("replayed start created a second job")
	}
	wait(decodeJSON[storage.Job](t, first).ID)
	if dry.State != storage.StateCompleted || dry.Copied != 5 || stores["s3"].Len() != 0 {
		t.Fatalf("dry run %+v", dry)
	}
	w = o.request("POST", "/api/v1/storage/migrations", `{"action":"migrate","to":"s3","dry_run":false,"throttle_bytes_per_second":1000000}`, idk(), "")
	expectSourceStatus(t, w, 202)
	done := wait(decodeJSON[storage.Job](t, w).ID)
	if done.State != storage.StateCompleted || done.Copied != 5 || stores["s3"].Len() != 5 || done.SwitchedAt == nil {
		t.Fatalf("migration %+v", done)
	}
	st = decodeJSON[StorageMigrationStatus](t, o.get("/api/v1/storage/migration"))
	if st.ActiveBackend != "s3" || st.PreviousBackend != "disk" || !st.CleanupPending || st.CurrentJob == nil {
		t.Fatalf("%+v", st)
	}
	if u := decodeJSON[StorageUsage](t, o.get("/api/v1/storage/usage")); u.Backend != "s3" {
		t.Fatalf("usage reports %q", u.Backend)
	}
	expectSourceStatus(t, o.request("POST", "/api/v1/storage/migrations", `{"action":"migrate","to":"disk","dry_run":false}`, idk(), ""), 409)
	list := decodeJSON[StorageJobPage](t, o.get("/api/v1/storage/migrations"))
	if len(list.Items) != 3 {
		t.Fatalf("jobs %d", len(list.Items))
	}

	w = o.request("POST", "/api/v1/storage/migrations", `{"action":"cleanup","dry_run":false}`, idk(), "")
	expectSourceStatus(t, w, 202)
	clean := wait(decodeJSON[storage.Job](t, w).ID)
	if clean.State != storage.StateCompleted || clean.Deleted != 5 || stores["disk"].Len() != 0 {
		t.Fatalf("cleanup %+v", clean)
	}
	if st = decodeJSON[StorageMigrationStatus](t, o.get("/api/v1/storage/migration")); st.CleanupPending {
		t.Fatalf("%+v", st)
	}
	expectSourceStatus(t, o.get("/api/v1/storage/migrations/00000000-0000-4000-8000-000000000000"), 404)
	var audits int
	if err := o.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action IN ('storage.migrate','storage.cleanup')`).Scan(&audits); err != nil || audits != 4 {
		t.Fatalf("audit events %d %v", audits, err)
	}
	if n := o.count(`SELECT count(*) FROM outbox_events WHERE topic='storage.migration.completed'`); n != 4 {
		t.Fatalf("completion events %d", n)
	}
}
