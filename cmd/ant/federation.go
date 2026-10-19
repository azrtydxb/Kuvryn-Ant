package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/observability"
	"github.com/jackc/pgx/v5/pgxpool"
)

// federationRuntime binds federation to the serving instance. It exists only
// while artifact serving is active and secret keys are configured. Nothing in
// it is on the serving path: a listener bind failure, an unreachable
// authority or a sync error is logged and reported in federation status, and
// the instance keeps serving.
type federationRuntime struct {
	svc     *federation.Service
	pool    *pgxpool.Pool
	listen  string
	joinDir string
	logger  *slog.Logger
}

func openFederation(ctx context.Context, pool *pgxpool.Pool, r *artifactRuntime, cfg config.Config, apiURL string, logger *slog.Logger) *federationRuntime {
	if r == nil || cfg.SecretKeys == nil {
		return nil
	}
	svc, err := federation.New(federation.Config{DB: pool, Keys: cfg.SecretKeys, InstanceID: r.instanceID, PublicURL: cfg.Federation.PublicURL,
		ListenAddress: cfg.Federation.Listen, APIURL: apiURL, SoftwareVersion: buildVersion(), Logger: logger,
		RouteMaxStreams: cfg.Federation.RouteMaxStreams, RouteMaxStreamsPerPrincipal: cfg.Federation.RouteStreamsPerPrincipal, RouteMaxStreamsPerClient: cfg.Federation.RouteStreamsPerClient,
		RoutePrincipalRate: cfg.Federation.RoutePrincipalRate, RouteMinTransferRate: cfg.Federation.RouteMinTransferRate, RouteTransferGrace: cfg.Federation.RouteTransferGrace, RouteTransferTimeout: cfg.Federation.RouteTransferTimeout})
	if err != nil {
		logger.Error("federation disabled", "reason", err)
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := svc.Refresh(rctx); err != nil {
		logger.Error("federation state unavailable; federation features degraded", "reason", err)
	}
	observability.RegisterCollector("federation", svc.WriteMetrics)
	return &federationRuntime{svc: svc, pool: pool, listen: cfg.Federation.Listen, joinDir: cfg.Federation.JoinDir, logger: logger}
}

func (f *federationRuntime) service() *federation.Service {
	if f == nil {
		return nil
	}
	return f.svc
}

// serve binds the dedicated mTLS listener when configured. It is a separate
// http.Server and is never mounted on the public mux.
func (f *federationRuntime) serve(ctx context.Context) {
	if f == nil || f.listen == "" {
		return
	}
	ln, err := net.Listen("tcp", f.listen)
	if err != nil {
		f.logger.Error("cannot bind federation listener; serving continues without federation", "address", f.listen)
		return
	}
	f.logger.Info("federation listener", "address", ln.Addr().String())
	go func() {
		if err := f.svc.Serve(ctx, ln); err != nil {
			f.logger.Error("federation listener stopped", "reason", err)
		}
	}()
}

// StartFederation runs the sync/ping/renewal worker and the optional
// automatic join under the instance writer lease.
func (w *backgroundWorkers) StartFederation(f *federationRuntime) {
	if w == nil || f == nil {
		return
	}
	w.done.Add(1)
	go func() { defer w.done.Done(); f.svc.Run(w.ctx) }()
	if f.joinDir != "" {
		w.done.Add(1)
		go func() { defer w.done.Done(); f.autoJoin(w.ctx) }()
	}
}

// autoJoin joins with the invitation mounted in joinDir when this instance
// has never been federated. A consumed or invalid invitation stops retrying.
func (f *federationRuntime) autoJoin(ctx context.Context) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(f.joinDir, name))
		if err != nil || len(b) > 4096 {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	// authority_cert_sha256 is optional: it pins the authority certificate
	// before the invitation is sent.
	in := federation.JoinInput{AuthorityURL: read("authority_url"), CASHA256: read("ca_sha256"), AuthorityCertSHA256: read("authority_cert_sha256"), Invitation: read("invitation")}
	if in.AuthorityURL == "" || in.CASHA256 == "" || in.Invitation == "" {
		f.logger.Error("federation auto-join skipped: authority_url, ca_sha256 and invitation files are required")
		return
	}
	for {
		st, err := f.svc.Status(ctx)
		if err == nil && st.State != "none" {
			return
		}
		if err == nil {
			owner, berr := identity.LoadBootstrapIdentity(ctx, f.pool)
			if berr == nil {
				_, err = f.svc.Join(ctx, owner.PrincipalID, in)
				if err == nil {
					f.logger.Info("federation joined automatically")
					return
				}
			} else {
				err = berr
			}
			var input *federation.InputError
			if errors.Is(err, federation.ErrInvitationInvalid) || errors.Is(err, federation.ErrDuplicateInstance) || errors.As(err, &input) {
				f.logger.Error("federation auto-join rejected; issue a new invitation", "reason", err)
				return
			}
			f.logger.Warn("federation auto-join failed; retrying", "reason", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// attachRouting wires placement-aware routing (P2-M5): proxied reads and
// signed blob URLs are answered by the unwrapped adapters (never routed
// again), and the optional entry cache uses the artifact stores under the
// writer lease.
func (f *federationRuntime) attachRouting(r *artifactRuntime) {
	if f == nil || r == nil {
		return
	}
	feeds := http.NewServeMux()
	r.Register(feeds)
	f.svc.SetRouteRuntime(federation.RouteRuntime{Feeds: feeds, Storage: r.stores.Resolve, Lease: r.manager.Lease})
}

// registerRouted mounts the adapters with routing (when federation is
// available) and the public signed blob URL endpoint.
func (f *federationRuntime) registerRouted(mux *http.ServeMux, r *artifactRuntime) {
	if f == nil {
		r.Register(mux)
		return
	}
	r.RegisterRouted(mux, f.svc.RouteFeeds)
	mux.Handle("GET /federation/blob/{name}", f.svc.BlobHandler())
}
