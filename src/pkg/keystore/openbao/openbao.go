// Package openbao is the OpenBao Transit keystore provider
// (hivecommons/hive#18216): secrets are sealed and opened by an OpenBao
// server's transit engine over its HTTP API, so the key never exists on the
// spoke at all. OpenBao is the Linux Foundation / OpenSSF fork of Vault
// (MPL-2.0) and keeps the Vault API, so the same configuration works against
// either; the client below is plain net/http against the documented
// endpoints and vendors no SDK.
//
// Opt in with HIVE_KEYSTORE_PROVIDER=openbao. The provider refuses boot when
// the server is unreachable, sealed, rejects the credential or the transit
// key cannot be read or created: there is no fall-back to plaintext, because
// an operator who chose a key manager must not silently lose it.
//
// Nothing sensitive is cached: the client token lives in memory for the life
// of the process (renewed ahead of its lease, or re-obtained through the
// Kubernetes login), plaintext passes through on its way to and from the
// server, and the envelope on disk carries only the Transit ciphertext.
package openbao

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/keystore"
)

// ProviderName is the HIVE_KEYSTORE_PROVIDER value that selects this provider.
const ProviderName = "openbao"

// Environment variables.
const (
	// EnvAddr is the server URL, e.g. https://openbao.hive.svc:8200.
	EnvAddr = "HIVE_OPENBAO_ADDR"
	// EnvToken is a client token; EnvTokenFile names a file holding one.
	EnvToken     = "HIVE_OPENBAO_TOKEN"
	EnvTokenFile = "HIVE_OPENBAO_TOKEN_FILE"
	// EnvK8sRole selects Kubernetes auth: the pod's projected ServiceAccount
	// JWT is exchanged for a client token at /v1/auth/<mount>/login.
	EnvK8sRole = "HIVE_OPENBAO_K8S_ROLE"
	// EnvK8sMount is the Kubernetes auth mount (default "kubernetes").
	EnvK8sMount = "HIVE_OPENBAO_K8S_MOUNT"
	// EnvK8sJWTFile overrides where the ServiceAccount JWT is read from.
	EnvK8sJWTFile = "HIVE_OPENBAO_K8S_JWT_FILE"
	// EnvTransitMount is the transit engine mount (default "transit").
	EnvTransitMount = "HIVE_OPENBAO_TRANSIT_MOUNT"
	// EnvKey is the transit key name (default "hive-<hive id>").
	EnvKey = "HIVE_OPENBAO_KEY"
	// EnvNamespace sets X-Vault-Namespace on every request.
	EnvNamespace = "HIVE_OPENBAO_NAMESPACE"
	// EnvCAFile is a PEM bundle that verifies the server certificate instead
	// of the system roots.
	EnvCAFile = "HIVE_OPENBAO_CA_FILE"
	// EnvTimeout bounds each request (Go duration, default DefaultTimeout).
	EnvTimeout = "HIVE_OPENBAO_TIMEOUT"
	// EnvHiveID is the hive id the default key name is built from.
	EnvHiveID = "HIVE_ID"
)

// Defaults.
const (
	DefaultTransitMount = "transit"
	DefaultK8sMount     = "kubernetes"
	DefaultK8sJWTFile   = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	DefaultTimeout      = 10 * time.Second
	// DefaultKeyPrefix plus the hive id is the default transit key name.
	DefaultKeyPrefix = "hive-"
	// defaultKeyType is the transit key type created when the key is missing
	// and the token may create it; it is the type that supports
	// associated_data.
	defaultKeyType = "aes256-gcm96"
	// renewFraction of the lease is when the token is renewed.
	renewFraction = 2.0 / 3.0
	// maxResponseBytes bounds a response body read.
	maxResponseBytes = 1 << 20
	// hiveIDFile is read for the default key name when HIVE_ID is unset.
	hiveIDFile = "/data/hive-id"
)

// minRenewInterval keeps a very short lease from turning into a busy loop. A
// var only so the renewal loop can be exercised by tests at speed.
var minRenewInterval = 10 * time.Second

// Config is everything New needs. Factory fills it from the environment.
type Config struct {
	Addr         string
	Token        string
	K8sRole      string
	K8sMount     string
	K8sJWTFile   string
	TransitMount string
	Key          string
	Namespace    string
	CAFile       string
	Timeout      time.Duration
	// HTTPClient overrides the client (tests). CAFile is ignored when set.
	HTTPClient *http.Client
}

// Provider is a keystore.Provider backed by one transit key.
type Provider struct {
	cfg    Config
	client *http.Client
	base   *url.URL

	mu      sync.RWMutex
	token   string
	version int // latest key version seen, for KeyID

	renewCancel context.CancelFunc
	// renewDone is closed when the renewal goroutine has returned, so Close
	// can join it rather than leave it mid-request.
	renewDone chan struct{}
	// onRenewed, when set, observes each renewal attempt's outcome (tests).
	onRenewed func(leaseInfo, error)
}

// ErrNoHiveID is returned when neither HIVE_OPENBAO_KEY nor a hive id is
// available to name the transit key.
var ErrNoHiveID = errors.New("openbao: no transit key name: set " + EnvKey + ", or " + EnvHiveID)

func init() {
	keystore.Register(ProviderName, Factory)
}

// Factory builds the provider from the environment; it is what
// HIVE_KEYSTORE_PROVIDER=openbao runs at boot.
func Factory(ctx context.Context, getenv func(string) string) (keystore.Provider, error) {
	cfg, err := ConfigFromEnv(getenv, os.ReadFile)
	if err != nil {
		return nil, err
	}
	return New(ctx, cfg)
}

// ConfigFromEnv reads the HIVE_OPENBAO_* variables. readFile resolves the
// token file and the hive id file.
func ConfigFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	cfg := Config{
		Addr:         get(EnvAddr),
		Token:        get(EnvToken),
		K8sRole:      get(EnvK8sRole),
		K8sMount:     get(EnvK8sMount),
		K8sJWTFile:   get(EnvK8sJWTFile),
		TransitMount: get(EnvTransitMount),
		Key:          get(EnvKey),
		Namespace:    get(EnvNamespace),
		CAFile:       get(EnvCAFile),
		Timeout:      DefaultTimeout,
	}
	if cfg.Addr == "" {
		return cfg, errors.New("openbao: " + EnvAddr + " is not set")
	}
	if tf := get(EnvTokenFile); tf != "" && cfg.Token == "" {
		b, err := readFile(tf)
		if err != nil {
			return cfg, fmt.Errorf("openbao: %s: %w", EnvTokenFile, err)
		}
		cfg.Token = strings.TrimSpace(string(b))
	}
	if cfg.Token == "" && cfg.K8sRole == "" {
		return cfg, errors.New("openbao: no credential: set " + EnvToken + ", " + EnvTokenFile + " or " + EnvK8sRole)
	}
	if cfg.Key == "" {
		id := get(EnvHiveID)
		if id == "" {
			if b, err := readFile(hiveIDFile); err == nil {
				id = strings.TrimSpace(string(b))
			}
		}
		if id == "" {
			return cfg, ErrNoHiveID
		}
		cfg.Key = DefaultKeyPrefix + id
	}
	if raw := get(EnvTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("openbao: %s=%q is not a positive duration", EnvTimeout, raw)
		}
		cfg.Timeout = d
	}
	return cfg, nil
}

// New connects, authenticates, checks the server is unsealed and that the
// transit key exists (creating it when the token may), and starts token
// renewal. Any failure is returned; nothing is retried in the background.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.TransitMount == "" {
		cfg.TransitMount = DefaultTransitMount
	}
	if cfg.K8sMount == "" {
		cfg.K8sMount = DefaultK8sMount
	}
	if cfg.K8sJWTFile == "" {
		cfg.K8sJWTFile = DefaultK8sJWTFile
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if strings.TrimSpace(cfg.Key) == "" {
		return nil, ErrNoHiveID
	}
	if strings.ContainsAny(cfg.Key+cfg.TransitMount+cfg.K8sMount, "/?# ") {
		return nil, errors.New("openbao: key and mount names must not contain '/', '?', '#' or spaces")
	}
	base, err := url.Parse(cfg.Addr)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("openbao: %s=%q is not an http(s) URL", EnvAddr, cfg.Addr)
	}
	client := cfg.HTTPClient
	if client == nil {
		client, err = newHTTPClient(cfg)
		if err != nil {
			return nil, err
		}
	}
	p := &Provider{cfg: cfg, client: client, base: base, token: cfg.Token}

	if err := p.health(ctx); err != nil {
		return nil, err
	}
	var lease leaseInfo
	if p.token == "" {
		lease, err = p.k8sLogin(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		lease, err = p.lookupSelf(ctx)
		if err != nil {
			return nil, err
		}
	}
	if err := p.ensureKey(ctx); err != nil {
		return nil, err
	}
	p.startRenewal(lease)
	return p, nil
}

func newHTTPClient(cfg Config) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("openbao: default transport is not *http.Transport")
	}
	transport = transport.Clone()
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("openbao: %s: %w", EnvCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("openbao: %s: no certificates found in %s", EnvCAFile, cfg.CAFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport, Timeout: cfg.Timeout}, nil
}

// Name implements keystore.Provider.
func (p *Provider) Name() string { return ProviderName }

// KeyID implements keystore.Provider: the transit key name and the latest
// key version seen ("hive-abc@v3"). A rotation on the server (bao write -f
// transit/keys/<key>/rotate) changes it, so files sealed under the previous
// version read as stale and are re-sealed on their next read.
func (p *Provider) KeyID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg.Key + "@v" + strconv.Itoa(p.version)
}

// Close stops token renewal and waits for the renewal goroutine to return.
// The provider is unusable afterwards only in the sense that the token may
// expire; nothing else is torn down. Safe to call more than once.
func (p *Provider) Close() {
	p.mu.Lock()
	cancel, done := p.renewCancel, p.renewDone
	p.renewCancel, p.renewDone = nil, nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Encrypt implements keystore.Provider via POST /v1/<mount>/encrypt/<key>.
// The AAD travels as associated_data, so the server binds it into the GCM
// tag exactly as the env provider does locally.
func (p *Provider) Encrypt(ctx context.Context, plaintext, aad []byte) ([]byte, error) {
	body := map[string]any{"plaintext": base64.StdEncoding.EncodeToString(plaintext)}
	if len(aad) > 0 {
		body["associated_data"] = base64.StdEncoding.EncodeToString(aad)
	}
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
			KeyVersion int    `json:"key_version"`
		} `json:"data"`
	}
	if err := p.do(ctx, http.MethodPost, p.transitPath("encrypt"), body, &out); err != nil {
		return nil, err
	}
	if out.Data.Ciphertext == "" {
		return nil, errors.New("openbao: encrypt returned no ciphertext")
	}
	p.noteVersion(versionFromCiphertext(out.Data.Ciphertext))
	return []byte(out.Data.Ciphertext), nil
}

// Decrypt implements keystore.Provider via POST /v1/<mount>/decrypt/<key>.
// keyID is informational: the transit ciphertext names its own key version.
// A rejected associated_data surfaces as keystore.ErrAuthentication.
func (p *Provider) Decrypt(ctx context.Context, _ string, ciphertext, aad []byte) ([]byte, error) {
	body := map[string]any{"ciphertext": string(ciphertext)}
	if len(aad) > 0 {
		body["associated_data"] = base64.StdEncoding.EncodeToString(aad)
	}
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := p.do(ctx, http.MethodPost, p.transitPath("decrypt"), body, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest {
			return nil, errors.Join(keystore.ErrAuthentication, err)
		}
		return nil, err
	}
	pt, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("openbao: decrypt returned plaintext that is not base64: %w", err)
	}
	return pt, nil
}

// APIError is a non-2xx reply, with the server's errors list.
type APIError struct {
	Status int
	Path   string
	Errors []string
}

func (e *APIError) Error() string {
	msg := strings.Join(e.Errors, "; ")
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("openbao: %s: HTTP %d: %s", e.Path, e.Status, msg)
}

func (p *Provider) transitPath(op string) string {
	return path.Join("/v1", p.cfg.TransitMount, op, p.cfg.Key)
}

// health checks /v1/sys/health: 200 active and 429 standby are fine (a
// standby forwards), 501 is uninitialised, 503 is sealed.
func (p *Provider) health(ctx context.Context) error {
	status, _, err := p.raw(ctx, http.MethodGet, "/v1/sys/health?standbyok=true", nil, false)
	if err != nil {
		return fmt.Errorf("openbao: %s unreachable: %w", p.cfg.Addr, err)
	}
	switch status {
	case http.StatusOK, http.StatusTooManyRequests:
		return nil
	case http.StatusNotImplemented:
		return errors.New("openbao: server is not initialised")
	case http.StatusServiceUnavailable:
		return errors.New("openbao: server is sealed")
	}
	return fmt.Errorf("openbao: /v1/sys/health returned HTTP %d", status)
}

type leaseInfo struct {
	seconds   int
	renewable bool
}

// k8sLogin exchanges the ServiceAccount JWT for a client token.
func (p *Provider) k8sLogin(ctx context.Context) (leaseInfo, error) {
	jwt, err := os.ReadFile(p.cfg.K8sJWTFile)
	if err != nil {
		return leaseInfo{}, fmt.Errorf("openbao: kubernetes auth: read service account token: %w", err)
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
			Renewable     bool   `json:"renewable"`
		} `json:"auth"`
	}
	body := map[string]any{"role": p.cfg.K8sRole, "jwt": strings.TrimSpace(string(jwt))}
	if err := p.do(ctx, http.MethodPost, path.Join("/v1/auth", p.cfg.K8sMount, "login"), body, &out); err != nil {
		return leaseInfo{}, fmt.Errorf("openbao: kubernetes auth (role %q): %w", p.cfg.K8sRole, err)
	}
	if out.Auth.ClientToken == "" {
		return leaseInfo{}, errors.New("openbao: kubernetes auth returned no client token")
	}
	p.mu.Lock()
	p.token = out.Auth.ClientToken
	p.mu.Unlock()
	return leaseInfo{seconds: out.Auth.LeaseDuration, renewable: out.Auth.Renewable}, nil
}

// lookupSelf validates a supplied token and learns its lease.
func (p *Provider) lookupSelf(ctx context.Context) (leaseInfo, error) {
	var out struct {
		Data struct {
			TTL       int  `json:"ttl"`
			Renewable bool `json:"renewable"`
		} `json:"data"`
	}
	if err := p.do(ctx, http.MethodGet, "/v1/auth/token/lookup-self", nil, &out); err != nil {
		return leaseInfo{}, fmt.Errorf("openbao: token rejected: %w", err)
	}
	return leaseInfo{seconds: out.Data.TTL, renewable: out.Data.Renewable}, nil
}

// renewSelf extends the current token's lease.
func (p *Provider) renewSelf(ctx context.Context) (leaseInfo, error) {
	var out struct {
		Auth struct {
			LeaseDuration int  `json:"lease_duration"`
			Renewable     bool `json:"renewable"`
		} `json:"auth"`
	}
	if err := p.do(ctx, http.MethodPost, "/v1/auth/token/renew-self", map[string]any{}, &out); err != nil {
		return leaseInfo{}, err
	}
	return leaseInfo{seconds: out.Auth.LeaseDuration, renewable: out.Auth.Renewable}, nil
}

// ensureKey reads the transit key, creating it when it is absent and the
// token may; the latest version seeds KeyID.
func (p *Provider) ensureKey(ctx context.Context) error {
	v, err := p.readKeyVersion(ctx)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		body := map[string]any{"type": defaultKeyType}
		if cerr := p.do(ctx, http.MethodPost, path.Join("/v1", p.cfg.TransitMount, "keys", p.cfg.Key), body, nil); cerr != nil {
			return fmt.Errorf("openbao: transit key %q does not exist and could not be created (create it: bao write -f %s/keys/%s): %w",
				p.cfg.Key, p.cfg.TransitMount, p.cfg.Key, cerr)
		}
		v, err = p.readKeyVersion(ctx)
	}
	if err != nil {
		return fmt.Errorf("openbao: transit key %q: %w", p.cfg.Key, err)
	}
	p.noteVersion(v)
	return nil
}

// readKeyVersion returns the highest version in the key's `keys` map.
func (p *Provider) readKeyVersion(ctx context.Context) (int, error) {
	var out struct {
		Data struct {
			Keys               map[string]any `json:"keys"`
			LatestVersion      int            `json:"latest_version"`
			SupportsEncryption bool           `json:"supports_encryption"`
		} `json:"data"`
	}
	if err := p.do(ctx, http.MethodGet, path.Join("/v1", p.cfg.TransitMount, "keys", p.cfg.Key), nil, &out); err != nil {
		return 0, err
	}
	latest := out.Data.LatestVersion
	for k := range out.Data.Keys {
		if n, err := strconv.Atoi(k); err == nil && n > latest {
			latest = n
		}
	}
	if latest == 0 {
		latest = 1
	}
	return latest, nil
}

func (p *Provider) noteVersion(v int) {
	if v <= 0 {
		return
	}
	p.mu.Lock()
	if v > p.version {
		p.version = v
	}
	p.mu.Unlock()
}

// versionFromCiphertext parses the N in "vault:vN:...", or 0.
func versionFromCiphertext(ct string) int {
	parts := strings.SplitN(ct, ":", 3)
	if len(parts) != 3 || !strings.HasPrefix(parts[1], "v") {
		return 0
	}
	n, err := strconv.Atoi(parts[1][1:])
	if err != nil {
		return 0
	}
	return n
}

// startRenewal renews the token at renewFraction of its lease, re-logging in
// through Kubernetes auth when a renewal is refused. A non-renewable or
// unlimited (ttl 0) token needs nothing.
func (p *Provider) startRenewal(lease leaseInfo) {
	if lease.seconds <= 0 || !lease.renewable {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.mu.Lock()
	p.renewCancel, p.renewDone = cancel, done
	p.mu.Unlock()
	go func() {
		defer close(done)
		p.renewLoop(ctx, lease)
	}()
}

func (p *Provider) renewLoop(ctx context.Context, lease leaseInfo) {
	for {
		wait := renewInterval(lease.seconds)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		next, err := p.renewOnce(ctx)
		if p.onRenewed != nil {
			p.onRenewed(next, err)
		}
		if err != nil {
			// Logged by the keystore's caller through the next failing
			// Encrypt/Decrypt; here we keep trying on a short cadence so a
			// transient outage does not end renewal for good.
			lease = leaseInfo{seconds: int(minRenewInterval.Seconds()) * 2, renewable: true}
			continue
		}
		if next.seconds <= 0 || !next.renewable {
			return
		}
		lease = next
	}
}

// renewOnce renews the token, falling back to a fresh Kubernetes login when
// the renewal is refused and a role is configured.
func (p *Provider) renewOnce(ctx context.Context) (leaseInfo, error) {
	rctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	next, err := p.renewSelf(rctx)
	if err == nil {
		return next, nil
	}
	if p.cfg.K8sRole == "" {
		return leaseInfo{}, err
	}
	return p.k8sLogin(rctx)
}

func renewInterval(leaseSeconds int) time.Duration {
	d := time.Duration(float64(leaseSeconds)*renewFraction) * time.Second
	if d < minRenewInterval {
		d = minRenewInterval
	}
	return d
}

// do performs an authenticated JSON request and decodes a 2xx body into out
// (nil out accepts 200 or 204). Non-2xx replies become *APIError.
func (p *Provider) do(ctx context.Context, method, reqPath string, body any, out any) error {
	status, data, err := p.raw(ctx, method, reqPath, body, true)
	if err != nil {
		return fmt.Errorf("openbao: %s: %w", reqPath, err)
	}
	if status < 200 || status > 299 {
		apiErr := &APIError{Status: status, Path: reqPath}
		var envelope struct {
			Errors []string `json:"errors"`
		}
		if json.Unmarshal(data, &envelope) == nil {
			apiErr.Errors = envelope.Errors
		}
		return apiErr
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("openbao: %s: decode response: %w", reqPath, err)
	}
	return nil
}

// raw sends one request bounded by cfg.Timeout and returns the status and
// body. withToken adds X-Vault-Token when a token is held.
func (p *Provider) raw(ctx context.Context, method, reqPath string, body any, withToken bool) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	u := *p.base
	ref, err := url.Parse(reqPath)
	if err != nil {
		return 0, nil, err
	}
	u.Path = path.Join(p.base.Path, ref.Path)
	u.RawQuery = ref.RawQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", p.cfg.Namespace)
	}
	if withToken {
		p.mu.RLock()
		tok := p.token
		p.mu.RUnlock()
		if tok != "" {
			req.Header.Set("X-Vault-Token", tok)
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}
