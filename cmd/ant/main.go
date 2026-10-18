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
	"github.com/azrtydxb/Kuvryn-Ant/internal/server"
)

func run(ctx context.Context, logger *slog.Logger) int { return execute(ctx, logger, nil, os.Stdout) }

func execute(ctx context.Context, logger *slog.Logger, args []string, output io.Writer) int {
	mode := "serve"
	name := "local"
	if len(args) > 0 {
		mode = args[0]
	}
	if mode != "serve" && mode != "bootstrap" && mode != "bootstrap-token" || len(args) > 2 || len(args) > 1 && mode != "bootstrap" {
		logger.Error("usage: ant [serve | bootstrap [name] | bootstrap-token]")
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
		if err = artifacts.nuget.SetPublicURL(publicURL); err != nil {
			logger.Error("invalid NuGet public origin")
			return 1
		}
		if err = artifacts.npm.SetPublicURL(publicURL); err != nil {
			logger.Error("invalid npm public origin")
			return 1
		}
		if err = artifacts.cargo.SetPublicURL(publicURL); err != nil {
			logger.Error("invalid artifact public origin")
			return 1
		}
	}

	logger.Info("service listening", "address", listener.Addr().String())
	svc := server.New(pool, cfg.MaxRequests, cfg.ShutdownTimeout, logger, func(mux *http.ServeMux) {
		management.RegisterFeeds(mux, pool, management.NewCursorCodec(cfg.RootKey))
		management.RegisterSources(mux, pool, management.NewCursorCodec(cfg.RootKey))
		management.RegisterPublishPolicies(mux, pool)
		management.RegisterIdentities(mux, pool)
		management.RegisterWebhooks(mux, pool, cfg.SecretKeys)
		artifacts.Register(mux)
		browser.Register(mux)
		mux.Handle("/console", console.Handler())
		mux.Handle("/console/", console.Handler())
	})
	svc.WrapHTTP(browser.Middleware)
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
