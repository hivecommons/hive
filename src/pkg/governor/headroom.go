package governor

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProviderClass classifies the billing and limit enforcement model of an LLM provider.
//
// RFC #3958 (§2) establishes that cooldowns and rotation logic must treat classes
// differently:
//   - Subscription pools (Anthropic, OpenAI) have a hard account-wide weekly quota.
//     High-cadence agents must not drain subscription allowances.
//   - Metered pools (DeepSeek) charge per token with credit balances; they do not
//     block unless balance reaches zero, but running hot incurs real monetary cost.
//   - Unmeasurable providers (Google agy) lack programmatic headroom probes and
//     serve as a last-resort rung.
type ProviderClass string

const (
	ProviderClassSubscription ProviderClass = "subscription"
	ProviderClassMetered      ProviderClass = "metered"
	ProviderClassUnmeasurable ProviderClass = "unmeasurable"
)

// ProviderHeadroom captures the latest measured headroom status for a provider.
type ProviderHeadroom struct {
	Provider     string        `json:"provider"`
	Class        ProviderClass `json:"class"`
	Available    bool          `json:"available"`
	UsagePct     *float64      `json:"usage_pct,omitempty"`
	TotalBalance string        `json:"total_balance,omitempty"`
	LimitResetAt *time.Time    `json:"limit_reset_at,omitempty"`
	RawStatus    string        `json:"raw_status,omitempty"`
	ProbedAt     time.Time     `json:"probed_at"`
	Error        string        `json:"error,omitempty"`
}

// StrandRecord records when an agent is stranded due to provider exhaustion
// instead of silently stalling (RFC #3958 §5/§6).
type StrandRecord struct {
	Agent       string    `json:"agent"`
	Provider    string    `json:"provider"`
	Backend     string    `json:"backend"`
	Timestamp   time.Time `json:"timestamp"`
	Reason      string    `json:"reason"`
	CooldownEnd time.Time `json:"cooldown_end,omitempty"`
	AutoResume  bool      `json:"auto_resume"`
}

// BackendProvider maps a backend name to its canonical upstream LLM provider slug.
// Mirrors backend_provider() in config/backends.conf (#3958).
func BackendProvider(backend string) string {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "claude", "litellm":
		return "anthropic"
	case "copilot", "codex":
		return "openai"
	case "agy":
		return "google"
	case "bob":
		return "ibm"
	case "deepseek":
		return "deepseek"
	case "goose":
		return "goose"
	case "pi":
		return "pi"
	case "aider":
		return "aider"
	default:
		return "unknown"
	}
}

// DefaultProviderClass returns the default ProviderClass for a given provider slug.
func DefaultProviderClass(provider string) ProviderClass {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "openai":
		return ProviderClassSubscription
	case "deepseek":
		return ProviderClassMetered
	case "google", "ibm", "goose", "pi", "aider":
		return ProviderClassUnmeasurable
	default:
		return ProviderClassUnmeasurable
	}
}

var (
	// Anthropic /usage probe regexes:
	// OPERATIONAL FINDING (#3958): Claude /usage prints multiple blocks (e.g. per-model
	// sub-limits). Only "Current week (all models)" is the binding limit for the account;
	// a loose regex that matches "Sonnet 50%" will under-report account-level exhaustion.
	reClaudeAllModels = regexp.MustCompile(`(?i)Current\s+week\s*\(all\s+models\)\s*:\s*([0-9]+(?:\.[0-9]+)?)\s*%\s*used`)
	reClaudeGeneric   = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*%\s*used`)

	// OpenAI /status probe regexes:
	// OpenAI Codex status prints e.g. "Weekly limit: 88% left (resets 23:45 on 19 Aug)"
	// or "Weekly limit: 12% used".
	reOpenAILeft = regexp.MustCompile(`(?i)Weekly\s+limit\s*:\s*([0-9]+(?:\.[0-9]+)?)\s*%\s*left`)
	reOpenAIUsed = regexp.MustCompile(`(?i)Weekly\s+limit\s*:\s*([0-9]+(?:\.[0-9]+)?)\s*%\s*used`)
)

type deepseekBalanceResponse struct {
	IsAvailable  bool   `json:"is_available"`
	TotalBalance string `json:"total_balance"`
	Error        string `json:"error,omitempty"`
}

// ParseHeadroomOutput parses the raw probe output for a given provider.
//
// Operational notes from live fleet experience (RFC #3958):
//  1. DeepSeek: Authoritative structured endpoint (GET api.deepseek.com/user/balance).
//     Returns json with is_available and total_balance.
//  2. Anthropic: Scrapes output of /usage command. Must prioritize "Current week (all models)"
//     over sub-model sections. Dismiss overlay via Escape in tmux sessions.
//  3. OpenAI: Scrapes output of /status command (weekly limit left/used).
//  4. Google (agy): Unmeasurable, treated as last resort.
func ParseHeadroomOutput(provider string, rawOutput string) (*ProviderHeadroom, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := time.Now()

	hr := &ProviderHeadroom{
		Provider:  provider,
		Class:     DefaultProviderClass(provider),
		Available: true,
		ProbedAt:  now,
		RawStatus: strings.TrimSpace(rawOutput),
	}

	switch provider {
	case "deepseek":
		// Level 1: Structured JSON response
		var ds deepseekBalanceResponse
		if err := json.Unmarshal([]byte(rawOutput), &ds); err != nil {
			hr.Available = false
			hr.Error = fmt.Sprintf("invalid deepseek balance JSON: %v", err)
			return hr, err
		}
		hr.Available = ds.IsAvailable
		hr.TotalBalance = ds.TotalBalance
		if !ds.IsAvailable {
			hr.Error = "deepseek account unavailable or credit balance exhausted"
		}
		return hr, nil

	case "anthropic":
		// Level 2: Interactive CLI /usage output
		if match := reClaudeAllModels.FindStringSubmatch(rawOutput); len(match) > 1 {
			pct, err := strconv.ParseFloat(match[1], 64)
			if err == nil {
				hr.UsagePct = &pct
				if pct >= 100.0 {
					hr.Available = false
				}
				return hr, nil
			}
		}
		if match := reClaudeGeneric.FindStringSubmatch(rawOutput); len(match) > 1 {
			pct, err := strconv.ParseFloat(match[1], 64)
			if err == nil {
				hr.UsagePct = &pct
				if pct >= 100.0 {
					hr.Available = false
				}
				return hr, nil
			}
		}
		if strings.Contains(strings.ToLower(rawOutput), "rate limit exceeded") ||
			strings.Contains(strings.ToLower(rawOutput), "usage limit reached") {
			hr.Available = false
			p100 := 100.0
			hr.UsagePct = &p100
			return hr, nil
		}
		hr.Error = "unable to parse anthropic /usage output"
		return hr, fmt.Errorf("unable to parse anthropic /usage output: %q", rawOutput)

	case "openai":
		// Level 2: Interactive CLI /status output
		if match := reOpenAILeft.FindStringSubmatch(rawOutput); len(match) > 1 {
			leftPct, err := strconv.ParseFloat(match[1], 64)
			if err == nil {
				usedPct := 100.0 - leftPct
				if usedPct < 0 {
					usedPct = 0
				}
				hr.UsagePct = &usedPct
				if leftPct <= 0 {
					hr.Available = false
				}
				return hr, nil
			}
		}
		if match := reOpenAIUsed.FindStringSubmatch(rawOutput); len(match) > 1 {
			usedPct, err := strconv.ParseFloat(match[1], 64)
			if err == nil {
				hr.UsagePct = &usedPct
				if usedPct >= 100.0 {
					hr.Available = false
				}
				return hr, nil
			}
		}
		hr.Error = "unable to parse openai /status output"
		return hr, fmt.Errorf("unable to parse openai /status output: %q", rawOutput)

	case "google", "ibm":
		// Level 3: Unmeasurable providers
		hr.Class = ProviderClassUnmeasurable
		return hr, nil

	default:
		hr.Class = ProviderClassUnmeasurable
		return hr, nil
	}
}

// HeadroomTracker maintains headroom probe observations and strand records.
type HeadroomTracker struct {
	mu       sync.RWMutex
	headroom map[string]ProviderHeadroom
	strands  map[string]StrandRecord
}

// NewHeadroomTracker creates an initialized HeadroomTracker.
func NewHeadroomTracker() *HeadroomTracker {
	return &HeadroomTracker{
		headroom: make(map[string]ProviderHeadroom),
		strands:  make(map[string]StrandRecord),
	}
}

// RecordHeadroom saves a probed headroom snapshot for a provider.
func (t *HeadroomTracker) RecordHeadroom(hr ProviderHeadroom) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.headroom[hr.Provider] = hr
}

// GetHeadroom returns the latest headroom snapshot for a provider.
func (t *HeadroomTracker) GetHeadroom(provider string) (ProviderHeadroom, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	hr, ok := t.headroom[strings.ToLower(provider)]
	return hr, ok
}

// AllHeadroom returns all currently recorded provider headroom snapshots.
func (t *HeadroomTracker) AllHeadroom() []ProviderHeadroom {
	t.mu.RLock()
	defer t.mu.RUnlock()
	res := make([]ProviderHeadroom, 0, len(t.headroom))
	for _, hr := range t.headroom {
		res = append(res, hr)
	}
	return res
}

// IsExhausted checks if a provider is unavailable or exceeded thresholdPct usage.
func (t *HeadroomTracker) IsExhausted(provider string, thresholdPct float64) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	hr, ok := t.headroom[strings.ToLower(provider)]
	if !ok {
		return false
	}
	if !hr.Available {
		return true
	}
	if hr.UsagePct != nil && thresholdPct > 0 && *hr.UsagePct >= thresholdPct {
		return true
	}
	return false
}

// RecordStrand records an agent stranding event in the strand journal.
func (t *HeadroomTracker) RecordStrand(sr StrandRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.strands[sr.Agent] = sr
}

// ActiveStrands returns all recorded stranded agent records.
func (t *HeadroomTracker) ActiveStrands() []StrandRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()
	res := make([]StrandRecord, 0, len(t.strands))
	for _, s := range t.strands {
		res = append(res, s)
	}
	return res
}

// ClearStrand removes an agent's strand record upon successful rotation or recovery.
func (t *HeadroomTracker) ClearStrand(agent string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.strands, agent)
}
