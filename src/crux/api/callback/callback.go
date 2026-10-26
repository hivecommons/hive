// Package callback is the wire contract of the hive-side callback endpoint
// the Crux runtime calls for hive-provided capabilities (hivecommons/hive#14138
// stage 2; src/docs/design/crux.md "Credentials and hive-provided
// capabilities"). It imports only the standard library and crux/api (itself
// stdlib-only), so a hive that mounts it links nothing of the runtime and a
// runtime that calls it links nothing of hive.
//
// The direction is runtime → hive. The runtime never holds the GitHub App
// private key, the canary registry or any other hive-owned credential; it
// asks hive for a short-lived scoped token, a scan verdict, a created PR, and
// reports accounting and conditions back. Capabilities is the one interface
// set both ends implement: *Client over HTTP, hive's pkg/cruxhost directly.
// Every method fails closed: an unreachable or refusing hive is an error the
// caller treats as a refusal, never a less-safe default.
//
// Transport follows crux/api: HTTP+JSON under /v1, api.Error bodies with
// stable codes, a 0600 unix socket in a uid-owned 0700 directory, optional
// TCP only behind a bearer token, and a token-less server that answers only
// requests it saw arrive on its own socket.
//
//	GET  /v1/health                 server is up; which capabilities exist
//	POST /v1/tokens                 mint a scoped GitHub token
//	POST /v1/github/rest            record one relayed GitHub REST response
//	GET  /v1/github/rate-limit      shared core-bucket snapshot
//	POST /v1/usage                  record inference token usage
//	POST /v1/scan/canary            scan a GitHub write body for canaries
//	POST /v1/scan/text              scan tool payload text
//	POST /v1/conditions             observe or clear a runtime condition
//	GET  /v1/linear/credential      the Linear credential to inject
//	POST /v1/pulls                  open a pull request as the hive's App
//	GET  /v1/policy                 the host policy snapshot (stage 4)
//	POST /v1/policy/repo-auto-merge whether auto-merge is enabled for a repo
package callback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Version is the API version every route is prefixed with.
const Version = "v1"

// Routes.
const (
	PathHealth           = "/" + Version + "/health"
	PathTokens           = "/" + Version + "/tokens"
	PathGitHubREST       = "/" + Version + "/github/rest"
	PathGitHubRateLimit  = "/" + Version + "/github/rate-limit"
	PathUsage            = "/" + Version + "/usage"
	PathScanCanary       = "/" + Version + "/scan/canary"
	PathScanText         = "/" + Version + "/scan/text"
	PathConditions       = "/" + Version + "/conditions"
	PathLinearCredential = "/" + Version + "/linear/credential"
	PathPulls            = "/" + Version + "/pulls"
	PathPolicy           = "/" + Version + "/policy"
	PathRepoAutoMerge    = "/" + Version + "/policy/repo-auto-merge"
)

// Environment variables.
const (
	// SocketEnvVar overrides the default unix socket path hive serves the
	// callback endpoint on and the runtime dials.
	SocketEnvVar = "CRUX_HIVE_SOCKET"
	// TokenEnvVar is the bearer token hive requires and the runtime presents.
	// Optional on the socket (the file mode is the credential), required
	// for TCP.
	TokenEnvVar = "CRUX_HIVE_TOKEN"
	// ListenEnvVar is an optional loopback TCP address hive also serves on
	// (the k8s sidecar-container option, where the containers share the
	// pod's network namespace). It always requires TokenEnvVar and is
	// refused off loopback: the listener is plain http.
	ListenEnvVar = "CRUX_HIVE_LISTEN"
	// SocketName is the socket file name; it sits next to the runtime's
	// crux.sock in the same private directory.
	SocketName = "hive.sock"
	// socketDirName mirrors crux/config.ServeSocketDirName.
	socketDirName = "crux"
)

// File modes, the same as the runtime socket's.
const (
	SocketMode    os.FileMode = 0o600
	SocketDirMode os.FileMode = 0o700
)

// Media type.
const ContentTypeJSON = "application/json"

// Error codes carried in api.Error bodies.
const (
	CodeUnauthorized = "unauthorized"
	CodeBadRequest   = "bad_request"
	// CodeNotConfigured is a route for a capability this hive does not have
	// (no GitHub App, no canary registry, no Linear). The runtime treats it
	// exactly as the nil seam it stands in for.
	CodeNotConfigured = "not_configured"
	// CodeUpstream is a configured capability whose own upstream (GitHub,
	// Linear) refused; the message says why.
	CodeUpstream = "upstream_error"
	// CodeHive is an unexpected hive-side failure.
	CodeHive = "hive_error"
)

// ConditionClassUnavailable is the runtime condition class reserved for
// "the hive callback endpoint is unreachable"; Client.Status carries it
// locally and stage 4 surfaces it on the runtime's health route.
const ConditionClassUnavailable = "hive_callback_unavailable"

// DefaultTimeout bounds one callback when the caller's context has no
// deadline of its own. Fail-closed answers are cheap, so a dead socket
// costs at most this per call.
const DefaultTimeout = 5 * time.Second

// Sentinel errors a caller can test with errors.Is. Both are refusals.
var (
	// ErrUnavailable wraps a transport failure, timeout or 5xx: hive could
	// not answer.
	ErrUnavailable = errors.New("hive callback endpoint unavailable")
	// ErrUnauthorized wraps a 401: hive rejected the runtime's credential.
	// It is never retried with another credential.
	ErrUnauthorized = errors.New("hive callback endpoint rejected the bearer token")
	// ErrNotConfigured wraps a 404 CodeNotConfigured.
	ErrNotConfigured = errors.New("capability not configured on this hive")
)

// DefaultSocket is the socket path hive and the runtime agree on when
// neither is told otherwise: $CRUX_HIVE_SOCKET if set, else
// $XDG_RUNTIME_DIR/crux/hive.sock, else <tmp>/crux-<uid>/hive.sock. The
// rule is crux/config.DefaultServeSocket's with a different file name so the
// two sockets share one private directory.
func DefaultSocket() string {
	if p := os.Getenv(SocketEnvVar); p != "" {
		return p
	}
	return filepath.Join(DefaultSocketDir(), SocketName)
}

// DefaultSocketDir is the directory DefaultSocket places the socket in.
func DefaultSocketDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, socketDirName)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", socketDirName, os.Getuid()))
}

// Health is the body of GET /v1/health.
type Health struct {
	Version string `json:"version"`
	// Capabilities names the capabilities this hive has configured, as the
	// Capability* constants.
	Capabilities []string `json:"capabilities"`
}

// Capability names, as Health reports them.
const (
	CapabilityTokens     = "tokens"
	CapabilityGitHubREST = "github_rest"
	CapabilityUsage      = "usage"
	CapabilityCanary     = "canary"
	CapabilityText       = "text"
	CapabilityConditions = "conditions"
	CapabilityLinear     = "linear"
	CapabilityPulls      = "pulls"
	// CapabilityPolicy covers both policy routes. It is served only by a
	// Capabilities that also implements PolicySource.
	CapabilityPolicy = "policy"
)

// MintTokenRequest is the body of POST /v1/tokens. Tier is the permission
// tier (pushbroker.DefaultTier when empty). Repo is "owner/name" and Repos
// bare repository names; the server narrows the token to the union of the
// two (installation-wide when both are empty, the existing
// ScopedTokenForRepos contract). TTLSeconds is a request; the response says
// what was granted.
type MintTokenRequest struct {
	Agent      string   `json:"agent,omitempty"`
	Repo       string   `json:"repo,omitempty"`
	Repos      []string `json:"repos,omitempty"`
	Tier       string   `json:"tier,omitempty"`
	TTLSeconds int      `json:"ttl_seconds,omitempty"`
}

// MintTokenResponse is the body of a successful POST /v1/tokens.
type MintTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// String prints the response with the token redacted so an accidental %v in
// a log line cannot leak it.
func (r MintTokenResponse) String() string {
	return fmt.Sprintf("callback.MintTokenResponse{Token: <redacted>, ExpiresAt: %s}", r.ExpiresAt.Format(time.RFC3339))
}

// GoString is String, for %#v.
func (r MintTokenResponse) GoString() string { return r.String() }

// RESTRecord is the body of POST /v1/github/rest: one relayed GitHub REST
// response. Injected says the daemon's credential was used (its rate-limit
// headers describe the shared core bucket) rather than the agent's own.
// RateLimit carries only the X-RateLimit-* response headers.
type RESTRecord struct {
	Caller    string            `json:"caller"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Status    int               `json:"status"`
	Injected  bool              `json:"injected"`
	RateLimit map[string]string `json:"rate_limit,omitempty"`
}

// RateLimitHeaderPrefix selects the response headers RESTRecord carries.
const RateLimitHeaderPrefix = "X-Ratelimit-"

// RateLimitSnapshot is the body of GET /v1/github/rate-limit. OK is false
// when hive has no snapshot yet.
type RateLimitSnapshot struct {
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset"`
	OK        bool      `json:"ok"`
}

// Usage kinds, the three UsageSink record paths.
const (
	UsageKindGateway      = "gateway"
	UsageKindCopilot      = "copilot"
	UsageKindCopilotCache = "copilot_cache"
)

// UsageRecord is the body of POST /v1/usage.
type UsageRecord struct {
	Agent         string `json:"agent"`
	Model         string `json:"model"`
	Kind          string `json:"kind"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CacheRead     int64  `json:"cache_read_tokens,omitempty"`
	CacheCreation int64  `json:"cache_creation_tokens,omitempty"`
}

// CanaryScanRequest is the body of POST /v1/scan/canary.
type CanaryScanRequest struct {
	Agent  string `json:"agent"`
	Text   string `json:"text"`
	Source string `json:"source"`
}

// CanaryScanResponse is the body of a successful canary scan. It never
// carries the matched canary token: hive knows which one it planted, and
// the runtime only needs to refuse.
type CanaryScanResponse struct {
	Detected bool   `json:"detected"`
	Agent    string `json:"agent,omitempty"`
	Source   string `json:"source,omitempty"`
}

// Scan directions.
const (
	ScanInput  = "input"
	ScanOutput = "output"
)

// TextScanRequest is the body of POST /v1/scan/text.
type TextScanRequest struct {
	Direction string `json:"direction"`
	Text      string `json:"text"`
}

// TextScanResponse is the body of a successful text scan; it is the wire
// form of toolapprove.TextVerdict.
type TextScanResponse struct {
	Blocked           bool          `json:"blocked"`
	CriticalInjection bool          `json:"critical_injection,omitempty"`
	Findings          []TextFinding `json:"findings,omitempty"`
}

// TextFinding is one rule hit; Snippet is already masked by the scanner.
// High is the severity bit toolapprove turns into a violation.
type TextFinding struct {
	Kind    string `json:"kind"`
	Snippet string `json:"snippet,omitempty"`
	Rule    string `json:"rule,omitempty"`
	High    bool   `json:"high,omitempty"`
}

// Condition actions.
const (
	ConditionObserve = "observe"
	ConditionClear   = "clear"
)

// ConditionReport is the body of POST /v1/conditions: one Observe or Clear
// of crux.ConditionReporter.
type ConditionReport struct {
	Action      string `json:"action"`
	Class       string `json:"class"`
	Key         string `json:"key"`
	Message     string `json:"message,omitempty"`
	Diagnostics string `json:"diagnostics,omitempty"`
}

// LinearCredential is the body of GET /v1/linear/credential. Both fields
// empty means hive has none to inject.
type LinearCredential struct {
	AccessToken string `json:"access_token,omitempty"`
	APIKey      string `json:"api_key,omitempty"`
}

// Empty reports whether no credential is available.
func (c LinearCredential) Empty() bool { return c.AccessToken == "" && c.APIKey == "" }

// String redacts both secrets.
func (c LinearCredential) String() string {
	return fmt.Sprintf("callback.LinearCredential{AccessToken: %s, APIKey: %s}", redactedIf(c.AccessToken), redactedIf(c.APIKey))
}

// GoString is String, for %#v.
func (c LinearCredential) GoString() string { return c.String() }

func redactedIf(s string) string {
	if s == "" {
		return `""`
	}
	return "<redacted>"
}

// CreatePRRequest is the body of POST /v1/pulls.
type CreatePRRequest struct {
	Repo  string `json:"repo"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

// CreatePRResponse is the body of a successful POST /v1/pulls: the fields
// of crux.PRResult.
type CreatePRResponse struct {
	Number          int    `json:"number"`
	URL             string `json:"url"`
	Author          string `json:"author,omitempty"`
	AlreadyExisted  bool   `json:"already_existed,omitempty"`
	DuplicateTree   bool   `json:"duplicate_tree,omitempty"`
	DuplicateReason string `json:"duplicate_reason,omitempty"`
}

// Capabilities is the full capability set, as both ends see it: *Client
// implements it over HTTP, hive's pkg/cruxhost directly, and the runtime's
// crux/hivecap adapters are written against it so either serves. A nil
// error means hive answered; the error cases are the sentinels above, an
// *api.StatusError for anything else the server said, or a plain error.
type Capabilities interface {
	TokenMinter
	GitHubAccounting
	UsageRecorder
	CanaryScanner
	TextScanner
	ConditionSink
	LinearCredentials
	PRCreator
}

// TokenMinter mints a scoped GitHub token.
type TokenMinter interface {
	MintToken(ctx context.Context, req MintTokenRequest) (MintTokenResponse, error)
}

// GitHubAccounting records relayed REST traffic and reads the shared
// core-bucket snapshot.
type GitHubAccounting interface {
	RecordREST(ctx context.Context, rec RESTRecord) error
	RateLimit(ctx context.Context) (RateLimitSnapshot, error)
}

// UsageRecorder records inference token usage.
type UsageRecorder interface {
	RecordUsage(ctx context.Context, rec UsageRecord) error
}

// CanaryScanner scans a GitHub write body for planted canaries.
type CanaryScanner interface {
	ScanCanary(ctx context.Context, req CanaryScanRequest) (CanaryScanResponse, error)
}

// TextScanner scans tool payload text.
type TextScanner interface {
	ScanText(ctx context.Context, req TextScanRequest) (TextScanResponse, error)
}

// ConditionSink receives runtime condition observations and clears.
type ConditionSink interface {
	ReportCondition(ctx context.Context, rep ConditionReport) error
}

// LinearCredentials returns the Linear credential to inject.
type LinearCredentials interface {
	LinearCredential(ctx context.Context) (LinearCredential, error)
}

// PRCreator opens a pull request as the hive's App.
type PRCreator interface {
	CreatePR(ctx context.Context, req CreatePRRequest) (CreatePRResponse, error)
}

// Policy is the body of GET /v1/policy: the values hive hands an in-process
// runtime through the crux.Host policy setters (SetBuildInfo,
// SetExplainModeDefaultResolver, SetKickLogSourceResolver,
// SetGatewayBackendChecker, SetRepoAutoMergeEnabledResolver,
// SetDeadSessionRecoveryOwner, SetSandboxConfig), as one snapshot a sidecar
// runtime polls. An absent (nil / false) field is a setter hive has not
// called; the runtime leaves its own default in place for it.
type Policy struct {
	Build *BuildInfo `json:"build,omitempty"`
	// ExplainModeDefault is the hive-wide default explain mode, already
	// resolved against hive's environment fallback.
	ExplainModeDefault *string `json:"explain_mode_default,omitempty"`
	// KickLogSource is the kick log source in force, already resolved
	// against hive's environment override.
	KickLogSource *string `json:"kick_log_source,omitempty"`
	// GatewayBackends names the configured model gateways the runtime treats
	// as inference-routable backends (matched case-insensitively).
	GatewayBackends *[]string `json:"gateway_backends,omitempty"`
	// RepoAutoMerge says hive answers POST /v1/policy/repo-auto-merge; the
	// per-repository answer is a call, not part of the snapshot.
	RepoAutoMerge bool `json:"repo_auto_merge,omitempty"`
	// DeadSessionRecoveryOwnedElsewhere is SetDeadSessionRecoveryOwner's
	// argument: true when hive's watchdog owns dead-session restarts.
	DeadSessionRecoveryOwnedElsewhere *bool `json:"dead_session_recovery_owned_elsewhere,omitempty"`
	// Sandbox is the agent_sandbox block (crux/config.AgentSandboxConfig)
	// as JSON. It stays raw so this package keeps importing only the
	// standard library and crux/api.
	Sandbox json.RawMessage `json:"sandbox,omitempty"`
}

// BuildInfo is the hive version and commit recorded in kick log headers.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// RepoAutoMergeRequest is the body of POST /v1/policy/repo-auto-merge.
type RepoAutoMergeRequest struct {
	Repo string `json:"repo"`
}

// RepoAutoMergeResponse is its answer.
type RepoAutoMergeResponse struct {
	Enabled bool `json:"enabled"`
}

// PolicySource is the policy half of the callback contract. It is separate
// from Capabilities so existing implementations stay valid: the server
// answers the policy routes only when its Caps also implement it (404
// CodeNotConfigured otherwise); *Client always does.
type PolicySource interface {
	Policy(ctx context.Context) (Policy, error)
	RepoAutoMergeEnabled(ctx context.Context, repo string) (bool, error)
}
