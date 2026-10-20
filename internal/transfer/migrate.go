package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by Manager.Open when the manager has no such asset.
var ErrNotFound = errors.New("not found in the source manager")

// SourceRepo is a repository of another artifact manager in neutral terms.
type SourceRepo struct {
	Name      string
	Format    string // the manager's own format name
	Type      string // hosted, proxy or group
	RemoteURL string // proxy only
	// Members of a group, in resolution order.
	Members []string
	// WritableMember is the group member that receives publishes, when set.
	WritableMember  string
	CleanupPolicies []string
	// Cache settings of a proxy, in seconds; zero means not set.
	MetadataMaxAge int64
	// Authenticated reports that a proxy used credentials upstream.
	Authenticated bool
	// ConfigErr is set when the repository's configuration could not be read;
	// the repository is reported and not mapped rather than mapped incompletely.
	ConfigErr string
	// Notes are source settings Ant cannot express, reported as unmapped.
	Notes []string
}

// RepoStat is the stored size of a repository as the manager reports it.
type RepoStat struct {
	Files int
	Bytes int64
}

// StatsProvider is implemented by managers that report repository sizes
// without listing every asset.
type StatsProvider interface {
	Stats(ctx context.Context) (map[string]RepoStat, error)
}

// IdentityReporter is implemented by managers whose users, groups and
// permissions can be listed. Ant does not import accounts; the entries are
// reported as unmapped with guidance.
type IdentityReporter interface {
	Identities(ctx context.Context) ([]Unmapped, error)
}

// ContentCopier is implemented by managers that can supply content for more
// formats than the default (raw and Maven layouts).
type ContentCopier interface {
	CopyableFormats() map[string]bool
}

// CleanupPolicy is a manager cleanup policy in neutral terms.
type CleanupPolicy struct {
	Name         string
	MaxAgeDays   *int // last updated
	MaxIdleDays  *int // last downloaded
	Retain       *int
	ReleaseType  string
	UnmappedNote []string // criteria Ant cannot express
}

// SourceAsset is one stored file of the source manager.
type SourceAsset struct {
	Repository  string
	Path        string
	DownloadURL string
	SHA256      string
	Size        int64 // -1 when unknown
	ContentType string
	// Kind, Image, Tags and Subject describe registry objects (OCI): Kind is
	// "manifest" or "blob" and Image the repository path inside the registry.
	Kind    string
	Image   string
	Tags    []string
	Subject *SubjectDef
	// Note marks an object the manager could not describe (reported, not copied).
	Note string
}

// Manager reads the inventory of another artifact manager. Implementations hold
// credentials only for the lifetime of the value; nothing is persisted. Nexus 3
// and JFrog Artifactory are implemented.
type Manager interface {
	Name() string
	// AntFormat maps the manager's format name to an Ant format.
	AntFormat(format string) (string, bool)
	Repositories(ctx context.Context) ([]SourceRepo, error)
	CleanupPolicies(ctx context.Context) ([]CleanupPolicy, error)
	// Assets streams the assets of one repository to fn.
	Assets(ctx context.Context, repo string, fn func(SourceAsset) error) error
	// Open downloads an asset. It returns ErrNotFound for a missing asset.
	Open(ctx context.Context, a SourceAsset) (io.ReadCloser, error)
}

// MapOptions control BuildPlan.
type MapOptions struct {
	// Repositories limits the migration to these repositories (and group members
	// are resolved from the full inventory). Empty selects all.
	Repositories []string
	// CopyContent lists assets of hosted repositories and copies them.
	CopyContent bool
	// CountAssets lists assets of every selected hosted repository to report
	// counts and sizes even when content is not copied.
	CountAssets bool
	// MaxAssets bounds how many assets are listed per repository (0: 1,000,000).
	MaxAssets int
}

// RepoInventory is one source repository and how it maps.
type RepoInventory struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Format    string `json:"format"`
	AntFormat string `json:"ant_format,omitempty"`
	MappedTo  string `json:"mapped_to"`
	// RemoteURL is the upstream origin of a mapped remote source.
	RemoteURL string `json:"remote_url,omitempty"`
	Assets    int    `json:"assets,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
}

// Plan is the mapped Ant configuration plus the content to copy.
type Plan struct {
	Document     Document
	Content      Content
	Repositories []RepoInventory
	Unmapped     []Unmapped
}

var slugReplace = regexp.MustCompile(`[^a-z0-9_-]+`)

// Slug derives a feed slug from a repository name.
func Slug(name string) string {
	s := slugReplace.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.TrimLeft(s, "-_")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

// BuildPlan maps the manager's repositories onto Ant sources and feeds:
// hosted becomes a local source, proxy a remote source, a group a feed whose
// ordered sources are the group members, and every hosted or proxy repository
// also gets a single-source feed of the same name so client URLs can be kept.
func BuildPlan(ctx context.Context, m Manager, opt MapOptions) (*Plan, error) {
	repos, err := m.Repositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	policies := map[string]CleanupPolicy{}
	if ps, err := m.CleanupPolicies(ctx); err == nil {
		for _, p := range ps {
			policies[p.Name] = p
		}
	}
	byName := map[string]SourceRepo{}
	for _, r := range repos {
		byName[r.Name] = r
	}
	selected := func(n string) bool { return len(opt.Repositories) == 0 || slices.Contains(opt.Repositories, n) }
	for _, n := range opt.Repositories {
		if _, ok := byName[n]; !ok {
			return nil, fmt.Errorf("%w: repository %q not found in %s", ErrInput, n, m.Name())
		}
	}
	plan := &Plan{Document: Document{FormatVersion: FormatVersion, Sources: []SourceDef{}, Feeds: []FeedDef{}, Components: []ComponentDef{}}}
	doc := &plan.Document
	un := func(kind, name, reason string) { plan.Unmapped = append(plan.Unmapped, Unmapped{kind, name, reason}) }
	// mappedSource reports whether a repo becomes an Ant source.
	sources := map[string]SourceRepo{}
	slugs := map[string]string{}
	claim := func(r SourceRepo, kind string) (string, bool) {
		slug := Slug(r.Name)
		if slug == "" || (slugs[slug] != "" && slugs[slug] != r.Name) {
			un("feed", r.Name, "feed slug "+slug+" collides or is empty; repository not mapped to a feed")
			return "", false
		}
		slugs[slug] = r.Name
		if slug != r.Name {
			un("feed", r.Name, "feed slug normalized to "+slug)
		}
		return slug, true
	}
	var expand func(name string, seen map[string]bool) []string
	expand = func(name string, seen map[string]bool) []string {
		r, ok := byName[name]
		if !ok || seen[name] {
			return nil
		}
		seen[name] = true
		if r.Type != "group" {
			return []string{name}
		}
		var out []string
		for _, mem := range r.Members {
			out = append(out, expand(mem, seen)...)
		}
		return out
	}
	// Repositories in a stable order.
	slices.SortFunc(repos, func(a, b SourceRepo) int { return strings.Compare(a.Name, b.Name) })
	var memberNeeded = map[string]bool{}
	for _, r := range repos {
		if r.Type == "group" && selected(r.Name) {
			for _, n := range expand(r.Name, map[string]bool{}) {
				memberNeeded[n] = true
			}
		}
	}
	for _, r := range repos {
		inv := RepoInventory{Name: r.Name, Type: r.Type, Format: r.Format}
		af, ok := m.AntFormat(r.Format)
		if !selected(r.Name) && !memberNeeded[r.Name] {
			continue
		}
		if !ok {
			inv.MappedTo = "unmapped"
			un("repository", r.Name, "format "+r.Format+" has no Ant equivalent")
			plan.Repositories = append(plan.Repositories, inv)
			continue
		}
		inv.AntFormat = af
		if r.ConfigErr != "" || r.Type == "proxy" && r.RemoteURL == "" {
			why := r.ConfigErr
			if why == "" {
				why = "remote URL could not be read"
			}
			inv.MappedTo = "unmapped"
			un("repository", r.Name, "configuration could not be read ("+why+"); repository not mapped")
			plan.Repositories = append(plan.Repositories, inv)
			continue
		}
		for _, note := range r.Notes {
			un("repository", r.Name, note)
		}
		switch r.Type {
		case "hosted", "proxy":
			def := SourceDef{Name: r.Name, Format: af, Kind: "local"}
			inv.MappedTo = "local source"
			if r.Type == "proxy" {
				def.Kind = "remote"
				inv.MappedTo = "remote source"
				inv.RemoteURL = r.RemoteURL
				cfg := remoteconfig.DefaultConfig(r.RemoteURL)
				if r.MetadataMaxAge > 0 {
					cfg.Cache.MetadataTTLSeconds = int(r.MetadataMaxAge)
				}
				enc, _ := json.Marshal(cfg)
				raw := json.RawMessage(enc)
				def.RemoteConfig = &raw
				if r.Authenticated {
					def.Credential = &CredentialRef{Purpose: m.Name() + ":" + r.Name}
				}
			}
			def.Retention = mapRetention(r, policies, un)
			sources[r.Name] = r
			doc.Sources = append(doc.Sources, def)
			if selected(r.Name) {
				if slug, ok := claim(r, "feed"); ok {
					doc.Feeds = append(doc.Feeds, FeedDef{Slug: slug, Format: af, Sources: []FeedSourceRef{{Source: r.Name, Priority: 1}}})
					inv.MappedTo += " + feed " + slug
				}
			}
		case "group":
			inv.MappedTo = "feed"
			if !selected(r.Name) {
				continue
			}
			slug, ok := claim(r, "feed")
			if !ok {
				inv.MappedTo = "unmapped"
				plan.Repositories = append(plan.Repositories, inv)
				continue
			}
			feed := FeedDef{Slug: slug, Format: af}
			for _, n := range expand(r.Name, map[string]bool{}) {
				feed.Sources = append(feed.Sources, FeedSourceRef{Source: n, Priority: len(feed.Sources) + 1})
			}
			for _, mem := range r.Members {
				if mr, ok := byName[mem]; ok && mr.Type == "group" {
					un("feed", r.Name, "nested group "+mem+" was flattened into the ordered source list")
				}
			}
			if r.WritableMember != "" && slices.ContainsFunc(feed.Sources, func(s FeedSourceRef) bool { return s.Source == r.WritableMember }) {
				feed.PublishPolicy = &PublishPolicyDef{DefaultSource: r.WritableMember}
			} else if r.WritableMember != "" {
				un("publish_policy", r.Name, "default deployment repository "+r.WritableMember+" is not a member; no publish policy was created")
			}
			doc.Feeds = append(doc.Feeds, feed)
		default:
			inv.MappedTo = "unmapped"
			un("repository", r.Name, "unknown repository type "+r.Type)
		}
		plan.Repositories = append(plan.Repositories, inv)
	}
	// A group member that could not be mapped (unreadable or unsupported) is not a
	// source; drop it from the ordered list and say so.
	have := map[string]bool{}
	for _, sd := range doc.Sources {
		have[sd.Name] = true
	}
	for i := range doc.Feeds {
		f := &doc.Feeds[i]
		if len(f.Sources) == 0 || !slices.ContainsFunc(f.Sources, func(r FeedSourceRef) bool { return !have[r.Source] }) {
			continue
		}
		kept := f.Sources[:0]
		for _, r := range f.Sources {
			if !have[r.Source] {
				un("feed", f.Slug, "member "+r.Source+" was not mapped and is missing from the ordered sources")
				continue
			}
			r.Priority = len(kept) + 1
			kept = append(kept, r)
		}
		f.Sources = kept
		if f.PublishPolicy != nil && !have[f.PublishPolicy.DefaultSource] {
			f.PublishPolicy = nil
		}
	}
	// Sizes reported by the manager need no asset listing.
	if sp, ok := m.(StatsProvider); ok {
		if stats, err := sp.Stats(ctx); err == nil {
			for i := range plan.Repositories {
				inv := &plan.Repositories[i]
				st, ok := stats[inv.Name]
				if !ok {
					st, ok = stats[inv.Name+"-cache"]
				}
				if ok && inv.AntFormat != "" {
					inv.Assets, inv.Bytes = st.Files, st.Bytes
				}
			}
		}
	}
	if ir, ok := m.(IdentityReporter); ok {
		if list, err := ir.Identities(ctx); err == nil {
			plan.Unmapped = append(plan.Unmapped, list...)
		} else {
			un("identity", "users, groups and permissions", "could not be read ("+err.Error()+"); recreate access control manually")
		}
	}
	copyable := map[string]bool{"raw": true, "maven": true}
	if cc, ok := m.(ContentCopier); ok {
		copyable = cc.CopyableFormats()
	}
	// Content.
	if opt.CopyContent || opt.CountAssets {
		nc := &listedContent{m: m, byDigest: map[string]SourceAsset{}}
		plan.Content = nc
		limit := opt.MaxAssets
		if limit <= 0 {
			limit = 1_000_000
		}
		for i := range plan.Repositories {
			inv := &plan.Repositories[i]
			r := byName[inv.Name]
			if r.Type != "hosted" || inv.AntFormat == "" {
				continue
			}
			var assets []SourceAsset
			err := m.Assets(ctx, r.Name, func(a SourceAsset) error {
				if len(assets) >= limit {
					return errStop
				}
				assets = append(assets, a)
				return nil
			})
			if err != nil && !errors.Is(err, errStop) {
				return nil, fmt.Errorf("list assets of %s: %w", r.Name, err)
			}
			if errors.Is(err, errStop) {
				un("repository", r.Name, fmt.Sprintf("asset listing stopped at %d assets", limit))
			}
			inv.Assets = len(assets)
			for _, a := range assets {
				if a.Size > 0 {
					inv.Bytes += a.Size
				}
			}
			if !opt.CopyContent {
				continue
			}
			if !copyable[inv.AntFormat] {
				un("content", r.Name, "content copy is not supported for format "+r.Format+"; definitions only")
				continue
			}
			comps, skipped := mapAssets(r.Name, inv.AntFormat, assets, nc)
			doc.Components = append(doc.Components, comps...)
			for _, s := range skipped {
				un("asset", r.Name+"/"+s.path, s.reason)
			}
		}
	}
	return plan, nil
}

var errStop = errors.New("stop")

func mapRetention(r SourceRepo, policies map[string]CleanupPolicy, un func(kind, name, reason string)) *RetentionDef {
	if len(r.CleanupPolicies) == 0 {
		return nil
	}
	var out *RetentionDef
	for _, n := range r.CleanupPolicies {
		p, ok := policies[n]
		if !ok {
			un("retention", r.Name, "cleanup policy "+n+" could not be read from the source manager")
			continue
		}
		for _, note := range p.UnmappedNote {
			un("retention", r.Name, "cleanup policy "+n+": "+note)
		}
		if out != nil {
			un("retention", r.Name, "cleanup policy "+n+" ignored; only one policy per source is supported")
			continue
		}
		d := RetentionDef{MinKeep: 1, Enabled: true, AllowReleaseDelete: p.ReleaseType == "RELEASES"}
		if p.Retain != nil && *p.Retain >= 1 {
			v := int32(*p.Retain)
			d.KeepLast = &v
		}
		if p.MaxAgeDays != nil && *p.MaxAgeDays >= 1 {
			v := int64(*p.MaxAgeDays) * 86400
			d.MaxAgeSeconds = &v
		}
		if p.MaxIdleDays != nil && *p.MaxIdleDays >= 1 {
			v := int64(*p.MaxIdleDays) * 86400
			d.MaxIdleSeconds = &v
		}
		if d.KeepLast == nil && d.MaxAgeSeconds == nil && d.MaxIdleSeconds == nil {
			un("retention", r.Name, "cleanup policy "+n+" has no criteria Ant can express")
			continue
		}
		out = &d
	}
	return out
}

type skippedAsset struct{ path, reason string }

// mapAssets groups assets into components for formats with a known layout.
func mapAssets(source, format string, assets []SourceAsset, lc *listedContent) ([]ComponentDef, []skippedAsset) {
	var skipped []skippedAsset
	type key struct{ ns, coord string }
	idx := map[key]int{}
	var comps []ComponentDef
	add := func(ns, coord, version string, a SourceAsset) *ComponentDef {
		akey := a.Path
		if a.Kind != "" {
			akey = a.Kind
		}
		k := key{ns, coord}
		i, ok := idx[k]
		if !ok {
			i = len(comps)
			idx[k] = i
			comps = append(comps, ComponentDef{Source: source, Namespace: ns, Coordinate: coord, Versions: []VersionDef{}})
		}
		c := &comps[i]
		vi := slices.IndexFunc(c.Versions, func(v VersionDef) bool { return v.Version == version })
		if vi < 0 {
			vi = len(c.Versions)
			c.Versions = append(c.Versions, VersionDef{Version: version})
		}
		media := a.ContentType
		if media == "" {
			media = "application/octet-stream"
		}
		c.Versions[vi].Assets = append(c.Versions[vi].Assets, AssetDef{Key: akey, MediaType: media, Digest: a.SHA256, Size: a.Size, Subject: a.Subject})
		if _, dup := lc.byDigest[a.SHA256]; !dup {
			lc.byDigest[a.SHA256] = a
		}
		return c
	}
	for _, a := range assets {
		if a.Note != "" {
			skipped = append(skipped, skippedAsset{a.Path, a.Note})
			continue
		}
		if a.SHA256 == "" || !digestPattern.MatchString(a.SHA256) {
			skipped = append(skipped, skippedAsset{a.Path, "source manager did not provide a SHA-256 checksum"})
			continue
		}
		switch format {
		case "oci":
			if a.Kind == "" || a.Image == "" {
				skipped = append(skipped, skippedAsset{a.Path, "not a registry manifest or blob"})
				continue
			}
			ns := ""
			if i := strings.LastIndexByte(a.Image, '/'); i >= 0 {
				ns = a.Image[:i]
			}
			// A blob is stored once per image version it belongs to; it is
			// keyed by its own digest like the registry adapter does.
			c := add(ns, a.Image, a.SHA256, a)
			for _, tag := range a.Tags {
				c.Tags = append(c.Tags, TagDef{Name: tag, Version: a.SHA256})
			}
		case "raw":
			ns := ""
			if i := strings.LastIndexByte(a.Path, '/'); i >= 0 {
				ns = a.Path[:i]
			}
			add(ns, a.Path, "first", a)
		case "maven":
			parts := strings.Split(a.Path, "/")
			file := parts[len(parts)-1]
			if strings.HasPrefix(file, "maven-metadata.xml") {
				continue // Ant generates repository metadata itself
			}
			if len(parts) < 4 {
				skipped = append(skipped, skippedAsset{a.Path, "path is not a Maven artifact layout"})
				continue
			}
			version, name := parts[len(parts)-2], parts[len(parts)-3]
			group := strings.Join(parts[:len(parts)-3], ".")
			if strings.Contains(strings.ToUpper(version), "SNAPSHOT") {
				skipped = append(skipped, skippedAsset{a.Path, "snapshot versions are not supported by Ant"})
				continue
			}
			if !validMavenFile(file, name, version) {
				skipped = append(skipped, skippedAsset{a.Path, "file type is not supported by Ant's Maven repository"})
				continue
			}
			add(group, group+":"+name, version, a)
		}
	}
	return comps, skipped
}

func validMavenFile(file, name, version string) bool {
	for _, s := range []string{".sha256", ".sha1", ".md5"} {
		if strings.HasSuffix(file, s) {
			file = strings.TrimSuffix(file, s)
			break
		}
	}
	base := name + "-" + version
	if file == base+".pom" || file == base+".jar" {
		return true
	}
	return strings.HasPrefix(file, base+"-") && strings.HasSuffix(file, ".jar")
}

// listedContent serves digests to download from a source manager. A digest maps
// to the first asset that has it.
type listedContent struct {
	m        Manager
	byDigest map[string]SourceAsset
}

func (l *listedContent) Check(_ context.Context, digest string, _ int64) error {
	if _, ok := l.byDigest[digest]; !ok {
		return errors.New("asset is not in the source inventory")
	}
	return nil
}

func (l *listedContent) Open(ctx context.Context, digest string) (io.ReadCloser, error) {
	a, ok := l.byDigest[digest]
	if !ok {
		return nil, ErrNotFound
	}
	return l.m.Open(ctx, a)
}

// MigrationReport is the dry-run or apply report of a migration.
type MigrationReport struct {
	*Report
	Manager      string          `json:"manager"`
	Repositories []RepoInventory `json:"repositories"`
}

// Migrate maps a source manager onto Ant and imports it. With DryRun it only
// reports; content is downloaded and digest-verified only on apply.
func Migrate(ctx context.Context, pool *pgxpool.Pool, stores Stores, instance string, m Manager, mo MapOptions, imp ImportOptions) (*MigrationReport, error) {
	plan, err := BuildPlan(ctx, m, mo)
	if err != nil {
		return nil, err
	}
	var content Content
	if plan.Content != nil {
		content = plan.Content
	}
	rep, err := Import(ctx, pool, stores, instance, plan.Document, content, imp)
	if err != nil {
		return nil, err
	}
	rep.Unmapped = append(append([]Unmapped{}, plan.Unmapped...), rep.Unmapped...)
	if plan.Repositories == nil {
		plan.Repositories = []RepoInventory{}
	}
	return &MigrationReport{Report: rep, Manager: m.Name(), Repositories: plan.Repositories}, nil
}
