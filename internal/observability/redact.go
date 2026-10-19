package observability

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
)

// Blob tickets (federation `t=`) and signed-download signatures (`ant-sig=`)
// travel in URL query strings and are bearer credentials until they expire.
// Ant's request log records only fixed labels, but error strings from HTTP
// clients (net/url.Error) embed the full URL, so every log record passes
// through RedactHandler before it is written.
var secretQuery = regexp.MustCompile(`(?i)([?&;](?:t|ant-sig|ticket|token|access_token|sig|signature|x-amz-signature|x-amz-security-token)=)[^&\s"'<>)\]]*`)

// RedactURLSecrets replaces the value of credential-bearing query parameters.
func RedactURLSecrets(s string) string {
	return secretQuery.ReplaceAllString(s, "${1}REDACTED")
}

// RedactHandler wraps next, redacting credential query parameters from the
// message and every string, error and Stringer attribute value.
func RedactHandler(next slog.Handler) slog.Handler { return redactHandler{next} }

type redactHandler struct{ next slog.Handler }

func (h redactHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }
func (h redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, RedactURLSecrets(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool { out.AddAttrs(redactAttr(a)); return true })
	return h.next.Handle(ctx, out)
}
func (h redactHandler) WithAttrs(a []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(a))
	for i := range a {
		red[i] = redactAttr(a[i])
	}
	return redactHandler{h.next.WithAttrs(red)}
}
func (h redactHandler) WithGroup(n string) slog.Handler { return redactHandler{h.next.WithGroup(n)} }

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactURLSecrets(v.String()))
	case slog.KindGroup:
		g := v.Group()
		red := make([]any, len(g))
		for i := range g {
			red[i] = redactAttr(g[i])
		}
		return slog.Group(a.Key, red...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			if s := x.Error(); RedactURLSecrets(s) != s {
				return slog.String(a.Key, RedactURLSecrets(s))
			}
		case fmt.Stringer:
			if s := x.String(); RedactURLSecrets(s) != s {
				return slog.String(a.Key, RedactURLSecrets(s))
			}
		}
	}
	return a
}
