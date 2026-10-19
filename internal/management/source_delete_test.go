package management

import (
	"fmt"
	"testing"
	"time"
)

func TestDeleteSource(t *testing.T) {
	c := newConsoleFixture(t)
	del := func(s Source, key, force, token string) int {
		path := "/api/v1/sources/" + s.ID
		if force != "" {
			path += "?force=" + force
		}
		return c.requestAs("DELETE", path, "", key, fmt.Sprintf(`"%d"`, s.Revision), token).Code
	}

	// An unused source goes away; replaying the key returns the same result.
	empty := c.source("empty", "local", "raw")
	if code := del(empty, "del-empty", "", c.configure); code != 200 {
		t.Fatalf("delete empty source: %d", code)
	}
	if code := del(empty, "del-empty", "", c.configure); code != 200 {
		t.Fatalf("idempotent replay: %d", code)
	}
	if n := c.count(`SELECT count(*) FROM sources WHERE id=$1`, empty.ID); n != 0 {
		t.Fatal("source still present")
	}
	if c.auditCount("source.delete") != 1 {
		t.Fatalf("audit events: %d", c.auditCount("source.delete"))
	}
	if code := del(empty, "del-empty-2", "", c.configure); code != 404 {
		t.Fatalf("second delete: %d", code)
	}

	// Attached or populated sources are refused without force, and force is admin-only.
	used := c.source("used", "remote", "raw")
	feed := c.attach(c.feed("f", "raw"), used, 0)
	c.seed(used.ID, "pkg/a", "pkg", time.Hour)
	if code := del(used, "del-used", "", c.configure); code != 409 {
		t.Fatalf("attached source deleted without force: %d", code)
	}
	if code := del(used, "del-used-force-denied", "true", c.configure); code != 403 {
		t.Fatalf("force without admin: %d", code)
	}
	if code := del(used, "del-used-missing-if-match", "false", c.readOnly); code != 404 && code != 403 {
		t.Fatalf("read-only caller: %d", code)
	}
	if code := del(Source{ID: used.ID, Revision: used.Revision + 5}, "del-stale", "true", c.credential); code != 412 {
		t.Fatalf("stale revision: %d", code)
	}
	w := c.requestAs("DELETE", "/api/v1/sources/"+used.ID+"?force=true", "", "del-used-force", fmt.Sprintf(`"%d"`, used.Revision), c.credential)
	expectSourceStatus(t, w, 200)
	out := decode[SourceDeleted](t, w)
	if out.FeedsDetached != 1 || out.ComponentsRemoved != 1 || out.AssetsRemoved != 1 {
		t.Fatalf("force result: %+v", out)
	}
	if n := c.count(`SELECT count(*) FROM feed_sources WHERE source_id=$1`, used.ID) + c.count(`SELECT count(*) FROM components WHERE source_id=$1`, used.ID); n != 0 {
		t.Fatal("attachments or content left behind")
	}
	if n := c.count(`SELECT revision FROM feeds WHERE id=$1`, feed.ID); int64(n) <= feed.Revision {
		t.Fatalf("feed revision not bumped: %d", n)
	}

}
