package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/localadmin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// adminCommand implements local, host-privileged recovery commands. It requires
// the same configuration as the server (database and the root keyring); anyone
// who can run it already controls the deployment, which is the break-glass model.
//
//	ant admin break-glass [--password-stdin]
func adminCommand(ctx context.Context, logger *slog.Logger, args []string, in io.Reader, output io.Writer) int {
	if len(args) == 0 || args[0] != "break-glass" || len(args) > 2 || len(args) == 2 && args[1] != "--password-stdin" {
		logger.Error("usage: ant admin break-glass [--password-stdin]")
		return 1
	}
	fromStdin := len(args) == 2
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("configuration rejected", "reason", err)
		return 1
	}
	defer clear(cfg.RootKey)
	if cfg.SecretKeys == nil {
		logger.Error("the root keyring is required (ANT_ROOT_KEY_* configuration)")
		return 1
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
	if err := runBreakGlass(ctx, pool, cfg, in, fromStdin, output); err != nil {
		logger.Error("break-glass reset failed", "reason", err)
		return 1
	}
	return 0
}

// runBreakGlass creates or resets the break-glass administrator for the
// bootstrapped instance. The reason strings returned never contain the password.
func runBreakGlass(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, in io.Reader, fromStdin bool, output io.Writer) error {
	owner, err := identity.LoadBootstrapIdentity(ctx, pool)
	if err != nil {
		return errors.New("no bootstrapped instance found; run `ant bootstrap` first")
	}
	password := ""
	if fromStdin {
		line, err := bufio.NewReader(io.LimitReader(in, 1024)).ReadString('\n')
		if err != nil && err != io.EOF {
			return errors.New("cannot read password")
		}
		password = strings.TrimRight(line, "\r\n")
	} else if password, err = localadmin.GeneratePassword(); err != nil {
		return errors.New("cannot generate password")
	}
	svc := &localadmin.Service{DB: pool, Keys: cfg.SecretKeys, InstanceID: owner.InstanceID}
	created, err := svc.ResetBreakGlass(ctx, password)
	if err != nil {
		return err
	}
	result := map[string]any{"username": localadmin.BreakGlassUser, "created": created, "mfa": "disabled", "sessions": "revoked", "lockout": "cleared"}
	if !fromStdin {
		result["password"] = password
		result["note"] = "shown once; sign in at /console/ under Emergency access"
	}
	return json.NewEncoder(output).Encode(result)
}
