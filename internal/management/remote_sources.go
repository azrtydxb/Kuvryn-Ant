package management

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/feedgraph"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RemoteCredentialInput struct {
	Authorization string `json:"authorization,omitempty"`
	APIKey        string `json:"api_key,omitempty"`
}
type remoteSourceHandlers struct {
	sourceHandlers
	keys           *secrets.Keyring
	caRefs         []string
	fingerprintKey []byte
}

// fingerprintKey is the externally mounted root key. Derivation separates this
// use from encryption; stored idempotency hashes cannot become a password oracle.
// After root-key rotation, replaying an old credential request fails conflict.
func RegisterRemoteSources(mux *http.ServeMux, pool *pgxpool.Pool, keys *secrets.Keyring, allowedCARefs []string, fingerprintKey []byte) {
	s := remoteSourceHandlers{sourceHandlers: sourceHandlers{feedHandlers{db: pool}}, keys: keys, caRefs: append([]string(nil), allowedCARefs...)}
	if len(fingerprintKey) == 32 {
		mac := hmac.New(sha256.New, fingerprintKey)
		_, _ = mac.Write([]byte("kuvryn-ant/remote-credential/idempotency/v1"))
		s.fingerprintKey = mac.Sum(nil)
	}
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	mux.Handle("GET /api/v1/sources/{id}/remote-config", bearer.Wrap(http.HandlerFunc(s.getRemoteConfig)))
	mux.Handle("PUT /api/v1/sources/{id}/remote-config", bearer.Wrap(http.HandlerFunc(s.putRemoteConfig)))
	mux.Handle("PUT /api/v1/sources/{id}/remote-credential", bearer.Wrap(http.HandlerFunc(s.putRemoteCredential)))
}
func remoteError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, remoteconfig.ErrInput), errors.Is(err, secrets.ErrStoreInput):
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid remote source configuration")
	case errors.Is(err, remoteconfig.ErrNotFound), errors.Is(err, remoteconfig.ErrDenied), errors.Is(err, secrets.ErrNotFound):
		Error(w, r, 404, api.ErrorCodeNotFound, "remote source not found")
	case errors.Is(err, remoteconfig.ErrConflict), errors.Is(err, secrets.ErrRevisionConflict):
		Error(w, r, 412, api.ErrorCodePreconditionFailed, "source revision changed")
	default:
		sourceMutationError(w, r, err)
	}
}
func writeRemoteConfig(w http.ResponseWriter, dto remoteconfig.Record) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(dto.Revision, 10)))
	JSON(w, 200, dto)
}
func (s remoteSourceHandlers) authorizeRemote(w http.ResponseWriter, r *http.Request) (identity.Principal, identity.Policy, string, bool) {
	source, ok := feedID(r.PathValue("id"))
	if !ok {
		remoteError(w, r, remoteconfig.ErrInput)
		return identity.Principal{}, identity.Policy{}, "", false
	}
	p, policy, ok := s.policy(w, r)
	if !ok {
		return p, policy, source, false
	}
	if !sourceAllowed(policy, p, source, identity.Configure) {
		remoteError(w, r, remoteconfig.ErrDenied)
		return p, policy, source, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sources WHERE instance_id=$1 AND id=$2 AND kind='remote')`, p.InstanceID, source).Scan(&exists); err != nil {
		remoteError(w, r, remoteconfig.ErrUnavailable)
		return p, policy, source, false
	}
	if !exists {
		remoteError(w, r, remoteconfig.ErrNotFound)
		return p, policy, source, false
	}
	return p, policy, source, true
}
func (s remoteSourceHandlers) store(instance string) remoteconfig.Store {
	return remoteconfig.Store{DB: s.db, InstanceID: instance, AllowedCARefs: s.caRefs}
}
func (s remoteSourceHandlers) getRemoteConfig(w http.ResponseWriter, r *http.Request) {
	p, _, source, ok := s.authorizeRemote(w, r)
	if !ok {
		return
	}
	dto, err := s.store(p.InstanceID).Load(r.Context(), source)
	if err != nil {
		remoteError(w, r, err)
		return
	}
	writeRemoteConfig(w, dto)
}
func readRemoteConfig(w http.ResponseWriter, r *http.Request) (remoteconfig.Config, bool) {
	var raw map[string]json.RawMessage
	if !sourceJSON(w, r, &raw, "origin", "redirect_origins", "credential_id", "ca_ref", "allow_private", "timeout_seconds", "cache") {
		return remoteconfig.Config{}, false
	}
	fail := func() (remoteconfig.Config, bool) {
		remoteError(w, r, remoteconfig.ErrInput)
		return remoteconfig.Config{}, false
	}
	for _, key := range []string{"origin", "redirect_origins", "allow_private", "timeout_seconds", "cache"} {
		if raw[key] == nil {
			return fail()
		}
	}
	cacheFields := []string{"metadata_ttl_seconds", "negative_ttl_seconds", "stale_if_error_seconds", "max_entries", "max_bytes"}
	if _, err := exactObject(raw["cache"], cacheFields, cacheFields); err != nil {
		return fail()
	}
	encoded, _ := json.Marshal(raw)
	var c remoteconfig.Config
	if json.Unmarshal(encoded, &c) != nil {
		return fail()
	}
	normalized, err := remoteconfig.Normalize(c)
	if err != nil {
		return fail()
	}
	return normalized, true
}
func (s remoteSourceHandlers) putRemoteConfig(w http.ResponseWriter, r *http.Request) {
	p, policy, source, ok := s.authorizeRemote(w, r)
	if !ok {
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match source revision required")
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	c, ok := readRemoteConfig(w, r)
	if !ok {
		return
	}
	canonical, _ := json.Marshal(struct {
		Config   remoteconfig.Config
		Revision int64
	}{c, revision})
	var applyErr error
	result, err := database.ExecuteMutation(r.Context(), s.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "source.remote.configure", Resource: source, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		dto, e := s.store(p.InstanceID).SetTx(ctx, tx, p, policy, source, revision, c)
		if e != nil {
			applyErr = e
			return nil, e
		}
		// The new origin may name a feed of this instance that resolves back here.
		if loop := feedgraph.Check(ctx, tx, p.InstanceID); loop != nil {
			applyErr = loop
			return nil, loop
		}
		return json.Marshal(dto)
	})
	s.remoteResult(w, r, result, err, applyErr)
}
func validRemoteCredential(input RemoteCredentialInput) bool {
	if input.Authorization == "" && input.APIKey == "" {
		return false
	}
	for _, value := range []string{input.Authorization, input.APIKey} {
		if len(value) > 8192 || !utf8.ValidString(value) || value != "" && strings.TrimSpace(value) == "" {
			return false
		}
		for _, r := range value {
			if r < 0x20 || r > 0x7e {
				return false
			}
		}
	}
	return true
}
func (s remoteSourceHandlers) putRemoteCredential(w http.ResponseWriter, r *http.Request) {
	p, policy, source, ok := s.authorizeRemote(w, r)
	if !ok {
		return
	}
	if s.keys == nil || len(s.fingerprintKey) != 32 {
		remoteError(w, r, remoteconfig.ErrUnavailable)
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match source revision required")
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	var input RemoteCredentialInput
	if !sourceJSON(w, r, &input, "authorization", "api_key") {
		return
	}
	if !validRemoteCredential(input) {
		remoteError(w, r, remoteconfig.ErrInput)
		return
	}
	canonical, _ := json.Marshal(struct {
		Credential RemoteCredentialInput
		Revision   int64
	}{input, revision})
	defer clear(canonical)
	mac := hmac.New(sha256.New, s.fingerprintKey)
	_, _ = mac.Write(canonical)
	var fingerprint [32]byte
	copy(fingerprint[:], mac.Sum(nil))
	plaintext, _ := json.Marshal(input)
	defer clear(plaintext)
	var applyErr error
	result, err := database.ExecuteMutation(r.Context(), s.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "source.remote.credential", Resource: source, Key: key, Fingerprint: fingerprint, NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		dto, e := s.changeCredential(ctx, tx, p, policy, source, revision, plaintext)
		if e != nil {
			applyErr = e
			return nil, e
		}
		return json.Marshal(dto)
	})
	s.remoteResult(w, r, result, err, applyErr)
}
func (s remoteSourceHandlers) changeCredential(ctx context.Context, tx pgx.Tx, p identity.Principal, policy identity.Policy, source string, expected int64, plaintext []byte) (remoteconfig.Record, error) {
	var revision int64
	err := tx.QueryRow(ctx, `SELECT revision FROM sources WHERE instance_id=$1 AND id=$2 AND kind='remote' FOR UPDATE`, p.InstanceID, source).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return remoteconfig.Record{}, remoteconfig.ErrNotFound
	}
	if err != nil {
		return remoteconfig.Record{}, remoteconfig.ErrUnavailable
	}
	if revision != expected {
		return remoteconfig.Record{}, remoteconfig.ErrConflict
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT config FROM remote_source_configs WHERE instance_id=$1 AND source_id=$2`, p.InstanceID, source).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return remoteconfig.Record{}, remoteconfig.ErrNotFound
	}
	if err != nil || len(raw) > 32768 {
		return remoteconfig.Record{}, remoteconfig.ErrUnavailable
	}
	var c remoteconfig.Config
	if json.Unmarshal(raw, &c) != nil {
		return remoteconfig.Record{}, remoteconfig.ErrUnavailable
	}
	secretStore := secrets.Store{Keys: s.keys}
	purpose := remoteconfig.CredentialPurpose(source)
	if c.CredentialID == "" {
		record, err := secretStore.CreateTx(ctx, tx, p.InstanceID, p.PrincipalID, purpose, plaintext)
		if err != nil {
			return remoteconfig.Record{}, err
		}
		c.CredentialID = record.ID
	} else {
		var secretRevision int64
		err := tx.QueryRow(ctx, `SELECT revision FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 AND purpose=$3 FOR UPDATE`, p.InstanceID, c.CredentialID, purpose).Scan(&secretRevision)
		if err != nil {
			return remoteconfig.Record{}, remoteconfig.ErrUnavailable
		}
		if _, err = secretStore.UpdateTx(ctx, tx, p.InstanceID, p.PrincipalID, c.CredentialID, purpose, secretRevision, plaintext); err != nil {
			return remoteconfig.Record{}, err
		}
	}
	return s.store(p.InstanceID).SetTx(ctx, tx, p, policy, source, expected, c)
}
func (s remoteSourceHandlers) remoteResult(w http.ResponseWriter, r *http.Request, result database.MutationResult, err, applyErr error) {
	if err != nil {
		if applyErr != nil {
			err = applyErr
		}
		if rejection(w, r, err) {
			return
		}
		remoteError(w, r, err)
		return
	}
	var dto remoteconfig.Record
	if json.Unmarshal(result.JSON, &dto) != nil {
		remoteError(w, r, remoteconfig.ErrUnavailable)
		return
	}
	writeRemoteConfig(w, dto)
}
