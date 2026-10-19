package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

const federationUsage = "usage: ant federation status | re-identify --confirm | purge --confirm | recovery sign-rotation|sign-authority-cert|sign-challenge ..."

// federationCommand runs the offline federation operations: status (local
// view from the database), re-identify (give a restored copy a new instance
// ID before it joins; §13) and purge (delete federation rows after leaving,
// so a downgrade is possible; §15), and the offline recovery signer
// (federation_recovery.go, no database). re-identify and purge refuse while a
// serving process holds the writer lease and require --confirm.
func federationCommand(ctx context.Context, logger *slog.Logger, args []string, output io.Writer) int {
	if len(args) > 0 && args[0] == "recovery" {
		return federationRecoveryCommand(logger, args[1:], os.Stdin, output)
	}
	if len(args) == 0 || len(args) > 2 || (args[0] != "status" && args[0] != "re-identify" && args[0] != "purge") ||
		(args[0] == "status" && len(args) != 1) || (args[0] != "status" && (len(args) != 2 || args[1] != "--confirm")) {
		logger.Error(federationUsage)
		return 1
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
	owner, err := identity.LoadBootstrapIdentity(ctx, pool)
	if err != nil {
		logger.Error("no bootstrapped instance", "reason", err)
		return 1
	}
	instance := owner.InstanceID
	if cfg.InstanceID != "" && !strings.EqualFold(cfg.InstanceID, instance) {
		logger.Error("ANT_INSTANCE_ID does not match the local recovery identity")
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	enc := json.NewEncoder(output)
	enc.SetIndent("", "  ")
	switch args[0] {
	case "status":
		svc, err := federation.New(federation.Config{DB: pool, Keys: cfg.SecretKeys, InstanceID: instance, PublicURL: cfg.Federation.PublicURL, ListenAddress: cfg.Federation.Listen, SoftwareVersion: buildVersion(), Logger: logger})
		if err != nil {
			logger.Error("federation unavailable", "reason", err)
			return 1
		}
		st, err := svc.Status(ctx)
		if err != nil {
			logger.Error("federation status unavailable", "reason", err)
			return 1
		}
		_ = enc.Encode(st)
	case "re-identify":
		if cfg.SecretKeys == nil {
			logger.Error("the root keyring is required to re-seal secrets (ANT_ROOT_KEY_* configuration)")
			return 1
		}
		fresh, err := federation.ReIdentify(ctx, pool, cfg.SecretKeys, instance)
		if err != nil {
			logger.Error("re-identify failed; nothing was changed", "reason", err)
			return 1
		}
		_ = enc.Encode(map[string]string{"previous_instance_id": instance, "instance_id": fresh,
			"note": "federation state reset to none; update ANT_INSTANCE_ID if it is set, then start the instance and join with a new invitation"})
	case "purge":
		if err := federation.Purge(ctx, pool, instance); err != nil {
			logger.Error("purge failed; nothing was changed", "reason", err)
			return 1
		}
		_, _ = fmt.Fprintln(output, `{"purged": true}`)
	}
	return 0
}
