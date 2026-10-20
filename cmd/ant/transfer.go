package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/azrtydxb/Kuvryn-Ant/internal/transfer"
	"github.com/jackc/pgx/v5/pgxpool"
)

const transferUsage = `usage: ant export [--feed SLUG]... [--namespace NS]... [--no-content] <new-or-empty-dir>
       ant export verify [--expect-manifest-sha256 HEX] <dir>
       ant import [--apply] [--expect-manifest-sha256 HEX] <dir>
       ant migrate nexus|artifactory --url URL [--user NAME] [--ca-ref ID] [--repo NAME]... [--copy-content] [--no-count] [--apply]
       ant compare nexus|artifactory --url URL --coordinates FILE   (lines of "<repository> <path> [<feed>]")
Source credentials are read at run time from ANT_NEXUS_PASSWORD / ANT_NEXUS_TOKEN or
ANT_ARTIFACTORY_PASSWORD (password or API key) / ANT_ARTIFACTORY_TOKEN and are never stored.
--ca-ref selects a CA bundle from ANT_UPSTREAM_CA_MAP_FILE for a private certificate authority.
import and migrate only report (dry run) unless --apply is given.`

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// transferCommand handles export, import, migrate and compare. Like backup, it
// is a local operation authorized by possession of the deployment's database
// and storage configuration.
func transferCommand(ctx context.Context, logger *slog.Logger, args []string, output io.Writer) int {
	mode, rest := args[0], args[1:]
	if mode == "export" && len(rest) > 0 && rest[0] == "verify" {
		mode, rest = "verify", rest[1:]
	}
	var feeds, namespaces, repos listFlag
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(&feeds, "feed", "")
	fs.Var(&namespaces, "namespace", "")
	fs.Var(&repos, "repo", "")
	noContent := fs.Bool("no-content", false, "")
	apply := fs.Bool("apply", false, "")
	expect := fs.String("expect-manifest-sha256", "", "")
	url := fs.String("url", "", "")
	user := fs.String("user", "", "")
	caRef := fs.String("ca-ref", "", "")
	copyContent := fs.Bool("copy-content", false, "")
	noCount := fs.Bool("no-count", false, "")
	coords := fs.String("coordinates", "", "")
	var manager string
	if (mode == "migrate" || mode == "compare") && len(rest) > 0 {
		manager, rest = rest[0], rest[1:]
	}
	if fs.Parse(rest) != nil {
		logger.Error(transferUsage)
		return 1
	}
	switch mode {
	case "export":
		if fs.NArg() != 1 || *apply || *expect != "" {
			logger.Error(transferUsage)
			return 1
		}
	case "verify":
		if fs.NArg() != 1 {
			logger.Error(transferUsage)
			return 1
		}
	case "import":
		if fs.NArg() != 1 {
			logger.Error(transferUsage)
			return 1
		}
	case "migrate", "compare":
		if manager != "nexus" && manager != "artifactory" || *url == "" || fs.NArg() != 0 || mode == "compare" && *coords == "" {
			logger.Error(transferUsage)
			return 1
		}
	}
	if mode == "verify" {
		summary, err := transfer.Verify(ctx, fs.Arg(0), *expect)
		if err != nil {
			logger.Error("bundle verification failed", "reason", err)
			return 1
		}
		return emit(logger, output, struct {
			Verified bool `json:"verified"`
			transfer.Summary
		}{true, summary})
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("configuration rejected", "reason", err)
		return 1
	}
	defer clear(cfg.RootKey)
	pool, err := database.Open(ctx, cfg.Database)
	if err != nil {
		logger.Error("startup failed", "reason", err)
		return 1
	}
	defer pool.Close()
	if mode != "export" {
		if err = database.Migrate(ctx, pool); err != nil {
			logger.Error("schema initialization failed", "reason", err)
			return 1
		}
	}
	instance, err := localInstance(ctx, pool, cfg)
	if err != nil {
		logger.Error("instance not available", "reason", err)
		return 1
	}
	stores, err := storage.New(cfg.Storage)
	if err != nil {
		logger.Error("storage configuration rejected", "reason", err)
		return 1
	}
	defer stores.Close()
	switch mode {
	case "export":
		sum, err := transfer.ExportBundle(ctx, pool, stores, instance, fs.Arg(0), transfer.ExportOptions{Selection: transfer.Selection{Feeds: feeds, Namespaces: namespaces}, Content: !*noContent})
		if err != nil {
			logger.Error("export failed", "reason", err)
			return 1
		}
		return emit(logger, output, struct {
			Exported bool `json:"exported"`
			transfer.Summary
		}{true, sum})
	case "import":
		b, err := transfer.OpenBundle(fs.Arg(0), *expect)
		if err != nil {
			logger.Error("import rejected", "reason", err)
			return 1
		}
		defer b.Close()
		rep, err := transfer.Import(ctx, pool, stores, instance, b.Document, b, transfer.ImportOptions{DryRun: !*apply})
		if err != nil {
			logger.Error("import failed", "reason", err)
			return 1
		}
		return reportExit(logger, output, rep)
	}
	var caPool *x509.CertPool
	if *caRef != "" {
		path, ok := cfg.UpstreamCAs[*caRef]
		if !ok {
			logger.Error("source manager rejected", "reason", "unknown CA reference")
			return 1
		}
		if caPool, err = transfer.LoadCAPool(path); err != nil {
			logger.Error("source manager rejected", "reason", err)
			return 1
		}
	}
	var nx transfer.Manager
	if manager == "artifactory" {
		nx, err = transfer.NewArtifactory(transfer.ArtifactoryConfig{BaseURL: *url, Username: *user, Password: os.Getenv("ANT_ARTIFACTORY_PASSWORD"), Token: os.Getenv("ANT_ARTIFACTORY_TOKEN"), RootCAs: caPool})
	} else {
		nx, err = transfer.NewNexus(transfer.NexusConfig{BaseURL: *url, Username: *user, Password: os.Getenv("ANT_NEXUS_PASSWORD"), Token: os.Getenv("ANT_NEXUS_TOKEN"), RootCAs: caPool})
	}
	if err != nil {
		logger.Error("source manager rejected", "reason", err)
		return 1
	}
	if mode == "compare" {
		items, err := readCoordinates(*coords)
		if err != nil {
			logger.Error("coordinates rejected", "reason", err)
			return 1
		}
		rep, err := transfer.Compare(ctx, pool, instance, nx, items)
		if err != nil {
			logger.Error("compare failed", "reason", err)
			return 1
		}
		return emit(logger, output, rep)
	}
	rep, err := transfer.Migrate(ctx, pool, stores, instance, nx, transfer.MapOptions{Repositories: repos, CopyContent: *copyContent, CountAssets: !*noCount}, transfer.ImportOptions{DryRun: !*apply})
	if err != nil {
		logger.Error("migration failed", "reason", err)
		return 1
	}
	return reportExit(logger, output, rep)
}

// reportExit prints the report; integrity failures make the exit status nonzero.
func reportExit(logger *slog.Logger, output io.Writer, rep any) int {
	code := emit(logger, output, rep)
	var failures []transfer.Failure
	switch r := rep.(type) {
	case *transfer.Report:
		failures = r.IntegrityFailures
	case *transfer.MigrationReport:
		failures = r.IntegrityFailures
	}
	if code == 0 && len(failures) > 0 {
		logger.Error("integrity failures were found", "count", len(failures))
		return 2
	}
	return code
}

func localInstance(ctx context.Context, pool *pgxpool.Pool, cfg config.Config) (string, error) {
	owner, err := identity.LoadBootstrapIdentity(ctx, pool)
	if err != nil {
		return "", err
	}
	if cfg.InstanceID != "" && !strings.EqualFold(cfg.InstanceID, owner.InstanceID) {
		return "", errors.New("configured instance does not match the local recovery identity")
	}
	return owner.InstanceID, nil
}

func readCoordinates(path string) ([]transfer.CompareItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var items []transfer.CompareItem
	sc := bufio.NewScanner(io.LimitReader(f, 4<<20))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 || len(parts) > 3 {
			return nil, errors.New("each line must be \"<repository> <path> [<feed>]\"")
		}
		it := transfer.CompareItem{Repository: parts[0], Path: parts[1]}
		if len(parts) == 3 {
			it.Feed = parts[2]
		}
		items = append(items, it)
	}
	return items, sc.Err()
}
