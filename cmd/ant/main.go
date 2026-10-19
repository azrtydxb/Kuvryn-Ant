package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/console"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/denyaudit"
	"github.com/azrtydxb/Kuvryn-Ant/internal/diagnostics"
	"github.com/azrtydxb/Kuvryn-Ant/internal/diskusage"
	"github.com/azrtydxb/Kuvryn-Ant/internal/feedgraph"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/limits"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/azrtydxb/Kuvryn-Ant/internal/mcp"
	"github.com/azrtydxb/Kuvryn-Ant/internal/observability"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/prewarm"
	"github.com/azrtydxb/Kuvryn-Ant/internal/server"
	"github.com/azrtydxb/Kuvryn-Ant/internal/tracing"
	"github.com/azrtydxb/Kuvryn-Ant/internal/transfer"
	"github.com/jackc/pgx/v5/pgxpool"
)

func run(ctx context.Context, logger *slog.Logger) int { return execute(ctx, logger, nil, os.Stdout) }

// codeReload asks execute to start serving again in-process.
const codeReload = -2

// execute runs one command. A server that started on a database without an
// instance restarts itself in-process once ant bootstrap creates one, so the
// data routes appear without restarting the pod.
func execute(ctx context.Context, logger *slog.Logger, args []string, output io.Writer) int {
	for {
		if code := executeOnce(ctx, logger, args, output); code != codeReload {
			return code
		}
		logger.Info("instance bootstrapped; reloading to serve artifacts")
	}
}

// watchBootstrap stops serving (reload) when an instance appears in a database
// that had none at startup.
func watchBootstrap(ctx context.Context, pool *pgxpool.Pool, interval time.Duration, onBootstrap func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var one int
			qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := pool.QueryRow(qctx, "SELECT 1 FROM bootstrap_state WHERE singleton=true").Scan(&one)
			cancel()
			if err == nil {
				onBootstrap()
				return
			}
		}
	}
}

func executeOnce(ctx context.Context, logger *slog.Logger, args []string, output io.Writer) int {
	if len(args) > 0 && args[0] == "diagnostics" {
		return diagnosticsCommand(ctx, logger, args[1:], output)
	}
	if len(args) > 0 && args[0] == "repair" {
		return repairCommand(ctx, logger, args[1:], output)
	}
	if len(args) > 0 && (args[0] == "backup" || args[0] == "restore") {
		return backupCommand(ctx, logger, args, output)
	}
	if len(args) > 0 && args[0] == "storage" {
		return storageCommand(ctx, logger, args, output)
	}
	if len(args) > 0 && args[0] == "federation" {
		return federationCommand(ctx, logger, args[1:], output)
	}
	if len(args) > 0 && args[0] == "admin" {
		return adminCommand(ctx, logger, args[1:], os.Stdin, output)
	}
	if len(args) > 0 && (args[0] == "export" || args[0] == "import" || args[0] == "migrate" || args[0] == "compare") {
		return transferCommand(ctx, logger, args, output)
	}
	mode := "serve"
	name := "local"
	if len(args) > 0 {
		mode = args[0]
	}
	if mode != "serve" && mode != "bootstrap" && mode != "bootstrap-token" && mode != "rotate-secrets" && mode != "gc" || len(args) > 2 || len(args) > 1 && mode != "bootstrap" && mode != "gc" || mode == "gc" && len(args) == 2 && args[1] != "--dry-run" {
		logger.Error("usage: ant [serve | bootstrap [name] | bootstrap-token | rotate-secrets | gc [--dry-run] | storage | diagnostics [-o FILE] | repair metadata|oci-index-children [--dry-run] | backup | restore | admin break-glass|local-user | export | import | migrate | compare | federation status|re-identify|purge|recovery]")
		return 1
	}
	if len(args) == 2 {
		name = args[1]
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("configuration rejected", "reason", err)
		return 1
	}
	prepareStagingDir(logger, cfg.Storage.Directory, mode == "serve")
	defer clear(cfg.RootKey)
	identity.SetTrustedProxies(cfg.TrustedProxies)
	cfg.Tracing.Version = buildVersion()
	shutdownTracing, err := tracing.Setup(ctx, cfg.Tracing)
	if err != nil {
		logger.Error("tracing configuration rejected", "reason", err)
		return 1
	}
	defer func() { _ = shutdownTracing(context.WithoutCancel(ctx)) }()
	if cfg.Tracing.Enabled {
		cfg.Database.ConnConfig.Tracer = tracing.PGTracer{}
	}
	pool, err := database.Open(ctx, cfg.Database)
	if err != nil {
		logger.Error("startup failed", "reason", err)
		return 1
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		logger.Error("schema initialization failed", "reason", err)
		return 1
	}
	if mode == "rotate-secrets" {
		if err := rotateSecrets(ctx, pool, cfg, output); err != nil {
			logger.Error("secret rotation failed")
			return 1
		}
		return 0
	}
	if mode == "gc" {
		if err := runGC(ctx, pool, cfg, len(args) == 2, output); err != nil {
			logger.Error("garbage collection failed", "reason", err)
			return 1
		}
		return 0
	}
	if mode != "serve" {
		var owner identity.BootstrapIdentity
		if mode == "bootstrap" {
			owner, err = identity.Bootstrap(ctx, pool, name)
		} else {
			owner, err = identity.LoadBootstrapIdentity(ctx, pool)
		}
		if err != nil {
			logger.Error("bootstrap failed", "reason", err)
			return 1
		}
		scopes := []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Configure, identity.Replicate, identity.Admin}}}
		token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, scopes, time.Now().Add(24*time.Hour))
		if err != nil {
			logger.Error("token issuance failed; use bootstrap-token to retry", "reason", err)
			return 1
		}
		result := struct {
			InstanceID  string    `json:"instance_id"`
			PrincipalID string    `json:"principal_id"`
			TokenID     string    `json:"token_id"`
			Token       string    `json:"token"`
			ExpiresAt   time.Time `json:"expires_at"`
		}{owner.InstanceID, owner.PrincipalID, token.ID, token.Secret, token.ExpiresAt}
		if err := json.NewEncoder(output).Encode(result); err != nil {
			logger.Error("cannot deliver bootstrap credential; use bootstrap-token to retry")
			return 1
		}
		return 0
	}
	artifact.SetUploadSlots(cfg.UploadSlots)
	artifacts, err := openArtifacts(ctx, pool, cfg)
	if err != nil {
		logger.Error("artifact startup rejected", "reason", err)
		return 1
	}
	defer artifacts.Close()
	var servingInstance string
	if artifacts != nil {
		servingInstance = artifacts.instanceID
	}
	browser, err := openBrowser(ctx, pool, cfg, servingInstance)
	if err != nil {
		logger.Error("browser login startup rejected")
		return 1
	}
	workers, err := startWorkers(ctx, pool, artifacts, cfg.SecretKeys, browser)
	if err != nil {
		logger.Error("background worker startup rejected")
		return 1
	}
	defer workers.Close()
	workers.startRetention(pool, artifacts, cfg.Retention, logger)
	workers.startAuditPurge(pool, artifacts, cfg.AuditRetention, logger)
	ops := newOperations(pool, artifacts, cfg)
	ops.start(workers)
	management.EnableStatusDetail(pool, ops.readiness)
	replicationRT, err := openReplication(pool, artifacts, cfg)
	if err != nil {
		logger.Error("replication startup rejected")
		return 1
	}
	workers.StartReplication(replicationRT, logger)
	serveCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()
	var reload atomic.Bool
	if artifacts != nil {
		go func() {
			select {
			case <-artifacts.manager.Done():
				stopServing()
			case <-serveCtx.Done():
			}
		}()
	} else if cfg.InstanceID == "" {
		go watchBootstrap(serveCtx, pool, bootstrapPoll, func() { reload.Store(true); stopServing() })
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Error("cannot bind HTTP listener")
		return 1
	}
	defer listener.Close()
	publicURL := cfg.PublicURL
	if artifacts != nil {
		if publicURL == "" {
			host, _, e := net.SplitHostPort(listener.Addr().String())
			ip := net.ParseIP(host)
			if e != nil || ip == nil || !ip.IsLoopback() {
				logger.Error("ANT_PUBLIC_URL required for non-loopback artifact serving")
				return 1
			}
			publicURL = "http://" + listener.Addr().String()
		}
		parsed, e := url.Parse(publicURL)
		if e != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
			logger.Error("invalid ANT_PUBLIC_URL")
			return 1
		}
		origins := []interface{ SetPublicURL(string) error }{artifacts.nuget, artifacts.npm, artifacts.cargo, artifacts.gitlfs, artifacts.composer, artifacts.pubrepo, artifacts.conan, artifacts.conda, artifacts.ansible, artifacts.cran, artifacts.apk, artifacts.apt, artifacts.rpm, artifacts.terraform, artifacts.swift, artifacts.cocoapods, artifacts.huggingface, artifacts.p2}
		artifacts.terraform.SetSigningKey(cfg.RootKey)
		for _, adapter := range origins {
			if err = adapter.SetPublicURL(publicURL); err != nil {
				logger.Error("invalid artifact public origin")
				return 1
			}
		}
	}

	// Remote sources and replication peers that point back at this instance are
	// rejected at configuration time (feed resolution loops).
	feedgraph.SetSelfOrigins(publicURL, "http://"+listener.Addr().String())
	observability.RegisterCollector("limits", limits.Default.WriteMetrics)
	observability.RegisterCollector("prewarm", prewarm.WriteMetrics)
	// Free space of the data directory's filesystem, for disk-full alerts.
	observability.RegisterCollector("datadir", diskusage.Collector(cfg.Storage.Directory))
	// Loopback development: let the login runtime learn its own origin.
	browser.SetDefaultOrigin(serveCtx, publicURL)
	clientAuth, githubOIDC := newClientAuth(pool, artifacts, browser)
	// Federation is optional and never on the serving path (Phase 2, D1/D4).
	fed := openFederation(ctx, pool, artifacts, cfg, publicURL, logger)
	if svc := fed.service(); svc != nil {
		// Federated principals and group bindings are evaluated at sign-in.
		oidclogin.SetLoginHook(svc)
	}
	// Coordinated replication rides on federation (export over mTLS, rules).
	replicationRT.attachFederation(fed.service())
	fed.attachRouting(artifacts)
	workers.StartFederation(fed)
	fed.serve(serveCtx)
	logger.Info("service listening", "address", listener.Addr().String())
	svc := server.New(pool, cfg.MaxRequests, cfg.ShutdownTimeout, logger, func(mux *http.ServeMux) {
		management.RegisterFeeds(mux, pool, management.NewCursorCodec(cfg.RootKey))
		management.RegisterSources(mux, pool, management.NewCursorCodec(cfg.RootKey))
		management.RegisterPublishPolicies(mux, pool)
		management.RegisterPackages(mux, pool, management.NewCursorCodec(cfg.RootKey))
		management.RegisterConsole(mux, pool, management.NewCursorCodec(cfg.RootKey))
		caRefs := make([]string, 0, len(cfg.UpstreamCAs))
		for ref := range cfg.UpstreamCAs {
			caRefs = append(caRefs, ref)
		}
		management.RegisterRemoteSources(mux, pool, cfg.SecretKeys, caRefs, cfg.RootKey)
		management.RegisterIdentities(mux, pool)
		management.RegisterIdentityProviders(mux, management.IdentityAdminDeps{Pool: pool, Keys: cfg.SecretKeys, Origin: browser.Origin(), Reload: browser.Reload, Active: browser.Active, FileProviders: browser.FileProviders()})
		clientAuthDeps := management.ClientAuthDeps{Pool: pool, GitHub: githubOIDC}
		if clientAuth != nil {
			clientAuthDeps.Invalidate = clientAuth.Invalidate
		}
		management.RegisterClientAuth(mux, clientAuthDeps)
		management.RegisterWebhooks(mux, pool, cfg.SecretKeys)
		management.RegisterOperations(mux, ops.deps(management.NewCursorCodec(cfg.RootKey)))
		management.RegisterFederation(mux, pool, fed.service())
		management.RegisterFederationSearch(mux, pool, fed.service(), management.NewCursorCodec(cfg.RootKey))
		var transferStores transfer.Stores
		if artifacts != nil {
			transferStores = artifacts.stores
		}
		management.RegisterTransfer(mux, management.TransferDeps{Pool: pool, Stores: transferStores})
		replicationRT.Register(mux)
		fed.registerRouted(mux, artifacts)
		browser.Register(mux)
		mcp.Register(mux, pool, logger)
		mux.Handle("/console", console.Handler())
		mux.Handle("/console/", console.Handler())
		mux.Handle("GET /{$}", rootRedirect())
	})
	// Installed inside the browser-session middleware so session principals
	// are visible to per-principal limits.
	svc.WrapHTTP(ops.limiter.Middleware)
	svc.WrapHTTP(browser.Middleware)
	if clientAuth != nil {
		svc.WrapHTTP(clientAuth.Middleware)
	}
	svc.WrapHTTP(ops.traffic.Middleware)
	if servingInstance != "" {
		// Outermost: records 401/403 outcomes (rate limited) for management, MCP and package protocols.
		svc.WrapHTTP((&denyaudit.Recorder{DB: pool, InstanceID: servingInstance}).Middleware)
	}
	if err := svc.Serve(serveCtx, listener); err != nil {
		logger.Error("HTTP service failed to drain or serve")
		return 1
	}
	if reload.Load() && ctx.Err() == nil {
		return codeReload
	}
	if artifacts != nil && ctx.Err() == nil && artifacts.manager.Err() != nil {
		logger.Error("instance ownership lost")
		return 1
	}
	logger.Info("service stopped")
	return 0
}

func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}

func main() {
	errorRing = diagnostics.NewRing(500)
	logger := slog.New(observability.RedactHandler(tracing.LogHandler(diagnostics.NewHandler(slog.NewJSONHandler(os.Stderr, nil), errorRing))))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := execute(ctx, logger, os.Args[1:], os.Stdout)
	stop()
	os.Exit(code)
}

// bootstrapPoll is how often a server without an instance looks for one.
var bootstrapPoll = 2 * time.Second

// rootRedirect sends exactly "/" to the console; "/{$}" matches no other path.
func rootRedirect() http.Handler {
	return http.RedirectHandler("/console/", http.StatusFound)
}
