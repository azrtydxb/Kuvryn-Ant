package federation

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// State is this instance's federation_state row.
type State struct {
	FederationID        string
	Name                string
	Role                string
	State               string
	AuthorityURL        string
	FederationURL       string
	CASHA256            string
	CASecretID          string
	AuthoritySecretID   string
	MemberSecretID      string
	MemberCert          string
	MemberCertSerial    string
	MemberCertExpiresAt *time.Time
	AppliedGeneration   int64
	AppliedHash         string
	AppliedAt           *time.Time
	LastSyncAttemptAt   *time.Time
	LastSyncSuccessAt   *time.Time
	LastSyncError       string
	LastSyncSource      string
	Revision            int64
}

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const stateColumns = `coalesce(federation_id::text,''),coalesce(name,''),coalesce(role,''),state,coalesce(authority_url,''),coalesce(federation_url,''),
coalesce(ca_sha256,''),coalesce(ca_secret_id::text,''),coalesce(authority_secret_id::text,''),coalesce(member_secret_id::text,''),coalesce(member_cert,''),
coalesce(member_cert_serial,''),member_cert_expires_at,applied_generation,applied_hash,applied_at,last_sync_attempt_at,last_sync_success_at,last_sync_error,last_sync_source,revision`

func scanState(row pgx.Row) (State, error) {
	var s State
	err := row.Scan(&s.FederationID, &s.Name, &s.Role, &s.State, &s.AuthorityURL, &s.FederationURL, &s.CASHA256, &s.CASecretID, &s.AuthoritySecretID,
		&s.MemberSecretID, &s.MemberCert, &s.MemberCertSerial, &s.MemberCertExpiresAt, &s.AppliedGeneration, &s.AppliedHash, &s.AppliedAt,
		&s.LastSyncAttemptAt, &s.LastSyncSuccessAt, &s.LastSyncError, &s.LastSyncSource, &s.Revision)
	return s, err
}

// loadState returns the state row, or the zero state "none" when absent.
func loadState(ctx context.Context, q queryer, instance string) (State, error) {
	s, err := scanState(q.QueryRow(ctx, `SELECT `+stateColumns+` FROM federation_state WHERE instance_id=$1`, instance))
	if errors.Is(err, pgx.ErrNoRows) {
		return State{State: "none"}, nil
	}
	return s, err
}

// lockState creates the row if needed and locks it for the transaction, which
// serializes every federation state change on this instance.
func lockState(ctx context.Context, tx pgx.Tx, instance string) (State, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO federation_state(instance_id) VALUES($1) ON CONFLICT DO NOTHING`, instance); err != nil {
		return State{}, err
	}
	return scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+` FROM federation_state WHERE instance_id=$1 FOR UPDATE`, instance))
}

func loadDocument(ctx context.Context, q queryer, instance, federation string, generation int64) (*Membership, error) {
	var body []byte
	err := q.QueryRow(ctx, `SELECT body FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='membership' AND generation=$3`, instance, federation, generation).Scan(&body)
	if err != nil {
		return nil, err
	}
	var m Membership
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// documentsAfter returns up to limit stored documents after a generation.
func documentsAfter(ctx context.Context, q queryer, instance, federation string, after int64, limit int) ([]Membership, error) {
	rows, err := q.Query(ctx, `SELECT body FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='membership' AND generation>$3 ORDER BY generation LIMIT $4`, instance, federation, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var m Membership
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// storeDocument appends a verified document and materializes its members.
func storeDocument(ctx context.Context, tx pgx.Tx, instance string, doc *Membership) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO federation_documents(instance_id,federation_id,kind,generation,authority_epoch,hash,prev_hash,body) VALUES($1,$2,'membership',$3,$4,$5,$6,$7)
ON CONFLICT(instance_id,federation_id,kind,generation) DO NOTHING`,
		instance, doc.FederationID, doc.Generation, doc.AuthorityEpoch, Hash(doc), doc.PrevHash, body); err != nil {
		return err
	}
	for _, m := range doc.Members {
		labels, _ := json.Marshal(nonNil(m.Labels))
		var joined *time.Time
		if t, err := time.Parse(time.RFC3339Nano, m.JoinedAt); err == nil {
			joined = &t
		}
		if _, err := tx.Exec(ctx, `INSERT INTO federation_members(instance_id,federation_id,member_instance_id,name,api_url,federation_url,cert_sha256,cert_serial,signing_key,labels,status,software_version,protocol_min,protocol_max,joined_at,membership_generation)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
ON CONFLICT(instance_id,federation_id,member_instance_id) DO UPDATE SET name=EXCLUDED.name,api_url=EXCLUDED.api_url,federation_url=EXCLUDED.federation_url,
cert_sha256=EXCLUDED.cert_sha256,cert_serial=EXCLUDED.cert_serial,signing_key=EXCLUDED.signing_key,labels=EXCLUDED.labels,status=EXCLUDED.status,
software_version=EXCLUDED.software_version,protocol_min=EXCLUDED.protocol_min,protocol_max=EXCLUDED.protocol_max,joined_at=EXCLUDED.joined_at,membership_generation=EXCLUDED.membership_generation`,
			instance, doc.FederationID, m.InstanceID, m.Name, m.APIURL, m.FederationURL, m.CertSHA256, m.CertSerial, m.SigningKey, labels, m.Status,
			m.SoftwareVersion, m.Protocol.Min, m.Protocol.Max, joined, doc.Generation); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE federation_state SET applied_generation=$2,applied_hash=$3,applied_at=clock_timestamp(),revision=revision+1,updated_at=clock_timestamp() WHERE instance_id=$1`,
		instance, doc.Generation, Hash(doc))
	return err
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// audit records a federation event and its outbox message. principal is empty
// for peer- or worker-initiated events; resource is a UUID or empty.
func audit(ctx context.Context, tx pgx.Tx, instance, principal, action, resource, result string, details any) error {
	if !uuidPattern.MatchString(principal) {
		principal = "" // peer- or worker-initiated: recorded in details, not as a principal
	}
	if !uuidPattern.MatchString(resource) {
		resource = ""
	}
	extra, err := json.Marshal(details)
	if err != nil {
		return err
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,details,result) VALUES($1,nullif($2,'')::uuid,$3,nullif($4,'')::uuid,$5,$6) RETURNING id`,
		instance, principal, action, resource, extra, result).Scan(&id); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,jsonb_build_object('resource_id',$3::text)||$4::jsonb)`, id, action, resource, extra)
	return err
}
