package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/transfer"
)

func TestTransferEndpointsAreAdminOnly(t *testing.T) {
	o := newOpsFixture(t)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/v1/transfer/export", `{}`},
		{"POST", "/api/v1/transfer/import", `{"document":{"format_version":1,"sources":[],"feeds":[],"components":[]}}`},
		{"GET", "/api/v1/transfer/runs", ""},
		{"POST", "/api/v1/migrations/nexus", `{"url":"http://127.0.0.1:1"}`},
		{"POST", "/api/v1/migrations/compare", `{"url":"http://127.0.0.1:1","items":[{"repository":"r","path":"p"}]}`},
	} {
		expectSourceStatus(t, o.requestAs(c.method, c.path, c.body, "", "", o.limited), 403)
		expectSourceStatus(t, o.requestAs(c.method, c.path, c.body, "", "", ""), 401)
	}
}

func TestExportImportAndRunHistory(t *testing.T) {
	o := newOpsFixture(t)
	o.feed("tx-feed", "raw")
	src := o.source("tx-src", "local", "raw")
	w := o.request("POST", "/api/v1/transfer/export", `{"feeds":["tx-feed"]}`, "", "")
	expectSourceStatus(t, w, 200)
	var doc transfer.Document
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || len(doc.Feeds) != 1 || doc.Feeds[0].Slug != "tx-feed" {
		t.Fatalf("export: %v %s", err, w.Body.String())
	}
	expectSourceStatus(t, o.request("POST", "/api/v1/transfer/export", `{"feeds":["missing"]}`, "", ""), 400)
	// Re-import of what already exists changes nothing; a new feed is created
	// only on apply, never in the dry run.
	doc.Feeds = append(doc.Feeds, transfer.FeedDef{Slug: "imported", Format: "raw", Sources: []transfer.FeedSourceRef{}})
	body, _ := json.Marshal(map[string]any{"document": doc})
	w = o.request("POST", "/api/v1/transfer/import", string(body), "", "")
	expectSourceStatus(t, w, 200)
	var rep transfer.Report
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if !rep.DryRun || rep.Count("feed", transfer.ActionCreated) != 1 {
		t.Fatalf("dry run default: %s", w.Body.String())
	}
	if n := o.feeds(t, "imported"); n != 0 {
		t.Fatal("dry run created a feed")
	}
	body, _ = json.Marshal(map[string]any{"document": doc, "dry_run": false})
	expectSourceStatus(t, o.request("POST", "/api/v1/transfer/import", string(body), "", ""), 200)
	if n := o.feeds(t, "imported"); n != 1 {
		t.Fatal("apply did not create the feed")
	}
	w = o.request("POST", "/api/v1/transfer/import", string(body), "", "")
	expectSourceStatus(t, w, 200)
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if len(rep.Items) != 0 {
		t.Fatalf("apply is not idempotent: %+v", rep.Items)
	}
	expectSourceStatus(t, o.request("POST", "/api/v1/transfer/import", `{"document":{"format_version":9}}`, "", ""), 400)
	_ = src
	runs := decodeJSON[TransferRunPage](t, o.get("/api/v1/transfer/runs"))
	if len(runs.Items) < 4 || runs.Items[0].Kind != "import" {
		t.Fatalf("history: %+v", runs.Items)
	}
}

func (o *opsFixture) feeds(t *testing.T, slug string) (n int) {
	t.Helper()
	if err := o.db.QueryRow(t.Context(), `SELECT count(*) FROM feeds WHERE instance_id=$1 AND slug=$2`, o.instance, slug).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNexusMigrationEndpointsNeverStoreCredentials(t *testing.T) {
	o := newOpsFixture(t)
	nexus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "hunter2-secret" {
			http.Error(w, "no", 401)
			return
		}
		switch {
		case r.URL.Path == "/service/rest/v1/repositories":
			_, _ = w.Write([]byte(`[{"name":"raw-hosted","format":"raw","type":"hosted"}]`))
		case strings.HasPrefix(r.URL.Path, "/repository/raw-hosted/"):
			_, _ = w.Write([]byte("hello"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer nexus.Close()
	body := `{"url":"` + nexus.URL + `","username":"admin","password":"hunter2-secret","count_assets":false}`
	w := o.request("POST", "/api/v1/migrations/nexus", body, "", "")
	expectSourceStatus(t, w, 200)
	var rep transfer.MigrationReport
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if rep.Report == nil || !rep.DryRun || rep.Count("source", transfer.ActionCreated) != 1 || len(rep.Repositories) != 1 {
		t.Fatalf("migration dry run: %s", w.Body.String())
	}
	if o.feeds(t, "raw-hosted") != 0 {
		t.Fatal("dry run wrote")
	}
	expectSourceStatus(t, o.request("POST", "/api/v1/migrations/nexus", strings.Replace(body, "hunter2-secret", "wrong", 1), "", ""), 502)
	expectSourceStatus(t, o.request("POST", "/api/v1/migrations/nexus", `{"url":"ftp://x"}`, "", ""), 400)
	w = o.request("POST", "/api/v1/migrations/compare", `{"url":"`+nexus.URL+`","username":"admin","password":"hunter2-secret","items":[{"repository":"raw-hosted","path":"a.txt"}]}`, "", "")
	expectSourceStatus(t, w, 200)
	var cmp transfer.CompareReport
	_ = json.Unmarshal(w.Body.Bytes(), &cmp)
	if len(cmp.Results) != 1 || cmp.Results[0].Status != "missing_in_ant" {
		t.Fatalf("compare: %s", w.Body.String())
	}
	var stored int
	if err := o.db.QueryRow(t.Context(), `SELECT count(*) FROM transfer_runs WHERE summary::text LIKE '%hunter2%'`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("credential persisted in run history: %v", err)
	}
	if err := o.db.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE details::text LIKE '%hunter2%'`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("credential persisted in audit log: %v", err)
	}
}
