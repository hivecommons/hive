package sandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/hivecommons/hive/crux/sandbox/substrateapi"
)

// Real Substrate client: the Control API over gRPC plus the in-actor bridge
// over an atenet-router CONNECT tunnel. Constructed lazily by
// SubstrateLauncher.client() so unit tests never dial anything.

// substrateRouterHeader selects the target actor on the router's CONNECT
// request (api-guide "Workload Connectivity").
const substrateRouterHeader = "ate-target-actor"

type grpcSubstrateClient struct {
	conn    *grpc.ClientConn
	control *grpcControl
	bridge  *routerBridge
}

func (c *grpcSubstrateClient) Control() substrateControl { return c.control }
func (c *grpcSubstrateClient) Bridge() substrateBridge   { return c.bridge }
func (c *grpcSubstrateClient) Close() error              { return c.conn.Close() }

func newSubstrateGRPCClient(o SubstrateOptions) (substrateClient, error) {
	addr, useTLS, err := substrateAddress(o.endpoint())
	if err != nil {
		return nil, err
	}
	creds := insecure.NewCredentials()
	if useTLS {
		cfg, err := substrateControlTLS(o)
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(cfg)
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if o.Token != "" || o.TokenFile != "" || useTLS {
		opts = append(opts, grpc.WithPerRPCCredentials(substrateBearer{token: o.Token, file: o.tokenFile(), secure: useTLS}))
	}
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("substrate: connecting to control api %s: %w", addr, err)
	}
	bridge, err := newRouterBridge(o)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &grpcSubstrateClient{conn: conn, control: &grpcControl{c: substrateapi.NewControlClient(conn)}, bridge: bridge}, nil
}

func (o SubstrateOptions) tokenFile() string {
	if f := strings.TrimSpace(o.TokenFile); f != "" {
		return f
	}
	return SubstrateDefaultTokenFile
}

// substrateControlTLS verifies the Control API against ca_file (the
// servicedns ClusterTrustBundle projection) when present, the system roots
// otherwise, or nothing with insecure_tls.
func substrateControlTLS(o SubstrateOptions) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: o.InsecureTLS} //nolint:gosec // operator opt-in
	if o.InsecureTLS {
		return cfg, nil
	}
	caFile := strings.TrimSpace(o.CAFile)
	if caFile == "" {
		if _, err := os.Stat(SubstrateDefaultCAFile); err == nil {
			caFile = SubstrateDefaultCAFile
		}
	}
	if caFile != "" {
		pool, err := substrateCertPool(caFile)
		if err != nil {
			return nil, fmt.Errorf("substrate: ca_file: %w", err)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

func substrateCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificates", path)
	}
	return pool, nil
}

// substrateBearer sends the projected ServiceAccount token as a bearer
// token, re-reading the file per RPC so kubelet rotation is picked up.
type substrateBearer struct {
	token  string
	file   string
	secure bool
}

func (b substrateBearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	tok := b.token
	if tok == "" {
		raw, err := os.ReadFile(b.file)
		if err != nil {
			return nil, fmt.Errorf("substrate: reading token file %s: %w", b.file, err)
		}
		tok = strings.TrimSpace(string(raw))
	}
	if tok == "" {
		return nil, errors.New("substrate: control api token is empty")
	}
	return map[string]string{"authorization": "Bearer " + tok}, nil
}

func (b substrateBearer) RequireTransportSecurity() bool { return b.secure }

// grpcControl adapts the generated ControlClient to substrateControl.
type grpcControl struct{ c substrateapi.ControlClient }

func (g *grpcControl) CreateActorTemplate(ctx context.Context, tpl *substrateapi.ActorTemplate) (*substrateapi.ActorTemplate, error) {
	return g.c.CreateActorTemplate(ctx, &substrateapi.CreateActorTemplateRequest{ActorTemplate: tpl})
}

func (g *grpcControl) DeleteActorTemplate(ctx context.Context, ref *substrateapi.ObjectRef) error {
	_, err := g.c.DeleteActorTemplate(ctx, &substrateapi.DeleteActorTemplateRequest{ActorTemplate: ref})
	return err
}

func (g *grpcControl) CreateActor(ctx context.Context, actor *substrateapi.Actor) (*substrateapi.Actor, error) {
	return g.c.CreateActor(ctx, &substrateapi.CreateActorRequest{Actor: actor})
}

func (g *grpcControl) GetActor(ctx context.Context, ref *substrateapi.ObjectRef) (*substrateapi.Actor, error) {
	return g.c.GetActor(ctx, &substrateapi.GetActorRequest{Actor: ref})
}

func (g *grpcControl) ResumeActor(ctx context.Context, ref *substrateapi.ObjectRef) error {
	_, err := g.c.ResumeActor(ctx, &substrateapi.ResumeActorRequest{Actor: ref})
	return err
}

func (g *grpcControl) SuspendActor(ctx context.Context, ref *substrateapi.ObjectRef, mode substrateapi.DurabilityMode) error {
	_, err := g.c.SuspendActor(ctx, &substrateapi.SuspendActorRequest{Actor: ref, Options: &substrateapi.SuspendOptions{Mode: mode}})
	return err
}

func (g *grpcControl) DeleteActor(ctx context.Context, ref *substrateapi.ObjectRef) error {
	_, err := g.c.DeleteActor(ctx, &substrateapi.DeleteActorRequest{Actor: ref})
	return err
}

func (g *grpcControl) CreateActorEgressPolicy(ctx context.Context, ref *substrateapi.ObjectRef, policy *substrateapi.EgressPolicy) error {
	_, err := g.c.CreateActorEgressPolicy(ctx, &substrateapi.CreateActorEgressPolicyRequest{Actor: ref, EgressPolicy: policy})
	return err
}

func (g *grpcControl) ListWorkers(ctx context.Context) ([]*substrateapi.Worker, error) {
	var out []*substrateapi.Worker
	req := &substrateapi.ListWorkersRequest{Options: &substrateapi.ListOptions{}}
	for {
		resp, err := g.c.ListWorkers(ctx, req)
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetWorkers()...)
		if resp.GetNextPageToken() == "" {
			return out, nil
		}
		req.Options.PageToken = resp.GetNextPageToken()
	}
}

// routerBridge reaches the in-actor bridge through atenet-router: an mTLS
// CONNECT to the router with the actor named in ate-target-actor, then plain
// HTTP to the bridge over the tunnel.
type routerBridge struct {
	routerAddr string
	tlsConfig  *tls.Config
	port       int
	dial       func(ctx context.Context, network, addr string) (net.Conn, error)
}

func newRouterBridge(o SubstrateOptions) (*routerBridge, error) {
	addr, err := substrateRouterAddress(o.routerURL())
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(addr)
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, InsecureSkipVerify: o.InsecureTLS} //nolint:gosec // operator opt-in
	bundle := o.RouterClientBundle
	if bundle == "" {
		bundle = SubstrateDefaultRouterClientBundle
	}
	cert, err := tls.LoadX509KeyPair(bundle, bundle)
	if err != nil {
		return nil, fmt.Errorf("substrate: router_client_bundle %s: %w — atenet-router needs a podCertificate projection for hive (see deploy/substrate.md)", bundle, err)
	}
	cfg.Certificates = []tls.Certificate{cert}
	if !o.InsecureTLS {
		caFile := o.RouterCAFile
		if caFile == "" {
			caFile = SubstrateDefaultRouterCAFile
		}
		pool, err := substrateCertPool(caFile)
		if err != nil {
			return nil, fmt.Errorf("substrate: router_ca_file: %w", err)
		}
		cfg.RootCAs = pool
	}
	d := &net.Dialer{Timeout: substrateRequestWait}
	return &routerBridge{routerAddr: addr, tlsConfig: cfg, port: o.bridgePort(), dial: d.DialContext}, nil
}

// tunnel opens one CONNECT tunnel to the actor's bridge port.
func (b *routerBridge) tunnel(ctx context.Context, ref *substrateapi.ObjectRef) (net.Conn, error) {
	raw, err := b.dial(ctx, "tcp", b.routerAddr)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, b.tlsConfig)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("router tls handshake: %w", err)
	}
	target := net.JoinHostPort("actor", fmt.Sprint(b.port))
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	req.Header.Set(substrateRouterHeader, ref.GetAtespace()+"/"+ref.GetName())
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("router connect: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("router connect: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("router refused connect: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func (b *routerBridge) do(ctx context.Context, ref *substrateapi.ObjectRef, method, path string, body io.Reader) (*http.Response, error) {
	transport := &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return b.tunnel(ctx, ref) },
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://actor"+path, body)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("bridge %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}
