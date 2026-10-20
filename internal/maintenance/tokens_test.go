package maintenance

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
)

// tokenRow inserts a service or personal token whose expiry / revocation lie
// the given durations in the past (negative: in the future) and returns its id.
func (f *fixture) tokenRow(t *testing.T, kind, principal string, expired, revoked time.Duration, isRevoked bool) string {
	t.Helper()
	secs := func(d time.Duration) string { return fmt.Sprintf("%d seconds", int(d.Seconds())) }
	table, name := "service_tokens", ""
	if kind == "personal" {
		table, name = "personal_access_tokens", ",name"
	}
	vals := ""
	if kind == "personal" {
		vals = ",'t'"
	}
	revokedSQL := "CASE WHEN $5::boolean THEN now()-$3::interval END"
	var id string
	err := f.pool.QueryRow(context.Background(), `INSERT INTO `+table+`(instance_id,principal_id`+name+`,verifier,scopes,created_at,expires_at,revoked_at)
 VALUES($1,$4`+vals+`,decode(md5(random()::text)||md5(random()::text),'hex'),'[]',now()-interval '1000 days',now()-$2::interval,`+revokedSQL+`) RETURNING id::text`,
		f.instance, secs(expired), secs(revoked), principal, isRevoked).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) principal(t *testing.T, kind, subject string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO principals(instance_id,kind,subject) VALUES($1,$2,$3) RETURNING id::text`, f.instance, kind, subject).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) purge(days int) TokenPurge {
	return TokenPurge{DB: f.pool, InstanceID: f.instance, BatchSize: 2, Days: func(context.Context) (int, error) { return days, nil }}
}

func TestTokenPurgeAgeBoundariesAndActiveTokensSurvive(t *testing.T) {
	f := newFixture(t)
	svc := f.principal(t, "service", "robot")
	user := f.principal(t, "user", "alice")
	hour := time.Hour
	old := f.tokenRow(t, "service", svc, 31*day, 0, false)
	justOver := f.tokenRow(t, "service", svc, 30*day+hour, 0, false)
	justUnder := f.tokenRow(t, "service", svc, 30*day-hour, 0, false)
	active := f.tokenRow(t, "service", svc, -10*day, 0, false)
	revokedOld := f.tokenRow(t, "service", svc, -10*day, 31*day, true)    // revoked long ago, never expired
	revokedNew := f.tokenRow(t, "service", svc, -10*day, 2*day, true)     // revoked recently
	expiredRevokedNew := f.tokenRow(t, "service", svc, 90*day, day, true) // expired long ago, revoked yesterday: dead since expiry
	patOld := f.tokenRow(t, "personal", user, 45*day, 0, false)
	patNew := f.tokenRow(t, "personal", user, 3*day, 0, false)
	patActive := f.tokenRow(t, "personal", user, -day, 0, false)
	patRevokedOld := f.tokenRow(t, "personal", user, -5*day, 60*day, true)

	dry, err := CountDeadTokens(context.Background(), f.pool, f.instance, TokenCleanupCutoff(time.Now(), 30))
	if err != nil || dry.ServiceTokens != 4 || dry.PersonalAccessTokens != 2 {
		t.Fatalf("dry run counted %+v %v", dry, err)
	}
	report, err := f.purge(30).Run(context.Background())
	if err != nil || report.ServiceTokens != 4 || report.PersonalAccessTokens != 2 {
		t.Fatalf("purge %+v %v", report, err)
	}
	gone := []string{old, justOver, revokedOld, expiredRevokedNew}
	for _, id := range gone {
		if f.count(t, `SELECT count(*) FROM service_tokens WHERE id=$1`, id) != 0 {
			t.Fatalf("token %s should be gone", id)
		}
	}
	for _, id := range []string{justUnder, active, revokedNew} {
		if f.count(t, `SELECT count(*) FROM service_tokens WHERE id=$1`, id) != 1 {
			t.Fatalf("token %s must survive", id)
		}
	}
	for _, id := range []string{patOld, patRevokedOld} {
		if f.count(t, `SELECT count(*) FROM personal_access_tokens WHERE id=$1`, id) != 0 {
			t.Fatalf("pat %s should be gone", id)
		}
	}
	for _, id := range []string{patNew, patActive} {
		if f.count(t, `SELECT count(*) FROM personal_access_tokens WHERE id=$1`, id) != 1 {
			t.Fatalf("pat %s must survive", id)
		}
	}
	// One summary event per run with the counts; a second run has nothing to do and records nothing.
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action=$1 AND principal_id IS NULL AND (details->>'service_tokens')::int=4 AND (details->>'personal_access_tokens')::int=2 AND details->>'trigger'='scheduled' AND (details->>'after_days')::int=30`, TokenCleanupAction); n != 1 {
		t.Fatalf("summary events: %d", n)
	}
	if again, err := f.purge(30).Run(context.Background()); err != nil || again.Total() != 0 {
		t.Fatalf("second run %+v %v", again, err)
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE action=$1`, TokenCleanupAction) != 1 {
		t.Fatal("empty run wrote an audit event")
	}
	var out bytes.Buffer
	WriteTokenMetrics(&out)
	if !strings.Contains(out.String(), `ant_token_cleanup_deleted_total{kind="service",trigger="scheduled"}`) {
		t.Fatalf("metrics: %s", out.String())
	}
}

func TestTokenPurgeBatchesAndDisabled(t *testing.T) {
	f := newFixture(t)
	svc := f.principal(t, "service", "robot")
	for i := 0; i < 7; i++ { // batch size 2: needs several batches
		f.tokenRow(t, "service", svc, 40*day, 0, false)
	}
	if r, err := f.purge(0).Run(context.Background()); err != nil || !r.Disabled || r.Total() != 0 {
		t.Fatalf("0 days must never delete: %+v %v", r, err)
	}
	if f.count(t, `SELECT count(*) FROM service_tokens`) != 7 {
		t.Fatal("disabled purge deleted rows")
	}
	r, err := f.purge(30).Run(context.Background())
	if err != nil || r.ServiceTokens != 7 || f.count(t, `SELECT count(*) FROM service_tokens`) != 0 {
		t.Fatalf("batched purge %+v %v", r, err)
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE action=$1`, TokenCleanupAction) != 1 {
		t.Fatal("expected a single summary event for all batches")
	}
}

func TestTokenPurgeKeepsAuditReferences(t *testing.T) {
	f := newFixture(t)
	svc := f.principal(t, "service", "robot")
	id := f.tokenRow(t, "service", svc, 90*day, 0, false)
	f.exec(t, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,'token.issue',$3),($1,$2,'token.revoke',$3)`, f.instance, svc, id)
	if r, err := f.purge(30).Run(context.Background()); err != nil || r.ServiceTokens != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE resource_id=$1`, id) != 2 {
		t.Fatal("audit references must survive token deletion")
	}
}

func TestTokenPurgeIsFencedByLease(t *testing.T) {
	f := newFixture(t)
	svc := f.principal(t, "service", "robot")
	f.tokenRow(t, "service", svc, 90*day, 0, false)
	p := f.purge(30)
	p.Lease = func() ownership.Lease {
		return ownership.Lease{InstanceID: f.instance, OwnerID: "00000000-0000-0000-0000-000000000009", Epoch: 7, ExpiresAt: time.Now().Add(time.Minute)}
	}
	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("purge ran without the writer lease")
	}
	if f.count(t, `SELECT count(*) FROM service_tokens`) != 1 || f.count(t, `SELECT count(*) FROM audit_events WHERE action=$1`, TokenCleanupAction) != 0 {
		t.Fatal("fenced purge changed data")
	}
}

func TestEffectiveTokenCleanupDays(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if d, src, err := EffectiveTokenCleanupDays(ctx, f.pool, f.instance, nil); err != nil || d != 30 || src != "default" {
		t.Fatalf("default: %d %s %v", d, src, err)
	}
	f.exec(t, `INSERT INTO instance_settings(instance_id,token_cleanup_after_days) VALUES($1,0)`, f.instance)
	if d, src, _ := EffectiveTokenCleanupDays(ctx, f.pool, f.instance, nil); d != 0 || src != "stored" {
		t.Fatalf("stored zero must mean never: %d %s", d, src)
	}
	env := 7
	if d, src, _ := EffectiveTokenCleanupDays(ctx, f.pool, f.instance, &env); d != 7 || src != "env" {
		t.Fatalf("env wins: %d %s", d, src)
	}
}
