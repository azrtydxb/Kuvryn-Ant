package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/server"
)

func run(ctx context.Context, logger *slog.Logger) int {
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
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Error("cannot bind HTTP listener")
		return 1
	}
	defer listener.Close()
	logger.Info("service listening", "address", listener.Addr().String())
	svc := server.New(pool, cfg.MaxRequests, cfg.ShutdownTimeout, logger)
	if err := svc.Serve(ctx, listener); err != nil {
		logger.Error("HTTP service failed to drain or serve")
		return 1
	}
	logger.Info("service stopped")
	return 0
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, logger)
	stop()
	os.Exit(code)
}
