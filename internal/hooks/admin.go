package hooks

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// SendTest queues (in q, usually a transaction; call Notify after it commits) a sample delivery for a hook, as the console's "Send test"
// button does. For an HMAC hook the sample is signed with the hook's real secret
// and passed through the same verification as a live call, so a broken secret or
// format shows up here. For the other modes the call is recorded as a test
// without a credential. It returns the new delivery's ID.
func (s *Service) SendTest(ctx context.Context, q Querier, hook Hook) (string, error) {
	body, headers := SamplePayload(hook.Action)
	if hook.AuthMode == AuthHMAC {
		keys, err := s.signingKeys(ctx, hook.SigningSecretID)
		if err != nil {
			return "", err
		}
		defer clearKeys(keys)
		name, value := SignBody(hook.HMACFormat, body, keys[0])
		headers[name] = value
		req := headerOf(headers)
		if !VerifyHMAC(hook.HMACFormat, req, body, keys) {
			return "", ErrUnavailable
		}
	}
	id, err := s.enqueue(ctx, q, newDelivery{hook: &hook, ip: "console", auth: AuthTest, body: body, headers: pickHeaders(headerOf(headers))})
	if err != nil {
		return "", err
	}
	return id, nil
}

// Redeliver queues a copy of a logged delivery that still has its payload, for
// testing an action again. Replay protection does not apply to a redelivery.
func (s *Service) Redeliver(ctx context.Context, q Querier, hook Hook, delivery string) (string, error) {
	var payload []byte
	var headers []byte
	var state string
	err := q.QueryRow(ctx, `SELECT payload,headers,state FROM incoming_hook_deliveries WHERE instance_id=$1 AND hook_id=$2 AND id=$3`, s.cfg.InstanceID, hook.ID, delivery).Scan(&payload, &headers, &state)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (payload == nil || state == StateRejected)) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var h map[string]string
	_ = json.Unmarshal(headers, &h)
	id, err := s.enqueue(ctx, q, newDelivery{hook: &hook, ip: "console", auth: AuthRedelivery, body: payload, headers: h, redeliveryOf: delivery})
	if err != nil {
		return "", err
	}
	return id, nil
}
