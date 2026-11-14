package spoke

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Report-only heartbeat mirror to a second hub (#18148, RFC #18060 section 6).
//
// A spoke that its own hub manages can also SHOW on a second hub, for example
// a self-hosted hive listed on the public hub through the self-service
// registration flow (#18142, #18155). The mirror is strictly one-way:
//
//   - The spoke POSTs a REDUCED copy of each heartbeat to the mirror hub,
//     authenticated with a separate per-hive key (EnvMirrorHubHeartbeatKey).
//     The primary key is never sent to the mirror, and the mirror key is never
//     sent to the primary.
//   - The mirror's response is never applied. Nothing in it reaches
//     processHeartbeatResponse or any callback: no upgrade, restart, branch
//     switch, breaker, GitHub App config, funded gateway, project claim,
//     access list, visibility, cluster id or experiment flag. The response is
//     read only to COUNT those fields so an owner can see a mirror that is
//     trying to manage them (mirrorResponseIgnoredEvent).
//   - A mirror failure never affects the primary heartbeat or the hive. The
//     POST runs in its own goroutine with its own short timeout, after the
//     primary hub has accepted the beat, and never touches the liveness
//     clocks (lastHeartbeatSuccessUnix / lastHeartbeatAttemptUnix) or the
//     activity publisher's acknowledgement state.
//
// The reduced copy is driven by heartbeatMirrorFieldPolicy, which classifies
// every HeartbeatPayload field as mirrored or dropped; the inventory test
// fails when a field is added without a classification, so a new sensitive
// field cannot reach the mirror by default (same pattern as the hub's
// heartbeatResponseFieldFence).

const (
	// EnvMirrorHubURL is the base URL of the second hub. Unset disables the
	// mirror entirely and nothing in this file runs.
	EnvMirrorHubURL = "HIVE_MIRROR_HUB_URL"
	// EnvMirrorHubHeartbeatKey is the per-hive heartbeat key the MIRROR hub
	// issued (self-service registration, or an operator-minted key). It is
	// required: without it no mirror beat is sent, because the primary key must
	// never be offered to another hub.
	EnvMirrorHubHeartbeatKey = "HIVE_MIRROR_HUB_HEARTBEAT_KEY"
	// EnvMirrorHubHiveID optionally overrides hive_id on the mirrored copy, for
	// a mirror hub that knows this hive under a different (for example byo-)
	// id. Empty keeps the primary hive id.
	EnvMirrorHubHiveID = "HIVE_MIRROR_HUB_HIVE_ID"
	// EnvMirrorHubTimeout overrides the per-beat mirror POST timeout (Go
	// duration syntax). Invalid or non-positive values keep the default.
	EnvMirrorHubTimeout = "HIVE_MIRROR_HUB_TIMEOUT"

	// defaultMirrorHubTimeout bounds one mirror POST. Deliberately shorter than
	// heartbeatTimeout: the mirror has no liveness role, so a slow mirror hub
	// should simply miss a beat rather than hold a goroutine for long.
	defaultMirrorHubTimeout = 5 * time.Second

	// mirrorResponseIgnoredEvent is the log event name written at most once per
	// mirrorIgnoredLogWindow when the mirror's response carried authority
	// fields (upgrade, App config, gateway, ...) that this spoke ignored.
	mirrorResponseIgnoredEvent = "mirror_hub_response_ignored"

	// mirrorResponseMaxBytes caps how much of the mirror's response is read.
	// Only the authority-field count comes out of it, so a modest cap is plenty
	// and a hostile mirror cannot grow spoke memory.
	mirrorResponseMaxBytes = 256 * 1024

	// mirrorErrorBodyMaxBytes caps the rejection body kept for the status block.
	mirrorErrorBodyMaxBytes = 256

	// mirrorHeartbeatPath is the hub route the copy is POSTed to, the same route
	// the primary beat uses.
	mirrorHeartbeatPath = "/api/heartbeat"
)

// mirrorIgnoredLogWindow is how often (at most) the ignored-authority log line
// repeats while a mirror keeps sending instructions. A var so tests can shorten
// it; production keeps the default.
var mirrorIgnoredLogWindow = time.Hour

// mirrorFieldPolicy is how the reducer treats one HeartbeatPayload field.
type mirrorFieldPolicy int

const (
	// mirrorFieldMirrored is sent to the mirror hub as-is.
	mirrorFieldMirrored mirrorFieldPolicy = iota + 1
	// mirrorFieldDropped is zeroed before the copy leaves the spoke: a
	// credential-adjacent identifier (token hash, key fingerprints), the text of
	// an authentication error, the list of signed-in users, the activity digest,
	// or consent that belongs to the primary hub only.
	mirrorFieldDropped
)

// heartbeatMirrorFieldPolicy classifies every HeartbeatPayload field by its Go
// field name. Adding a field to HeartbeatPayload requires adding it here;
// TestHeartbeatMirrorFieldPolicyInventory fails otherwise. Any field missing
// from this table is DROPPED by the reducer, so an unclassified field fails
// closed even in a build that skipped the inventory test.
var heartbeatMirrorFieldPolicy = map[string]mirrorFieldPolicy{
	// Identity and project.
	"HiveID":            mirrorFieldMirrored,
	"Org":               mirrorFieldMirrored,
	"Repos":             mirrorFieldMirrored,
	"PrimaryRepo":       mirrorFieldMirrored,
	"AIAuthor":          mirrorFieldMirrored,
	"AIAuthorEffective": mirrorFieldMirrored,
	"GitHubAPIURL":      mirrorFieldMirrored,
	"GitHubHost":        mirrorFieldMirrored,
	"GitHubBaseURL":     mirrorFieldMirrored,
	"Owner":             mirrorFieldMirrored,
	"HiveType":          mirrorFieldMirrored,
	"ClusterID":         mirrorFieldMirrored,
	"IsPublic":          mirrorFieldMirrored,
	"Reporter":          mirrorFieldMirrored,
	"StartedAt":         mirrorFieldMirrored,
	"Timestamp":         mirrorFieldMirrored,
	// Build and deployment.
	"Version":   mirrorFieldMirrored,
	"GitHash":   mirrorFieldMirrored,
	"GitBranch": mirrorFieldMirrored,
	"ImageRef":  mirrorFieldMirrored,
	// Agents, governor, stats.
	"ACMMLevel":              mirrorFieldMirrored,
	"Agents":                 mirrorFieldMirrored,
	"Governor":               mirrorFieldMirrored,
	"Tokens24h":              mirrorFieldMirrored,
	"Contributors":           mirrorFieldMirrored,
	"Leaderboard":            mirrorFieldMirrored,
	"OpenFDs":                mirrorFieldMirrored,
	"FDSoftLimit":            mirrorFieldMirrored,
	"Health":                 mirrorFieldMirrored,
	"DashboardURL":           mirrorFieldMirrored,
	"SnapshotURL":            mirrorFieldMirrored,
	"Providers":              mirrorFieldMirrored,
	"ProviderLimitReason":    mirrorFieldMirrored,
	"ProviderLimitRebuffs":   mirrorFieldMirrored,
	"ProviderLimitHiveWide":  mirrorFieldMirrored,
	"ProviderLimitAgents":    mirrorFieldMirrored,
	"LastWriteCapableKickAt": mirrorFieldMirrored,
	"LastKickDisposition":    mirrorFieldMirrored,
	"LastKickSkipReason":     mirrorFieldMirrored,
	"NotWritableQueued":      mirrorFieldMirrored,
	"PublicURLSelfCheck":     mirrorFieldMirrored,
	"RouteExists":            mirrorFieldMirrored,
	"ForgeUnreachable":       mirrorFieldMirrored,
	"ClusterHealth":          mirrorFieldMirrored,
	"StatsStale":             mirrorFieldMirrored,
	"FreshAgentStats":        mirrorFieldMirrored,
	// Advisory digest telemetry (counts and timestamps; the error text is dropped below).
	"AdvisoryLastPostedAt":  mirrorFieldMirrored,
	"AdvisoryFindingCount":  mirrorFieldMirrored,
	"AdvisoryOverflowCount": mirrorFieldMirrored,
	// GitHub App posture (non-secret state tokens, ids and status words).
	"GitHubAppRequired":        mirrorFieldMirrored,
	"GitHubAppTokenStatus":     mirrorFieldMirrored,
	"GitHubAppTokenLastMintAt": mirrorFieldMirrored,
	"GitHubAppErrorClass":      mirrorFieldMirrored,
	"GitHubAppHTTPStatus":      mirrorFieldMirrored,
	"GitHubAppState":           mirrorFieldMirrored,
	"GitHubAppID":              mirrorFieldMirrored,
	"GitHubAppSlug":            mirrorFieldMirrored,
	"GitHubInstallationID":     mirrorFieldMirrored,
	"GitHubAppKeyPerHive":      mirrorFieldMirrored,
	"PendingGitHubAppInstall":  mirrorFieldMirrored,
	"RepoTargetMisconfigured":  mirrorFieldMirrored,
	"RepoTargetIssue":          mirrorFieldMirrored,
	// Upgrade state (what the spoke reports about itself).
	"AutoUpgrade":           mirrorFieldMirrored,
	"Upgrading":             mirrorFieldMirrored,
	"UpgradeTargetSHA":      mirrorFieldMirrored,
	"UpgradeFailed":         mirrorFieldMirrored,
	"UpgradeError":          mirrorFieldMirrored,
	"UpgradePendingRestart": mirrorFieldMirrored,
	"UpgradeDrain":          mirrorFieldMirrored,
	// Fleet contribution, repo activity and quadrant signals.
	"PRsMerged90d":                 mirrorFieldMirrored,
	"PRsRejected90d":               mirrorFieldMirrored,
	"CVEsClosed":                   mirrorFieldMirrored,
	"FleetStatsCollectedAt":        mirrorFieldMirrored,
	"RepoActivity":                 mirrorFieldMirrored,
	"RepoActivityCollectedAt":      mirrorFieldMirrored,
	"RepoActivityWindowHours":      mirrorFieldMirrored,
	"RepoActivityCountWindowHours": mirrorFieldMirrored,
	"BudgetCurrentSpend":           mirrorFieldMirrored,
	"BudgetLimit":                  mirrorFieldMirrored,
	"BudgetWindowStartsAt":         mirrorFieldMirrored,
	"BudgetWindowEndsAt":           mirrorFieldMirrored,
	"BudgetExhausted":              mirrorFieldMirrored,
	"BudgetIgnored":                mirrorFieldMirrored,
	"HoldTotal":                    mirrorFieldMirrored,
	"AwaitingReview":               mirrorFieldMirrored,
	"AgentErrorStreaks":            mirrorFieldMirrored,
	"ConsentWedged":                mirrorFieldMirrored,
	"NoCadenceAgents":              mirrorFieldMirrored,
	"SLAViolations":                mirrorFieldMirrored,
	"TasksCompleted7d":             mirrorFieldMirrored,
	"AgentsWithModel":              mirrorFieldMirrored,
	"GatewayNames":                 mirrorFieldMirrored,
	"GatewayHealth":                mirrorFieldMirrored,
	"ComponentReach":               mirrorFieldMirrored,
	"Compliance":                   mirrorFieldMirrored,
	"Evidence":                     mirrorFieldMirrored,

	// Dropped: credential-adjacent identifiers. The primary hub matches these
	// against records it holds; a report-only hub has no such records and must
	// not learn which key or token this hive carries.
	"DashboardTokenHash":         mirrorFieldDropped,
	"GitHubAppKeyFingerprint":    mirrorFieldDropped,
	"WebhookRelayKeyFingerprint": mirrorFieldDropped,
	"GitHubAppKeysHeld":          mirrorFieldDropped,
	// Dropped: the text of authentication errors.
	"InferenceAuthError":  mirrorFieldDropped,
	"GitHubAppTokenError": mirrorFieldDropped,
	"GitHubAppPermIssue":  mirrorFieldDropped,
	"AdvisoryError":       mirrorFieldDropped,
	// Dropped: who is signed in to this hive.
	"ActiveSessionUsers":  mirrorFieldDropped,
	"EngagedSessionUsers": mirrorFieldDropped,
	"UserLastActions":     mirrorFieldDropped,
	// Dropped: the activity digest (its acknowledgement protocol belongs to the
	// primary hub's activity publisher).
	"Activity": mirrorFieldDropped,
	// Dropped: consent to receive flags, which only the primary may deliver.
	"SelfExperimentsOptIn": mirrorFieldDropped,
}

// heartbeatMirrorResponseHarmless names the HeartbeatResponse fields that
// carry no authority over the spoke: protocol, version display and signature
// binding. Every OTHER field of a mirror response is an attempt to deliver
// credentials or instructions and is counted as ignored. Fail-closed by
// construction: a new response field is counted until someone lists it here.
var heartbeatMirrorResponseHarmless = map[string]bool{
	"OK":                true,
	"HubGitHash":        true,
	"LatestSHA":         true,
	"LatestTag":         true,
	"SigHiveID":         true,
	"SigSeq":            true,
	"SigSignedAt":       true,
	"SigVersion":        true,
	"ActivityETag":      true,
	"ActivityEventsAck": true,
}

// mirrorHubConfig is the mirror's effective configuration, read from the
// environment on every beat (like SpokeHeartbeatKey) so a rotated key or a
// changed URL takes effect without a restart.
type mirrorHubConfig struct {
	url     string
	key     string
	hiveID  string
	timeout time.Duration
}

// mirrorHubConfigured reports whether EnvMirrorHubURL is set at all. It is the
// only check the hot path pays when the mirror is off.
func mirrorHubConfigured() bool {
	return strings.TrimSpace(os.Getenv(EnvMirrorHubURL)) != ""
}

// loadMirrorHubConfig validates the mirror environment. It returns a
// user-facing error (never containing the key) when the mirror is configured
// but unusable, so the status block can say why nothing is being sent.
func loadMirrorHubConfig(primaryHubURL string) (mirrorHubConfig, error) {
	raw := strings.TrimRight(strings.TrimSpace(os.Getenv(EnvMirrorHubURL)), "/")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return mirrorHubConfig{}, fmt.Errorf("%s must be an absolute http(s) URL", EnvMirrorHubURL)
	}
	if primaryHubURL != "" && strings.TrimRight(strings.TrimSpace(primaryHubURL), "/") == raw {
		return mirrorHubConfig{}, fmt.Errorf("%s is the primary hub URL; the mirror must be a different hub", EnvMirrorHubURL)
	}
	key := strings.TrimSpace(os.Getenv(EnvMirrorHubHeartbeatKey))
	if key == "" {
		return mirrorHubConfig{}, fmt.Errorf("%s is not set; the primary heartbeat key is never sent to a mirror", EnvMirrorHubHeartbeatKey)
	}
	timeout := defaultMirrorHubTimeout
	if v := strings.TrimSpace(os.Getenv(EnvMirrorHubTimeout)); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil && d > 0 {
			timeout = d
		}
	}
	return mirrorHubConfig{
		url:     raw,
		key:     key,
		hiveID:  strings.TrimSpace(os.Getenv(EnvMirrorHubHiveID)),
		timeout: timeout,
	}, nil
}

// MirrorHubStatus is the owner-facing view of the mirror, surfaced in the
// spoke's deep health check under hub_heartbeat.mirror. It names the mirror
// HOST only; the key never appears anywhere observable.
type MirrorHubStatus struct {
	// URLHost is the host (and port) of EnvMirrorHubURL, or empty when the
	// configured value could not be parsed.
	URLHost string `json:"url_host"`
	// LastOK is when the mirror last accepted a beat (RFC3339 UTC), empty if never.
	LastOK string `json:"last_ok,omitempty"`
	// LastAttempt is when a mirror beat was last attempted (RFC3339 UTC).
	LastAttempt string `json:"last_attempt,omitempty"`
	// LastError is the most recent failure (configuration, transport, or a
	// rejection with its HTTP status), cleared on the next accepted beat.
	LastError string `json:"last_error,omitempty"`
	// IgnoredFields is the running total of authority-carrying fields the
	// mirror's responses tried to deliver and this spoke ignored.
	IgnoredFields int64 `json:"ignored_fields"`
	// LastIgnoredFields lists the JSON names of the authority fields present in
	// the most recent mirror response that carried any (names only, never values).
	LastIgnoredFields []string `json:"last_ignored_fields,omitempty"`
}

// mirrorState is the process-global mirror bookkeeping. Written by the mirror
// goroutine, read by the dashboard's health handler.
var mirrorState = struct {
	sync.Mutex
	lastOK            time.Time
	lastAttempt       time.Time
	lastError         string
	lastIgnoredFields []string
	ignoredLoggedAt   time.Time
}{}

// mirrorIgnoredTotal is the running count of ignored authority fields.
var mirrorIgnoredTotal atomic.Int64

// mirrorInFlight refuses a second concurrent mirror POST: a stalled mirror hub
// must not accumulate goroutines beat after beat.
var mirrorInFlight atomic.Bool

// MirrorHubStatusSnapshot returns the mirror's status block and whether a
// mirror is configured at all. ok is false (and the block empty) when EnvMirrorHubURL is
// unset, so callers can omit the block entirely.
func MirrorHubStatusSnapshot() (MirrorHubStatus, bool) {
	if !mirrorHubConfigured() {
		return MirrorHubStatus{}, false
	}
	out := MirrorHubStatus{IgnoredFields: mirrorIgnoredTotal.Load()}
	if u, err := url.Parse(strings.TrimSpace(os.Getenv(EnvMirrorHubURL))); err == nil {
		out.URLHost = u.Host
	}
	mirrorState.Lock()
	defer mirrorState.Unlock()
	if !mirrorState.lastOK.IsZero() {
		out.LastOK = mirrorState.lastOK.UTC().Format(time.RFC3339)
	}
	if !mirrorState.lastAttempt.IsZero() {
		out.LastAttempt = mirrorState.lastAttempt.UTC().Format(time.RFC3339)
	}
	out.LastError = mirrorState.lastError
	out.LastIgnoredFields = append([]string(nil), mirrorState.lastIgnoredFields...)
	return out, true
}

func recordMirrorAttempt() {
	mirrorState.Lock()
	mirrorState.lastAttempt = time.Now()
	mirrorState.Unlock()
}

func recordMirrorError(msg string) {
	mirrorState.Lock()
	mirrorState.lastError = msg
	mirrorState.Unlock()
}

func recordMirrorSuccess() {
	mirrorState.Lock()
	mirrorState.lastOK = time.Now()
	mirrorState.lastError = ""
	mirrorState.Unlock()
}

// resetMirrorStateForTest returns the mirror bookkeeping to process-start values.
func resetMirrorStateForTest() {
	mirrorState.Lock()
	mirrorState.lastOK = time.Time{}
	mirrorState.lastAttempt = time.Time{}
	mirrorState.lastError = ""
	mirrorState.lastIgnoredFields = nil
	mirrorState.ignoredLoggedAt = time.Time{}
	mirrorState.Unlock()
	mirrorIgnoredTotal.Store(0)
	mirrorInFlight.Store(false)
}

// reduceHeartbeatForMirror returns the copy of p that may leave for the mirror
// hub: a deep clone with every dropped (or unclassified) field zeroed and, when
// hiveIDOverride is non-empty, hive_id replaced. The caller's payload is never
// modified. Returns nil when the payload cannot be cloned.
func reduceHeartbeatForMirror(p *HeartbeatPayload, hiveIDOverride string) *HeartbeatPayload {
	c := clonePayload(p)
	if c == nil {
		return nil
	}
	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if heartbeatMirrorFieldPolicy[t.Field(i).Name] == mirrorFieldMirrored {
			continue
		}
		if fv := v.Field(i); fv.CanSet() {
			fv.Set(reflect.Zero(fv.Type()))
		}
	}
	if hiveIDOverride != "" {
		c.HiveID = hiveIDOverride
	}
	return c
}

// mirrorResponseAuthorityFields decodes a mirror hub response and returns the
// JSON names of every non-harmless field it set. It never returns the decoded
// response: nothing in it may be applied.
func mirrorResponseAuthorityFields(raw []byte) ([]string, error) {
	var resp HeartbeatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	v := reflect.ValueOf(resp)
	t := v.Type()
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if heartbeatMirrorResponseHarmless[f.Name] || v.Field(i).IsZero() {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" {
			name = f.Name
		}
		names = append(names, name)
	}
	return names, nil
}

// mirrorHeartbeatAsync sends the reduced copy of payload to the mirror hub in
// its own goroutine. Called from postHeartbeatToHub after the primary hub
// accepted the beat; it returns immediately. payload is read synchronously
// (cloned) before the goroutine starts, so the caller may reuse it.
func mirrorHeartbeatAsync(ctx context.Context, primaryHubURL string, payload *HeartbeatPayload, logger *slog.Logger) {
	if payload == nil || !mirrorHubConfigured() {
		return
	}
	cfg, err := loadMirrorHubConfig(primaryHubURL)
	if err != nil {
		recordMirrorError(err.Error())
		return
	}
	copyForMirror := reduceHeartbeatForMirror(payload, cfg.hiveID)
	if copyForMirror == nil {
		recordMirrorError("mirror payload could not be cloned")
		return
	}
	if !mirrorInFlight.CompareAndSwap(false, true) {
		recordMirrorError("previous mirror beat still in flight; skipped this beat")
		return
	}
	go func() {
		defer mirrorInFlight.Store(false)
		postHeartbeatToMirror(ctx, cfg, copyForMirror, logger)
	}()
}

// postHeartbeatToMirror performs one mirror POST. It records the outcome in
// mirrorState and returns nothing: the response is parsed only to count the
// authority fields the mirror tried to deliver.
func postHeartbeatToMirror(ctx context.Context, cfg mirrorHubConfig, payload *HeartbeatPayload, logger *slog.Logger) {
	recordMirrorAttempt()
	body, err := json.Marshal(payload)
	if err != nil {
		recordMirrorError("marshal failed: " + err.Error())
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.url+mirrorHeartbeatPath, bytes.NewReader(body))
	if err != nil {
		recordMirrorError("request failed: " + err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		msg := "unreachable: " + mirrorTransportError(err)
		recordMirrorError(msg)
		if logger != nil {
			logger.Debug("mirror hub heartbeat failed", "mirror_host", req.URL.Host, "error", mirrorTransportError(err))
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, mirrorErrorBodyMaxBytes))
		msg := fmt.Sprintf("rejected: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		recordMirrorError(msg)
		if logger != nil {
			logger.Debug("mirror hub heartbeat rejected", "mirror_host", req.URL.Host, "status", resp.StatusCode)
		}
		return
	}
	recordMirrorSuccess()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, mirrorResponseMaxBytes))
	if err != nil {
		return
	}
	ignored, err := mirrorResponseAuthorityFields(raw)
	if err != nil || len(ignored) == 0 {
		return
	}
	noteMirrorIgnoredFields(ignored, req.URL.Host, logger)
}

// noteMirrorIgnoredFields counts authority fields a mirror response carried and
// logs mirrorResponseIgnoredEvent at most once per mirrorIgnoredLogWindow.
// Names only: the values (which may be credentials) are never logged.
func noteMirrorIgnoredFields(ignored []string, host string, logger *slog.Logger) {
	mirrorIgnoredTotal.Add(int64(len(ignored)))
	now := time.Now()
	mirrorState.Lock()
	mirrorState.lastIgnoredFields = append([]string(nil), ignored...)
	shouldLog := mirrorState.ignoredLoggedAt.IsZero() || now.Sub(mirrorState.ignoredLoggedAt) >= mirrorIgnoredLogWindow
	if shouldLog {
		mirrorState.ignoredLoggedAt = now
	}
	mirrorState.Unlock()
	if shouldLog && logger != nil {
		logger.Warn(mirrorResponseIgnoredEvent+": the mirror hub's heartbeat response carried config, credential or upgrade fields; a report-only mirror has no authority over this hive and they were not applied",
			"event", mirrorResponseIgnoredEvent,
			"mirror_host", host,
			"fields", strings.Join(ignored, ","),
			"ignored_total", mirrorIgnoredTotal.Load(),
		)
	}
}

// mirrorTransportError reduces a transport error to a short, URL-free form so
// the status block and logs never echo a query string or userinfo.
func mirrorTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		if errors.Is(uerr.Err, context.DeadlineExceeded) {
			return "timeout"
		}
		return uerr.Err.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return err.Error()
}
