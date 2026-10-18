package main

import (
	"net/http"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/replication"
	"github.com/jackc/pgx/v5/pgxpool"
)

// replicationRuntime binds the export API (this instance as an authority) and
// the destination worker/management API (this instance as a replica) to the
// serving instance. It exists only while artifact serving is active, and the
// worker runs only under the instance writer lease.
type replicationRuntime struct {
	pool     *pgxpool.Pool
	service  *replication.Service
	exporter replication.Exporter
}

// openReplication returns nil when artifact serving is inactive or secret keys
// are not configured (peer credentials cannot then be stored).
func openReplication(pool *pgxpool.Pool, r *artifactRuntime, cfg config.Config) (*replicationRuntime, error) {
	if r == nil || cfg.SecretKeys == nil {
		return nil, nil
	}
	service, err := replication.New(replication.Config{
		DB: pool, Keys: cfg.SecretKeys, InstanceID: r.instanceID, Lease: r.manager.Lease,
		Storage: r.stores.Resolve, CAs: cfg.UpstreamCAs,
	})
	if err != nil {
		return nil, err
	}
	return &replicationRuntime{pool: pool, service: service, exporter: replication.Exporter{DB: pool, Storage: r.stores.Resolve}}, nil
}

// Register mounts the peer-facing export API and the management API.
func (rr *replicationRuntime) Register(mux *http.ServeMux) {
	if rr != nil {
		replication.RegisterExport(mux, rr.pool, rr.exporter)
		rr.service.Register(mux, identity.Tokens{DB: rr.pool})
	}
}
