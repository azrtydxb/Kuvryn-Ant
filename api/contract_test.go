package api_test

import (
	"context"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/getkin/kin-openapi/openapi3"
)

func TestManagementContractIsValid(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		var doc *openapi3.T
		var err error
		if embedded {
			doc, err = api.GetSwagger()
		} else {
			doc, err = openapi3.NewLoader().LoadFromFile("openapi.yaml")
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := doc.Validate(context.Background()); err != nil {
			t.Fatalf("embedded=%v: %v", embedded, err)
		}
	}
}

func TestResponseSchemasRejectBrokenWireContracts(t *testing.T) {
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, schema string
		body         map[string]any
		valid        bool
	}{
		{"service identity", "ServiceStatus", map[string]any{"name": "kuvryn-ant", "api_version": "v1"}, true},
		{"missing version", "ServiceStatus", map[string]any{"name": "kuvryn-ant"}, false},
		{"unsupported version", "ServiceStatus", map[string]any{"name": "kuvryn-ant", "api_version": "v2"}, false},
		{"accidental detail leak", "ServiceStatus", map[string]any{"name": "kuvryn-ant", "api_version": "v1", "database_url": "secret"}, false},
		{"stable error", "Error", map[string]any{"code": "unavailable", "message": "stopping", "request_id": "0123456789abcdef"}, true},
		{"unknown error code", "Error", map[string]any{"code": "oops", "message": "stopping", "request_id": "0123456789abcdef"}, false},
		{"missing correlation", "Error", map[string]any{"code": "unavailable", "message": "stopping"}, false},
		{"short correlation", "Error", map[string]any{"code": "unavailable", "message": "stopping", "request_id": "x"}, false},
		{"page end", "PageInfo", map[string]any{"has_more": false}, true},
		{"missing page indicator", "PageInfo", map[string]any{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := doc.Components.Schemas[tt.schema].Value.VisitJSON(tt.body)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v, validation error=%v", tt.valid, err)
			}
		})
	}
}
