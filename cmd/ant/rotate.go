package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rotateSecrets is a local recovery operation, authorized by possession of the
// deployment's database and external root keys. It never issues a login token.
// One bounded transaction permits interrupted operators to safely resume.
func rotateSecrets(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, output io.Writer) error {
	owner, err := identity.LoadBootstrapIdentity(ctx, pool)
	if err != nil {
		return err
	}
	if cfg.InstanceID != "" && !strings.EqualFold(cfg.InstanceID, owner.InstanceID) {
		return errors.New("rotation instance does not match local recovery identity")
	}
	result, err := (secrets.Store{DB: pool, Keys: cfg.SecretKeys}).RotateBatch(ctx, owner.InstanceID, owner.PrincipalID, 100)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		Rotated    int `json:"rotated"`
		BatchLimit int `json:"batch_limit"`
	}{result.Rotated, 100})
}
