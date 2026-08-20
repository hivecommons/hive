// Package rotation implements automatic provider failover for hive agents.
// When a provider's subscription/credit is exhausted, it moves agents to a
// different provider at the same capability tier. See RFC #3958.
package rotation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

// probeTimeout bounds each CLI/HTTP headroom probe.
const probeTimeout = 10 * time.Second

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
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
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

// ClaudeProber probes Anthropic subscription usage via `claude /usage`.
type ClaudeProber struct {
	ThresholdPct int
}

var claudeUsageRe = regexp.MustCompile(`Current week \(all models\)[^\d]*(\d+)%`)

func (p ClaudeProber) Provider() string { return "anthropic" }

func (p ClaudeProber) Probe(ctx context.Context) Headroom {
	out, err := runCLI(ctx, "claude", "/usage", "--output-format", "text")
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	m := claudeUsageRe.FindStringSubmatch(out)
	if m == nil {
		return failOpen(p.Provider(), fmt.Errorf("claude usage output did not match"))
	}
	used, _ := strconv.Atoi(m[1])
	return Headroom{
		Provider:     p.Provider(),
		Available:    used < p.ThresholdPct,
		PctRemaining: fullPct - used,
	}
}

// CodexProber probes OpenAI subscription usage via `codex /status`.
type CodexProber struct {
	ThresholdPct int
}

var codexWeeklyRe = regexp.MustCompile(`Weekly limit:[^\d]*(\d+)%`)

func (p CodexProber) Provider() string { return "openai" }

func (p CodexProber) Probe(ctx context.Context) Headroom {
	out, err := runCLI(ctx, "codex", "/status", "--output-format", "text")
	if err != nil {
		return failOpen(p.Provider(), err)
	}
	m := codexWeeklyRe.FindStringSubmatch(out)
	if m == nil {
		return failOpen(p.Provider(), fmt.Errorf("codex status output did not match"))
	}
	remaining, _ := strconv.Atoi(m[1])
	return Headroom{
		Provider:     p.Provider(),
		Available:    fullPct-remaining < p.ThresholdPct,
		PctRemaining: remaining,
	}
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
	defer resp.Body.Close()
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

// NextBackendForCadence is NextBackend with the agent's cadence applied, so
// high-volume agents are never offered a subscription provider.
func (m *Manager) NextBackendForCadence(agentName, currentBackend string, cadenceS int) string {
	return m.nextBackend(agentName, currentBackend, cadenceS)
}

func (m *Manager) nextBackend(agentName, currentBackend string, cadenceS int) string {
	currentProvider := m.providerForBackend(currentBackend)
	highVolume := cadenceS > 0 && cadenceS <= m.cfg.EffectiveHighVolumeCadenceS()

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
		if highVolume && pc.Class == ClassSubscription {
			// High-volume agents must never land on subscription providers.
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
