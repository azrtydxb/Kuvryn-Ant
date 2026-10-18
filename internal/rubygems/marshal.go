package rubygems

// Write-only Ruby Marshal 4.8 encoder. No uploaded Marshal is decoded. The
// emitted classes and instance variable names are fixed by this implementation.
import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"strings"
)

type symbol string
type object struct {
	class  string
	fields []any
}
type userMarshal struct {
	class string
	value any
}
type userDump struct {
	class string
	data  []byte
}
type marshalWriter struct{ bytes.Buffer }

func (w *marshalWriter) number(n int) {
	if n == 0 {
		w.WriteByte(0)
		return
	}
	if n < 123 {
		w.WriteByte(byte(n + 5))
		return
	}
	b := []byte{}
	for n > 0 {
		b = append(b, byte(n))
		n >>= 8
	}
	w.WriteByte(byte(len(b)))
	w.Write(b)
}
func (w *marshalWriter) raw(s string) { w.number(len(s)); w.WriteString(s) }
func (w *marshalWriter) value(x any) {
	switch v := x.(type) {
	case nil:
		w.WriteByte('0')
	case bool:
		if v {
			w.WriteByte('T')
		} else {
			w.WriteByte('F')
		}
	case int:
		w.WriteByte('i')
		w.number(v)
	case string:
		w.WriteByte('"')
		w.raw(v)
	case symbol:
		w.WriteByte(':')
		w.raw(string(v))
	case []any:
		w.WriteByte('[')
		w.number(len(v))
		for _, a := range v {
			w.value(a)
		}
	case object:
		w.WriteByte('o')
		w.value(symbol(v.class))
		w.number(len(v.fields) / 2)
		for _, a := range v.fields {
			w.value(a)
		}
	case userMarshal:
		w.WriteByte('U')
		w.value(symbol(v.class))
		w.value(v.value)
	case userDump:
		w.WriteByte('u')
		w.value(symbol(v.class))
		w.raw(string(v.data))
	}
}
func marshal(x any) []byte {
	w := &marshalWriter{}
	w.Write([]byte{4, 8})
	w.value(x)
	return w.Bytes()
}
func gemVersion(s string) any { return userMarshal{"Gem::Version", []any{s}} }
func gemRequirement(req []string) any {
	pairs := []any{}
	for _, r := range req {
		op, v, _ := strings.Cut(r, " ")
		pairs = append(pairs, []any{op, gemVersion(v)})
	}
	return userMarshal{"Gem::Requirement", []any{pairs}}
}
func quickSpec(m metadata) []byte {
	deps := []any{}
	for _, d := range m.Dependencies {
		req := gemRequirement(d.Requirements)
		deps = append(deps, object{"Gem::Dependency", []any{symbol("@name"), d.Name, symbol("@requirement"), req, symbol("@type"), symbol(strings.TrimPrefix(d.Type, ":")), symbol("@prerelease"), false, symbol("@version_requirements"), req}})
	}
	authors, licenses := []any{}, []any{}
	for _, a := range m.Authors {
		authors = append(authors, a)
	}
	for _, l := range m.Licenses {
		licenses = append(licenses, l)
	}
	fields := []any{m.RubygemsVersion, 4, m.Name, gemVersion(m.Version), m.Date, m.Summary, gemRequirement(m.Ruby), gemRequirement(m.RubyGems), m.Platform, deps, "", "", authors, m.Description, m.Homepage, true, m.Platform, licenses, nil}
	b := marshal(userDump{"Gem::Specification", marshal(fields)})
	var out bytes.Buffer
	z := zlib.NewWriter(&out)
	z.Write(b)
	z.Close()
	return out.Bytes()
}
func legacyIndex(ms []metadata, pre bool) []byte {
	items := []any{}
	for _, m := range ms {
		prerelease := strings.IndexFunc(m.Version, func(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' }) >= 0
		if prerelease != pre {
			continue
		}
		items = append(items, []any{m.Name, gemVersion(m.Version), m.Platform})
	}
	var b bytes.Buffer
	g := gzip.NewWriter(&b)
	g.Write(marshal(items))
	g.Close()
	return b.Bytes()
}
