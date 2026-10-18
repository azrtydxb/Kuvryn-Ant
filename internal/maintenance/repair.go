package maintenance

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RepairFinding is the outcome of one consistency check.
type RepairFinding struct {
	Check       string   `json:"check"`
	Description string   `json:"description"`
	Found       int64    `json:"found"`
	Sample      []string `json:"sample,omitempty"`
	// Fixed is the number of rows changed (always 0 on a dry run).
	Fixed int64 `json:"fixed"`
	// ReportOnly checks need operator action (restore) and are never changed.
	ReportOnly bool `json:"report_only,omitempty"`
}

// RepairReport lists every check, including those that found nothing.
type RepairReport struct {
	DryRun   bool            `json:"dry_run"`
	Findings []RepairFinding `json:"findings"`
	Total    int64           `json:"total_found"`
	Fixed    int64           `json:"total_fixed"`
}

type check struct {
	name, desc string
	// from is "FROM ... WHERE ..." and id the sample/identity expression.
	from, id string
	// fix statements run in order; nil makes the check report-only.
	fix []string
}

func del(name, desc, table, alias, id, cond string) check {
	return check{name, desc, "FROM " + table + " " + alias + " WHERE " + cond, id, []string{"DELETE FROM " + table + " " + alias + " WHERE " + cond}}
}

// Ordering matters: dependents are repaired before the rows they hang off, so
// one run converges.
var checks = []check{
	del("tags_dangling", "tags whose component or version row is missing or mismatched", "tags", "t", "t.component_id::text||'/'||t.name",
		`NOT EXISTS (SELECT 1 FROM components c WHERE c.id=t.component_id) OR NOT EXISTS (SELECT 1 FROM versions v WHERE v.id=t.version_id AND v.component_id=t.component_id)`),
	del("subjects_without_asset", "artifact subject links whose referrer asset is missing", "artifact_subjects", "s", "s.asset_id::text",
		`NOT EXISTS (SELECT 1 FROM assets a WHERE a.id=s.asset_id)`),
	{"subjects_component_mismatch", "artifact subject links recorded against a different component than their asset's",
		`FROM artifact_subjects s WHERE EXISTS (SELECT 1 FROM assets a JOIN versions v ON v.id=a.version_id WHERE a.id=s.asset_id AND v.component_id<>s.component_id)`,
		"s.asset_id::text",
		[]string{`UPDATE artifact_subjects s SET component_id=v.component_id FROM assets a JOIN versions v ON v.id=a.version_id WHERE a.id=s.asset_id AND v.component_id<>s.component_id`}},
	del("assets_without_blob", "assets whose blob metadata row is missing", "assets", "a", "a.id::text",
		`NOT EXISTS (SELECT 1 FROM blobs b WHERE b.id=a.blob_id)`),
	del("assets_without_version", "assets whose version row is missing", "assets", "a", "a.id::text",
		`NOT EXISTS (SELECT 1 FROM versions v WHERE v.id=a.version_id)`),
	del("assets_without_component", "assets of versions whose component row is missing", "assets", "a", "a.id::text",
		`EXISTS (SELECT 1 FROM versions v WHERE v.id=a.version_id AND NOT EXISTS (SELECT 1 FROM components c WHERE c.id=v.component_id))`),
	del("versions_without_component", "versions whose component row is missing", "versions", "v", "v.id::text",
		`NOT EXISTS (SELECT 1 FROM components c WHERE c.id=v.component_id)`),
	{"versions_without_assets", "versions with no assets (older than one hour, so in-flight publishes are untouched); their tags are removed too",
		`FROM versions v WHERE v.created_at<now()-interval '1 hour' AND NOT EXISTS (SELECT 1 FROM assets a WHERE a.version_id=v.id)`,
		"v.id::text",
		[]string{
			`DELETE FROM tags WHERE version_id IN (SELECT v.id FROM versions v WHERE v.created_at<now()-interval '1 hour' AND NOT EXISTS (SELECT 1 FROM assets a WHERE a.version_id=v.id))`,
			`DELETE FROM versions v WHERE v.created_at<now()-interval '1 hour' AND NOT EXISTS (SELECT 1 FROM assets a WHERE a.version_id=v.id)`,
		}},
	del("components_without_versions", "components with no versions", "components", "c", "c.id::text",
		`NOT EXISTS (SELECT 1 FROM versions v WHERE v.component_id=c.id)`),
	{"stale_integrity_findings", "open integrity findings for blobs that no longer exist (marked resolved)",
		`FROM blob_integrity_findings f WHERE f.resolved_at IS NULL AND NOT EXISTS (SELECT 1 FROM blobs b WHERE b.dedup_domain_id=f.dedup_domain_id AND b.digest=f.digest)`,
		"f.id::text",
		[]string{`UPDATE blob_integrity_findings f SET resolved_at=now() WHERE f.resolved_at IS NULL AND NOT EXISTS (SELECT 1 FROM blobs b WHERE b.dedup_domain_id=f.dedup_domain_id AND b.digest=f.digest)`}},
	{"assets_with_missing_blob_object", "assets whose blob object is missing from storage per the integrity scan; restore from backup or a replica (not changed)",
		`FROM blob_integrity_findings f WHERE f.resolved_at IS NULL AND f.kind='missing' AND EXISTS (SELECT 1 FROM blobs b JOIN assets a ON a.blob_id=b.id WHERE b.dedup_domain_id=f.dedup_domain_id AND b.digest=f.digest)`,
		"f.digest", nil},
}

const sampleSize = 20

// RepairMetadata finds orphaned or inconsistent metadata rows and, unless
// dryRun, fixes them in one transaction. Findings are always gathered first;
// a dry run changes nothing. Blob objects and unreferenced blobs are out of
// scope here (garbage collection owns those). Run it during low traffic: it
// deletes only rows that are already unreachable through foreign keys.
func RepairMetadata(ctx context.Context, pool *pgxpool.Pool, dryRun bool) (RepairReport, error) {
	report := RepairReport{DryRun: dryRun, Findings: []RepairFinding{}}
	if pool == nil {
		return report, errors.New("repair requires a database")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return report, err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	for _, c := range checks {
		f := RepairFinding{Check: c.name, Description: c.desc, ReportOnly: c.fix == nil}
		if err = tx.QueryRow(ctx, `SELECT count(*) `+c.from).Scan(&f.Found); err != nil {
			return report, err
		}
		if f.Found > 0 {
			rows, e := tx.Query(ctx, `SELECT `+c.id+` `+c.from+` ORDER BY 1 LIMIT $1`, sampleSize)
			if e != nil {
				return report, e
			}
			if f.Sample, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
				return report, err
			}
			if !dryRun && c.fix != nil {
				for _, stmt := range c.fix {
					tag, e := tx.Exec(ctx, stmt)
					if e != nil {
						return report, e
					}
					f.Fixed = tag.RowsAffected()
				}
			}
		}
		report.Total += f.Found
		report.Fixed += f.Fixed
		report.Findings = append(report.Findings, f)
	}
	if dryRun {
		return report, nil
	}
	return report, tx.Commit(ctx)
}
