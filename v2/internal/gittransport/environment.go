package gittransport

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type controllerTokenKey struct{}
type controllerTokenSourceKey struct{}

// WithControllerToken binds an ephemeral repository transport credential to
// controller-owned Git child processes. The token is never added to the
// controller environment or persisted in a checkout.
func WithControllerToken(ctx context.Context, token string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if !validControllerToken(token) {
		return ctx
	}
	return context.WithValue(ctx, controllerTokenKey{}, token)
}

// WithControllerTokenSource binds a controller-owned refresh callback without
// resolving a token early. Network Git calls refresh it immediately before
// spawning their isolated transport process.
func WithControllerTokenSource(ctx context.Context, source func(context.Context) (string, error)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if source == nil {
		return ctx
	}
	return context.WithValue(ctx, controllerTokenSourceKey{}, source)
}

// RefreshControllerToken resolves a fresh controller token when a source is
// bound. Static PAT contexts are returned unchanged; tokenless public/local
// transports remain supported.
func RefreshControllerToken(ctx context.Context) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if source, ok := ctx.Value(controllerTokenSourceKey{}).(func(context.Context) (string, error)); ok && source != nil {
		token, err := source(ctx)
		if err != nil {
			return ctx, err
		}
		if !validControllerToken(token) {
			return ctx, fmt.Errorf("controller Git transport token source returned an empty or invalid token")
		}
		return context.WithValue(ctx, controllerTokenKey{}, token), nil
	}
	return ctx, nil
}

// RefreshControllerTokenForURL resolves authority only for an exact canonical
// GitHub HTTPS transport. Local/file/test remotes remain credentialless and do
// not consume or expose an installation-token refresh callback.
func RefreshControllerTokenForURL(ctx context.Context, remoteURL string) (context.Context, error) {
	if exactGitHubTransportURL(remoteURL) == "" {
		return ctx, nil
	}
	return RefreshControllerToken(ctx)
}

// HasControllerToken reports whether strict controller-owned authentication
// is available without exposing the credential bytes to callers.
func HasControllerToken(ctx context.Context) bool {
	return controllerToken(ctx) != ""
}

// LocalEnvironment returns a credentialless, non-interactive Git environment.
// It deliberately ignores every controller token that may be bound to the
// caller's context so local repository operations cannot pass transport
// authority to repository-configured child processes.
func LocalEnvironment(base []string) []string {
	return environment(context.Background(), base, true, "")
}

// LocalEnvironmentWithOverrides returns the same credentialless environment
// as LocalEnvironment, then adds only explicitly allowlisted Git plumbing
// overrides. This keeps ambient repository/configuration selectors out of
// controller Git children while still supporting isolated temporary indexes.
func LocalEnvironmentWithOverrides(base []string, overrides map[string]string) []string {
	result := environment(context.Background(), base, true, "")
	if indexFile, ok := overrides["GIT_INDEX_FILE"]; ok && indexFile != "" && strings.TrimSpace(indexFile) == indexFile && !strings.ContainsAny(indexFile, "\r\n\x00") {
		result = append(result, "GIT_INDEX_FILE="+indexFile)
	}
	return result
}

// TransportEnvironment returns the strict environment for one explicitly
// controller-owned network Git command. Callers must validate the exact remote
// URL before selecting this environment.
func TransportEnvironment(ctx context.Context, base []string) []string {
	return environment(ctx, base, true, "https://github.com/")
}

// TransportEnvironmentForURL scopes controller authentication to one exact
// canonical GitHub HTTPS URL. Invalid, redirected, credential-bearing, or
// non-HTTPS URLs receive no controller authentication.
func TransportEnvironmentForURL(ctx context.Context, base []string, remoteURL string) []string {
	return environment(ctx, base, true, exactGitHubTransportURL(remoteURL))
}

// Environment returns a deterministic Git child environment. Repository,
// system, and user configuration cannot re-enable hooks or interactive
// credentials. In strict mode, credential helpers and inherited HTTP auth are
// reset and the in-memory controller token is supplied only to this Git child.
func Environment(ctx context.Context, base []string, strictCredentials bool) []string {
	return environment(ctx, base, strictCredentials, "https://github.com/")
}

func environment(ctx context.Context, base []string, strictCredentials bool, transportURL string) []string {
	result := make([]string, 0, len(base)+32)
	for _, pair := range base {
		name, _, _ := strings.Cut(pair, "=")
		upper := strings.ToUpper(strings.TrimSpace(name))
		if controllerCredentialEnvironment(upper) || blockedGitEnvironment(upper) {
			continue
		}
		result = append(result, pair)
	}

	config := [][2]string{
		{"credential.interactive", "false"},
		{"core.hooksPath", os.DevNull},
		{"core.fsmonitor", "false"},
		{"core.pager", "cat"},
		{"commit.gpgSign", "false"},
		{"tag.gpgSign", "false"},
		{"log.showSignature", "false"},
		{"diff.external", ""},
		{"core.autocrlf", "false"},
		{"core.bare", "false"},
		// Local transports spawn upload-pack in the source repository and pass
		// this command-scope configuration through. An empty value disables a
		// target-local packObjectsHook during controller object transfer.
		{"uploadpack.packObjectsHook", ""},
	}
	if strictCredentials {
		config = append(config,
			[2]string{"credential.helper", ""},
			[2]string{"credential.https://github.com.helper", ""},
			[2]string{"http.extraheader", ""},
			[2]string{"http.https://github.com/.extraheader", ""},
		)
		if token := controllerToken(ctx); token != "" && transportURL != "" {
			config = append(config,
				[2]string{"protocol.allow", "never"},
				[2]string{"protocol.https.allow", "always"},
				[2]string{"http.followRedirects", "false"},
				[2]string{"http.proxy", ""},
				[2]string{"http.sslVerify", "true"},
				[2]string{"http." + transportURL + ".extraheader", ""},
			)
			header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
			config = append(config, [2]string{"http." + transportURL + ".extraheader", header})
		}
	}

	result = append(result,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_CONFIG_COUNT="+strconv.Itoa(len(config)),
	)
	for index, pair := range config {
		result = append(result,
			"GIT_CONFIG_KEY_"+strconv.Itoa(index)+"="+pair[0],
			"GIT_CONFIG_VALUE_"+strconv.Itoa(index)+"="+pair[1],
		)
	}
	return result
}

func exactGitHubTransportURL(raw string) string {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\r\n\x00") {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.Port() != "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" || parsed.Path == "/" {
		return ""
	}
	return raw
}

func controllerCredentialEnvironment(upper string) bool {
	switch upper {
	case "GH_TOKEN", "GITHUB_TOKEN", "HIVE_GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "HIVE_AGENT_TOKEN_CACHE":
		return true
	}
	return strings.HasPrefix(upper, "GH_APP_") || strings.HasPrefix(upper, "GITHUB_APP_") || strings.HasPrefix(upper, "HIVE_GITHUB_APP_")
}

func blockedGitEnvironment(upper string) bool {
	if strings.HasPrefix(upper, "GIT_CONFIG") || strings.HasPrefix(upper, "GIT_TRACE") {
		return true
	}
	switch upper {
	case "GIT_ATTR_NOSYSTEM", "GIT_TERMINAL_PROMPT",
		"GIT_ASKPASS", "SSH_ASKPASS", "GIT_PROXY_COMMAND",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_INDEX_FILE", "GIT_NAMESPACE",
		"GIT_SHALLOW_FILE", "GIT_GRAFT_FILE", "GIT_REPLACE_REF_BASE",
		"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
		"GIT_EXEC_PATH", "GIT_TEMPLATE_DIR", "GIT_SSH", "GIT_SSH_COMMAND",
		"GIT_SSH_VARIANT", "GIT_PROTOCOL", "GIT_EXTERNAL_DIFF", "GIT_DIFF_OPTS",
		"GIT_PAGER", "GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_CURL_VERBOSE",
		"GIT_REDACT_COOKIES", "GIT_ALLOW_PROTOCOL", "GIT_PROTOCOL_FROM_USER",
		"GIT_HTTP_PROXY_AUTHMETHOD", "GIT_SSL_NO_VERIFY", "GIT_SSL_CAINFO",
		"GIT_SSL_CERT", "GIT_SSL_KEY", "GIT_SSL_VERSION", "GIT_SSL_CIPHER_LIST",
		"GIT_SSL_CERT_PASSWORD_PROTECTED", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"CURL_CA_BUNDLE":
		return true
	default:
		return false
	}
}

func controllerToken(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	token, _ := ctx.Value(controllerTokenKey{}).(string)
	return token
}

func validControllerToken(token string) bool {
	return token != "" && strings.TrimSpace(token) == token && !strings.ContainsAny(token, "\r\n\x00")
}
