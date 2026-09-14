// Package rotation implements automatic provider failover for hive agents.
// When a provider's subscription/credit is exhausted, it moves agents to a
// different provider at the same capability tier. See RFC #3958.
package rotation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
	"github.com/hivecommons/hive/pkg/config"
)

// probeTimeout bounds each CLI/HTTP headroom probe.
const probeTimeout = 10 * time.Second

// probeWaitDelay bounds how long exec.Cmd.Wait may block on the probe's I/O
// pipes after the child is killed or the context expires. CLI probes spawn
// processes (codex app-server, claude) that fork grandchildren which inherit
// the stdout/stderr pipe write ends; killing the direct child then leaves the
// pipe open, exec's copier goroutine never sees EOF, and a bare Wait blocks
// FOREVER. Observed live on weavster: the watchdog's codex auth probe wedged
// the main governor goroutine for 2.5h (no evals, no advisory digest — the
// hub flagged the digest stale). WaitDelay is the stdlib's remedy: after the
// delay Wait force-closes the pipes and returns ErrWaitDelay instead of
// hanging.
const probeWaitDelay = 5 * time.Second

// pollInterval is how often the Manager re-probes provider headroom.
const pollInterval = 5 * time.Minute

// deepSeekMinBalanceUSD is the balance below which DeepSeek is considered
// exhausted.
const deepSeekMinBalanceUSD = 1.00

// fullPct is 100% — the "no usage observed" remaining headroom.
const fullPct = 100

// Provider classes (see config.ProviderRotationConfig.Class).
const (
	ClassSubscription = "subscription"
	ClassMetered      = "metered"
)

// LimitWindow is the normalized capacity window shape from RFC #5698. A
// headroom reading may carry several windows because short-term, weekly, and
// scoped limits reset independently and must not be collapsed by consumers that
// need deterministic admission decisions.
type LimitWindow struct {
	ID           string            `json:"id,omitempty"`
	Kind         string            `json:"kind"`
	PercentUsed  int               `json:"percent_used"`
	PctRemaining int               `json:"pct_remaining"`
	ResetAt      time.Time         `json:"resets_at,omitempty"`
	Scope        map[string]string `json:"scope,omitempty"`
	// DurationMins is the provider-stated length of the window
	// (kubestellar/hive#6952). #6833's normalized reading is (ID, duration,
	// used/remaining, reset) and the duration was the missing member: without
	// it a consumer cannot tell a 5-hour window from a weekly one except by
	// trusting Kind, and Kind used to be hard-coded at the probe site. Zero
	// means the provider did not state one.
	DurationMins int `json:"duration_mins,omitempty"`
}

// Headroom describes a provider's current capacity.
type Headroom struct {
	Provider     string        `json:"provider"`
	Available    bool          `json:"available"`     // false = exhausted or probe failed
	PctRemaining int           `json:"pct_remaining"` // 0–100; 0 when probe failed
	ResetAt      time.Time     `json:"reset_at,omitempty"`
	Limits       []LimitWindow `json:"limits,omitempty"`
	// PlanType and PaidCreditsAvailable carry #6833's "whether the provider
	// reports that paid credits or extra usage are available, without enabling
	// them" (kubestellar/hive#6952). PaidCreditsAvailable is a pointer because
	// "the provider did not say" must stay distinguishable from "no" — the
	// terminal warning about spending real money should not be driven by a
	// zero value.
	PlanType             string `json:"plan_type,omitempty"`
	PaidCreditsAvailable *bool  `json:"paid_credits_available,omitempty"`
	// OrdinaryUsageAllowed is tri-state: true, false, and nil for "the provider
	// did not say". nil must never be read as recovery (#6833), so consumers
	// that need a definite answer treat it as unknown.
	OrdinaryUsageAllowed *bool `json:"ordinary_usage_allowed,omitempty"`
	ProbeErr             error `json:"-"` // non-nil = measurement failed (NOT treated as exhausted)
}

// ProbeError surfaces ProbeErr as a string for JSON consumers.
func (h Headroom) ProbeError() string {
	if h.ProbeErr != nil {
		return h.ProbeErr.Error()
	}
	return ""
}

// MarshalJSON includes the probe error text alongside the exported fields.
func (h Headroom) MarshalJSON() ([]byte, error) {
	type alias struct {
		Provider     string        `json:"provider"`
		Available    bool          `json:"available"`
		PctRemaining int           `json:"pct_remaining"`
		ResetAt      time.Time     `json:"reset_at,omitempty"`
		Limits       []LimitWindow `json:"limits,omitempty"`
		ProbeErr     string        `json:"probe_error,omitempty"`
	}
	return json.Marshal(alias{
		Provider:     h.Provider,
		Available:    h.Available,
		PctRemaining: h.PctRemaining,
		ResetAt:      h.ResetAt,
		Limits:       h.Limits,
		ProbeErr:     h.ProbeError(),
	})
}

// Prober probes a single provider's headroom.
type Prober interface {
	Provider() string
	Probe(ctx context.Context) Headroom
}

// runCLI executes a CLI probe command with a bounded timeout and returns the
// combined output.
func runCLI(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// See probeWaitDelay: without this a grandchild holding the output pipe
	// makes CombinedOutput block past the context timeout, indefinitely.
	cmd.WaitDelay = probeWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s probe failed: %w", name, err)
	}
	return string(out), nil
}

// failOpen returns a Headroom marking a failed measurement: Available stays
// true because "couldn't probe" is NOT "exhausted" (RFC #3958 invariant 7).
func failOpen(provider string, err error) Headroom {
	return Headroom{Provider: provider, Available: true, ProbeErr: err}
}

// ClaudeProber probes Anthropic subscription usage via the OAuth usage API.
//
// SOURCE DECISION (kubestellar/hive#6965): #6833's adapter table specifies the
// documented status-line JSON `rate_limits.five_hour` / `rate_limits.seven_day`
// fields, chaining an existing user status-line command. This adapter instead
// reads the HTTP usage endpoint below. That is a deliberate, recorded choice,
// not an oversight: the HTTP source needs no contributor status-line command to
// exist, needs no ephemeral status-line overlay in container launch modes, and
// works headless — so it cannot overwrite or depend on a contributor's own
// status line to enable a safety feature. #6833's adapter table is amended to
// match in src/docs/contributor-relay.md so the next reader does not "fix" this
// back to the status line. The two windows the status line would expose
// (`five_hour`, `seven_day`) are the same two this endpoint returns as
// `session` and `weekly_all`; claudeWindowDurationMins ties them to the shared
// banding. Obtaining a reading here sends no model prompt — it is a plain
// authenticated GET — so it consumes no model turn.
//
// `claude /usage` no longer renders the weekly-quota block (current builds
// show session stats only) and headless `/status` is unavailable, so the probe
// uses the same undocumented endpoint Claude Code's own HUD polls:
//
//	GET https://api.anthropic.com/api/oauth/usage
//	Authorization: Bearer <accessToken from ~/.claude/.credentials.json>
//	anthropic-beta: oauth-2025-04-20        (required, else 401)
//
// The `limits` array carries per-kind percent + resets_at (session /
// weekly_all / weekly_scoped); the binding limit is the max percent. The
// endpoint rate-limits aggressively, so a 429/401/parse failure is fail-open
// (never evidence of exhaustion).
type ClaudeProber struct {
	ThresholdPct int
	// BaseURL overrides the API endpoint (tests). Default production host.
	BaseURL string
	// Client overrides the HTTP client (tests).
	Client *http.Client
	// CredentialsPath overrides the default ~/.claude/.credentials.json.
	CredentialsPath string
}

const claudeUsageBaseURL = "https://api.anthropic.com"
const claudeOAuthBeta = "oauth-2025-04-20"

// sharedCLIHome is the durable shared CLI home on the PVC — the HOME the
// manager gives agent tmux sessions and where fleet-level CLI auth state
// (.claude, .codex, .copilot) persists across pod restarts.
const sharedCLIHome = "/data/home"

type claudeUsageResponse struct {
	Limits []struct {
		Kind     string     `json:"kind"`
		Percent  *float64   `json:"percent"`
		ResetsAt *time.Time `json:"resets_at"`
	} `json:"limits"`
}

func (p ClaudeProber) Provider() string { return "anthropic" }

func (p ClaudeProber) Probe(ctx context.Context) Headroom {
	credPath := p.CredentialsPath
	if credPath == "" {
		// The hive main process runs with HOME=/home/dev (Dockerfile), but the
		// durable OAuth credentials live in the shared CLI home on the PVC —
		// claude.CredentialsPath (/data/home/.claude/.credentials.json), the
		// same canonical location authprobe and the session watcher use. Try
		// $HOME first (dev shells, tests), then fall back to the shared home.
		home, err := os.UserHomeDir()
		if err != nil {
			return failOpen(p.Provider(), err)
		}
		credPath = filepath.Join(home, ".claude", ".credentials.json")
		if _, statErr := os.Stat(credPath); statErr != nil {
			if _, sharedErr := os.Stat(claude.CredentialsPath); sharedErr == nil {
				credPath = claude.CredentialsPath
			}
		}
	}
	raw, err := os.ReadFile(credPath)
	if err != nil {
		return failOpen(p.Provider(), fmt.Errorf("claude credentials: %w", err))
	}
	var creds claude.Credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return failOpen(p.Provider(), fmt.Errorf("claude credentials parse: %w", err))
	}
	if creds.ClaudeAIOAuth == nil || creds.ClaudeAIOAuth.AccessToken == "" {
		// An empty token is the signature of an expired OAuth session; the
		// CLI reports "Login expired" and serves nothing. Not exhaustion.
		return failOpen(p.Provider(), errors.New("claude credentials: empty accessToken"))
	}
	oauth := creds.ClaudeAIOAuth
	if oauth.ExpiresAt > 0 && oauth.ExpiresAt < time.Now().UnixMilli() {
		if oauth.RefreshToken != "" {
			// Claude access tokens are short-lived. The CLI silently redeems the
			// refresh token on its next real request, so sending the stale access
			// token to /usage would manufacture a 401 that the watchdog mistakes
			// for a dead fleet credential (#5165). This probe is inconclusive,
			// exactly like every other failed headroom measurement; it is not
			// evidence of exhaustion or a need for human re-authentication.
			return failOpen(p.Provider(), errors.New("claude access token expired; refresh token present, CLI will refresh on next use"))
		}
		// With no refresh grant, expiry is conclusive and the watchdog should
		// preserve its high-severity re-authentication alert.
		return failOpen(p.Provider(), errors.New("claude credentials: login expired (no refresh token)"))
	}
	base := p.BaseURL
	if base == "" {
		base = claudeUsageBaseURL
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: probeTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/oauth/usage", nil)
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	req.Header.Set("Authorization", "Bearer "+oauth.AccessToken)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	resp, err := client.Do(req)
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return failOpen(p.Provider(), fmt.Errorf("claude usage HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	h, err := claudeHeadroom(p.Provider(), p.ThresholdPct, body)
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	return h
}

// claudeHeadroom builds a normalized reading from an `/api/oauth/usage` payload
// (kubestellar/hive#6965).
//
// Like codexHeadroom, it reports an error rather than a permissive reading
// whenever the payload carries no window it recognizes — an empty `limits`
// array, or a schema that has moved on so nothing carries a percent. That must
// surface as unknown so the caller enters the configured unknown-data
// behaviour: a guard that silently reports full headroom off an unrecognized
// schema is worse than no guard, because it manufactures confidence that Hive
// will stop in time (#6833). Before this the same input returned a confident
// 100%-remaining, healthy reading.
func claudeHeadroom(provider string, thresholdPct int, body []byte) (Headroom, error) {
	var parsed claudeUsageResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Headroom{}, err
	}
	used := 0
	var resetAt time.Time
	limits := make([]LimitWindow, 0, len(parsed.Limits))
	for _, l := range parsed.Limits {
		if l.Percent == nil {
			// Some kinds omit percent; they carry no usable reading and must
			// not be counted as 0% used (the binding limit is the max percent
			// among those that have one).
			continue
		}
		pct := int(*l.Percent)
		lw := LimitWindow{
			ID:          l.Kind,
			Kind:        normalizeLimitKind(l.Kind),
			PercentUsed: pct,
			// DurationMins completes #6833's reading shape and ties each named
			// window to the SHARED banding (codexWindowKind) rather than a
			// second scheme; the provider-native Kind label is preserved so the
			// relay guard's short/weekly/scoped reserves keep matching.
			DurationMins: claudeWindowDurationMins(l.Kind),
			PctRemaining: fullPct - pct,
		}
		if l.ResetsAt != nil {
			lw.ResetAt = *l.ResetsAt
		}
		limits = append(limits, lw)
		// The binding window is the most-used one; reporting a roomier window
		// would let an exhausted one pass unnoticed.
		if pct > used {
			used = pct
			if l.ResetsAt != nil {
				resetAt = *l.ResetsAt
			}
		}
	}
	if len(limits) == 0 {
		return Headroom{}, errors.New("claude usage: no limit window carried a percent (unrecognized schema)")
	}
	return Headroom{
		Provider:     provider,
		Available:    used < thresholdPct,
		PctRemaining: fullPct - used,
		ResetAt:      resetAt,
		Limits:       limits,
	}, nil
}

func normalizeLimitKind(kind string) string {
	switch kind {
	case "weekly_all":
		return "weekly"
	default:
		return kind
	}
}

// claudeWindowDurationMins maps a Claude usage window kind to its documented
// duration so the reading carries #6833's duration member and the shared
// codexWindowKind banding applies (kubestellar/hive#6965). Claude reports a
// rolling ~5-hour window as `session` and the long window as `weekly_all` /
// `weekly_scoped`; the documented status-line fields name the same two windows
// `rate_limits.five_hour` and `rate_limits.seven_day`. A kind with no known
// duration yields zero, matching "the provider did not state one".
func claudeWindowDurationMins(kind string) int {
	switch kind {
	case "session", "five_hour":
		return 300
	case "weekly", "weekly_all", "weekly_scoped", "seven_day":
		return 10080
	default:
		return 0
	}
}

// CodexProber probes OpenAI subscription usage via the codex app-server
// JSON-RPC interface.
//
// `codex /status` is TUI-only: current builds reject `--output-format` and
// headless invocations die with "stdin is not a terminal", so the probe drives
// the app-server protocol directly: spawn `codex app-server`, exchange an
// initialize handshake, then call `account/rateLimits/read`. The reply carries
// rateLimits.primary (the binding window) with usedPercent + resetsAt. A probe
// failure is fail-open: never evidence of exhaustion.
type CodexProber struct {
	ThresholdPct int
}

// codexRateLimitsResult mirrors the subset of `account/rateLimits/read` this
// probe consumes. The shape is from `codex app-server generate-json-schema`
// on codex-cli 0.154.0 (kubestellar/hive#6952) — previously only `primary`
// was declared, so the second window was invisible and its exhaustion could
// not hold work.
type codexRateLimitsResult struct {
	RateLimits struct {
		Primary   *codexRateLimitWindow `json:"primary"`
		Secondary *codexRateLimitWindow `json:"secondary"`
		// RateLimitsByLimitID carries any provider-scoped limits beyond the two
		// positional windows (kubestellar/hive#6964). The documented
		// RateLimitSnapshot schema (codex-cli 0.154.0) reports these keyed by
		// limitId; folding them into the reading means an exhausted scoped
		// limit holds work rather than hiding behind roomier primary/secondary
		// windows.
		RateLimitsByLimitID map[string]*codexRateLimitWindow `json:"rateLimitsByLimitId"`
		PlanType            string                           `json:"planType"`
		Credits             *struct {
			// Available is the provider's own statement that paid credits or
			// extra usage COULD be spent. Reading it is not enabling it: see
			// TestNoCodexSpendOrBillingMutation, which pins that nothing in
			// this tree calls the consume endpoint.
			Available *bool `json:"available"`
			Balance   *int  `json:"balance"`
		} `json:"credits"`
	} `json:"rateLimits"`
	// OrdinaryUsageAllowed is a THREE-state field: true, false, and absent.
	// Absent means the provider could not say, and #6833 requires that be
	// carried as unknown rather than read as recovery — a nil deref into
	// `false` here would silently hand back headroom nobody confirmed.
	OrdinaryUsageAllowed *bool `json:"ordinaryUsageAllowed"`
}

type codexRateLimitWindow struct {
	UsedPercent        int   `json:"usedPercent"`
	ResetsAt           int64 `json:"resetsAt"`
	WindowDurationMins int   `json:"windowDurationMins"`
	// LimitID/LimitName identify a scoped window (kubestellar/hive#6964). The
	// ID lets rateLimitsByLimitId entries that duplicate a positional window be
	// deduped; the name is surfaced (not acted on) for the terminal message.
	LimitID   string `json:"limitId"`
	LimitName string `json:"limitName"`
}

// codexWindowKind derives the window kind from the provider-stated duration
// (kubestellar/hive#6952). The duration is the only authoritative
// discriminator; the probe used to hard-code "weekly" onto whichever window
// happened to be `primary`, which made the label unverifiable by construction
// and could apply the weekly reserve to a five-hour window.
//
// A duration outside the known bands deliberately yields an unrecognized kind
// rather than a guessed one. The contributor guard evaluates unknown kinds
// against the default reserve (kubestellar/hive#6951), so an unfamiliar window
// is still enforced — inventing a familiar-looking label would not be.
func codexWindowKind(durationMins int) string {
	switch {
	case durationMins <= 0:
		return "unknown"
	case durationMins <= 60:
		return "session"
	case durationMins <= 360:
		return "five_hour"
	case durationMins <= 2880:
		return "daily"
	case durationMins <= 20160:
		return "weekly"
	default:
		return fmt.Sprintf("window_%dm", durationMins)
	}
}

func codexLimitWindow(id string, w *codexRateLimitWindow) LimitWindow {
	lw := LimitWindow{
		ID:           id,
		Kind:         codexWindowKind(w.WindowDurationMins),
		PercentUsed:  w.UsedPercent,
		PctRemaining: fullPct - w.UsedPercent,
		DurationMins: w.WindowDurationMins,
	}
	if w.LimitName != "" {
		lw.Scope = map[string]string{"limit_name": w.LimitName}
	}
	if w.ResetsAt > 0 {
		lw.ResetAt = time.Unix(w.ResetsAt, 0).UTC()
	}
	return lw
}

func (p CodexProber) Provider() string { return "openai" }

func (p CodexProber) Probe(ctx context.Context) Headroom {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout+5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "app-server")
	// See probeWaitDelay: codex app-server forks helpers that inherit the
	// stdout pipe, so the deferred Kill+Wait below blocked forever without
	// this — wedging the caller (the watchdog tick on the main governor
	// goroutine) and with it every eval and advisory-digest post.
	cmd.WaitDelay = probeWaitDelay
	// The hive main process runs with HOME=/home/dev, but codex auth state
	// lives in the shared CLI home on the PVC (/data/home/.codex — the HOME
	// the manager gives agent sessions). Point the app-server there when it
	// exists so the probe sees the fleet's real login, not an empty home.
	if fi, err := os.Stat(sharedCLIHome); err == nil && fi.IsDir() {
		cmd.Env = append(os.Environ(), "HOME="+sharedCLIHome)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return failOpen(p.Provider(), err)
	}
	// Kill AND reap: without Wait the killed app-server stays a zombie for the
	// life of the hive process, one per probe cycle.
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	send := func(id int, method string, params any) error {
		msg := struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params any    `json:"params,omitempty"`
		}{ID: id, Method: method, Params: params}
		b, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(b, '\n'))
		return err
	}
	if err := send(0, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "hive-rotation", "title": "Hive Rotation", "version": "1.0"},
	}); err != nil {
		return failOpen(p.Provider(), err)
	}
	handshake := false
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m struct {
			ID     int              `json:"id"`
			Result json.RawMessage  `json:"result"`
			Error  *json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			continue // keepalives / config warnings that are not JSON objects
		}
		if m.Error != nil {
			return failOpen(p.Provider(), fmt.Errorf("codex app-server error: %s", string(*m.Error)))
		}
		if m.ID == 0 && !handshake {
			handshake = true
			if err := send(1, "account/rateLimits/read", map[string]any{}); err != nil {
				return failOpen(p.Provider(), err)
			}
			continue
		}
		if m.ID == 1 {
			h, err := codexHeadroom(p.Provider(), p.ThresholdPct, m.Result)
			if err != nil {
				return failOpen(p.Provider(), err)
			}
			return h
		}
	}
	if err := sc.Err(); err != nil {
		return failOpen(p.Provider(), err)
	}
	return failOpen(p.Provider(), errors.New("codex app-server: no rateLimits response"))
}

// codexHeadroom builds a normalized reading from a rateLimits payload
// (kubestellar/hive#6952).
//
// It reports an error rather than a permissive reading whenever the payload is
// not one it recognizes. A CLI too old to carry `rateLimits`, or a schema that
// has moved on, must surface as unknown: a guard that silently misreads is
// worse than no guard, because it manufactures confidence that Hive will stop
// in time.
func codexHeadroom(provider string, thresholdPct int, result json.RawMessage) (Headroom, error) {
	var res codexRateLimitsResult
	if err := json.Unmarshal(result, &res); err != nil {
		return Headroom{}, err
	}
	if res.RateLimits.Primary == nil && res.RateLimits.Secondary == nil && len(res.RateLimits.RateLimitsByLimitID) == 0 {
		return Headroom{}, errors.New("codex rateLimits: no primary, secondary, or rateLimitsByLimitId window (unrecognized schema)")
	}

	h := Headroom{Provider: provider, PlanType: res.RateLimits.PlanType, OrdinaryUsageAllowed: res.OrdinaryUsageAllowed}
	if res.RateLimits.Credits != nil {
		h.PaidCreditsAvailable = res.RateLimits.Credits.Available
	}
	windows := map[string]*codexRateLimitWindow{}
	if res.RateLimits.Primary != nil {
		windows["primary"] = res.RateLimits.Primary
	}
	if res.RateLimits.Secondary != nil {
		windows["secondary"] = res.RateLimits.Secondary
	}
	// Fold in every scoped limit from rateLimitsByLimitId (kubestellar/hive#6964)
	// so an exhausted scoped window binds too, but skip a key that just repeats
	// a positional window's own limitId — that is the same window reported twice,
	// not a second one.
	positional := map[string]bool{}
	if w := res.RateLimits.Primary; w != nil && w.LimitID != "" {
		positional[w.LimitID] = true
	}
	if w := res.RateLimits.Secondary; w != nil && w.LimitID != "" {
		positional[w.LimitID] = true
	}
	for id, w := range res.RateLimits.RateLimitsByLimitID {
		if w == nil || positional[id] {
			continue
		}
		windows[id] = w
	}
	for id, w := range windows {
		h.Limits = append(h.Limits, codexLimitWindow(id, w))
	}
	sort.Slice(h.Limits, func(i, j int) bool { return h.Limits[i].ID < h.Limits[j].ID })

	// The binding window is the most-used one. Reporting the headroom of the
	// roomier window would let an exhausted second window pass unnoticed, which
	// is the gap that made parsing `secondary` worth doing at all.
	worst := h.Limits[0]
	for _, w := range h.Limits[1:] {
		if w.PercentUsed > worst.PercentUsed {
			worst = w
		}
	}
	h.PctRemaining = worst.PctRemaining
	h.ResetAt = worst.ResetAt
	// ordinaryUsageAllowed is tri-state and only ever REMOVES headroom here
	// (kubestellar/hive#6952). An explicit false is the provider saying
	// ordinary usage is refused, and it overrides healthy-looking percentages.
	// Absent means the provider could not say, and #6833 requires that never be
	// read as recovery — so it grants nothing, and availability falls back to
	// the windows alone rather than being manufactured from a nil.
	h.Available = worst.PercentUsed < thresholdPct
	if res.OrdinaryUsageAllowed != nil && !*res.OrdinaryUsageAllowed {
		h.Available = false
	}
	return h, nil
}

// AgyProber probes Google (Agy) subscription usage via the CLI's documented
// status-line `quota` map.
//
// SOURCE (kubestellar/hive#6966): #6833's adapter table rules out scraping the
// decorative `agy --print "/usage"` text — an earlier prober matched a
// `Weekly Limit Remaining: N%` line with a regex, whose failure mode was a
// confident WRONG number rather than an error, and whose window carried no
// reset time. This adapter instead requests the structured status-line `quota`
// map (`remaining_fraction`, reset fields, plan tier) and normalizes it, so
// each window carries its reset timing and an unrecognized payload is reported
// as an explicit error rather than silently misread. Capability is detected by
// asking for the structured form: a CLI too old to emit it prints text or
// errors, and either way json parsing fails and the reading becomes unknown —
// no version-sniffing. Requesting usage sends no model prompt, so it consumes
// no model turn.
//
// PROVENANCE CAVEAT: the exact JSON shape agyHeadroom parses is derived from
// the field names #6833/#6966 document (`quota`, `remaining_fraction`, reset
// fields, plan tier), NOT from a live agy capture — agy 1.1.22 exposed no auth
// surface on the verifying host, so no real payload was obtainable. The
// reject-unrecognized-schema path is fully verified; the accepted-shape details
// (field nesting/spelling) must be validated against real agy output and the
// fixture replaced. See testdata/README.md.
type AgyProber struct {
	ThresholdPct int
}

// agyUsageResponse mirrors the subset of agy's structured `/usage` output this
// probe consumes (kubestellar/hive#6966). The `quota` map carries the plan tier
// and one entry per usage window; each window states its remaining fraction, a
// reset time, and (where present) its duration.
type agyUsageResponse struct {
	Quota *struct {
		Plan     string                     `json:"plan"`
		PlanTier string                     `json:"plan_tier"`
		Windows  map[string]*agyQuotaWindow `json:"windows"`
	} `json:"quota"`
}

type agyQuotaWindow struct {
	// RemainingFraction is 0..1; a pointer so "the field was absent" stays
	// distinguishable from a real 0.0 (fully exhausted).
	RemainingFraction *float64   `json:"remaining_fraction"`
	ResetAt           *time.Time `json:"reset_at"`
	ResetsAt          *time.Time `json:"resets_at"`
	DurationMins      int        `json:"duration_mins"`
}

func (p AgyProber) Provider() string { return "google" }

func (p AgyProber) Probe(ctx context.Context) Headroom {
	out, err := runCLI(ctx, "agy", "--print", "/usage", "--output-format", "json")
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	h, err := agyHeadroom(p.Provider(), p.ThresholdPct, []byte(out))
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	return h
}

// agyWindowDurationMins maps a documented agy window key to its duration so the
// reading carries #6833's duration member and the shared codexWindowKind
// banding applies (kubestellar/hive#6966). A key with no known duration yields
// zero, matching "the provider did not state one".
func agyWindowDurationMins(name string) int {
	switch name {
	case "session":
		return 60
	case "five_hour", "short":
		return 300
	case "daily":
		return 1440
	case "weekly", "seven_day":
		return 10080
	default:
		return 0
	}
}

// agyHeadroom builds a normalized reading from agy's structured `quota` map
// (kubestellar/hive#6966).
//
// Like codexHeadroom and claudeHeadroom it reports an error rather than a
// permissive reading whenever the payload carries no window it recognizes — a
// missing `quota` map, no windows, or windows with no `remaining_fraction`.
// That must surface as unknown so the caller enters the configured unknown-data
// behaviour: the earlier scraper's failure mode was a confident wrong number,
// which #6833 calls worse than no guard at all.
func agyHeadroom(provider string, thresholdPct int, body []byte) (Headroom, error) {
	var parsed agyUsageResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Headroom{}, err
	}
	if parsed.Quota == nil || len(parsed.Quota.Windows) == 0 {
		return Headroom{}, errors.New("agy usage: no quota windows (unrecognized schema)")
	}
	planType := parsed.Quota.Plan
	if planType == "" {
		planType = parsed.Quota.PlanTier
	}
	names := make([]string, 0, len(parsed.Quota.Windows))
	for name := range parsed.Quota.Windows {
		names = append(names, name)
	}
	sort.Strings(names)
	used := 0
	var resetAt time.Time
	limits := make([]LimitWindow, 0, len(names))
	for _, name := range names {
		w := parsed.Quota.Windows[name]
		if w == nil || w.RemainingFraction == nil {
			// A window with no remaining_fraction carries no usable reading; it
			// must not be counted as 0% used (fully available).
			continue
		}
		remainingPct := int(*w.RemainingFraction*fullPct + 0.5)
		if remainingPct < 0 {
			remainingPct = 0
		}
		if remainingPct > fullPct {
			remainingPct = fullPct
		}
		pctUsed := fullPct - remainingPct
		duration := w.DurationMins
		if duration == 0 {
			duration = agyWindowDurationMins(name)
		}
		kind := codexWindowKind(duration)
		if duration == 0 {
			// No duration to band; fall back to the provider's own window key
			// so the guard still evaluates it rather than dropping it.
			kind = name
		}
		lw := LimitWindow{
			ID:           name,
			Kind:         kind,
			PercentUsed:  pctUsed,
			PctRemaining: remainingPct,
			DurationMins: duration,
		}
		reset := w.ResetAt
		if reset == nil {
			reset = w.ResetsAt
		}
		if reset != nil {
			lw.ResetAt = *reset
		}
		limits = append(limits, lw)
		// The binding window is the most-used one; reporting a roomier window
		// would let an exhausted one pass unnoticed.
		if pctUsed > used {
			used = pctUsed
			if reset != nil {
				resetAt = *reset
			}
		}
	}
	if len(limits) == 0 {
		return Headroom{}, errors.New("agy usage: no quota window carried remaining_fraction (unrecognized schema)")
	}
	return Headroom{
		Provider:     provider,
		Available:    used < thresholdPct,
		PctRemaining: fullPct - used,
		ResetAt:      resetAt,
		PlanType:     planType,
		Limits:       limits,
	}, nil
}

// DeepSeekProber probes DeepSeek credit balance via its balance API.
type DeepSeekProber struct {
	APIKey string
	// BaseURL overrides the API endpoint (tests). Default production URL.
	BaseURL string
	// Client overrides the HTTP client (tests).
	Client *http.Client
}

// deepSeekBaseURL is the production balance endpoint host.
const deepSeekBaseURL = "https://api.deepseek.com"

func (p DeepSeekProber) Provider() string { return "deepseek" }

type deepSeekBalanceResponse struct {
	IsAvailable  bool   `json:"is_available"`
	TotalBalance string `json:"total_balance"`
	BalanceInfos []struct {
		TotalBalance string `json:"total_balance"`
	} `json:"balance_infos"`
}

func (p DeepSeekProber) Probe(ctx context.Context) Headroom {
	base := p.BaseURL
	if base == "" {
		base = deepSeekBaseURL
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: probeTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/user/balance", nil)
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := client.Do(req)
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return failOpen(p.Provider(), fmt.Errorf("deepseek balance HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	var parsed deepSeekBalanceResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return failOpen(p.Provider(), err)
	}
	balStr := parsed.TotalBalance
	if balStr == "" && len(parsed.BalanceInfos) > 0 {
		balStr = parsed.BalanceInfos[0].TotalBalance
	}
	bal, err := strconv.ParseFloat(strings.TrimSpace(balStr), 64)
	if err != nil {
		return failOpen(p.Provider(), fmt.Errorf("deepseek balance parse: %w", err))
	}
	available := parsed.IsAvailable && bal >= deepSeekMinBalanceUSD
	pct := 0
	if available {
		pct = fullPct
	}
	return Headroom{Provider: p.Provider(), Available: available, PctRemaining: pct}
}

// Manager runs the rotation loop: it polls provider headroom and answers
// "should this agent rotate, and where to?".
type Manager struct {
	cfg     config.RotationConfig
	probers []Prober

	mu       sync.RWMutex
	headroom map[string]Headroom
}

// NewManager builds a Manager with the default prober set for every provider
// named in cfg.Providers. Unknown provider names get no prober (their
// headroom stays unknown, which fails open).
func NewManager(cfg config.RotationConfig) *Manager {
	m := &Manager{
		cfg:      cfg,
		headroom: make(map[string]Headroom),
	}
	threshold := cfg.EffectiveThreshold()
	for name := range cfg.Providers {
		switch name {
		case "anthropic":
			m.probers = append(m.probers, ClaudeProber{ThresholdPct: threshold})
		case "openai":
			m.probers = append(m.probers, CodexProber{ThresholdPct: threshold})
		case "google":
			m.probers = append(m.probers, AgyProber{ThresholdPct: threshold})
		case "deepseek":
			m.probers = append(m.probers, DeepSeekProber{})
		}
	}
	return m
}

// SetProbers replaces the prober set (tests, custom deployments).
func (m *Manager) SetProbers(probers []Prober) {
	m.probers = probers
}

// Start begins the headroom polling loop. It probes once immediately and
// then every pollInterval until ctx is cancelled.
func (m *Manager) Start(ctx context.Context) {
	go func() {
		m.probeAll(ctx)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.probeAll(ctx)
			}
		}
	}()
}

func (m *Manager) probeAll(ctx context.Context) {
	for _, p := range m.probers {
		h := p.Probe(ctx)
		m.mu.Lock()
		m.headroom[p.Provider()] = h
		m.mu.Unlock()
	}
}

// SetHeadroom records a headroom observation directly (tests, external feeds).
func (m *Manager) SetHeadroom(h Headroom) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.headroom[h.Provider] = h
}

// HeadroomFor returns the last known headroom for a provider. An unknown
// provider reads as available with a "never probed" error — fail-open.
func (m *Manager) HeadroomFor(provider string) Headroom {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if h, ok := m.headroom[provider]; ok {
		return h
	}
	return failOpen(provider, fmt.Errorf("provider %q never probed", provider))
}

// providerForBackend maps a hive backend name to its rotation provider, ""
// when the backend is not covered by any configured provider.
func (m *Manager) providerForBackend(backend string) string {
	for name, pc := range m.cfg.Providers {
		for _, b := range pc.Backends {
			if b == backend {
				return name
			}
		}
	}
	return ""
}

// tierOf returns the configured capability tier for an agent, "" if untiered.
func (m *Manager) tierOf(agentName string) string {
	return m.cfg.AgentTiers[agentName]
}

// ShouldRotate returns true when an agent on currentBackend should be moved:
// its provider is exhausted (by a SUCCESSFUL probe — a failed measurement is
// never treated as exhaustion) AND at least one other suitable backend
// exists.
func (m *Manager) ShouldRotate(agentName, currentBackend string, cadenceS int) bool {
	provider := m.providerForBackend(currentBackend)
	if provider == "" {
		return false
	}
	h := m.HeadroomFor(provider)
	if h.ProbeErr != nil {
		// Never rotate on a failed measurement.
		return false
	}
	if h.Available {
		return false
	}
	return m.nextBackend(agentName, currentBackend, cadenceS) != ""
}

// Exhausted reports whether the provider fronting `backend` was positively
// measured as out of headroom. Used by the eval loop to decide between
// rotating (an alternative exists) and stranding (nothing has headroom).
func (m *Manager) Exhausted(backend string) bool {
	provider := m.providerForBackend(backend)
	if provider == "" {
		return false
	}
	h := m.HeadroomFor(provider)
	return h.ProbeErr == nil && !h.Available
}

// NextBackend returns the best backend to rotate agentName to, or "" if no
// suitable backend exists (strand the agent).
func (m *Manager) NextBackend(agentName, currentBackend string) string {
	return m.nextBackend(agentName, currentBackend, 0)
}

// NextBackendForCadence is NextBackend with the agent's cadence applied.
// High-volume agents avoid subscription providers during normal operation, but
// may use one as an availability failover when their current metered provider
// is positively measured exhausted.
func (m *Manager) NextBackendForCadence(agentName, currentBackend string, cadenceS int) string {
	return m.nextBackend(agentName, currentBackend, cadenceS)
}

func (m *Manager) nextBackend(agentName, currentBackend string, cadenceS int) string {
	currentProvider := m.providerForBackend(currentBackend)
	highVolume := cadenceS > 0 && cadenceS <= m.cfg.EffectiveHighVolumeCadenceS()

	// A high-cadence agent normally must not consume a subscription pool: it
	// can exhaust a weekly allowance and take the operator's own CLI down with
	// it. But stranding that same agent after a positively measured prepaid
	// provider exhaustion is worse: the available subscription alternatives are
	// exactly the failover path. This exception is deliberately narrow:
	// metered current provider, successful probe, and unavailable headroom.
	// Probe errors remain fail-open and never trigger a rotation.
	allowSubscriptionFailover := false
	if current, ok := m.cfg.Providers[currentProvider]; ok && current.Class == ClassMetered {
		h := m.HeadroomFor(currentProvider)
		allowSubscriptionFailover = h.ProbeErr == nil && !h.Available
	}

	type candidate struct {
		provider string
		backend  string
		pct      int
	}
	var candidates []candidate
	names := make([]string, 0, len(m.cfg.Providers))
	for name := range m.cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == currentProvider {
			continue
		}
		pc := m.cfg.Providers[name]
		if len(pc.Backends) == 0 {
			continue
		}
		if highVolume && pc.Class == ClassSubscription && !allowSubscriptionFailover {
			// Preserve the subscription guard unless a confirmed exhausted
			// metered provider leaves this high-volume agent without service.
			continue
		}
		h := m.HeadroomFor(name)
		// Only rotate ONTO a provider whose headroom was positively
		// measured: never place new load on a fail-open (probe error) target.
		if h.ProbeErr != nil || !h.Available {
			continue
		}
		candidates = append(candidates, candidate{provider: name, backend: pc.Backends[0], pct: h.PctRemaining})
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].pct > candidates[j].pct })
	// Capability tiers: rotation stays sideways. All configured providers
	// currently serve every tier, so the tier lookup is retained here as the
	// single seam for future per-tier provider sets.
	_ = m.tierOf(agentName)
	return candidates[0].backend
}

// StrandRecovered reports whether the provider fronting `backend` has
// recovered headroom (a successful probe reporting available) — used to
// auto-resume stranded agents.
func (m *Manager) StrandRecovered(backend string) bool {
	provider := m.providerForBackend(backend)
	if provider == "" {
		return false
	}
	h := m.HeadroomFor(provider)
	return h.ProbeErr == nil && h.Available
}

// HeadroomResponse is the GET /api/providers/headroom payload.
type HeadroomResponse struct {
	Providers []Headroom `json:"providers"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// HeadroomResponse snapshots the last known headroom for every provider.
func (m *Manager) HeadroomResponse() HeadroomResponse {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.headroom))
	for name := range m.headroom {
		names = append(names, name)
	}
	sort.Strings(names)
	providers := make([]Headroom, 0, len(names))
	for _, name := range names {
		providers = append(providers, m.headroom[name])
	}
	return HeadroomResponse{Providers: providers, UpdatedAt: time.Now()}
}
