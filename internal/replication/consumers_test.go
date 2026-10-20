package replication

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func outgoing(t *testing.T, src *site, svc *Service) []Outgoing {
	t.Helper()
	if err := src.tracker.Flush(context.Background(), src.pool); err != nil {
		t.Fatal(err)
	}
	items, err := svc.ListOutgoing(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// A consumer pulling a feed shows up on the authority, is upserted (not
// duplicated) by later pulls, and its lag follows the change head.
func TestOutgoingTrackedOnPullWithLag(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	src.publish(src.publisher, src.feed, "team", "team/app", "1.0", "app.bin", "", data("app-bytes"))
	srv := src.serve(t)
	dsvc := dst.service(t, srv, nil)
	srcSvc := src.service(t, srv, nil)
	if got := outgoing(t, src, srcSvc); len(got) != 0 {
		t.Fatalf("outgoing before any pull: %+v", got)
	}
	job := link(t, src, dst, srv, dsvc, src.feed, dst.feed, JobInput{Name: "pull-artifacts"})
	if err := dsvc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got := outgoing(t, src, srcSvc)
	if len(got) != 1 {
		t.Fatalf("want one consumer, got %+v", got)
	}
	o := got[0]
	if o.ConsumerKind != ConsumerPrincipal || !strings.HasPrefix(o.ConsumerName, "peer-") || o.FeedID != src.feed || o.FeedSlug != "artifacts" {
		t.Fatalf("consumer identity wrong: %+v", o)
	}
	if o.RemoteJob != "pull-artifacts" {
		t.Fatalf("remote job %q", o.RemoteJob)
	}
	if o.Pulls == 0 || o.BytesServed < int64(len("app-bytes")) || o.Status != "active" {
		t.Fatalf("pull accounting wrong: %+v", o)
	}
	if o.LastSequence == nil || *o.LastSequence != o.HeadSequence || o.LagChanges != 0 || o.LagSeconds != 0 || o.HeadSequence == 0 {
		t.Fatalf("expected zero lag after a full pull: %+v", o)
	}
	firstPulls := o.Pulls

	// New publishes move the head; the consumer is behind until it pulls again.
	src.publish(src.publisher, src.feed, "team", "team/app", "2.0", "app.bin", "", data("v2"))
	behind := outgoing(t, src, srcSvc)[0]
	if behind.LagChanges < 1 || behind.HeadSequence <= *behind.LastSequence {
		t.Fatalf("lag not computed: %+v", behind)
	}
	if err := dsvc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got = outgoing(t, src, srcSvc)
	if len(got) != 1 || got[0].LagChanges != 0 || got[0].Pulls <= firstPulls {
		t.Fatalf("second pull not upserted into the same row: %+v", got)
	}
	if len(got[0].History) == 0 {
		t.Fatal("lag history sample missing")
	}
}

// Failed requests mark the consumer as error; silence marks it stale.
func TestOutgoingStatusErrorAndStale(t *testing.T) {
	src := newSite(t, "src", "artifacts")
	src.publish(src.publisher, src.feed, "team", "team/app", "1.0", "app.bin", "", data("x"))
	srv := src.serve(t)
	svc := src.service(t, srv, nil)
	token := src.grantReplicate("consumer-a", src.feed)
	c := srv.Client()
	base := srv.URL + "/api/v1/replication/export/feeds/" + src.feed
	if code, _ := call(t, c, "GET", base+"/changes?after=0", token, nil); code != 200 {
		t.Fatalf("changes: %d", code)
	}
	src.exec(`UPDATE replication_heads SET low_water=seq WHERE instance_id=$1`, src.instance)
	if code, _ := call(t, c, "GET", base+"/changes?after=0", token, nil); code != 410 {
		t.Fatalf("expected 410 below low-water, got %d", code)
	}
	got := outgoing(t, src, svc)
	if len(got) != 1 || got[0].Status != "error" || got[0].LastError == "" || got[0].LastErrorAt == nil {
		t.Fatalf("expected error status: %+v", got)
	}
	if code, _ := call(t, c, "GET", base+"/changes?after=1", token, nil); code != 200 {
		t.Fatalf("changes at low-water: %d", code)
	}
	if got = outgoing(t, src, svc); got[0].Status != "active" {
		t.Fatalf("recovered consumer should be active: %+v", got[0])
	}
	src.exec(`UPDATE replication_consumers SET last_seen=now()-interval '2 hours' WHERE instance_id=$1`, src.instance)
	if got, err := svc.ListOutgoing(context.Background(), nil); err != nil || got[0].Status != "stale" {
		t.Fatalf("expected stale: %+v %v", got, err)
	}
}

// The endpoint needs replicate or configure on the feed, like the jobs list.
func TestOutgoingAuthz(t *testing.T) {
	src := newSite(t, "src", "artifacts")
	other, _, _ := src.newFeed("other", "local")
	srv := src.serve(t)
	svc := src.service(t, srv, nil)
	mux := http.NewServeMux()
	svc.Register(mux, identity.Tokens{DB: src.pool})
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	token := src.grantReplicate("consumer-b", src.feed)
	base := srv.URL + "/api/v1/replication/export/feeds/"
	for _, feed := range []string{src.feed} {
		if code, _ := call(t, srv.Client(), "GET", base+feed+"/head", token, nil); code != 200 {
			t.Fatalf("head: %d", code)
		}
	}
	adminToken := src.adminTok
	if code, _ := call(t, srv.Client(), "GET", base+other+"/head", adminToken, nil); code != 200 {
		t.Fatalf("admin head on other feed: %d", code)
	}
	if err := src.tracker.Flush(context.Background(), src.pool); err != nil {
		t.Fatal(err)
	}
	list := func(tok string) (int, []Outgoing) {
		code, body := call(t, api.Client(), "GET", api.URL+"/api/v1/replication/outgoing", tok, nil)
		var out struct{ Items []Outgoing }
		_ = json.Unmarshal(body, &out)
		return code, out.Items
	}
	if code, _ := list(""); code != 401 {
		t.Fatalf("unauthenticated: %d", code)
	}
	if code, items := list(adminToken); code != 200 || len(items) != 2 {
		t.Fatalf("admin should see both feeds: %d %+v", code, items)
	}
	// A principal that may replicate only the primary feed sees only that one.
	limited := src.grantReplicate("viewer", src.feed)
	code, items := list(limited)
	if code != 200 || len(items) != 1 || items[0].FeedID != src.feed {
		t.Fatalf("scoped principal: %d %+v", code, items)
	}
	// A principal without any replicate or configure grant sees nothing.
	tok := src.grantReplicate("elsewhere", other)
	if code, items = list(tok); code != 200 || len(items) != 1 || items[0].FeedID != other {
		t.Fatalf("other-feed principal: %d %+v", code, items)
	}
}

// Federation members are named from the applied membership, and a rule that
// selects the member for the feed key is reported.
func TestOutgoingFederatedNaming(t *testing.T) {
	members := &federation.Membership{Members: []federation.Member{{InstanceID: "m1", Name: "kw", Status: "active", Labels: map[string]string{"site": "kw"}}}}
	if memberLabel(members, "m1") != "kw" || memberLabel(members, "unknown") != "unknown" || memberLabel(nil, "m1") != "m1" {
		t.Fatal("member naming")
	}
	topo := &federation.TopologyBody{Rules: []federation.ReplicationRule{
		{ID: "r-other", FeedKey: "other", Targets: federation.RuleTargets{Members: []string{"m1"}}},
		{ID: "r-site", FeedKey: "docs", Targets: federation.RuleTargets{Labels: map[string]string{"site": "kw"}}},
	}}
	if got := matchRule(topo, "docs", members.Members[0]); got != "r-site" {
		t.Fatalf("rule match %q", got)
	}
	if got := matchRule(topo, "missing", members.Members[0]); got != "" {
		t.Fatalf("unexpected rule %q", got)
	}
}

// A federation-member consumer row is listed with its instance id when no
// membership is attached, and the tracker never records unauthenticated calls.
func TestOutgoingMemberRowAndTrackerBounds(t *testing.T) {
	src := newSite(t, "src", "artifacts")
	srv := src.serve(t)
	svc := src.service(t, srv, nil)
	src.publish(src.publisher, src.feed, "team", "team/app", "1.0", "app.bin", "", data("x"))
	src.publish(src.publisher, src.feed, "team", "team/app", "2.0", "app.bin", "", data("y"))
	tr := src.tracker
	var cur int64
	if err := src.pool.QueryRow(context.Background(), `SELECT seq FROM replication_heads WHERE instance_id=$1`, src.instance).Scan(&cur); err != nil || cur < 2 {
		t.Fatalf("head %d %v", cur, err)
	}
	seq, head := cur-2, cur
	tr.record(src.instance, src.feed, consumer{ConsumerMember, "member-instance-1"}, pullEvent{served: &seq, head: &head, job: "j"})
	tr.record(src.instance, src.feed, consumer{ConsumerMember, ""}, pullEvent{})
	got := outgoing(t, src, svc)
	if len(got) != 1 || got[0].ConsumerKind != ConsumerMember || got[0].MemberInstanceID != "member-instance-1" || got[0].LagChanges != 2 {
		t.Fatalf("member row: %+v", got)
	}
}

// A job learns the remote feed's slug from the peer's head on every run.
func TestJobRecordsRemoteFeedSlug(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	src.publish(src.publisher, src.feed, "team", "team/app", "1.0", "app.bin", "", data("x"))
	srv := src.serve(t)
	svc := dst.service(t, srv, nil)
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{Name: "slug-job"})
	if job.RemoteFeedSlug != "" {
		t.Fatalf("slug known before any run: %q", job.RemoteFeedSlug)
	}
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetJob(ctx, dst.instance, job.ID)
	if err != nil || got.RemoteFeedSlug != "artifacts" {
		t.Fatalf("remote feed slug %q %v", got.RemoteFeedSlug, err)
	}
}
