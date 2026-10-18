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
	"syscall"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/console"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/azrtydxb/Kuvryn-Ant/internal/mcp"
	"github.com/azrtydxb/Kuvryn-Ant/internal/server"
)

func run(ctx context.Context, logger *slog.Logger) int { return execute(ctx, logger, nil, os.Stdout) }

func execute(ctx context.Context, logger *slog.Logger, args []string, output io.Writer) int {
	if len(args) > 0 && (args[0] == "backup" || args[0] == "restore") {
		return backupCommand(ctx, logger, args, output)
	}
	mode := "serve"
	name := "local"
	if len(args) > 0 {
		mode = args[0]
	}
	if mode != "serve" && mode != "bootstrap" && mode != "bootstrap-token" && mode != "rotate-secrets" && mode != "gc" || len(args) > 2 || len(args) > 1 && mode != "bootstrap" && mode != "gc" || mode == "gc" && len(args) == 2 && args[1] != "--dry-run" {
		logger.Error("usage: ant [serve | bootstrap [name] | bootstrap-token | rotate-secrets | gc [--dry-run] | backup | restore]")
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
	defer clear(cfg.RootKey)
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
	ops := newOperations(pool, artifacts, cfg)
	ops.start(workers)
	management.EnableStatusDetail(pool, ops.readiness)
	replicationRT, err := openReplication(pool, artifacts, cfg)
	if err != nil {
		logger.Error("replication startup rejected")
		return 1
	}
	workers.StartReplication(replicationRT)
	serveCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()
	if artifacts != nil {
		go func() {
			select {
			case <-artifacts.manager.Done():
				stopServing()
			case <-serveCtx.Done():
			}
		}()
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
		for _, adapter := range origins {
			if err = adapter.SetPublicURL(publicURL); err != nil {
				logger.Error("invalid artifact public origin")
				return 1
			}
		}
	}

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
		management.RegisterWebhooks(mux, pool, cfg.SecretKeys)
		management.RegisterOperations(mux, ops.deps(management.NewCursorCodec(cfg.RootKey)))
		replicationRT.Register(mux)
		artifacts.Register(mux)
		browser.Register(mux)
		mcp.Register(mux, pool, logger)
		mux.Handle("/console", console.Handler())
		mux.Handle("/console/", console.Handler())
	})
	svc.WrapHTTP(browser.Middleware)
	svc.WrapHTTP(ops.traffic.Middleware)
	if err := svc.Serve(serveCtx, listener); err != nil {
		logger.Error("HTTP service failed to drain or serve")
		return 1
	}
	if artifacts != nil && ctx.Err() == nil && artifacts.manager.Err() != nil {
		logger.Error("instance ownership lost")
		return 1
	}
	logger.Info("service stopped")
	return 0
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := execute(ctx, logger, os.Args[1:], os.Stdout)
	stop()
	os.Exit(code)
}
