package samllogin

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrReplayConfiguration = errors.New("invalid SAML replay configuration or claim")
var ErrReplayStorage = errors.New("SAML replay storage unavailable")

const MaxReplayLifetime = 24 * time.Hour
const maxReplayKeys = 16
const maxReplayKeyBytes = 4096

// PostgresReplay is scoped to one trusted provider configuration in one instance.
// Neither opaque message IDs nor provider identity are stored in plaintext.
// Expired records can be pruned by a separate housekeeping task; reuse of an
// expired key is handled atomically here, without relying on timely pruning.
type PostgresReplay struct {
	db       *pgxpool.Pool
	instance string
	provider [32]byte
}

func NewPostgresReplay(pool *pgxpool.Pool, instance, providerIdentity string) (*PostgresReplay, error) {
	var id pgtype.UUID
	if pool == nil || id.Scan(instance) != nil || !id.Valid || providerIdentity == "" || len(providerIdentity) > 2048 || !utf8.ValidString(providerIdentity) || strings.TrimSpace(providerIdentity) != providerIdentity || strings.ContainsFunc(providerIdentity, unicode.IsControl) {
		return nil, ErrReplayConfiguration
	}
	return &PostgresReplay{db: pool, instance: id.String(), provider: sha256.Sum256([]byte(providerIdentity))}, nil
}

// Claim returns true only when all keys become consumed atomically. Duplicate
// input keys represent one claim. All participating locks use deterministic order
// to avoid deadlock between overlapping key sets supplied in different orders.
// The database clock decides expiry, including time spent waiting for locks.
func (s *PostgresReplay) Claim(ctx context.Context, keys []string, expires time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if s == nil || s.db == nil || len(keys) < 1 || len(keys) > maxReplayKeys {
		return false, ErrReplayConfiguration
	}
	hashes := make([][]byte, 0, len(keys))
	seen := map[[32]byte]bool{}
	locks := make([]int64, 0, len(keys))
	for _, key := range keys {
		if len(key) < 1 || len(key) > maxReplayKeyBytes {
			return false, ErrReplayConfiguration
		}
		digest := sha256.Sum256([]byte(key))
		if seen[digest] {
			continue
		}
		seen[digest] = true
		hashes = append(hashes, append([]byte(nil), digest[:]...))
		scope, _ := json.Marshal([]any{"saml-replay-v1", s.instance, s.provider, digest})
		lockHash := sha256.Sum256(scope)
		locks = append(locks, int64(binary.BigEndian.Uint64(lockHash[:8])))
	}
	sort.Slice(locks, func(i, j int) bool { return locks[i] < locks[j] })
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, ErrReplayStorage
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	for _, lock := range locks {
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lock); err != nil {
			return false, ErrReplayStorage
		}
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return false, ErrReplayStorage
	}
	if !expires.After(now) || expires.After(now.Add(MaxReplayLifetime)) {
		return false, ErrReplayConfiguration
	}
	var claimed bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM saml_replay_claims WHERE instance_id=$1 AND provider_hash=$2 AND key_hash=ANY($3::bytea[]) AND expires_at>clock_timestamp())", s.instance, s.provider[:], hashes).Scan(&claimed); err != nil {
		return false, ErrReplayStorage
	}
	if claimed {
		return false, nil
	}
	for _, hash := range hashes {
		tag, err := tx.Exec(ctx, `INSERT INTO saml_replay_claims(instance_id,provider_hash,key_hash,expires_at) VALUES($1,$2,$3,$4)
 ON CONFLICT(instance_id,provider_hash,key_hash) DO UPDATE SET created_at=clock_timestamp(),expires_at=excluded.expires_at
 WHERE saml_replay_claims.expires_at<=clock_timestamp()`, s.instance, s.provider[:], hash, expires)
		if err != nil {
			return false, ErrReplayStorage
		}
		if tag.RowsAffected() != 1 {
			return false, nil
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, ErrReplayStorage
	}
	return true, nil
}
func (PostgresReplay) String() string   { return "samllogin.PostgresReplay{redacted}" }
func (PostgresReplay) GoString() string { return "samllogin.PostgresReplay{redacted}" }

var _ ReplayStore = (*PostgresReplay)(nil)
