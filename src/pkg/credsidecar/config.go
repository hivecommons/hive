package credsidecar

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Subcommand is the `hive` subcommand that runs the sidecar server.
const Subcommand = "credsidecar"

// Environment of the HIVE process (the proxy side, the signer).
const (
	// URLEnv is the sidecar-mode switch on the hive process: when set, the MITM
	// proxy sends every agent GitHub request to the sidecar at this URL instead
	// of attaching a token itself, and the hive never mints an agent token
	// in-process. Unset (the default everywhere) keeps today's behavior. Only a
	// loopback http:// URL is accepted - the sidecar is pod-local, and the
	// proxy-to-sidecar leg is plaintext.
	URLEnv = "HIVE_CRED_SIDECAR_URL"

	// KeyFileEnv names the file holding the per-spoke HMAC key. Read by BOTH
	// sides. It must be readable by the hive user and the sidecar, and by no
	// agent UID (on hosted spokes: the 0440 hive-secrets projection with the
	// pod fsGroup, exactly like the App private key).
	KeyFileEnv = "HIVE_CRED_SIDECAR_KEY_FILE"

	// DefaultKeyFile is where the hosted pod spec projects the key.
	DefaultKeyFile = "/secrets/cred-sidecar-hmac-key"
)

// Environment of the SIDECAR process.
const (
	// ListenEnv is the sidecar's listen address. Loopback only: the sidecar is
	// reached by the hive container over the pod's shared network namespace.
	ListenEnv = "HIVE_CRED_SIDECAR_LISTEN"
	// DefaultListenAddr is the default ListenEnv, and the address the default
	// hive-side URL (DefaultURL) points at.
	DefaultListenAddr = "127.0.0.1:18445"
	// DefaultURL is the URLEnv value matching DefaultListenAddr.
	DefaultURL = "http://" + DefaultListenAddr

	// AppIDEnv, InstallationIDEnv and AppKeyFileEnv identify the GitHub App the
	// sidecar mints agent tokens from.
	AppIDEnv          = "HIVE_CRED_SIDECAR_APP_ID"
	InstallationIDEnv = "HIVE_CRED_SIDECAR_INSTALLATION_ID"
	AppKeyFileEnv     = "HIVE_CRED_SIDECAR_APP_KEY_FILE"
	// DefaultAppKeyFile is where the hosted pod spec projects the App key.
	DefaultAppKeyFile = "/secrets/gh-app-key.pem"
	// GitHubAPIURLEnv is the GitHub API base URL for a GHE App; empty means
	// api.github.com.
	GitHubAPIURLEnv = "HIVE_CRED_SIDECAR_GITHUB_API_URL"
	// ExtraHostsEnv adds GitHub hosts (a GHE instance) to the sidecar's
	// forwarding allowlist, comma-separated. github.com and api.github.com are
	// always allowed; nothing else ever is unless named here.
	ExtraHostsEnv = "HIVE_CRED_SIDECAR_GITHUB_HOSTS"

	// MaxBodyBytesEnv caps the request body the proxy will buffer and sign and
	// the sidecar will accept. Read by BOTH sides.
	MaxBodyBytesEnv = "HIVE_CRED_SIDECAR_MAX_BODY_BYTES"
	// DefaultMaxBodyBytes: the signature covers the body digest, so the proxy
	// must hold the whole body before it can sign. That includes git pushes
	// (the pack is the body). 64 MiB covers an agent's pushes by a wide margin;
	// a larger push is refused with 413 rather than forwarded unsigned.
	DefaultMaxBodyBytes int64 = 64 << 20
)

// defaultGitHubHosts are the hosts the sidecar forwards to without any
// configuration. Anything else is refused (ErrHostNotAllowed).
var defaultGitHubHosts = []string{"api.github.com", "github.com"}

// Enabled reports whether sidecar mode is switched on for a hive process
// (URLEnv set). getenv is injected so the boot guard, pkg/github and the proxy
// share one reader.
func Enabled(getenv func(string) string) bool {
	return strings.TrimSpace(getenv(URLEnv)) != ""
}

// ClientConfig is what the proxy needs to sign and send requests.
type ClientConfig struct {
	URL          *url.URL
	Key          []byte
	MaxBodyBytes int64
}

// LoadClientConfig reads and validates the hive-side configuration. It is the
// single validation both the startup guard and the proxy constructor run, so a
// spoke that boots is a spoke whose proxy can build its client.
func LoadClientConfig(getenv func(string) string) (ClientConfig, error) {
	raw := strings.TrimSpace(getenv(URLEnv))
	if raw == "" {
		return ClientConfig{}, ErrNotConfigured
	}
	u, err := parseLoopbackURL(raw)
	if err != nil {
		return ClientConfig{}, err
	}
	key, err := loadKey(getenv)
	if err != nil {
		return ClientConfig{}, err
	}
	maxBody, err := maxBodyBytes(getenv)
	if err != nil {
		return ClientConfig{}, err
	}
	return ClientConfig{URL: u, Key: key, MaxBodyBytes: maxBody}, nil
}

// parseLoopbackURL accepts only http://<loopback>:<port> with no path, query or
// credentials: the leg is plaintext, so it must never leave the pod.
func parseLoopbackURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s=%q is not a URL: %w", URLEnv, raw, err)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("%s=%q must use http:// (the sidecar is pod-local)", URLEnv, raw)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s=%q must be a bare http://host:port", URLEnv, raw)
	}
	if !isLoopbackHostPort(u.Host) {
		return nil, fmt.Errorf("%s=%q must point at a loopback address (the proxy-to-sidecar leg is plaintext and must not leave the pod)", URLEnv, raw)
	}
	u.Path = ""
	return u, nil
}

// isLoopbackHostPort reports whether hostport is a loopback IP or "localhost"
// with an explicit port.
func isLoopbackHostPort(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || port == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loadKey reads the HMAC key from KeyFileEnv (DefaultKeyFile when unset).
// Surrounding whitespace is trimmed, so a key file written with a trailing
// newline is the same key on both sides.
func loadKey(getenv func(string) string) ([]byte, error) {
	path := strings.TrimSpace(getenv(KeyFileEnv))
	if path == "" {
		path = DefaultKeyFile
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the sidecar HMAC key (%s=%s): %w", KeyFileEnv, path, err)
	}
	key := []byte(strings.TrimSpace(string(data)))
	if len(key) < MinKeyBytes {
		return nil, fmt.Errorf("%w (%s=%s holds %d bytes)", ErrKeyTooShort, KeyFileEnv, path, len(key))
	}
	return key, nil
}

// maxBodyBytes reads MaxBodyBytesEnv, defaulting to DefaultMaxBodyBytes.
func maxBodyBytes(getenv func(string) string) (int64, error) {
	raw := strings.TrimSpace(getenv(MaxBodyBytesEnv))
	if raw == "" {
		return DefaultMaxBodyBytes, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q must be a positive byte count", MaxBodyBytesEnv, raw)
	}
	return n, nil
}

// ServerConfig is what the sidecar process needs.
type ServerConfig struct {
	ListenAddr     string
	Key            []byte
	MaxBodyBytes   int64
	AllowedHosts   map[string]bool
	AppID          int64
	InstallationID int64
	AppKeyFile     string
	APIURL         string
}

// LoadServerConfig reads and validates the sidecar's configuration.
func LoadServerConfig(getenv func(string) string) (ServerConfig, error) {
	listen := strings.TrimSpace(getenv(ListenEnv))
	if listen == "" {
		listen = DefaultListenAddr
	}
	if !isLoopbackHostPort(listen) {
		return ServerConfig{}, fmt.Errorf("%s=%q must be a loopback host:port (agents share the pod network, and the signature is the only thing keeping them out - do not widen who can reach it)", ListenEnv, listen)
	}
	key, err := loadKey(getenv)
	if err != nil {
		return ServerConfig{}, err
	}
	maxBody, err := maxBodyBytes(getenv)
	if err != nil {
		return ServerConfig{}, err
	}
	appID, err := positiveInt(getenv, AppIDEnv)
	if err != nil {
		return ServerConfig{}, err
	}
	instID, err := positiveInt(getenv, InstallationIDEnv)
	if err != nil {
		return ServerConfig{}, err
	}
	appKey := strings.TrimSpace(getenv(AppKeyFileEnv))
	if appKey == "" {
		appKey = DefaultAppKeyFile
	}
	return ServerConfig{
		ListenAddr:     listen,
		Key:            key,
		MaxBodyBytes:   maxBody,
		AllowedHosts:   allowedHosts(getenv(ExtraHostsEnv)),
		AppID:          appID,
		InstallationID: instID,
		AppKeyFile:     appKey,
		APIURL:         strings.TrimSpace(getenv(GitHubAPIURLEnv)),
	}, nil
}

// positiveInt reads a required positive integer variable.
func positiveInt(getenv func(string) string, name string) (int64, error) {
	raw := strings.TrimSpace(getenv(name))
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q must be a positive integer", name, raw)
	}
	return n, nil
}

// allowedHosts is the forwarding allowlist: the GitHub.com hosts plus any
// comma-separated extras (lowercased, blanks dropped).
func allowedHosts(extra string) map[string]bool {
	hosts := make(map[string]bool, len(defaultGitHubHosts))
	for _, h := range defaultGitHubHosts {
		hosts[h] = true
	}
	for _, h := range strings.Split(extra, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			hosts[h] = true
		}
	}
	return hosts
}
