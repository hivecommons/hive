package mailin

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/netutil"
)

// Environment variables hive-mx reads (documented in src/docs/env-vars.md).
const (
	EnvListen          = "HIVE_MX_LISTEN"
	EnvHealthListen    = "HIVE_MX_HEALTH_LISTEN"
	EnvDomain          = "HIVE_MX_DOMAIN"
	EnvHubURL          = "HIVE_MX_HUB_URL"
	EnvHubTokenFile    = "HIVE_MX_HUB_TOKEN_FILE"
	EnvHostsRefreshS   = "HIVE_MX_HOSTS_REFRESH_S"
	EnvMaxMessageBytes = "HIVE_MX_MAX_MESSAGE_BYTES"
	EnvMaxRecipients   = "HIVE_MX_MAX_RECIPIENTS"
	EnvMaxConnections  = "HIVE_MX_MAX_CONNECTIONS"
	EnvMaxFileBytes    = "HIVE_MX_MAX_FILE_BYTES"
	EnvMaxFiles        = "HIVE_MX_MAX_FILES"
	EnvIPPerHour       = "HIVE_MX_IP_MESSAGES_PER_HOUR"
	EnvDomainPerHour   = "HIVE_MX_DOMAIN_MESSAGES_PER_HOUR"
	EnvTLSCert         = "HIVE_MX_TLS_CERT"
	EnvTLSKey          = "HIVE_MX_TLS_KEY"
)

// Config defaults.
const (
	DefaultListen          = ":2525"
	DefaultHealthListen    = ":8080"
	DefaultHubTokenFile    = "/var/run/secrets/hive-mx/token"
	DefaultMaxConnections  = 100
	DefaultIPPerHour       = 60
	DefaultDomainPerHour   = 120
	rateBurstDivisor       = 4
	healthReadHeaderTimout = 5 * time.Second
	shutdownGrace          = 10 * time.Second
)

// Config is hive-mx's runtime configuration.
type Config struct {
	Listen, HealthListen string
	Domain               string
	HubURL, HubTokenFile string
	HostsRefresh         time.Duration
	MaxMessageBytes      int64
	MaxRecipients        int
	MaxConnections       int
	Limits               Limits
	IPPerHour            int
	DomainPerHour        int
	TLSCert, TLSKey      string
	// Resolver is the DNS used for SPF, DKIM and DMARC (net.DefaultResolver
	// when nil; tests pass a fake).
	Resolver Resolver
}

func envInt(getenv func(string) string, key string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(getenv(key))); err == nil && n > 0 {
		return n
	}
	return def
}

func envStr(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}

// ConfigFromEnv reads the configuration; a missing or invalid number falls
// back to its default. The hub URL is required.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{
		Listen:          envStr(getenv, EnvListen, DefaultListen),
		HealthListen:    envStr(getenv, EnvHealthListen, DefaultHealthListen),
		Domain:          envStr(getenv, EnvDomain, DefaultDomain),
		HubURL:          envStr(getenv, EnvHubURL, ""),
		HubTokenFile:    envStr(getenv, EnvHubTokenFile, DefaultHubTokenFile),
		HostsRefresh:    time.Duration(envInt(getenv, EnvHostsRefreshS, int(defaultHostsPoll/time.Second))) * time.Second,
		MaxMessageBytes: int64(envInt(getenv, EnvMaxMessageBytes, DefaultMaxMessageBytes)),
		MaxRecipients:   envInt(getenv, EnvMaxRecipients, DefaultMaxRecipients),
		MaxConnections:  envInt(getenv, EnvMaxConnections, DefaultMaxConnections),
		IPPerHour:       envInt(getenv, EnvIPPerHour, DefaultIPPerHour),
		DomainPerHour:   envInt(getenv, EnvDomainPerHour, DefaultDomainPerHour),
		TLSCert:         envStr(getenv, EnvTLSCert, ""),
		TLSKey:          envStr(getenv, EnvTLSKey, ""),
	}
	c.Limits = DefaultLimits()
	c.Limits.MaxFileBytes = envInt(getenv, EnvMaxFileBytes, DefaultMaxFileBytes)
	c.Limits.MaxFiles = envInt(getenv, EnvMaxFiles, DefaultMaxFiles)
	if c.HubURL == "" {
		return c, fmt.Errorf("%s is required", EnvHubURL)
	}
	if !strings.HasPrefix(c.HubURL, "https://") && !strings.HasPrefix(c.HubURL, "http://") {
		return c, fmt.Errorf("%s must be an http(s) URL", EnvHubURL)
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return c, fmt.Errorf("%s and %s must be set together", EnvTLSCert, EnvTLSKey)
	}
	return c, nil
}

// certReloader serves a key pair from files, re-reading them when the
// certificate file changes (cert-manager renews in place).
type certReloader struct {
	cert, key string
	mu        sync.Mutex
	mod       time.Time
	pair      *tls.Certificate
}

func (r *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	st, err := os.Stat(r.cert)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pair != nil && st.ModTime().Equal(r.mod) {
		return r.pair, nil
	}
	pair, err := tls.LoadX509KeyPair(r.cert, r.key)
	if err != nil {
		if r.pair != nil {
			return r.pair, nil
		}
		return nil, err
	}
	r.pair, r.mod = &pair, st.ModTime()
	return r.pair, nil
}

// tlsConfig is the STARTTLS config, nil when no certificate is configured.
func (c Config) tlsConfig() (*tls.Config, error) {
	if c.TLSCert == "" {
		return nil, nil
	}
	r := &certReloader{cert: c.TLSCert, key: c.TLSKey}
	if _, err := r.get(nil); err != nil {
		return nil, fmt.Errorf("load STARTTLS certificate: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.get}, nil
}

// Run serves SMTP and the health endpoints until ctx ends.
func Run(ctx context.Context, c Config, log *slog.Logger) error {
	tlsCfg, err := c.tlsConfig()
	if err != nil {
		return err
	}
	hub := &HubClient{BaseURL: c.HubURL, TokenFile: c.HubTokenFile}
	hosts := &HostSet{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go hosts.Refresh(ctx, hub, c.HostsRefresh, log)

	srv := NewServer(Options{
		Domain: c.Domain, MaxRecipients: c.MaxRecipients, MaxMessageBytes: c.MaxMessageBytes,
		Limits: c.Limits, Hosts: hosts, Hub: hub, TLSConfig: tlsCfg, Logger: log, Resolver: c.Resolver,
		IPLimiter:     NewRateLimiter(c.IPPerHour, time.Hour, max(1, c.IPPerHour/rateBurstDivisor), nil),
		DomainLimiter: NewRateLimiter(c.DomainPerHour, time.Hour, max(1, c.DomainPerHour/rateBurstDivisor), nil),
	})
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	ln = netutil.LimitListener(ln, c.MaxConnections)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !hosts.Ready() {
			http.Error(w, "host list not loaded", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	health := &http.Server{Addr: c.HealthListen, Handler: mux, ReadHeaderTimeout: healthReadHeaderTimout}
	hln, err := net.Listen("tcp", c.HealthListen)
	if err != nil {
		_ = ln.Close()
		return err
	}

	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()
	go func() { errc <- health.Serve(hln) }()
	log.Info("hive-mx: listening", "smtp", ln.Addr().String(), "health", hln.Addr().String(), "starttls", tlsCfg != nil)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, scancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer scancel()
	_ = health.Shutdown(sctx)
	_ = srv.Shutdown(sctx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
