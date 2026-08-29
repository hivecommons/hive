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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/pkg/claude"
	"github.com/kubestellar/hive/pkg/config"
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

// Headroom describes a provider's current capacity.
type Headroom struct {
	Provider     string    `json:"provider"`
	Available    bool      `json:"available"`     // false = exhausted or probe failed
	PctRemaining int       `json:"pct_remaining"` // 0–100; 0 when probe failed
	ResetAt      time.Time `json:"reset_at,omitempty"`
	ProbeErr     error     `json:"-"` // non-nil = measurement failed (NOT treated as exhausted)
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
		Provider     string    `json:"provider"`
		Available    bool      `json:"available"`
		PctRemaining int       `json:"pct_remaining"`
		ResetAt      time.Time `json:"reset_at,omitempty"`
		ProbeErr     string    `json:"probe_error,omitempty"`
	}
	return json.Marshal(alias{
		Provider:     h.Provider,
		Available:    h.Available,
		PctRemaining: h.PctRemaining,
		ResetAt:      h.ResetAt,
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

type claudeCredentials struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
	} `json:"claudeAiOauth"`
}

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
	var creds claudeCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return failOpen(p.Provider(), fmt.Errorf("claude credentials parse: %w", err))
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		// An empty token is the signature of an expired OAuth session; the
		// CLI reports "Login expired" and serves nothing. Not exhaustion.
		return failOpen(p.Provider(), errors.New("claude credentials: empty accessToken"))
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
	req.Header.Set("Authorization", "Bearer "+creds.ClaudeAiOauth.AccessToken)
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
	var parsed claudeUsageResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return failOpen(p.Provider(), err)
	}
	used := 0
	var resetAt time.Time
	for _, l := range parsed.Limits {
		if l.Percent == nil {
			continue
		}
		pct := int(*l.Percent)
		if pct > used {
			used = pct
			if l.ResetsAt != nil {
				resetAt = *l.ResetsAt
			}
		}
	}
	return Headroom{
		Provider:     p.Provider(),
		Available:    used < p.ThresholdPct,
		PctRemaining: fullPct - used,
		ResetAt:      resetAt,
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

type codexRateLimitsResult struct {
	RateLimits struct {
		Primary struct {
			UsedPercent int   `json:"usedPercent"`
			ResetsAt    int64 `json:"resetsAt"`
		} `json:"primary"`
	} `json:"rateLimits"`
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
			used, resetAt, err := parseCodexRateLimits(m.Result)
			if err != nil {
				return failOpen(p.Provider(), err)
			}
			return Headroom{
				Provider:     p.Provider(),
				Available:    used < p.ThresholdPct,
				PctRemaining: fullPct - used,
				ResetAt:      resetAt,
			}
		}
	}
	if err := sc.Err(); err != nil {
		return failOpen(p.Provider(), err)
	}
	return failOpen(p.Provider(), errors.New("codex app-server: no rateLimits response"))
}

func parseCodexRateLimits(result json.RawMessage) (usedPct int, resetAt time.Time, err error) {
	var res codexRateLimitsResult
	if err := json.Unmarshal(result, &res); err != nil {
		return 0, time.Time{}, err
	}
	return res.RateLimits.Primary.UsedPercent,
		time.Unix(res.RateLimits.Primary.ResetsAt, 0).UTC(), nil
}

// AgyProber probes Google usage via `agy --print "/usage"`.
type AgyProber struct {
	ThresholdPct int
}

var agyWeeklyRe = regexp.MustCompile(`Weekly Limit Remaining[^\d]*(\d+)%`)

func (p AgyProber) Provider() string { return "google" }

func (p AgyProber) Probe(ctx context.Context) Headroom {
	out, err := runCLI(ctx, "agy", "--print", "/usage", "--output-format", "text")
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	m := agyWeeklyRe.FindStringSubmatch(out)
	if m == nil {
		return failOpen(p.Provider(), fmt.Errorf("agy usage output did not match"))
	}
	remaining, _ := strconv.Atoi(m[1])
	return Headroom{
		Provider:     p.Provider(),
		Available:    fullPct-remaining < p.ThresholdPct,
		PctRemaining: remaining,
	}
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
