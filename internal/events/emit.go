package events

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Emit records a system event: an audit row plus an outbox row in the caller's
// transaction, so the event commits (or not) together with the state change it
// reports. principalID may be empty for events raised by background workers.
// resourceID, when non-empty, must be a UUID. The topic doubles as the audit
// action. Deliveries are fanned out to enabled subscriptions of the instance
// by the delivery worker.
func Emit(ctx context.Context, tx pgx.Tx, instanceID, principalID, topic, resourceID string, payload any) error {
	if instanceID == "" || topic == "" || len(topic) > 256 {
		return errors.New("invalid event")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var audit int64
	if err = tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,details) VALUES($1,NULLIF($2::text,'')::uuid,$3,NULLIF($4::text,'')::uuid,$5) RETURNING id`,
		instanceID, principalID, topic, resourceID, body).Scan(&audit); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,$3)`, audit, topic, body)
	return err
}
