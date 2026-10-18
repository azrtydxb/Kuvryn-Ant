package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PublishSelector struct {
	Namespace   string `json:"namespace,omitempty"`
	Package     string `json:"package,omitempty"`
	PathPrefix  string `json:"path_prefix,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
}
type PublishRule struct {
	ID             string          `json:"id"`
	Priority       int64           `json:"priority"`
	Selector       PublishSelector `json:"selector"`
	TargetSourceID string          `json:"target_source_id,omitempty"`
	Deny           bool            `json:"deny"`
}
type PublishPolicyInput struct {
	DefaultSourceID string        `json:"default_source_id"`
	Rules           []PublishRule `json:"rules"`
}
type PublishPolicy struct {
	FeedID          string        `json:"feed_id"`
	Revision        int64         `json:"revision"`
	DefaultSourceID string        `json:"default_source_id"`
	Rules           []PublishRule `json:"rules"`
}

func RegisterPublishPolicies(mux *http.ServeMux, pool *pgxpool.Pool) {
	handler := sourceHandlers{feedHandlers{db: pool}}
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	mux.Handle("GET /api/v1/feeds/{id}/publish-policy", bearer.Wrap(http.HandlerFunc(handler.getPublishPolicy)))
	mux.Handle("PUT /api/v1/feeds/{id}/publish-policy", bearer.Wrap(http.HandlerFunc(handler.setPublishPolicy)))
}
func policyError(w http.ResponseWriter, r *http.Request, err error) {
	var config *routing.ConfigError
	switch {
	case errors.Is(err, artifact.ErrDenied), errors.Is(err, artifact.ErrNotFound):
		Error(w, r, 404, api.ErrorCodeNotFound, "publish policy not found")
	case errors.Is(err, artifact.ErrConflict):
		Error(w, r, 412, api.ErrorCodePreconditionFailed, "feed revision changed")
	case errors.Is(err, artifact.ErrInput), errors.As(err, &config):
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid publish policy")
	default:
		sourceMutationError(w, r, err)
	}
}
func policyAllowed(policy identity.Policy, p identity.Principal, feed, defaultSource string, rules []routing.Rule) bool {
	if !feedAllowed(policy, p, feed, identity.Configure) {
		return false
	}
	if defaultSource != "" && !sourceAllowed(policy, p, defaultSource, identity.Configure) {
		return false
	}
	for _, rule := range rules {
		if rule.Target != "" && !sourceAllowed(policy, p, rule.Target, identity.Configure) {
			return false
		}
	}
	return true
}
func policyDTO(snapshot artifact.PolicySnapshot) PublishPolicy {
	dto := PublishPolicy{FeedID: snapshot.FeedID, Revision: snapshot.Revision, DefaultSourceID: snapshot.DefaultSource, Rules: []PublishRule{}}
	for _, rule := range snapshot.Rules {
		dto.Rules = append(dto.Rules, PublishRule{ID: rule.ID, Priority: int64(rule.Priority), TargetSourceID: rule.Target, Deny: rule.Deny, Selector: PublishSelector{Namespace: rule.Selector.Namespace, Package: rule.Selector.Package, PathPrefix: rule.Selector.PathPrefix, PrincipalID: rule.Selector.Principal}})
	}
	return dto
}
func writePublishPolicy(w http.ResponseWriter, dto PublishPolicy) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(dto.Revision, 10)))
	JSON(w, 200, dto)
}
func (s sourceHandlers) getPublishPolicy(w http.ResponseWriter, r *http.Request) {
	feed, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed ID")
		return
	}
	p, policy, ok := s.policy(w, r)
	if !ok {
		return
	}
	if !feedAllowed(policy, p, feed, identity.Configure) {
		policyError(w, r, artifact.ErrDenied)
		return
	}
	snapshot, err := (artifact.Policies{DB: s.db, InstanceID: p.InstanceID}).Snapshot(r.Context(), feed)
	if err != nil {
		policyError(w, r, err)
		return
	}
	if !policyAllowed(policy, p, feed, snapshot.DefaultSource, snapshot.Rules) {
		policyError(w, r, artifact.ErrDenied)
		return
	}
	writePublishPolicy(w, policyDTO(snapshot))
}
func exactObject(raw []byte, allowed, required []string) (map[string]json.RawMessage, error) {
	fields, err := readObject(json.NewDecoder(bytes.NewReader(raw)))
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, key := range allowed {
		names[key] = true
	}
	for key := range fields {
		if !names[key] {
			return nil, artifact.ErrInput
		}
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return nil, artifact.ErrInput
		}
	}
	return fields, nil
}
func readPublishPolicy(w http.ResponseWriter, r *http.Request) (PublishPolicyInput, []routing.Rule, bool) {
	var raw map[string]json.RawMessage
	if !sourceJSON(w, r, &raw, "default_source_id", "rules") {
		return PublishPolicyInput{}, nil, false
	}
	input, rules, err := parsePublishPolicy(raw)
	if err != nil {
		policyError(w, r, err)
		return PublishPolicyInput{}, nil, false
	}
	return input, rules, true
}

// parsePublishPolicy strictly validates an already-decoded policy object. It is
// shared by the saved-policy, batch-create and dry-run endpoints.
func parsePublishPolicy(raw map[string]json.RawMessage) (PublishPolicyInput, []routing.Rule, error) {
	fail := func() (PublishPolicyInput, []routing.Rule, error) {
		return PublishPolicyInput{}, nil, artifact.ErrInput
	}
	if raw["rules"] == nil {
		return fail()
	}
	var list []json.RawMessage
	if json.Unmarshal(raw["rules"], &list) != nil || len(list) > routing.MaxRules {
		return fail()
	}
	for _, rawRule := range list {
		fields, err := exactObject(rawRule, []string{"id", "priority", "selector", "target_source_id", "deny"}, []string{"id", "priority", "selector", "deny"})
		if err != nil {
			return fail()
		}
		if _, err = exactObject(fields["selector"], []string{"namespace", "package", "path_prefix", "principal_id"}, nil); err != nil {
			return fail()
		}
	}
	encoded, _ := json.Marshal(raw)
	var input PublishPolicyInput
	if json.Unmarshal(encoded, &input) != nil {
		return fail()
	}
	if input.Rules == nil {
		input.Rules = []PublishRule{}
	}
	if input.DefaultSourceID != "" {
		canonical, valid := feedID(input.DefaultSourceID)
		if !valid {
			return fail()
		}
		input.DefaultSourceID = canonical
	}
	var rules []routing.Rule
	targets := map[string]bool{}
	if input.DefaultSourceID != "" {
		targets[input.DefaultSourceID] = true
	}
	for i := range input.Rules {
		dto := &input.Rules[i]
		if dto.TargetSourceID != "" {
			canonical, valid := feedID(dto.TargetSourceID)
			if !valid {
				return fail()
			}
			dto.TargetSourceID = canonical
			targets[canonical] = true
		}
		if dto.Selector.PathPrefix != "" {
			normalized, err := routing.NormalizePath(dto.Selector.PathPrefix)
			if err != nil {
				return fail()
			}
			dto.Selector.PathPrefix = normalized
		}
		rules = append(rules, routing.Rule{ID: dto.ID, Priority: int(dto.Priority), Deny: dto.Deny, Target: dto.TargetSourceID, Selector: routing.Selector{Namespace: dto.Selector.Namespace, Package: dto.Selector.Package, PathPrefix: dto.Selector.PathPrefix, Principal: dto.Selector.PrincipalID}})
	}
	var sources []routing.Source
	for id := range targets {
		sources = append(sources, routing.Source{ID: id, Kind: routing.Local})
	}
	if _, err := routing.Compile(routing.Config{Sources: sources, Rules: rules, DefaultTarget: input.DefaultSourceID}); err != nil {
		return fail()
	}
	return input, rules, nil
}
func (s sourceHandlers) setPublishPolicy(w http.ResponseWriter, r *http.Request) {
	feed, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed ID")
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match feed revision required")
		return
	}
	input, rules, ok := readPublishPolicy(w, r)
	if !ok {
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	p, policy, ok := s.policy(w, r)
	if !ok {
		return
	}
	if !policyAllowed(policy, p, feed, input.DefaultSourceID, rules) {
		policyError(w, r, artifact.ErrDenied)
		return
	}
	canonical, _ := json.Marshal(struct {
		Policy   PublishPolicyInput
		Revision int64
	}{input, revision})
	var applyErr error
	result, err := database.ExecuteMutation(r.Context(), s.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "feed.publish_policy", Resource: feed, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		next, err := (artifact.Policies{DB: s.db, InstanceID: p.InstanceID}).SetTx(ctx, tx, p, policy, feed, revision, input.DefaultSourceID, rules)
		if err != nil {
			applyErr = err
			return nil, err
		}
		return json.Marshal(PublishPolicy{FeedID: feed, Revision: next, DefaultSourceID: input.DefaultSourceID, Rules: input.Rules})
	})
	if err != nil {
		// ExecuteMutation intentionally sanitizes arbitrary callback failures. These
		// typed domain errors carry no DB diagnostics and retain HTTP semantics.
		if applyErr != nil {
			err = applyErr
		}
		policyError(w, r, err)
		return
	}
	var dto PublishPolicy
	if json.Unmarshal(result.JSON, &dto) != nil {
		policyError(w, r, artifact.ErrUnavailable)
		return
	}
	writePublishPolicy(w, dto)
}
