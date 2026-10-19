package api

import (
	"context"
	"os"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// The peer protocol document is published next to the management API and must
// stay a valid OpenAPI document that names every federation listener route.
func TestFederationPeerSpecIsValid(t *testing.T) {
	raw, err := os.ReadFile("federation-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromData(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/federation/v1/join", "/federation/v1/join/confirm", "/federation/v1/heads", "/federation/v1/documents/{kind}", "/federation/v1/nudge",
		"/federation/v1/search", "/federation/v1/audit/query", "/federation/v1/authority/prepare", "/federation/v1/authority/commit",
		"/federation/v1/proxy/feeds/{fed}/{slug}/{rest}", "/federation/v1/replication/export/feeds/{feed}/{part}"} {
		if doc.Paths.Value(path) == nil {
			t.Errorf("peer protocol document lacks %s", path)
		}
	}
}
