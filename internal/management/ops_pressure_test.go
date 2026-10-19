package management

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/retention"
)

func TestRetentionSettingsPressureAndStorageUsage(t *testing.T) {
	o := newOpsFixture(t)
	idle := 72 * time.Hour
	svc := &retention.Service{DB: o.db, InstanceID: o.instance, Stores: func(context.Context, string) (blob.Store, error) { return nil, nil },
		Backend: func(context.Context) (string, error) { return "s3", nil }, Env: retention.Overrides{DefaultMaxIdle: &idle}}
	deps := o.deps
	deps.Retention = func(string) *retention.Service { return svc }
	deps.GC = GCSettingsView{IntervalSeconds: 21600, GraceSeconds: 86400, PressureIntervalSeconds: 300, PressureSweepGraceSeconds: 900}
	mux := http.NewServeMux()
	RegisterOperations(mux, deps)
	o.handler = WithRequestID(mux)

	w := o.get("/api/v1/retention/settings")
	expectSourceStatus(t, w, 200)
	s := decodeJSON[RetentionSettings](t, w)
	if s.Revision != 1 || s.DefaultPolicy.MaxIdleSeconds == nil || *s.DefaultPolicy.MaxIdleSeconds != 72*3600 || !s.DefaultPolicy.Enabled || s.GC.IntervalSeconds != 21600 {
		t.Fatalf("settings %+v", s)
	}
	locked := map[string]bool{}
	for _, l := range s.Locked {
		locked[l] = true
	}
	if !locked[retention.LockDefaultMaxIdle] || locked[retention.LockCapacityBytes] {
		t.Fatalf("locked %v", s.Locked)
	}

	body := `{"default_policy":{"enabled":true,"max_idle_seconds":259200,"min_keep":2,"include_tagged":false},"pressure":{"enabled":true,"high_percent":85,"low_percent":75,"capacity_bytes":1000000000}}`
	expectSourceStatus(t, o.request("PUT", "/api/v1/retention/settings", body, "r1", `"1"`), 200)
	expectSourceStatus(t, o.request("PUT", "/api/v1/retention/settings", body, "r2", `"1"`), 412)
	// A locked field cannot be changed, inverted watermarks are refused.
	locked409 := `{"default_policy":{"enabled":true,"max_idle_seconds":3600,"min_keep":1,"include_tagged":false},"pressure":{"enabled":true,"high_percent":85,"low_percent":75}}`
	expectSourceStatus(t, o.request("PUT", "/api/v1/retention/settings", locked409, "r3", `"2"`), 409)
	inverted := `{"default_policy":{"enabled":false,"min_keep":1,"include_tagged":false},"pressure":{"enabled":true,"high_percent":60,"low_percent":70}}`
	expectSourceStatus(t, o.request("PUT", "/api/v1/retention/settings", inverted, "r4", `"2"`), 400)
	if o.count(`SELECT count(*) FROM audit_events WHERE action='retention.settings.put'`) != 1 {
		t.Fatal("audit missing")
	}

	// With a quota, object storage reports a pressure status and the usage
	// endpoint explains unreferenced versus reclaimable bytes.
	st := decodeJSON[retention.PressureStatus](t, o.get("/api/v1/retention/pressure"))
	if !st.Enabled || st.State != retention.StateOK || st.CapacitySource != "quota" || st.CapacityBytes != 1000000000 {
		t.Fatalf("pressure %+v", st)
	}
	u := decodeJSON[StorageUsage](t, o.get("/api/v1/storage/usage"))
	if u.Pressure == nil || u.ReclaimableBytes == nil || u.InGraceBytes == nil {
		t.Fatalf("usage %+v", u)
	}

	// Without a quota, object storage has no capacity and says so.
	noQuota := `{"default_policy":{"enabled":true,"max_idle_seconds":259200,"min_keep":2,"include_tagged":false},"pressure":{"enabled":true,"high_percent":85,"low_percent":75}}`
	expectSourceStatus(t, o.request("PUT", "/api/v1/retention/settings", noQuota, "r5", `"2"`), 200)
	st = decodeJSON[retention.PressureStatus](t, o.get("/api/v1/retention/pressure"))
	if st.State != retention.StateNoCapacity || st.Detail == "" {
		t.Fatalf("no quota: %+v", st)
	}
	expectSourceStatus(t, o.request("GET", "/api/v1/retention/pressure", "", "", ""), 200)
}
