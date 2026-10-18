package management_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
)

func TestStatusGeneratedClientAndOpenAPIConformance(t *testing.T) {
	mux := http.NewServeMux()
	management.Register(mux)
	server := httptest.NewServer(management.WithRequestID(mux))
	defer server.Close()
	client, err := api.NewClientWithResponses(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.GetStatusWithResponse(context.Background()) // Public: no authentication headers.
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil {
		t.Fatalf("status response: %s %s", response.Status(), response.Body)
	}
	if response.JSON200.Name != "kuvryn-ant" || response.JSON200.ApiVersion != "v1" {
		t.Fatalf("unexpected identity: %+v", response.JSON200)
	}
	if response.Headers200 == nil || response.Headers200.XRequestID == nil || len(*response.Headers200.XRequestID) < 16 {
		t.Fatal("missing generated-client request ID")
	}
	if response.HTTPResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("management response must not be cached")
	}
	validateResponse(t, response.HTTPResponse.Request, response.StatusCode(), response.HTTPResponse.Header, response.Body)
}

func TestErrorCorrelationAndCallerIsolation(t *testing.T) {
	for _, middleware := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "middleware"}[middleware], func(t *testing.T) {
			var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				management.Error(w, r, http.StatusServiceUnavailable, api.ErrorCodeUnavailable, "service stopping")
			})
			if middleware {
				handler = management.WithRequestID(handler)
			}
			seen := map[string]bool{}
			for range 2 {
				request := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/status", nil)
				request.Header.Set("X-Request-ID", "caller-controlled-correlation")
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				var body api.Error
				if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				id := recorder.Header().Get("X-Request-ID")
				if len(id) < 16 || id == request.Header.Get("X-Request-ID") || seen[id] {
					t.Fatalf("request ID is not fresh and server owned: %q", id)
				}
				seen[id] = true
				if body.RequestId != id || body.Code != api.ErrorCodeUnavailable || body.Message != "service stopping" {
					t.Fatalf("error/header mismatch: %+v, id=%q", body, id)
				}
				validateResponse(t, request, recorder.Code, recorder.Header(), recorder.Body.Bytes())
			}
		})
	}
}

func TestUnknownManagementRouteUsesStructuredError(t *testing.T) {
	mux := http.NewServeMux()
	management.Register(mux)
	recorder := httptest.NewRecorder()
	management.WithRequestID(mux).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/missing", nil))
	var body api.Error
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusNotFound || body.Code != api.ErrorCodeNotFound || body.RequestId != recorder.Header().Get("X-Request-ID") {
		t.Fatalf("unexpected management fallback: %d %+v", recorder.Code, body)
	}
}

func validateResponse(t *testing.T, request *http.Request, status int, header http.Header, body []byte) {
	t.Helper()
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	route, params, err := router.FindRoute(request)
	if err != nil {
		t.Fatal(err)
	}
	input := &openapi3filter.RequestValidationInput{Request: request, PathParams: params, Route: route}
	if err := openapi3filter.ValidateRequest(request.Context(), input); err != nil {
		t.Fatalf("request contract: %v", err)
	}
	output := (&openapi3filter.ResponseValidationInput{RequestValidationInput: input, Status: status, Header: header}).SetBodyBytes(body)
	if err := openapi3filter.ValidateResponse(request.Context(), output); err != nil {
		t.Fatalf("response contract: %v", err)
	}
}
