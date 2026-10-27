package telemetry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestSignalsEnabled(t *testing.T) {
	cases := []struct {
		name            string
		env             map[string]string
		traces, metrics bool
	}{
		{"unset", nil, false, false},
		{"shared endpoint", map[string]string{envEndpoint: "http://collector:4318"}, true, true},
		{"traces only", map[string]string{envTracesEndpoint: "http://c/v1/traces"}, true, false},
		{"metrics only", map[string]string{envMetricsEndpoint: "http://c/v1/metrics"}, false, true},
		{"blank endpoint", map[string]string{envEndpoint: "  "}, false, false},
		{"sdk disabled", map[string]string{envEndpoint: "http://c", envSDKDisabled: "TRUE"}, false, false},
		{"traces exporter none", map[string]string{envEndpoint: "http://c", envTracesExporter: "none"}, false, true},
		{"metrics exporter none", map[string]string{envEndpoint: "http://c", envMetricsExporter: "None"}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			traces, metrics := signalsEnabled(envOf(tc.env))
			if traces != tc.traces || metrics != tc.metrics {
				t.Fatalf("signalsEnabled = (%v, %v), want (%v, %v)", traces, metrics, tc.traces, tc.metrics)
			}
		})
	}
}

// With no endpoint configured Init must leave the global providers alone:
// that is the zero-behaviour-change contract for current users.
func TestInitWithoutEndpointKeepsNoopProviders(t *testing.T) {
	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	shutdown, err := initFromEnv(context.Background(), "v1", envOf(nil))
	if err != nil {
		t.Fatalf("initFromEnv: %v", err)
	}
	shutdown()
	shutdown()
	if otel.GetTracerProvider() != tp || otel.GetMeterProvider() != mp {
		t.Fatal("initFromEnv replaced a global provider without an endpoint")
	}
}

func TestInitRejectsGRPCProtocol(t *testing.T) {
	tp := otel.GetTracerProvider()
	_, err := initFromEnv(context.Background(), "v1", envOf(map[string]string{
		envEndpoint: "http://collector:4317",
		envProtocol: "grpc",
	}))
	if err == nil || !strings.Contains(err.Error(), envProtocol) {
		t.Fatalf("err = %v, want an unsupported-protocol error naming %s", err, envProtocol)
	}
	if otel.GetTracerProvider() != tp {
		t.Fatal("a rejected configuration must not install a provider")
	}
}

func TestNewResourceDefaultsAndEnvOverride(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	res, err := newResource(context.Background(), "1.2.3")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	if got, _ := res.Set().Value(attribute.Key("service.name")); got.AsString() != defaultServiceName {
		t.Fatalf("service.name = %q, want %q", got.AsString(), defaultServiceName)
	}
	if got, _ := res.Set().Value(attribute.Key("service.version")); got.AsString() != "1.2.3" {
		t.Fatalf("service.version = %q, want 1.2.3", got.AsString())
	}

	t.Setenv("OTEL_SERVICE_NAME", "crux-edge")
	res, err = newResource(context.Background(), "")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	if got, _ := res.Set().Value(attribute.Key("service.name")); got.AsString() != "crux-edge" {
		t.Fatalf("service.name = %q, want OTEL_SERVICE_NAME to override", got.AsString())
	}
	if _, ok := res.Set().Value(attribute.Key("service.version")); ok {
		t.Fatal("an empty version must not stamp service.version")
	}
}

// With an endpoint set, Init must actually export: one span and one counter
// recorded through the OTel API reach the collector as /v1/traces and
// /v1/metrics POSTs when Shutdown flushes.
func TestInitExportsToCollectorOnShutdown(t *testing.T) {
	prevTP, prevMP, prevProp := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
	})

	var (
		mu    sync.Mutex
		posts = map[string]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			posts[r.URL.Path]++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(envEndpoint, srv.URL)

	flush, err := Init(context.Background(), "test")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, span := otel.Tracer("crux/telemetry/test").Start(context.Background(), "test.span")
	span.End()
	counter, err := otel.Meter("crux/telemetry/test").Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(context.Background(), 1)
	flush()

	mu.Lock()
	defer mu.Unlock()
	if posts["/v1/traces"] == 0 || posts["/v1/metrics"] == 0 {
		t.Fatalf("collector POSTs = %v, want /v1/traces and /v1/metrics", posts)
	}
}

// caCertPEM writes a self-signed certificate the exporters' CERTIFICATE env
// variables can load, so a TLS root can be configured against an http://
// endpoint — the one exporter construction error reachable through env.
func caCertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "crux-telemetry-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestInitResourceErrorInstallsNothing(t *testing.T) {
	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment") // no '=': ErrPartialResource
	shutdown, err := initFromEnv(context.Background(), "v1", envOf(map[string]string{envEndpoint: "http://collector:4318"}))
	if err == nil {
		t.Fatal("initFromEnv must surface an invalid OTEL_RESOURCE_ATTRIBUTES")
	}
	shutdown()
	if otel.GetTracerProvider() != tp || otel.GetMeterProvider() != mp {
		t.Fatal("a resource error must not install a provider")
	}
}

func TestInitTraceExporterErrorInstallsNothing(t *testing.T) {
	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	t.Setenv(envEndpoint, "http://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", caCertPEM(t))
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "true")
	shutdown, err := initFromEnv(context.Background(), "v1", envOf(map[string]string{envEndpoint: "http://collector:4318"}))
	if err == nil || !strings.Contains(err.Error(), "trace exporter") {
		t.Fatalf("err = %v, want a trace exporter error", err)
	}
	shutdown()
	if otel.GetTracerProvider() != tp || otel.GetMeterProvider() != mp {
		t.Fatal("a trace exporter error must not install a provider")
	}
}

// When the metric pipeline fails after the trace pipeline was built, the
// trace provider is shut down and neither global is replaced.
func TestInitMetricExporterErrorTearsDownTraces(t *testing.T) {
	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	t.Setenv(envEndpoint, "http://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", caCertPEM(t))
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_INSECURE", "true")
	shutdown, err := initFromEnv(context.Background(), "v1", envOf(map[string]string{envEndpoint: "http://collector:4318"}))
	if err == nil || !strings.Contains(err.Error(), "metric exporter") {
		t.Fatalf("err = %v, want a metric exporter error", err)
	}
	shutdown()
	if otel.GetTracerProvider() != tp || otel.GetMeterProvider() != mp {
		t.Fatal("a metric exporter error must not leave the trace provider installed")
	}
}
