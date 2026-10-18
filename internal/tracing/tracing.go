// Package tracing wires optional OpenTelemetry distributed tracing. It is off
// unless ANT_OTEL_ENABLED is true. Span names and attributes are low
// cardinality by construction: registered route patterns, SQL verbs and fixed
// operation names, never raw paths, queries, SQL text, digests or identities.
package tracing

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/azrtydxb/Kuvryn-Ant"

// Config selects the exporter. Standard OTEL_EXPORTER_OTLP_* variables (headers,
// timeouts, certificates, per-signal endpoints) are honored by the exporters.
type Config struct {
	Enabled     bool
	Endpoint    string // optional full URL; overrides OTEL_EXPORTER_OTLP_ENDPOINT
	Protocol    string // "http/protobuf" (default) or "grpc"
	ServiceName string
	Version     string
	SampleRatio float64 // parent-based trace-id ratio, 0..1
}

// Setup installs the global tracer provider and propagators. It returns a
// shutdown function that flushes pending spans; it is a no-op when disabled.
func Setup(ctx context.Context, c Config) (func(context.Context) error, error) {
	if !c.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	var client otlptrace.Client
	switch c.Protocol {
	case "", "http/protobuf", "http":
		opts := []otlptracehttp.Option{}
		if c.Endpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpointURL(c.Endpoint))
		}
		client = otlptracehttp.NewClient(opts...)
	case "grpc":
		opts := []otlptracegrpc.Option{}
		if c.Endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpointURL(c.Endpoint))
		}
		client = otlptracegrpc.NewClient(opts...)
	default:
		return nil, errors.New("unsupported OTLP protocol")
	}
	exporter, err := otlptrace.New(ctx, client)
	if err != nil {
		return nil, errors.New("cannot initialize OTLP trace exporter")
	}
	return install(c, sdktrace.WithBatcher(exporter))
}

// SetupWithExporter installs a provider that exports synchronously to e. It is
// intended for tests.
func SetupWithExporter(c Config, e sdktrace.SpanExporter) (func(context.Context) error, error) {
	return install(c, sdktrace.WithSyncer(e))
}

func install(c Config, exp sdktrace.TracerProviderOption) (func(context.Context) error, error) {
	name := c.ServiceName
	if name == "" {
		name = "kuvryn-ant"
	}
	attrs := []attribute.KeyValue{attribute.String("service.name", name)}
	if c.Version != "" {
		attrs = append(attrs, attribute.String("service.version", c.Version))
	}
	res := resource.NewSchemaless(attrs...)
	ratio := c.SampleRatio
	if ratio <= 0 || ratio > 1 {
		ratio = 1
	}
	tp := sdktrace.NewTracerProvider(exp, sdktrace.WithResource(res), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}, nil
}

// Start begins a span named by a fixed, low-cardinality string.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	return otel.Tracer(tracerName).Start(ctx, name, oteltrace.WithAttributes(attrs...))
}

// End records err (if any) and ends the span. The error text is never
// recorded because driver and backend errors can carry sensitive detail.
func End(span oteltrace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, "error")
	}
	span.End()
}

// TraceID returns the hex trace ID in ctx, or "" when absent.
func TraceID(ctx context.Context) string {
	sc := oteltrace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

// Middleware traces each HTTP request. resolve must return the registered mux
// pattern (for example "GET /api/v1/feeds/{id}"), which keeps names low
// cardinality. Incoming W3C trace context is honored. Liveness probes are not
// traced.
func Middleware(resolve func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route := ""
			if resolve != nil {
				route = resolve(r)
			}
			if route == "" {
				route = "unmatched"
			}
			if strings.HasSuffix(route, "/health/live") {
				next.ServeHTTP(w, r)
				return
			}
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			name := route
			if !strings.Contains(route, " ") { // patterns registered without a method
				name = methodLabel(r.Method) + " " + route
			}
			ctx, span := otel.Tracer(tracerName).Start(ctx, name, oteltrace.WithSpanKind(oteltrace.SpanKindServer),
				oteltrace.WithAttributes(attribute.String("http.request.method", methodLabel(r.Method)), attribute.String("http.route", route)))
			rec := &statusWriter{ResponseWriter: w}
			defer func() {
				failure := recover()
				status := rec.status
				if status == 0 {
					status = http.StatusOK
					if failure != nil {
						status = http.StatusInternalServerError
					}
				}
				span.SetAttributes(attribute.Int("http.response.status_code", status))
				if status >= 500 {
					span.SetStatus(codes.Error, "server error")
				}
				span.End()
				if failure != nil {
					panic(failure)
				}
			}()
			next.ServeHTTP(rec, r.WithContext(ctx))
		})
	}
}

func methodLabel(m string) string {
	switch m {
	case "GET", "HEAD", "POST", "PATCH", "PUT", "DELETE", "OPTIONS":
		return m
	}
	return "OTHER"
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *statusWriter) WriteHeader(code int) {
	if code >= 200 && w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// LogHandler adds trace_id to every record logged with a context carrying a span.
func LogHandler(next slog.Handler) slog.Handler { return logHandler{next} }

type logHandler struct{ slog.Handler }

func (h logHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := TraceID(ctx); id != "" {
		r.AddAttrs(slog.String("trace_id", id))
	}
	return h.Handler.Handle(ctx, r)
}
func (h logHandler) WithAttrs(a []slog.Attr) slog.Handler { return logHandler{h.Handler.WithAttrs(a)} }
func (h logHandler) WithGroup(n string) slog.Handler      { return logHandler{h.Handler.WithGroup(n)} }

// PGTracer implements pgx.QueryTracer. Each statement becomes a client span
// named by its SQL verb ("db select", "db begin", "db commit"); SQL text and
// arguments are never recorded. Transactions are visible as begin/commit spans.
// Statements outside a traced request (background workers) are not traced.
type PGTracer struct{}

type pgKey struct{}

func (PGTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if !oteltrace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	verb := sqlVerb(d.SQL)
	ctx, span := Start(ctx, "db "+verb, attribute.String("db.system", "postgresql"), attribute.String("db.operation", verb))
	return context.WithValue(ctx, pgKey{}, span)
}

func (PGTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if span, ok := ctx.Value(pgKey{}).(oteltrace.Span); ok {
		End(span, d.Err)
	}
}

func sqlVerb(sql string) string {
	s := strings.TrimLeft(sql, " \t\r\n(")
	for strings.HasPrefix(s, "--") || strings.HasPrefix(s, "/*") {
		var rest string
		if strings.HasPrefix(s, "--") {
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return "other"
			}
			rest = s[i:]
		} else {
			i := strings.Index(s, "*/")
			if i < 0 {
				return "other"
			}
			rest = s[i+2:]
		}
		s = strings.TrimLeft(rest, " \t\r\n")
	}
	end := strings.IndexAny(s, " \t\r\n;(")
	if end < 0 {
		end = len(s)
	}
	switch v := strings.ToLower(s[:end]); v {
	case "select", "insert", "update", "delete", "begin", "commit", "rollback", "with", "lock", "set", "show", "create", "alter", "drop", "truncate", "savepoint", "release":
		return v
	}
	return "other"
}
