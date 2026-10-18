package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func TestPublishPolicyExactJSONAndRoutingValidation(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"rules":null}`, `{"rules":[],"Rules":[]}`,
		`{"rules":[{"id":"a","priority":1,"selector":{},"deny":true,"Deny":false}]}`,
		`{"rules":[{"id":"a","priority":1,"priority":2,"selector":{},"deny":true}]}`,
		`{"rules":[{"id":"a","priority":1,"selector":{"namespace":"a","namespace":"b"},"deny":true}]}`,
		`{"rules":[{"id":"a","priority":1,"selector":{"Namespace":"a"},"deny":true}]}`,
		`{"rules":[{"id":"a","priority":1,"selector":{"principal_id":null},"deny":true}]}`,
		`{"rules":[{"id":"a","priority":1,"selector":{"path_prefix":"/a/%2f/b"},"deny":true}]}`,
		`{"rules":[{"id":"a","priority":1,"selector":{},"deny":false}]}`,
		`{"rules":[{"id":"a","priority":1,"selector":{}}]}`,
		`{"default_source_id":"bad","rules":[]}`,
	} {
		r := httptest.NewRequest("PUT", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if _, _, ok := readPublishPolicy(httptest.NewRecorder(), r); ok {
			t.Fatalf("invalid policy accepted: %s", body)
		}
	}
	r := httptest.NewRequest("PUT", "/", strings.NewReader(`{"rules":[{"id":"deny","priority":-1,"selector":{"path_prefix":"/a%20b/"},"deny":true}]}`))
	r.Header.Set("Content-Type", "application/json")
	input, rules, ok := readPublishPolicy(httptest.NewRecorder(), r)
	if !ok || input.Rules[0].Selector.PathPrefix != "/a b" || rules[0].Selector.PathPrefix != "/a b" {
		t.Fatal("valid normalized rule rejected")
	}
}
func attachPolicySource(t *testing.T, f *sourceHTTPFixture, feed Feed, source Source, revision int64, priority int) {
	t.Helper()
	expectSourceStatus(t, f.request("PUT", "/api/v1/feeds/"+feed.ID+"/sources/"+source.ID, fmt.Sprintf(`{"priority":%d}`, priority), "attach-policy-"+source.ID, strconv.Quote(strconv.FormatInt(revision, 10))), 200)
}

func TestPublishPolicyHTTPAtomicityContractAndPermissions(t *testing.T) {
	f := newSourceHTTPFixture(t)
	feed := f.feed("policy", "oci")
	local := f.source("policy-local", "local", "oci")
	remote := f.source("policy-remote", "remote", "oci")
	attachPolicySource(t, f, feed, local, 1, 1)
	attachPolicySource(t, f, feed, remote, 2, 2)
	path := "/api/v1/feeds/" + feed.ID + "/publish-policy"
	get := f.request("GET", path, "", "", "")
	expectSourceStatus(t, get, 200)
	var initial PublishPolicy
	json.Unmarshal(get.Body.Bytes(), &initial)
	if initial.Revision != 3 || len(initial.Rules) != 0 || initial.DefaultSourceID != "" {
		t.Fatal(initial)
	}
	body := fmt.Sprintf(`{"default_source_id":%q,"rules":[{"id":"deny-private","priority":-100,"selector":{"namespace":"private"},"deny":true}]}`, local.ID)
	set := f.request("PUT", path, body, "policy-set", `"3"`)
	expectSourceStatus(t, set, 200)
	if set.Header().Get("ETag") != `"4"` {
		t.Fatal("revision missing")
	}
	replay := f.request("PUT", path, body, "policy-set", `"3"`)
	expectSourceStatus(t, replay, 200)
	if replay.Body.String() != set.Body.String() {
		t.Fatal("idempotent result changed")
	}
	expectSourceStatus(t, f.request("PUT", path, `{"rules":[]}`, "policy-set", `"3"`), 409)
	expectSourceStatus(t, f.request("PUT", path, `{"rules":[]}`, "stale", `"3"`), 412)
	loaded, err := (artifact.Policies{DB: f.db, InstanceID: f.instance}).Load(context.Background(), feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	denied, err := loaded.Resolve(routing.Request{Namespace: "private"})
	if err != nil || denied.Allowed || denied.Reason != routing.DeniedByRule {
		t.Fatal("HTTP rule was not executable", err, denied)
	}
	allowed, err := loaded.Resolve(routing.Request{Namespace: "public"})
	if err != nil || !allowed.Allowed || allowed.TargetID != local.ID {
		t.Fatal("HTTP default destination lost", err, allowed)
	}
	expectSourceStatus(t, f.request("PUT", path, fmt.Sprintf(`{"default_source_id":%q,"rules":[]}`, remote.ID), "remote-destination", `"4"`), 400)
	unattached := f.source("policy-unattached", "local", "oci")
	expectSourceStatus(t, f.request("PUT", path, fmt.Sprintf(`{"default_source_id":%q,"rules":[]}`, unattached.ID), "unattached", `"4"`), 404)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/feeds/"+feed.ID+"/sources/"+local.ID, "", "detach-referenced", `"4"`), 409)
	if f.count("SELECT count(*) FROM audit_events WHERE action='feed.publish_policy'") != 1 || f.count("SELECT count(*) FROM outbox_events WHERE topic='feed.publish_policy'") != 1 {
		t.Fatal("duplicate or partial policy events")
	}
	readOnly, err := (identity.Tokens{DB: f.db}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.requestAs("GET", path, "", "", "", readOnly.Secret), 404)
	f.deny("source", local.ID, []string{"configure"})
	expectSourceStatus(t, f.request("GET", path, "", "", ""), 404)
	expectSourceStatus(t, f.request("PUT", path, body, "policy-set", `"3"`), 404)
	expectSourceStatus(t, f.request("PUT", path, body, "denied-new", `"4"`), 404)
}

func TestPublishPolicyHTTPConcurrentCASAndRollback(t *testing.T) {
	f := newSourceHTTPFixture(t)
	feed := f.feed("policy-race", "oci")
	source := f.source("policy-race", "local", "oci")
	attachPolicySource(t, f, feed, source, 1, 1)
	path := "/api/v1/feeds/" + feed.ID + "/publish-policy"
	body := fmt.Sprintf(`{"default_source_id":%q,"rules":[]}`, source.ID)
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses <- f.request("PUT", path, body, fmt.Sprintf("policy-race-%d", i), `"2"`).Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	wins, stale := 0, 0
	for status := range statuses {
		if status == 200 {
			wins++
		} else if status == 412 {
			stale++
		} else {
			t.Fatal(status)
		}
	}
	if wins != 1 || stale != 1 {
		t.Fatal("policy CAS did not select one winner")
	}
	if _, err := f.db.Exec(context.Background(), `ALTER TABLE outbox_events ADD CONSTRAINT fail_policy CHECK(topic<>'feed.publish_policy') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.request("PUT", path, `{"rules":[]}`, "policy-failed", `"3"`), 503)
	if f.count("SELECT revision FROM feeds WHERE id=$1", feed.ID) != 3 || f.count("SELECT count(*) FROM publish_policies WHERE feed_id=$1 AND default_source_id=$2", feed.ID, source.ID) != 1 || f.count("SELECT count(*) FROM audit_events WHERE action='feed.publish_policy'") != 1 {
		t.Fatal("policy mutation partially committed")
	}
}

func TestPublishPolicyRejectsConflictingTargets(t *testing.T) {
	a := "00000000-0000-4000-8000-000000000001"
	b := "00000000-0000-4000-8000-000000000002"
	body := fmt.Sprintf(`{"rules":[{"id":"namespace","priority":10,"selector":{"namespace":"team"},"deny":false,"target_source_id":%q},{"id":"package","priority":10,"selector":{"package":"app"},"deny":false,"target_source_id":%q}]}`, a, b)
	r := httptest.NewRequest("PUT", "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if _, _, ok := readPublishPolicy(httptest.NewRecorder(), r); ok {
		t.Fatal("intersecting target rules accepted")
	}
}
