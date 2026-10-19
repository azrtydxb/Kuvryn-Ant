package federation

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func TestNilMembershipIsEmpty(t *testing.T) {
	var m *Membership
	if _, ok := m.Member(tfA); ok {
		t.Fatal("nil membership reported a member")
	}
	if m.Revoked("x") {
		t.Fatal("nil membership reported a revocation")
	}
}

// TestStatusWithoutRefresh covers the CLI path: `ant federation status` builds
// a Service and calls Status without ever calling Refresh, so the in-memory
// trust document is nil while the database holds an applied topology.
func TestStatusWithoutRefresh(t *testing.T) {
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("PostgreSQL required")
		}
		t.Skip("PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "ant_fedcli_" + strings.ToLower(rand.Text())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.MaxConns = 1
	pool, err := database.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "Fed")
	if err != nil {
		t.Fatal(err)
	}

	f := newTestFed(t)
	doc := f.docs[0]
	doc.Members[0].InstanceID = owner.InstanceID
	doc.Authority.InstanceID = owner.InstanceID
	now := time.Now().UTC()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if err = storeDocument(ctx, tx, owner.InstanceID, doc); err != nil {
		t.Fatal(err)
	}
	topo := &TopologyDocument{Kind: "topology", FederationID: tfID, FederationName: "unit", Generation: 1, AuthorityEpoch: 1, IssuedAt: now.Format(time.RFC3339Nano)}
	topo.Normalize()
	body, _ := CanonicalJSON(topo)
	if _, err = tx.Exec(ctx, `INSERT INTO federation_documents(instance_id,federation_id,kind,generation,authority_epoch,hash,prev_hash,body) VALUES($1,$2,'topology',1,1,$4,'',$3)`, owner.InstanceID, tfID, body, TopologyHash(topo)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO federation_state(instance_id,federation_id,name,role,state,applied_generation,applied_hash,applied_at,topology_generation,topology_hash,topology_applied_at)
VALUES($1,$2,'unit','member','active',1,$3,$4,1,$5,$4) ON CONFLICT(instance_id) DO UPDATE SET federation_id=$2,name='unit',role='member',state='active',applied_generation=1,applied_hash=$3,applied_at=$4,topology_generation=1,topology_hash=$5,topology_applied_at=$4`,
		owner.InstanceID, tfID, Hash(doc), now, TopologyHash(topo)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	svc, err := New(Config{DB: pool, InstanceID: owner.InstanceID, PublicURL: tfURL})
	if err != nil {
		t.Fatal(err)
	}
	st, err := svc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Topology == nil || st.Topology.Generation != 1 || st.Members.Active != 1 {
		t.Fatalf("unexpected status: %+v", st)
	}
}
