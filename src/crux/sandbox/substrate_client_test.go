package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/hivecommons/hive/crux/sandbox/substrateapi"
	"github.com/hivecommons/hive/crux/sandbox/substratebridge"
)

// Real-client tests: the gRPC adapter against an in-memory Control server,
// TLS/credential plumbing against files in a temp dir, and the router bridge
// against a TLS server that speaks CONNECT and then the bridge protocol.

// selfSignedBundle writes a PEM file holding a self-signed cert and its key
// (the shape of a podCertificate credential bundle) plus a CA-only file.
func selfSignedBundle(t *testing.T, dir string) (bundle, caOnly string, cert tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "hive-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	bundle = filepath.Join(dir, "credential-bundle.pem")
	caOnly = filepath.Join(dir, "router-ca.pem")
	if err := os.WriteFile(bundle, append(certPEM, keyPEM...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caOnly, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return bundle, caOnly, cert
}

func TestNewSubstrateGRPCClientConstructs(t *testing.T) {
	dir := t.TempDir()
	bundle, ca, _ := selfSignedBundle(t, dir)

	// TLS endpoint verified against ca_file, router verified against its CA.
	c, err := newSubstrateGRPCClient(SubstrateOptions{
		Endpoint: "api.ate-system.svc:443", CAFile: ca, TokenFile: filepath.Join(dir, "token"),
		RouterClientBundle: bundle, RouterCAFile: ca,
	})
	if err != nil {
		t.Fatalf("tls client: %v", err)
	}
	if c.Control() == nil || c.Bridge() == nil {
		t.Error("nil control or bridge")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	// Plaintext endpoint with an env token and insecure router.
	c, err = newSubstrateGRPCClient(SubstrateOptions{Endpoint: "http://localhost:8080", Token: "t", InsecureTLS: true, RouterClientBundle: bundle})
	if err != nil {
		t.Fatalf("plaintext client: %v", err)
	}
	_ = c.Close()

	// Plaintext without any token still constructs (no per-RPC creds).
	c, err = newSubstrateGRPCClient(SubstrateOptions{Endpoint: "http://localhost:8080", InsecureTLS: true, RouterClientBundle: bundle})
	if err != nil {
		t.Fatalf("plaintext no-token client: %v", err)
	}
	_ = c.Close()

	// Insecure TLS skips the CA file entirely.
	c, err = newSubstrateGRPCClient(SubstrateOptions{InsecureTLS: true, CAFile: "/nonexistent", RouterClientBundle: bundle})
	if err != nil {
		t.Fatalf("insecure client: %v", err)
	}
	_ = c.Close()
}

func TestNewSubstrateGRPCClientErrors(t *testing.T) {
	dir := t.TempDir()
	bundle, ca, _ := selfSignedBundle(t, dir)
	noPEM := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(noPEM, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		o    SubstrateOptions
		want string
	}{
		{"endpoint", SubstrateOptions{Endpoint: "nope"}, "endpoint"},
		{"ca missing", SubstrateOptions{CAFile: "/nonexistent/ca.crt"}, "ca_file"},
		{"ca not pem", SubstrateOptions{CAFile: noPEM}, "no PEM certificates"},
		{"router url", SubstrateOptions{CAFile: ca, RouterURL: "http://r"}, "router_url"},
		{"router bundle", SubstrateOptions{CAFile: ca, RouterClientBundle: "/nonexistent/b.pem"}, "router_client_bundle"},
		{"router ca", SubstrateOptions{CAFile: ca, RouterClientBundle: bundle, RouterCAFile: "/nonexistent/ca.pem"}, "router_ca_file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newSubstrateGRPCClient(tc.o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSubstrateControlTLSDefaults(t *testing.T) {
	cfg, err := substrateControlTLS(SubstrateOptions{})
	if err != nil || cfg.RootCAs != nil || cfg.InsecureSkipVerify {
		t.Errorf("default tls = %+v, %v (system roots expected when the projected CA is absent)", cfg, err)
	}
	_, ca, _ := selfSignedBundle(t, t.TempDir())
	cfg, err = substrateControlTLS(SubstrateOptions{CAFile: ca})
	if err != nil || cfg.RootCAs == nil {
		t.Errorf("ca_file tls = %+v, %v", cfg, err)
	}
}

func TestSubstrateBearer(t *testing.T) {
	ctx := context.Background()
	md, err := substrateBearer{token: "abc", secure: true}.GetRequestMetadata(ctx)
	if err != nil || md["authorization"] != "Bearer abc" {
		t.Errorf("static token: %v %v", md, err)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "token")
	if err := os.WriteFile(f, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	md, err = substrateBearer{file: f}.GetRequestMetadata(ctx)
	if err != nil || md["authorization"] != "Bearer from-file" {
		t.Errorf("file token: %v %v", md, err)
	}
	if _, err := (substrateBearer{file: filepath.Join(dir, "missing")}).GetRequestMetadata(ctx); err == nil || !strings.Contains(err.Error(), "reading token file") {
		t.Errorf("missing file: %v", err)
	}
	if err := os.WriteFile(f, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (substrateBearer{file: f}).GetRequestMetadata(ctx); err == nil || !strings.Contains(err.Error(), "token is empty") {
		t.Errorf("empty file: %v", err)
	}
	if !(substrateBearer{secure: true}).RequireTransportSecurity() || (substrateBearer{}).RequireTransportSecurity() {
		t.Error("RequireTransportSecurity must follow the TLS choice")
	}
}

// controlServer is the in-memory Control API the adapter is tested against.
type controlServer struct {
	substrateapi.UnimplementedControlServer
	fail  bool
	pages int
}

func (s *controlServer) err() error {
	if s.fail {
		return status.Error(codes.Internal, "injected")
	}
	return nil
}

func (s *controlServer) CreateActorTemplate(_ context.Context, r *substrateapi.CreateActorTemplateRequest) (*substrateapi.ActorTemplate, error) {
	return r.GetActorTemplate(), s.err()
}
func (s *controlServer) DeleteActorTemplate(context.Context, *substrateapi.DeleteActorTemplateRequest) (*substrateapi.ActorTemplate, error) {
	return &substrateapi.ActorTemplate{}, s.err()
}
func (s *controlServer) CreateActor(_ context.Context, r *substrateapi.CreateActorRequest) (*substrateapi.Actor, error) {
	return r.GetActor(), s.err()
}
func (s *controlServer) GetActor(_ context.Context, r *substrateapi.GetActorRequest) (*substrateapi.Actor, error) {
	return &substrateapi.Actor{Metadata: &substrateapi.ResourceMetadata{Name: r.GetActor().GetName()}}, s.err()
}
func (s *controlServer) ResumeActor(context.Context, *substrateapi.ResumeActorRequest) (*substrateapi.ResumeActorResponse, error) {
	return &substrateapi.ResumeActorResponse{}, s.err()
}
func (s *controlServer) SuspendActor(_ context.Context, r *substrateapi.SuspendActorRequest) (*substrateapi.SuspendActorResponse, error) {
	if r.GetOptions().GetMode() != substrateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS {
		return nil, status.Error(codes.InvalidArgument, "mode not forwarded")
	}
	return &substrateapi.SuspendActorResponse{}, s.err()
}
func (s *controlServer) DeleteActor(context.Context, *substrateapi.DeleteActorRequest) (*substrateapi.Actor, error) {
	return &substrateapi.Actor{}, s.err()
}
func (s *controlServer) CreateActorEgressPolicy(_ context.Context, r *substrateapi.CreateActorEgressPolicyRequest) (*substrateapi.EgressPolicy, error) {
	return r.GetEgressPolicy(), s.err()
}
func (s *controlServer) ListWorkers(_ context.Context, r *substrateapi.ListWorkersRequest) (*substrateapi.ListWorkersResponse, error) {
	if err := s.err(); err != nil {
		return nil, err
	}
	s.pages++
	if r.GetOptions().GetPageToken() == "" {
		return &substrateapi.ListWorkersResponse{Workers: []*substrateapi.Worker{{Metadata: &substrateapi.ResourceMetadata{Name: "w1"}}}, NextPageToken: "next"}, nil
	}
	return &substrateapi.ListWorkersResponse{Workers: []*substrateapi.Worker{{Metadata: &substrateapi.ResourceMetadata{Name: "w2"}}}}, nil
}

func newBufconnControl(t *testing.T, srv substrateapi.ControlServer) *grpcControl {
	t.Helper()
	ln := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	substrateapi.RegisterControlServer(gs, srv)
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &grpcControl{c: substrateapi.NewControlClient(conn)}
}

func TestGRPCControlAdapter(t *testing.T) {
	srv := &controlServer{}
	g := newBufconnControl(t, srv)
	ctx := context.Background()
	ref := &substrateapi.ObjectRef{Atespace: "hive", Name: "a1"}
	if tpl, err := g.CreateActorTemplate(ctx, &substrateapi.ActorTemplate{Metadata: &substrateapi.ResourceMetadata{Name: "t"}}); err != nil || tpl.GetMetadata().GetName() != "t" {
		t.Errorf("CreateActorTemplate: %v %v", tpl, err)
	}
	if err := g.DeleteActorTemplate(ctx, ref); err != nil {
		t.Errorf("DeleteActorTemplate: %v", err)
	}
	if a, err := g.CreateActor(ctx, &substrateapi.Actor{Metadata: &substrateapi.ResourceMetadata{Name: "a1"}}); err != nil || a.GetMetadata().GetName() != "a1" {
		t.Errorf("CreateActor: %v %v", a, err)
	}
	if a, err := g.GetActor(ctx, ref); err != nil || a.GetMetadata().GetName() != "a1" {
		t.Errorf("GetActor: %v %v", a, err)
	}
	if err := g.ResumeActor(ctx, ref); err != nil {
		t.Errorf("ResumeActor: %v", err)
	}
	if err := g.SuspendActor(ctx, ref, substrateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS); err != nil {
		t.Errorf("SuspendActor: %v", err)
	}
	if err := g.DeleteActor(ctx, ref); err != nil {
		t.Errorf("DeleteActor: %v", err)
	}
	if err := g.CreateActorEgressPolicy(ctx, ref, &substrateapi.EgressPolicy{}); err != nil {
		t.Errorf("CreateActorEgressPolicy: %v", err)
	}
	workers, err := g.ListWorkers(ctx)
	if err != nil || len(workers) != 2 || srv.pages != 2 {
		t.Errorf("ListWorkers: %d workers, %d pages, %v", len(workers), srv.pages, err)
	}

	srv.fail = true
	if _, err := g.ListWorkers(ctx); status.Code(err) != codes.Internal {
		t.Errorf("ListWorkers error not propagated: %v", err)
	}
	if _, err := g.CreateActorTemplate(ctx, nil); status.Code(err) != codes.Internal {
		t.Errorf("CreateActorTemplate error not propagated: %v", err)
	}
}

// connectRouter is a TLS server that accepts CONNECT like atenet-router and
// then answers one bridge request over the tunnel.
type connectRouter struct {
	t        *testing.T
	handler  http.Handler
	refuse   bool
	lastHdr  string
	lastHost string
}

func (r *connectRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodConnect {
		http.Error(w, "not connect", http.StatusMethodNotAllowed)
		return
	}
	r.lastHdr = req.Header.Get(substrateRouterHeader)
	r.lastHost = req.Host
	if r.refuse {
		http.Error(w, "spiffe id not allowed", http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		r.t.Fatal("no hijacker")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		r.t.Fatal(err)
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
	_ = rw.Flush()
	inner, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, inner)
	resp := rec.Result()
	resp.ContentLength = int64(rec.Body.Len())
	_ = resp.Write(conn)
}

func newConnectRouter(t *testing.T, h http.Handler) (*connectRouter, *routerBridge) {
	t.Helper()
	cr := &connectRouter{t: t, handler: h}
	srv := httptest.NewUnstartedServer(cr)
	// The untrusted-certificate case below makes the server log a handshake
	// error; that is the expected outcome, not noise worth printing.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "https://")
	b := &routerBridge{
		routerAddr: addr,
		tlsConfig:  &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // test server
		port:       substratebridge.DefaultPort,
		dial:       (&net.Dialer{}).DialContext,
	}
	return cr, b
}

func TestRouterBridgeDrivesTheBridgeProtocol(t *testing.T) {
	actorWS := filepath.Join(t.TempDir(), "actor-ws")
	cr, b := newConnectRouter(t, &substratebridge.Handler{Workspace: actorWS})
	ctx := context.Background()
	ref := &substrateapi.ObjectRef{Atespace: "hive", Name: "hive-kick-deps-1"}

	if err := b.WaitReady(ctx, ref, time.Millisecond); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if cr.lastHdr != "hive/hive-kick-deps-1" || cr.lastHost != "actor:8477" {
		t.Errorf("CONNECT header=%q host=%q", cr.lastHdr, cr.lastHost)
	}

	local := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(filepath.Join(local, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "sub", "file.txt"), []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Push(ctx, ref, local); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(actorWS, "sub", "file.txt")); string(got) != "before" {
		t.Fatalf("actor workspace = %q", got)
	}
	var live strings.Builder
	out, err := b.Run(ctx, ref, substratebridge.RunRequest{Command: []string{"sh", "-c", "echo after > sub/file.txt; echo ran; exit 4"}}, &live)
	if err != nil || out.ExitCode != 4 || !strings.Contains(out.Stdout, "ran") {
		t.Fatalf("Run: %+v %v", out, err)
	}
	if live.String() != "ran\n" {
		t.Errorf("streamed output = %q", live.String())
	}
	if err := b.Pull(ctx, ref, local); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(local, "sub", "file.txt")); strings.TrimSpace(string(got)) != "after" {
		t.Errorf("local workspace after pull = %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Dir(local)); len(entries) != 1 {
		t.Errorf("staging dirs left behind: %v", entries)
	}
}

func TestRouterBridgeErrors(t *testing.T) {
	ctx := context.Background()
	ref := &substrateapi.ObjectRef{Atespace: "hive", Name: "a"}

	// Router refuses the CONNECT.
	cr, b := newConnectRouter(t, http.NotFoundHandler())
	cr.refuse = true
	if err := b.Push(ctx, ref, t.TempDir()); err == nil || !strings.Contains(err.Error(), "router refused connect") {
		t.Errorf("refused connect: %v", err)
	}

	// Bridge answers non-2xx.
	_, b = newConnectRouter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unpack failed", http.StatusBadRequest)
	}))
	if err := b.Push(ctx, ref, t.TempDir()); err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "unpack failed") {
		t.Errorf("non-2xx: %v", err)
	}
	if _, err := b.Run(ctx, ref, substratebridge.RunRequest{Command: []string{"x"}}, nil); err == nil {
		t.Error("Run swallowed a non-2xx")
	}
	if err := b.Pull(ctx, ref, t.TempDir()); err == nil {
		t.Error("Pull swallowed a non-2xx")
	}

	// Bridge answers 200 with garbage JSON / garbage tar.
	_, b = newConnectRouter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	if _, err := b.Run(ctx, ref, substratebridge.RunRequest{Command: []string{"x"}}, nil); err == nil || !strings.Contains(err.Error(), "decoding run response") {
		t.Errorf("bad json: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.Pull(ctx, ref, ws); err == nil || !strings.Contains(err.Error(), "unpacking") {
		t.Errorf("bad tar: %v", err)
	}

	// Push of a workspace that does not exist fails before any I/O.
	if err := b.Push(ctx, ref, filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "packing") {
		t.Errorf("missing workspace: %v", err)
	}

	// TLS to a server whose certificate is not trusted.
	_, strict := newConnectRouter(t, http.NotFoundHandler())
	strict.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: x509.NewCertPool(), ServerName: "localhost"}
	if _, err := strict.tunnel(ctx, ref); err == nil || !strings.Contains(err.Error(), "tls handshake") {
		t.Errorf("untrusted server: %v", err)
	}

	// Dial failure.
	b.dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no route") }
	if _, err := b.do(ctx, ref, http.MethodGet, substratebridge.PathHealth, nil); err == nil || !strings.Contains(err.Error(), "no route") {
		t.Errorf("dial failure: %v", err)
	}

	// Server closes the tunnel without a CONNECT response.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, _, serverCert := selfSignedBundle(t, t.TempDir())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12})
			_ = tc.Handshake()
			_ = tc.Close()
		}
	}()
	closing := &routerBridge{routerAddr: ln.Addr().String(), tlsConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, port: 1, dial: (&net.Dialer{}).DialContext} //nolint:gosec // test server
	if _, err := closing.tunnel(ctx, ref); err == nil || !strings.Contains(err.Error(), "router connect") {
		t.Errorf("closed tunnel: %v", err)
	}
}

func TestRouterBridgeWaitReadyGivesUpOnContext(t *testing.T) {
	// The bridge answers 503 and the kick is cancelled right after that
	// first probe, so WaitReady must stop without waiting for the ticker.
	ctx, cancel := context.WithCancel(context.Background())
	_, b := newConnectRouter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	err := b.WaitReady(ctx, &substrateapi.ObjectRef{Atespace: "hive", Name: "a"}, time.Hour)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "last:") {
		t.Fatalf("err = %v, want context.Canceled wrapping the last probe error", err)
	}
}

func TestRouterBridgeRequestContextCancelled(t *testing.T) {
	_, b := newConnectRouter(t, http.NotFoundHandler())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.do(ctx, &substrateapi.ObjectRef{}, http.MethodGet, substratebridge.PathHealth, nil); err == nil {
		t.Error("do with a cancelled context succeeded")
	}
	if _, err := b.do(context.Background(), &substrateapi.ObjectRef{}, "bad method", "/x", nil); err == nil {
		t.Error("do with an invalid method succeeded")
	}
}

func TestNewRouterBridgeLoadsCredentials(t *testing.T) {
	bundle, ca, _ := selfSignedBundle(t, t.TempDir())
	b, err := newRouterBridge(SubstrateOptions{RouterClientBundle: bundle, RouterCAFile: ca, BridgePort: 9001, RouterURL: "https://router.example:8444"})
	if err != nil {
		t.Fatalf("newRouterBridge: %v", err)
	}
	if b.routerAddr != "router.example:8444" || b.port != 9001 || len(b.tlsConfig.Certificates) != 1 || b.tlsConfig.RootCAs == nil || b.tlsConfig.ServerName != "router.example" {
		t.Errorf("bridge = %+v", b)
	}
	// Dialer is wired to the real network.
	if _, err := b.dial(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Error("dial to a closed port succeeded")
	}
}

func TestSubstrateReplaceDirErrors(t *testing.T) {
	var good bytes.Buffer
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := substratebridge.Pack(src, &good); err != nil {
		t.Fatal(err)
	}
	// Parent of the workspace is not writable for the staging dir.
	if err := substrateReplaceDir(bytes.NewReader(good.Bytes()), "/nonexistent-parent/ws"); err == nil {
		t.Error("staging in a missing parent succeeded")
	}
	// Workspace itself missing: rename of the old tree fails and staging is removed.
	parent := t.TempDir()
	if err := substrateReplaceDir(bytes.NewReader(good.Bytes()), filepath.Join(parent, "missing")); err == nil {
		t.Error("replacing a missing workspace succeeded")
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Errorf("staging left behind: %v", entries)
	}
}

// Preflight.

type fakeSubstrateFactory struct {
	fake *fakeSubstrate
	err  error
}

func (f fakeSubstrateFactory) new(SubstrateOptions) (substrateClient, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.fake, nil
}

func worker(name, state string, labels map[string]string, classes ...string) *substrateapi.Worker {
	w := &substrateapi.Worker{Metadata: &substrateapi.ResourceMetadata{Name: name}, Labels: labels, Status: &substrateapi.WorkerStatus{}}
	switch state {
	case "active":
		w.Status.State = substrateapi.WorkerState_WORKER_STATE_ACTIVE
	default:
		w.Status.State = substrateapi.WorkerState_WORKER_STATE_UNSPECIFIED
	}
	for _, c := range classes {
		w.Status.SandboxClasses = append(w.Status.SandboxClasses, &substrateapi.WorkerSandboxClass{SandboxClass: c})
	}
	return w
}

func TestSubstratePreflighterWarnings(t *testing.T) {
	ctx := context.Background()
	opts := SubstrateOptions{Endpoint: "api:443", Token: "t"}

	p := &SubstratePreflighter{newClient: fakeSubstrateFactory{err: errors.New("no bundle")}.new}
	if got := p.Check(ctx, opts); len(got) != 1 || !strings.Contains(got[0], "unreachable") || !strings.Contains(got[0], "no bundle") {
		t.Errorf("constructor failure: %q", got)
	}

	fake := &fakeSubstrate{listErr: errors.New("connection refused")}
	p = &SubstratePreflighter{newClient: fakeSubstrateFactory{fake: fake}.new}
	if got := p.Check(ctx, opts); len(got) != 1 || !strings.Contains(got[0], "unreachable") {
		t.Errorf("list failure: %q", got)
	}
	if !fake.closed {
		t.Error("client not closed after probe")
	}

	fake = &fakeSubstrate{}
	p = &SubstratePreflighter{newClient: fakeSubstrateFactory{fake: fake}.new}
	if got := p.Check(ctx, opts); len(got) != 1 || !strings.Contains(got[0], "no active workers") {
		t.Errorf("no workers: %q", got)
	}

	fake = &fakeSubstrate{workers: []*substrateapi.Worker{worker("w", "active", map[string]string{"class": "microvm"}, "microvm")}}
	p = &SubstratePreflighter{newClient: fakeSubstrateFactory{fake: fake}.new}
	sel := opts
	sel.WorkerSelector = map[string]string{"class": "gvisor"}
	if got := p.Check(ctx, sel); len(got) != 1 || !strings.Contains(got[0], "worker_selector") {
		t.Errorf("selector mismatch: %q", got)
	}

	fake = &fakeSubstrate{workers: []*substrateapi.Worker{worker("w", "active", nil)}}
	p = &SubstratePreflighter{newClient: fakeSubstrateFactory{fake: fake}.new}
	if got := p.Check(ctx, opts); len(got) != 1 || !strings.Contains(got[0], "none reports a sandbox class") {
		t.Errorf("no class: %q", got)
	}

	fake = &fakeSubstrate{workers: []*substrateapi.Worker{
		worker("down", "unspecified", nil, "gvisor"),
		worker("up", "active", map[string]string{"class": "gvisor"}, "GVISOR", ""),
	}}
	p = &SubstratePreflighter{newClient: fakeSubstrateFactory{fake: fake}.new}
	if got := p.Check(ctx, opts); len(got) != 0 {
		t.Errorf("healthy control plane warned: %q", got)
	}
}

func TestSubstratePreflighterCachesPerControlPlane(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSubstrate{listErr: errors.New("down")}
	now := time.Unix(1_700_000_000, 0)
	p := &SubstratePreflighter{newClient: fakeSubstrateFactory{fake: fake}.new, now: func() time.Time { return now }}
	a := SubstrateOptions{Endpoint: "a:443", Token: "t"}
	b := SubstrateOptions{Endpoint: "b:443", Token: "t"}
	p.Check(ctx, a)
	p.Check(ctx, a)
	p.Check(ctx, b)
	if n := len(fake.calls); n != 2 {
		t.Errorf("ListWorkers calls = %d, want 2 (one per control plane, second a cache hit)", n)
	}
	now = now.Add(substratePreflightTTL + time.Second)
	p.Check(ctx, a)
	if n := len(fake.calls); n != 3 {
		t.Errorf("ListWorkers calls after TTL = %d, want 3", n)
	}
	// Returned slices are copies: mutating one must not poison the cache.
	got := p.Check(ctx, a)
	got[0] = "mutated"
	if again := p.Check(ctx, a); again[0] == "mutated" {
		t.Error("cache returned its own backing slice")
	}
}

func TestSubstrateActiveWorkersAndLabels(t *testing.T) {
	n, classes := substrateActiveWorkers([]*substrateapi.Worker{
		worker("a", "active", map[string]string{"k": "v"}, "gvisor"),
		worker("b", "active", map[string]string{"k": "v"}, "microvm", "gvisor"),
		worker("c", "active", map[string]string{"k": "other"}, "firecracker"),
		nil,
	}, map[string]string{"k": "v"})
	if n != 2 || strings.Join(classes, ",") != "gvisor,microvm" {
		t.Errorf("active=%d classes=%v", n, classes)
	}
	if !substrateLabelsMatch(nil, nil) || substrateLabelsMatch(nil, map[string]string{"a": "b"}) {
		t.Error("label matching")
	}
}

func TestDefaultSubstratePreflighterUsesRealClient(t *testing.T) {
	p := &SubstratePreflighter{}
	got := p.Check(context.Background(), SubstrateOptions{Endpoint: "127.0.0.1:1", CAFile: "/nonexistent/ca.crt"})
	if len(got) != 1 || !strings.Contains(got[0], "unreachable") {
		t.Errorf("warnings = %q", got)
	}
}
