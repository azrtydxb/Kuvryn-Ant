package management

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/auditsink"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
)

// AuditSink is the audit streaming configuration. The credential is write-only:
// only HasCredential is returned. Revision 1 with Configured=false is the
// virtual revision of an unconfigured instance (use it as If-Match to create).
type AuditSink struct {
	Configured      bool       `json:"configured"`
	Kind            string     `json:"kind,omitempty"`
	Endpoint        string     `json:"endpoint,omitempty"`
	Enabled         bool       `json:"enabled"`
	HasCredential   bool       `json:"has_credential"`
	Checkpoint      int64      `json:"checkpoint"`
	LastForwardedAt *time.Time `json:"last_forwarded_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	Revision        int64      `json:"revision"`
}

// PutAuditSink configures the sink. Credential is sent as a bearer token for
// http sinks; omit it to keep the stored one, send an empty string to remove it.
type PutAuditSink struct {
	Kind       string  `json:"kind"`
	Endpoint   string  `json:"endpoint"`
	Enabled    *bool   `json:"enabled,omitempty"`
	Credential *string `json:"credential,omitempty"`
}

const sinkColumns = `kind,endpoint,enabled,secret_ref IS NOT NULL,checkpoint,last_forwarded_at,COALESCE(last_error,''),revision`

func scanSink(row pgx.Row) (AuditSink, error) {
	s := AuditSink{Configured: true}
	err := row.Scan(&s.Kind, &s.Endpoint, &s.Enabled, &s.HasCredential, &s.Checkpoint, &s.LastForwardedAt, &s.LastError, &s.Revision)
	return s, err
}

func (h opsHandlers) getAuditSink(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	s, err := scanSink(h.db.QueryRow(ctx, `SELECT `+sinkColumns+` FROM audit_sinks WHERE instance_id=$1`, p.InstanceID))
	if errors.Is(err, pgx.ErrNoRows) {
		s, err = AuditSink{Revision: 1}, nil
	}
	if err != nil {
		identityError(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(s.Revision, 10)))
	JSON(w, 200, s)
}

func (h opsHandlers) putAuditSink(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	var in PutAuditSink
	if !sourceJSON(w, r, &in, "kind", "endpoint", "enabled", "credential") {
		return
	}
	endpoint, err := auditsink.ValidateEndpoint(in.Kind, in.Endpoint)
	if err != nil || in.Credential != nil && (len(*in.Credential) > 4096 || in.Kind != auditsink.KindHTTP) {
		identityInput(w, r)
		return
	}
	if in.Credential != nil && *in.Credential != "" && h.d.Keys == nil {
		identityError(w, r, secrets.ErrKeyConfiguration)
		return
	}
	enabled := in.Enabled == nil || *in.Enabled
	h.mutate(w, r, p, "audit-sink.put", "audit-sink", struct {
		In       PutAuditSink
		Revision int64
	}{in, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		store := secrets.Store{DB: h.db, Keys: h.d.Keys}
		var current int64
		var ref *string
		err := tx.QueryRow(ctx, `SELECT revision,secret_ref::text FROM audit_sinks WHERE instance_id=$1 FOR UPDATE`, p.InstanceID).Scan(&current, &ref)
		exists := err == nil
		if errors.Is(err, pgx.ErrNoRows) {
			current, err = 1, nil
		}
		if err != nil {
			return nil, err
		}
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		// Credential handling: nil keeps; "" detaches; otherwise stores encrypted.
		newRef := ref
		if in.Credential != nil {
			switch {
			case *in.Credential == "":
				newRef = nil
			case ref != nil:
				rec, _, openErr := store.Open(ctx, p.InstanceID, *ref, auditsink.Purpose)
				if openErr != nil {
					return nil, openErr
				}
				plain := []byte(*in.Credential)
				defer clear(plain)
				if _, err = store.UpdateTx(ctx, tx, p.InstanceID, p.PrincipalID, *ref, auditsink.Purpose, rec.Revision, plain); err != nil {
					return nil, err
				}
			default:
				plain := []byte(*in.Credential)
				defer clear(plain)
				rec, createErr := store.CreateTx(ctx, tx, p.InstanceID, p.PrincipalID, auditsink.Purpose, plain)
				if createErr != nil {
					return nil, createErr
				}
				newRef = &rec.ID
			}
		}
		var sink AuditSink
		if exists {
			sink, err = scanSink(tx.QueryRow(ctx, `UPDATE audit_sinks SET kind=$2,endpoint=$3,enabled=$4,secret_ref=$5::uuid,last_error=NULL,revision=revision+1,updated_at=now() WHERE instance_id=$1 RETURNING `+sinkColumns, p.InstanceID, in.Kind, endpoint, enabled, newRef))
		} else {
			// A new sink forwards from "now": existing history is not replayed.
			sink, err = scanSink(tx.QueryRow(ctx, `INSERT INTO audit_sinks(instance_id,kind,endpoint,enabled,secret_ref,checkpoint,revision) VALUES($1,$2,$3,$4,$5::uuid,COALESCE((SELECT max(id) FROM audit_events WHERE instance_id=$1),0),2) RETURNING `+sinkColumns, p.InstanceID, in.Kind, endpoint, enabled, newRef))
		}
		if err != nil {
			return nil, err
		}
		return sink, identityEvent(ctx, tx, p, p.InstanceID, "audit-sink.put", sink.Revision)
	})
}

func (h opsHandlers) deleteAuditSink(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	h.mutate(w, r, p, "audit-sink.delete", "audit-sink", revision, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		var current int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM audit_sinks WHERE instance_id=$1 FOR UPDATE`, p.InstanceID).Scan(&current); err != nil {
			return nil, err
		}
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		if _, err := tx.Exec(ctx, `DELETE FROM audit_sinks WHERE instance_id=$1`, p.InstanceID); err != nil {
			return nil, err
		}
		return AuditSink{Revision: 1}, identityEvent(ctx, tx, p, p.InstanceID, "audit-sink.delete", current)
	})
}
