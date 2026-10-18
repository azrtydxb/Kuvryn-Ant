package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func setup(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := SetupWithExporter(Config{Enabled: true, ServiceName: "test"}, exp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	return exp
}

func TestDisabledSetupIsNoop(t *testing.T) {
	shutdown, err := Setup(context.Background(), Config{})
	if err != nil || shutdown(context.Background()) != nil {
		t.Fatal("disabled tracing must be a no-op", err)
	}
	if _, err := Setup(context.Background(), Config{Enabled: true, Protocol: "carrier-pigeon"}); err == nil {
		t.Fatal("unknown protocol accepted")
	}
}

func TestHTTPSpanUsesRoutePatternAndLogsTraceID(t *testing.T) {
	exp := setup(t)
	var logs bytes.Buffer
	logger := slog.New(LogHandler(slog.NewJSONHandler(&logs, nil)))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A child span, as a blob or upstream operation would create.
		_, child := Start(r.Context(), "blob open")
		End(child, errors.New("secret detail must not be exported"))
		logger.InfoContext(r.Context(), "inside")
		w.WriteHeader(http.StatusTeapot)
	})
	handler := Middleware(func(*http.Request) string { return "GET /api/v1/feeds/{id}" })(inner)
	req := httptest.NewRequest("GET", "/api/v1/feeds/3f2c9b1e-0000-0000-0000-000000000001?token=hunter2", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d", len(spans))
	}
	var server, child tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "GET /api/v1/feeds/{id}" {
			server = s
		} else {
			child = s
		}
	}
	if server.Name == "" || child.Name != "blob open" || child.Parent.SpanID() != server.SpanContext.SpanID() {
		t.Fatalf("unexpected span tree: %+v", spans)
	}
	if child.Status.Description != "error" {
		t.Fatalf("error text leaked into span status: %q", child.Status.Description)
	}
	for _, s := range spans {
		raw, _ := json.Marshal(s.Attributes)
		if bytes.Contains(raw, []byte("hunter2")) || bytes.Contains(raw, []byte("3f2c9b1e")) {
			t.Fatalf("span attributes carry request data: %s", raw)
		}
	}
	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["trace_id"] != server.SpanContext.TraceID().String() {
		t.Fatalf("trace_id not in log line: %v", line)
	}
}

func TestSQLVerbIsLowCardinality(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT * FROM assets WHERE id=$1": "select", "  begin": "begin", "commit": "commit",
		"-- note\nINSERT INTO x VALUES(1)": "insert", "/* c */ UPDATE t SET a=1": "update", "VACUUM": "other", "": "other",
	} {
		if got := sqlVerb(in); got != want {
			t.Fatalf("sqlVerb(%q)=%q want %q", in, got, want)
		}
	}
}

func TestLivenessIsNotTraced(t *testing.T) {
	exp := setup(t)
	h := Middleware(func(*http.Request) string { return "GET /health/live" })(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health/live", nil))
	if len(exp.GetSpans()) != 0 {
		t.Fatal("liveness probe produced a span")
	}
}
