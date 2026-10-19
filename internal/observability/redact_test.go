package observability

import (
	"bytes"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

func TestRedactHandlerHidesTicketsAndSignatures(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(RedactHandler(slog.NewJSONHandler(&out, nil))).With("pre", "https://a/x?t=SECRET0")
	u := &url.Error{Op: "Get", URL: "https://peer/federation/blob/b?k=feed&p=ab&t=SECRET1&b=ip", Err: errors.New("EOF")}
	logger.Warn("fetch https://a/dl?ant-sig=SECRET2.mac failed", "err", u, "g", slog.GroupValue(slog.String("u", "/x?a=1&t=SECRET3")), "n", 7)
	got := out.String()
	for _, s := range []string{"SECRET0", "SECRET1", "SECRET2", "SECRET3"} {
		if strings.Contains(got, s) {
			t.Fatalf("%s leaked: %s", s, got)
		}
	}
	for _, s := range []string{"k=feed", "p=ab", "b=ip", "t=REDACTED", "ant-sig=REDACTED", `"n":7`} {
		if !strings.Contains(got, s) {
			t.Fatalf("missing %q: %s", s, got)
		}
	}
	if RedactURLSecrets("a=1&text=keep&t=x") != "a=1&text=keep&t=REDACTED" {
		t.Fatal(RedactURLSecrets("a=1&text=keep&t=x"))
	}
}
