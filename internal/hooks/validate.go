package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/azrtydxb/Kuvryn-Ant/internal/prewarm"
	"github.com/jackc/pgx/v5"
)

// ValidationError is a configuration problem safe to show to the administrator.
type ValidationError string

func (e ValidationError) Error() string { return string(e) }

func invalid(format string, args ...any) error { return ValidationError(fmt.Sprintf(format, args...)) }

var (
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

func canonicalUUID(s string) (string, bool) {
	if !uuidPattern.MatchString(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// Normalize trims and defaults a hook before validation.
func Normalize(h *Hook) {
	h.Slug = strings.TrimSpace(h.Slug)
	if h.RatePerMinute == 0 {
		h.RatePerMinute = 60
	}
	if h.Action.Type == ActionUpstream && h.Action.Provider == "" {
		h.Action.Provider = ProviderAuto
	}
	if h.Action.Type == ActionPrewarm && h.Action.Kind == "" {
		h.Action.Kind = "package"
	}
}

// Validate checks a hook against the schema rules and against the instance:
// referenced feeds, sources, jobs and secrets must exist and fit the action.
// Templates are rendered against a sample payload so a template that can never
// produce a valid name is rejected at configuration time.
func Validate(ctx context.Context, q Querier, instance string, h *Hook, allowHTTP bool) error {
	Normalize(h)
	if !ValidSlug(h.Slug) {
		return invalid("slug must be 1 to 63 characters: lowercase letters, digits and hyphens")
	}
	if !printable(h.Description, 1000) {
		return invalid("description must be at most 1000 printable characters")
	}
	if h.RatePerMinute < 1 || h.RatePerMinute > 6000 {
		return invalid("rate limit must be between 1 and 6000 requests per minute")
	}
	switch h.AuthMode {
	case AuthHMAC:
		if _, ok := HMACFormats[h.HMACFormat]; !ok {
			return invalid("choose a signature format: github, gitlab, ant, npm, artifactory or nexus")
		}
		id, ok := canonicalUUID(h.SigningSecretID)
		if !ok {
			return invalid("choose a signing secret")
		}
		h.SigningSecretID = id
		if !exists(ctx, q, `SELECT EXISTS(SELECT 1 FROM webhook_signing_secrets WHERE instance_id=$1 AND id=$2::uuid)`, instance, id) {
			return invalid("the signing secret does not exist")
		}
		h.BearerScope = ""
	case AuthURLToken:
		h.HMACFormat, h.SigningSecretID, h.BearerScope = "", "", ""
	case AuthBearer:
		if h.BearerScope != ScopeConfigure && h.BearerScope != ScopeReplicate && h.BearerScope != ScopeAdmin {
			return invalid("choose the scope the token must hold: configure, replicate or admin")
		}
		h.HMACFormat, h.SigningSecretID = "", ""
	default:
		return invalid("auth mode must be hmac, url_token or bearer")
	}
	return validateAction(ctx, q, instance, &h.Action, allowHTTP)
}

func exists(ctx context.Context, q Querier, sql string, args ...any) bool {
	var ok bool
	return q.QueryRow(ctx, sql, args...).Scan(&ok) == nil && ok
}

func validateAction(ctx context.Context, q Querier, instance string, a *Action, allowHTTP bool) error {
	var sample any
	if body, _ := SamplePayload(*a); body != nil {
		sample = decodePayload(body)
	}
	check := func(label, tpl string, required bool) error {
		if tpl == "" {
			if required {
				return invalid("%s is required", label)
			}
			return nil
		}
		if err := CheckTemplate(tpl); err != nil {
			return invalid("%s: %s", label, strings.TrimPrefix(err.Error(), "template error: "))
		}
		return nil
	}
	feedFormat := func(id string) (format, purpose string, err error) {
		cid, ok := canonicalUUID(id)
		if !ok {
			return "", "", invalid("choose a feed")
		}
		if e := q.QueryRow(ctx, `SELECT format,purpose FROM feeds WHERE instance_id=$1 AND id=$2::uuid`, instance, cid).Scan(&format, &purpose); e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				return "", "", invalid("the feed does not exist")
			}
			return "", "", e
		}
		return format, purpose, nil
	}
	switch a.Type {
	case ActionPrewarm:
		format, _, err := feedFormat(a.FeedID)
		if err != nil {
			return err
		}
		a.FeedID, _ = canonicalUUID(a.FeedID)
		if a.Kind != "package" && a.Kind != "image" {
			return invalid("kind must be package or image")
		}
		if err = check("name", a.Name, true); err != nil {
			return err
		}
		if err = check("version", a.Version, false); err != nil {
			return err
		}
		kind := prewarm.KindPackages
		if a.Kind == "image" {
			kind = prewarm.KindImages
		}
		if err = prewarm.CheckKind(format, kind, ""); err != nil {
			return invalid("%s", err.Error())
		}
		name, err := Render(a.Name, sample)
		if err != nil {
			return invalid("name: sample payload: %s", strings.TrimPrefix(err.Error(), "template error: "))
		}
		item := prewarm.Item{Name: name}
		if a.Version != "" {
			if item.Version, err = Render(a.Version, sample); err != nil {
				return invalid("version: sample payload: %s", strings.TrimPrefix(err.Error(), "template error: "))
			}
		}
		if _, err = prewarm.ValidateItems(format, kind, []prewarm.Item{item}); err != nil {
			return invalid("the item this template produces for a sample payload is not valid for a %s feed: %s", format, err.Error())
		}
	case ActionReplicationRun:
		id, ok := canonicalUUID(a.JobID)
		if !ok {
			return invalid("choose a replication job")
		}
		a.JobID = id
		if !exists(ctx, q, `SELECT EXISTS(SELECT 1 FROM replication_jobs WHERE instance_id=$1 AND id=$2::uuid)`, instance, id) {
			return invalid("the replication job does not exist")
		}
	case ActionPurgeSource:
		id, ok := canonicalUUID(a.SourceID)
		if !ok {
			return invalid("choose a remote source")
		}
		a.SourceID = id
		if !exists(ctx, q, `SELECT EXISTS(SELECT 1 FROM sources WHERE instance_id=$1 AND id=$2::uuid AND kind='remote')`, instance, id) {
			return invalid("the remote source does not exist")
		}
		if err := check("coordinate", a.Coordinate, false); err != nil {
			return err
		}
		if len(a.Coordinate) > 512 {
			return invalid("coordinate is too long")
		}
	case ActionCleanCache:
		_, purpose, err := feedFormat(a.FeedID)
		if err != nil {
			return err
		}
		a.FeedID, _ = canonicalUUID(a.FeedID)
		if purpose != "cache" {
			return invalid("the feed is not a cache feed (purpose cache)")
		}
		if err = check("prefix", a.Prefix, true); err != nil {
			return err
		}
		if p, e := Render(a.Prefix, sample); e != nil || p == "" || len(p) > 512 {
			return invalid("prefix: the template must produce 1 to 512 characters for a sample payload")
		}
	case ActionUpstream:
		switch a.Provider {
		case ProviderAuto, ProviderDockerHub, ProviderGitHub, ProviderGitLab, ProviderNPM, ProviderArtifactory, ProviderNexus:
		default:
			return invalid("provider must be auto, dockerhub, github, gitlab, npm, artifactory or nexus")
		}
		if len(a.SourceIDs) > 50 || len(a.Origins) > 50 {
			return invalid("at most 50 sources and 50 origins")
		}
		for i, s := range a.SourceIDs {
			id, ok := canonicalUUID(s)
			if !ok || !exists(ctx, q, `SELECT EXISTS(SELECT 1 FROM sources WHERE instance_id=$1 AND id=$2::uuid AND kind='remote')`, instance, id) {
				return invalid("source %d is not a remote source of this instance", i+1)
			}
			a.SourceIDs[i] = id
		}
		for i, o := range a.Origins {
			o = strings.ToLower(strings.TrimSpace(o))
			if !hostPattern.MatchString(o) {
				return invalid("origin %q is not a host name", o)
			}
			a.Origins[i] = o
		}
		if a.FeedID != "" {
			if _, _, err := feedFormat(a.FeedID); err != nil {
				return err
			}
			a.FeedID, _ = canonicalUUID(a.FeedID)
		}
		if (a.Provider == ProviderArtifactory || a.Provider == ProviderNexus) && len(a.SourceIDs) == 0 && len(a.Origins) == 0 {
			return invalid("%s notifications do not name their server: choose the remote sources or origins they map to", a.Provider)
		}
	case ActionRelay:
		target, err := events.ValidateEndpointFor(a.TargetURL, allowHTTP)
		if err != nil {
			return invalid("target URL must be a public https URL without credentials, query or fragment")
		}
		a.TargetURL = target
		if a.ResignSecretID != "" {
			id, ok := canonicalUUID(a.ResignSecretID)
			if !ok || !exists(ctx, q, `SELECT EXISTS(SELECT 1 FROM webhook_signing_secrets WHERE instance_id=$1 AND id=$2::uuid)`, instance, id) {
				return invalid("the re-signing secret does not exist")
			}
			a.ResignSecretID = id
		}
	default:
		return invalid("action type must be prewarm, replication_run, purge_source_cache, clean_cache, upstream_notify or relay")
	}
	// Fields that do not belong to the type are dropped, so a stored action is minimal.
	*a = pruneAction(*a)
	return nil
}

func pruneAction(a Action) Action {
	out := Action{Type: a.Type}
	switch a.Type {
	case ActionPrewarm:
		out.FeedID, out.Kind, out.Name, out.Version = a.FeedID, a.Kind, a.Name, a.Version
	case ActionReplicationRun:
		out.JobID = a.JobID
	case ActionPurgeSource:
		out.SourceID, out.Coordinate = a.SourceID, a.Coordinate
	case ActionCleanCache:
		out.FeedID, out.Prefix = a.FeedID, a.Prefix
	case ActionUpstream:
		out.Provider, out.SourceIDs, out.Origins, out.Prewarm, out.FeedID = a.Provider, a.SourceIDs, a.Origins, a.Prewarm, a.FeedID
	case ActionRelay:
		out.TargetURL, out.ResignSecretID = a.TargetURL, a.ResignSecretID
	}
	return out
}

// Summary is a one-line description of an action for lists.
func Summary(a Action) string {
	switch a.Type {
	case ActionPrewarm:
		return "Prewarm " + a.Kind + " " + a.Name
	case ActionReplicationRun:
		return "Run replication job now"
	case ActionPurgeSource:
		if a.Coordinate != "" {
			return "Purge remote cache: " + a.Coordinate
		}
		return "Purge remote source cache"
	case ActionCleanCache:
		return "Clean cache feed: " + a.Prefix
	case ActionUpstream:
		s := "Upstream notification (" + a.Provider + ")"
		if a.Prewarm {
			s += " + prewarm"
		}
		return s
	case ActionRelay:
		return "Relay to " + a.TargetURL
	}
	return a.Type
}

// ActionJSON renders an action for storage in tests and tools.
func ActionJSON(a Action) []byte { b, _ := json.Marshal(a); return b }
