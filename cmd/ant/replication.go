package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/http"
	"os"
	"strconv"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
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
	instance string
	history  config.ReplicationHistory
}

// replicationFingerprintKey derives the MAC key for idempotent peer-token
// requests from the root key, separated from every other use of it.
func replicationFingerprintKey(root []byte) []byte {
	if len(root) != 32 {
		return nil
	}
	mac := hmac.New(sha256.New, root)
	_, _ = mac.Write([]byte("kuvryn-ant/replication/idempotency/v1"))
	return mac.Sum(nil)
}

// openReplication returns nil when artifact serving is inactive or secret keys
// are not configured (peer credentials cannot then be stored).
func openReplication(pool *pgxpool.Pool, r *artifactRuntime, cfg config.Config) (*replicationRuntime, error) {
	if r == nil || cfg.SecretKeys == nil {
		return nil, nil
	}
	service, err := replication.New(replication.Config{
		DB: pool, Keys: cfg.SecretKeys, InstanceID: r.instanceID, Lease: r.manager.Lease,
		Storage: r.stores.Resolve, CAs: cfg.UpstreamCAs, FingerprintKey: replicationFingerprintKey(cfg.RootKey), LagAlertThreshold: lagAlertThreshold(),
	})
	if err != nil {
		return nil, err
	}
	service.RegisterMetrics()
	return &replicationRuntime{pool: pool, service: service, exporter: replication.Exporter{DB: pool, Storage: r.stores.Resolve, Lease: r.manager.Lease, Consumers: replication.NewConsumerTracker()},
		instance: r.instanceID, history: cfg.Replication}, nil
}

// Register mounts the peer-facing export API and the management API.
func (rr *replicationRuntime) Register(mux *http.ServeMux) {
	if rr != nil {
		replication.RegisterExport(mux, rr.pool, rr.exporter)
		rr.service.Register(mux, identity.Tokens{DB: rr.pool})
	}
}

// lagAlertThreshold is the number of changes a job may trail its peer before a
// replication.job.lag event is raised (ANT_REPLICATION_LAG_ALERT_CHANGES, 0
// disables, default 10000).
func lagAlertThreshold() int64 {
	if v := os.Getenv("ANT_REPLICATION_LAG_ALERT_CHANGES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return 10000
}

// attachFederation enables coordinated replication: the export API on the
// federation listener for federation-mode peers, the rule reconciler and job
// adoption. It must run before the federation listener serves.
func (rr *replicationRuntime) attachFederation(fed *federation.Service) {
	if rr != nil && fed != nil {
		rr.service.AttachFederation(fed, rr.exporter)
	}
}
