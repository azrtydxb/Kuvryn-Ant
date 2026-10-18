package rpmrepo

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
)

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s)) // invalid XML characters become U+FFFD
	return b.String()
}

func attr(name, value string) string { return " " + name + `="` + esc(value) + `"` }

func (p *Package) versionTag() string {
	return "<version" + attr("epoch", p.Epoch) + attr("ver", p.Version) + attr("rel", p.Release) + "/>"
}

func depList(tag string, deps []dependency) string {
	if len(deps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("    <rpm:" + tag + ">\n")
	for _, d := range deps {
		b.WriteString("      <rpm:entry" + attr("name", d.Name))
		if d.Flags != "" {
			b.WriteString(attr("flags", d.Flags) + attr("epoch", d.Epoch) + attr("ver", d.Version))
			if d.Release != "" {
				b.WriteString(attr("rel", d.Release))
			}
		}
		if d.Pre {
			b.WriteString(` pre="1"`)
		}
		b.WriteString("/>\n")
	}
	b.WriteString("    </rpm:" + tag + ">\n")
	return b.String()
}

// primaryFile mirrors createrepo_c: only executables and /etc content are
// listed in primary.xml; filelists.xml carries every path.
func primaryFile(path string) bool {
	return strings.Contains(path, "bin/") || strings.HasPrefix(path, "/etc/") || path == "/usr/lib/sendmail"
}

// primaryXML renders this package's <package> element for primary.xml.
func (p *Package) primaryXML(pkgid, location string, size int64) string {
	var b strings.Builder
	b.WriteString(`<package type="rpm">` + "\n")
	b.WriteString("  <name>" + esc(p.Name) + "</name>\n  <arch>" + esc(p.Arch) + "</arch>\n  " + p.versionTag() + "\n")
	b.WriteString(`  <checksum type="sha256" pkgid="YES">` + pkgid + "</checksum>\n")
	b.WriteString("  <summary>" + esc(p.Summary) + "</summary>\n  <description>" + esc(p.Description) + "</description>\n")
	b.WriteString("  <packager>" + esc(p.Packager) + "</packager>\n  <url>" + esc(p.URL) + "</url>\n")
	t := strconv.FormatUint(p.BuildTime, 10)
	b.WriteString(`  <time file="` + t + `" build="` + t + `"/>` + "\n")
	b.WriteString(`  <size package="` + strconv.FormatInt(size, 10) + `" installed="` + strconv.FormatUint(p.Installed, 10) + `" archive="` + strconv.FormatUint(p.Archive, 10) + `"/>` + "\n")
	b.WriteString(`  <location` + attr("href", location) + "/>\n  <format>\n")
	b.WriteString("    <rpm:license>" + esc(p.License) + "</rpm:license>\n    <rpm:vendor>" + esc(p.Vendor) + "</rpm:vendor>\n")
	b.WriteString("    <rpm:group>" + esc(p.Group) + "</rpm:group>\n    <rpm:buildhost>" + esc(p.BuildHost) + "</rpm:buildhost>\n")
	b.WriteString("    <rpm:sourcerpm>" + esc(p.SourceRPM) + "</rpm:sourcerpm>\n")
	b.WriteString(`    <rpm:header-range start="` + strconv.FormatInt(p.HeaderStart, 10) + `" end="` + strconv.FormatInt(p.HeaderEnd, 10) + `"/>` + "\n")
	b.WriteString(depList("provides", p.Provides) + depList("requires", p.Requires) + depList("conflicts", p.Conflicts) + depList("obsoletes", p.Obsoletes))
	for _, f := range p.Files {
		if f.Kind != "ghost" && primaryFile(f.Path) {
			b.WriteString("    <file")
			if f.Kind == "dir" {
				b.WriteString(` type="dir"`)
			}
			b.WriteString(">" + esc(f.Path) + "</file>\n")
		}
	}
	b.WriteString("  </format>\n</package>\n")
	return b.String()
}

func (p *Package) filelistsXML(pkgid string) string {
	var b strings.Builder
	b.WriteString(`<package` + attr("pkgid", pkgid) + attr("name", p.Name) + attr("arch", p.Arch) + ">\n  " + p.versionTag() + "\n")
	for _, f := range p.Files {
		b.WriteString("  <file")
		if f.Kind != "" {
			b.WriteString(attr("type", f.Kind))
		}
		b.WriteString(">" + esc(f.Path) + "</file>\n")
	}
	b.WriteString("</package>\n")
	return b.String()
}

func (p *Package) otherXML(pkgid string) string {
	var b strings.Builder
	b.WriteString(`<package` + attr("pkgid", pkgid) + attr("name", p.Name) + attr("arch", p.Arch) + ">\n  " + p.versionTag() + "\n")
	for _, c := range p.Changes {
		b.WriteString("  <changelog" + attr("author", c.Author) + ` date="` + strconv.FormatUint(c.Time, 10) + `">` + esc(c.Text) + "</changelog>\n")
	}
	b.WriteString("</package>\n")
	return b.String()
}
