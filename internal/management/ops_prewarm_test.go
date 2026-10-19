package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/prewarm"
)

type notifyCounter struct{ n atomic.Int32 }

func (c *notifyCounter) Notify() { c.n.Add(1) }

func (o *opsFixture) prewarmFeed(slug, format string, withRemote bool) Feed {
	o.t.Helper()
	feed := o.feed(slug, format)
	if withRemote {
		src := o.source(slug+"-remote", "remote", format)
		if _, err := o.db.Exec(context.Background(), `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, o.instance, feed.ID, src.ID); err != nil {
			o.t.Fatal(err)
		}
	}
	return feed
}

func TestPrewarmJobLifecycle(t *testing.T) {
	o := newOpsFixture(t)
	feed := o.prewarmFeed("npmc", "npm", true)

	// supported inputs
	var formats struct{ Items []PrewarmFormat }
	w := o.get("/api/v1/prewarm/formats")
	expectSourceStatus(t, w, 200)
	_ = json.Unmarshal(w.Body.Bytes(), &formats)
	found := false
	for _, f := range formats.Items {
		if f.Format == "npm" && f.Packages && f.Feed && !f.Images && len(f.Lockfiles) == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("formats: %s", w.Body.String())
	}

	// a lockfile preview reports what would be warmed and what is ignored
	lock := `{"lockfileVersion":3,"packages":{"":{},"node_modules/left-pad":{"version":"1.3.0","resolved":"https://r/left-pad.tgz"},"node_modules/linked":{"link":true,"resolved":"../x"}}}`
	lockJSON, _ := json.Marshal(lock)
	body := fmt.Sprintf(`{"feed_id":%q,"source":{"kind":"lockfile","lockfile":{"type":"package-lock.json","content":%s}}}`, feed.ID, lockJSON)
	w = o.request("POST", "/api/v1/prewarm/preview", body, "", "")
	expectSourceStatus(t, w, 200)
	var preview PrewarmPreview
	_ = json.Unmarshal(w.Body.Bytes(), &preview)
	if preview.Total != 1 || preview.IgnoredCount != 1 || preview.Items[0].Name != "left-pad" {
		t.Fatalf("preview: %s", w.Body.String())
	}

	// create from a lockfile, scheduled, and run now
	create := fmt.Sprintf(`{"feed_id":%q,"name":"nightly","source":{"kind":"lockfile","lockfile":{"type":"package-lock.json","content":%s}},"interval_seconds":3600,"concurrency":2,"rate_per_second":5,"run_now":true}`, feed.ID, lockJSON)
	w = o.request("POST", "/api/v1/prewarm/jobs", create, "job-1", "")
	expectSourceStatus(t, w, 201)
	var job prewarm.Job
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	if job.ItemCount != 1 || job.Concurrency != 2 || job.IntervalSeconds == nil || *job.IntervalSeconds != 3600 || job.NextRunAt == nil || job.LastRun == nil || job.LastRun.State != "queued" || job.LastRun.Total != 1 || job.LastRun.Trigger != "manual" {
		t.Fatalf("job: %s", w.Body.String())
	}
	if o.prewarm.n.Load() != 1 {
		t.Fatalf("scheduler not notified: %d", o.prewarm.n.Load())
	}
	// replay returns the same job and does not queue again
	w2 := o.request("POST", "/api/v1/prewarm/jobs", create, "job-1", "")
	expectSourceStatus(t, w2, 201)
	var replay prewarm.Job
	_ = json.Unmarshal(w2.Body.Bytes(), &replay)
	if replay.ID != job.ID || replay.LastRun == nil || replay.LastRun.ID != job.LastRun.ID || o.count(`SELECT count(*) FROM prewarm_runs`) != 1 || o.count(`SELECT count(*) FROM prewarm_jobs`) != 1 {
		t.Fatalf("replay created more state: %s", w2.Body.String())
	}
	if o.count(`SELECT count(*) FROM audit_events WHERE action='prewarm.job.create'`) != 1 {
		t.Fatal("create not audited")
	}

	// read it back
	w = o.get("/api/v1/prewarm/jobs/" + job.ID)
	expectSourceStatus(t, w, 200)
	if w.Header().Get("ETag") != `"1"` {
		t.Fatalf("etag %q", w.Header().Get("ETag"))
	}
	w = o.get("/api/v1/prewarm/jobs?feed_id=" + feed.ID)
	expectSourceStatus(t, w, 200)
	var page struct{ Items []prewarm.Job }
	_ = json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.Items[0].ID != job.ID {
		t.Fatalf("list: %s", w.Body.String())
	}

	// patch: revision required, schedule can be removed with null
	expectSourceStatus(t, o.request("PATCH", "/api/v1/prewarm/jobs/"+job.ID, `{"enabled":false}`, idk(), ""), 428)
	expectSourceStatus(t, o.request("PATCH", "/api/v1/prewarm/jobs/"+job.ID, `{"enabled":false}`, idk(), `"9"`), 412)
	w = o.request("PATCH", "/api/v1/prewarm/jobs/"+job.ID, `{"enabled":false,"interval_seconds":null,"name":"renamed","options":{"platforms":["linux/arm64"]}}`, idk(), `"1"`)
	expectSourceStatus(t, w, 200)
	job = prewarm.Job{}
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	if job.Enabled || job.IntervalSeconds != nil || job.NextRunAt != nil || job.Name != "renamed" || job.Revision != 2 || len(job.Options.Platforms) != 1 {
		t.Fatalf("patched: %s", w.Body.String())
	}
	expectSourceStatus(t, o.request("PATCH", "/api/v1/prewarm/jobs/"+job.ID, `{"interval_seconds":10}`, idk(), `"2"`), 400)
	expectSourceStatus(t, o.request("PATCH", "/api/v1/prewarm/jobs/"+job.ID, `{"concurrency":99}`, idk(), `"2"`), 400)

	// one active run per job
	expectSourceStatus(t, o.request("POST", "/api/v1/prewarm/jobs/"+job.ID+"/runs", `{}`, idk(), ""), 409)
	runs := decodeJSON[struct{ Items []prewarm.Run }](t, o.get("/api/v1/prewarm/runs?job_id="+job.ID))
	if len(runs.Items) != 1 {
		t.Fatalf("runs: %+v", runs)
	}
	run := runs.Items[0]
	w = o.request("POST", "/api/v1/prewarm/runs/"+run.ID+"/cancel", "", idk(), "")
	expectSourceStatus(t, w, 200)
	var canceled prewarm.Run
	_ = json.Unmarshal(w.Body.Bytes(), &canceled)
	if canceled.State != "canceled" || canceled.Skipped != 1 {
		t.Fatalf("cancel: %s", w.Body.String())
	}
	expectSourceStatus(t, o.request("POST", "/api/v1/prewarm/runs/"+run.ID+"/cancel", "", idk(), ""), 409)

	// retry covers what was canceled; items expose state and reason
	w = o.request("POST", "/api/v1/prewarm/runs/"+run.ID+"/retry", "", idk(), "")
	expectSourceStatus(t, w, 202)
	var retry prewarm.Run
	_ = json.Unmarshal(w.Body.Bytes(), &retry)
	if retry.Trigger != "retry" || retry.RetryOf == nil || *retry.RetryOf != run.ID || retry.Total != 1 {
		t.Fatalf("retry: %s", w.Body.String())
	}
	expectSourceStatus(t, o.request("POST", "/api/v1/prewarm/runs/"+retry.ID+"/retry", "", idk(), ""), 409)
	items := decodeJSON[struct{ Items []prewarm.RunItem }](t, o.get("/api/v1/prewarm/runs/"+run.ID+"/items"))
	if len(items.Items) != 1 || items.Items[0].State != "skipped" || items.Items[0].Error == nil || *items.Items[0].Error != "canceled" {
		t.Fatalf("items: %+v", items)
	}
	expectSourceStatus(t, o.get("/api/v1/prewarm/runs/"+run.ID+"/items?state=failed"), 200)
	expectSourceStatus(t, o.get("/api/v1/prewarm/runs/"+run.ID+"/items?state=bogus"), 400)
	expectSourceStatus(t, o.get("/api/v1/prewarm/runs/"+run.ID), 200)
	expectSourceStatus(t, o.get("/api/v1/prewarm/runs?state=running"), 200)

	// delete needs the revision and takes the runs with it
	expectSourceStatus(t, o.request("DELETE", "/api/v1/prewarm/jobs/"+job.ID, "", idk(), `"1"`), 412)
	expectSourceStatus(t, o.request("DELETE", "/api/v1/prewarm/jobs/"+job.ID, "", idk(), `"2"`), 204)
	expectSourceStatus(t, o.get("/api/v1/prewarm/jobs/"+job.ID), 404)
	if o.count(`SELECT count(*) FROM prewarm_runs`) != 0 {
		t.Fatal("runs survived their job")
	}
}

func TestPrewarmValidation(t *testing.T) {
	o := newOpsFixture(t)
	npm := o.prewarmFeed("npmc", "npm", true)
	local := o.prewarmFeed("npm-local", "npm", false)
	helm := o.prewarmFeed("charts", "helm", true)
	oci := o.prewarmFeed("images", "oci", true)
	raw := o.prewarmFeed("files", "raw", true)
	pk := `{"kind":"packages","packages":[{"name":"left-pad","version":"1.3.0"}]}`
	cases := []struct {
		name, body string
		status     int
	}{
		{"ok", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":%s}`, npm.ID, pk), 201},
		{"no remote source", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":%s}`, local.ID, pk), 400},
		{"format without prewarm", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":%s}`, helm.ID, pk), 400},
		{"unknown feed", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":%s}`, "11111111-1111-4111-8111-111111111111", pk), 404},
		{"bad name", fmt.Sprintf(`{"feed_id":%q,"name":"","source":%s}`, npm.ID, pk), 400},
		{"unknown field", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":%s,"x":1}`, npm.ID, pk), 400},
		{"bad package", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"packages","packages":[{"name":"../x","version":"1"}]}}`, npm.ID), 400},
		{"empty list", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"packages","packages":[]}}`, npm.ID), 400},
		{"mixed source", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"packages","packages":[{"name":"a","version":"1"}],"images":["x"]}}`, npm.ID), 400},
		{"lockfile of another ecosystem", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"lockfile","lockfile":{"type":"go.sum","content":"example.com/m v1.0.0 h1:x="}}}`, npm.ID), 400},
		{"lockfile without registry packages", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"lockfile","lockfile":{"type":"package-lock.json","content":"{\"packages\":{\"node_modules/a\":{\"link\":true}}}"}}}`, npm.ID), 400},
		{"images for npm", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"images","images":["x"]}}`, npm.ID), 400},
		{"images", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"images","images":["library/alpine:3.20"]},"options":{"platforms":["linux/amd64","linux/arm64"]}}`, oci.ID), 201},
		{"bad platform", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"images","images":["library/alpine"]},"options":{"platforms":["nonsense"]}}`, oci.ID), 400},
		{"raw paths", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"packages","packages":[{"name":"dir/file.bin"}]}}`, raw.ID), 201},
		{"feed source of another format", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"feed","feed_id":%q}}`, npm.ID, raw.ID), 400},
		{"feed source", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"feed","feed_id":%q}}`, npm.ID, local.ID), 201},
		{"interval too small", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":%s,"interval_seconds":5}`, npm.ID, pk), 400},
		{"rate too high", fmt.Sprintf(`{"feed_id":%q,"name":"a","source":%s,"rate_per_second":5000}`, npm.ID, pk), 400},
	}
	for i, tc := range cases {
		w := o.request("POST", "/api/v1/prewarm/jobs", tc.body, fmt.Sprintf("v-%d", i), "")
		if w.Code != tc.status {
			t.Errorf("%s: status %d want %d: %s", tc.name, w.Code, tc.status, w.Body.String())
		}
	}
	// a feed source with nothing to warm is refused when run
	jobs := decodeJSON[struct{ Items []prewarm.Job }](t, o.get("/api/v1/prewarm/jobs"))
	for _, j := range jobs.Items {
		if j.SourceKind == "feed" {
			w := o.request("POST", "/api/v1/prewarm/jobs/"+j.ID+"/runs", `{}`, idk(), "")
			expectSourceStatus(t, w, 400)
			if !strings.Contains(w.Body.String(), "nothing to warm") {
				t.Fatalf("%s", w.Body.String())
			}
		}
	}
}

func TestPrewarmAuthorization(t *testing.T) {
	o := newOpsFixture(t)
	feed := o.prewarmFeed("npmc", "npm", true)
	create := fmt.Sprintf(`{"feed_id":%q,"name":"a","source":{"kind":"packages","packages":[{"name":"left-pad","version":"1.3.0"}]},"run_now":true}`, feed.ID)
	w := o.request("POST", "/api/v1/prewarm/jobs", create, idk(), "")
	expectSourceStatus(t, w, 201)
	var job prewarm.Job
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	runID := job.LastRun.ID

	// a principal without any role cannot even see the feed
	for _, c := range [][3]string{
		{"GET", "/api/v1/prewarm/jobs/" + job.ID, ""},
		{"GET", "/api/v1/prewarm/jobs?feed_id=" + feed.ID, ""},
		{"GET", "/api/v1/prewarm/runs/" + runID, ""},
		{"GET", "/api/v1/prewarm/runs/" + runID + "/items", ""},
		{"POST", "/api/v1/prewarm/runs/" + runID + "/cancel", ""},
		{"POST", "/api/v1/prewarm/jobs/" + job.ID + "/runs", "{}"},
		{"DELETE", "/api/v1/prewarm/jobs/" + job.ID, ""},
	} {
		key, rev := "", ""
		if c[0] != "GET" {
			key = idk()
		}
		if c[0] == "DELETE" {
			rev = `"1"`
		}
		expectSourceStatus(t, o.requestAs(c[0], c[1], c[2], key, rev, o.limited), 404)
		expectSourceStatus(t, o.requestAs(c[0], c[1], c[2], key, rev, ""), 401)
	}
	expectSourceStatus(t, o.requestAs("POST", "/api/v1/prewarm/jobs", create, idk(), "", o.limited), 404)
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/prewarm/jobs", "", "", "", o.limited), 403)
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/prewarm/runs", "", "", "", o.limited), 403)

	// a token that may only read sees the job but cannot change anything
	reader, err := (identity.Tokens{DB: o.db}).Issue(context.Background(), o.instance, o.principal, o.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/prewarm/jobs/"+job.ID, "", "", "", reader.Secret), 200)
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/prewarm/runs?feed_id="+feed.ID, "", "", "", reader.Secret), 200)
	expectSourceStatus(t, o.requestAs("POST", "/api/v1/prewarm/jobs", create, idk(), "", reader.Secret), 403)
	expectSourceStatus(t, o.requestAs("POST", "/api/v1/prewarm/jobs/"+job.ID+"/runs", "{}", idk(), "", reader.Secret), 403)
	expectSourceStatus(t, o.requestAs("POST", "/api/v1/prewarm/runs/"+runID+"/cancel", "", idk(), "", reader.Secret), 403)
	expectSourceStatus(t, o.requestAs("PATCH", "/api/v1/prewarm/jobs/"+job.ID, `{"enabled":false}`, idk(), `"1"`, reader.Secret), 403)
	expectSourceStatus(t, o.requestAs("DELETE", "/api/v1/prewarm/jobs/"+job.ID, "", idk(), `"1"`, reader.Secret), 403)
	expectSourceStatus(t, o.requestAs("POST", "/api/v1/prewarm/preview", fmt.Sprintf(`{"feed_id":%q,"source":{"kind":"packages","packages":[{"name":"a","version":"1"}]}}`, feed.ID), "", "", reader.Secret), 403)
}

func TestPrewarmUnavailableWithoutService(t *testing.T) {
	h := opsHandlers{d: OpsDeps{}}
	w := httptest.NewRecorder()
	if !h.prewarmDown(w, httptest.NewRequest("POST", "/api/v1/prewarm/jobs", nil)) || w.Code != 503 {
		t.Fatalf("down=%d", w.Code)
	}
}
