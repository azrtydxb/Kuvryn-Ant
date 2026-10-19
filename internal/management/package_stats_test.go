package management

import (
	"context"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

func strOrNil(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestLatestVersionByFormat(t *testing.T) {
	cases := []struct {
		name, format string
		versions     []string
		tags         []string
		want         string
	}{
		{"npm semver max not newest", "npm", []string{"1.2.0", "1.10.0", "1.9.0"}, nil, "1.10.0"},
		{"cargo prefers release", "cargo", []string{"2.0.0-rc.1", "1.9.0"}, nil, "1.9.0"},
		{"no parseable versions falls back to newest", "raw", []string{"zeta", "alpha"}, nil, "zeta"},
		{"empty", "npm", nil, nil, "<nil>"},
		{"oci latest tag", "oci", []string{"d1"}, []string{"latest", "1.0"}, "latest"},
		{"oci semver tag", "oci", []string{"d1"}, []string{"sha-abc", "1.2", "1.10", "edge"}, "1.10"},
		{"oci newest non-ci tag", "oci", []string{"d1"}, []string{"sha-abc", "main", "dev"}, "main"},
		{"oci only ci tag", "oci", []string{"d1"}, []string{"sha-abc"}, "sha-abc"},
		{"oci digests only", "oci", []string{"d1", "d2"}, nil, "<nil>"},
	}
	for _, c := range cases {
		if got := strOrNil(latestVersion(c.format, c.versions, c.tags)); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestLoadPackageStats(t *testing.T) {
	f := remotetest.New(t)
	ctx := context.Background()
	feed := f.AddFeed("hosted", "oci")
	local := f.AddLocal(feed, "oci", "hosted-local", 0)
	hub := f.AddRemote(feed, "oci", "hub", 1, "https://registry-1.docker.io", remotetest.RemoteOptions{})
	inst := f.Boot.InstanceID
	component := func(source, coordinate string) string {
		var id string
		if err := f.Pool.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,$3,'') RETURNING id::text`, inst, source, coordinate).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	version := func(component, v string, age time.Duration) string {
		var id string
		if err := f.Pool.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version,created_at) VALUES($1,$2,$3,now()-$4::interval) RETURNING id::text`, inst, component, v, age.String()).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	tag := func(component, name, version string) {
		if _, err := f.Pool.Exec(ctx, `INSERT INTO tags(component_id,name,version_id) VALUES($1,$2,$3)`, component, name, version); err != nil {
			t.Fatal(err)
		}
	}
	app := component(local, "team/app")
	idx := version(app, "dddd", time.Minute)
	c1 := version(app, "cccc", 2*time.Minute)
	c2 := version(app, "bbbb", 2*time.Minute)
	other := version(app, "eeee", time.Hour)
	tag(app, "sha-abc123", idx)
	tag(app, "v1.2.0", idx)
	tag(app, "v1.10.0", other)
	for _, ch := range []string{"cccc", "bbbb"} {
		if _, err := f.Pool.Exec(ctx, `INSERT INTO version_children(component_id,parent_version_id,child_version,platform) VALUES($1,$2,$3,'linux/amd64')`, app, idx, ch); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = c1, c2
	alpine := component(hub, "library/alpine")
	version(alpine, "ffff", time.Minute)
	bare := component(local, "team/bare")

	stats, err := loadPackageStats(ctx, f.Pool, inst, []string{app, alpine, bare})
	if err != nil {
		t.Fatal(err)
	}
	a := stats[app]
	if a.VersionCount != 2 || a.TagCount != 3 || strOrNil(a.LatestVersion) != "v1.10.0" || a.UpdatedAt == nil || a.DisplayName != "team/app" {
		t.Fatalf("app: %+v latest=%s", a, strOrNil(a.LatestVersion))
	}
	l := stats[alpine]
	if l.DisplayName != "alpine" || l.LatestVersion != nil || l.VersionCount != 1 || l.TagCount != 0 {
		t.Fatalf("alpine: %+v", l)
	}
	if b := stats[bare]; b.VersionCount != 0 || b.UpdatedAt != nil || b.LatestVersion != nil || b.DisplayName != "team/bare" {
		t.Fatalf("bare: %+v", b)
	}
}
