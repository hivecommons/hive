package dashboard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/tokens"
)

const (
	// costProbeTimeout bounds each native-cost HTTP probe. Kept short so a slow
	// or unreachable metered gateway can't stall the /api/cost response — on
	// timeout the gateway just contributes no native figure.
	costProbeTimeout = 6 * time.Second

	// costProbeMaxBody caps how much of a gateway response body is read, so a
	// misbehaving endpoint can't stream an unbounded body into memory.
	costProbeMaxBody = 64 * 1024

	// openRouterKeyPath is OpenRouter's credit/usage endpoint. Appended to the
	// gateway's OpenAI-compatible base URL (…/api/v1) to form …/api/v1/key.
	openRouterKeyPath = "/key"

	// litellmKeyInfoPath is the LiteLLM proxy endpoint that reports the calling
	// key's accumulated spend and (optional) max budget.
	litellmKeyInfoPath = "/key/info"
)

// gatewayCost is the per-gateway native-cost entry in the /api/cost response.
// Source is always "native" for these; fields that a given backend does not
// report are left nil so the UI can distinguish "unknown" from zero.
type gatewayCost struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Source    string   `json:"source"` // always "native" here
	SpentUSD  *float64 `json:"spent_usd,omitempty"`
	LimitUSD  *float64 `json:"limit_usd,omitempty"`
	RemainUSD *float64 `json:"remaining_usd,omitempty"`
	// Error, when set, explains why a native figure could not be fetched (the
	// gateway is still listed so the UI can show it fell back to estimate). The
	// message is already secret-redacted.
	Error string `json:"error,omitempty"`
}

// costResponse is the full /api/cost payload.
type costResponse struct {
	// Estimated cost derived from token counts × list prices.
	Estimated      costEstimated `json:"estimated"`
	Coins          float64       `json:"coins,omitempty"`
	CoinLabel      string        `json:"coin_label,omitempty"`
	CoinBudget     *float64      `json:"coin_budget,omitempty"`
	CoinsRemaining *float64      `json:"coins_remaining,omitempty"`
	CoinUSD        float64       `json:"coin_usd,omitempty"`
	CoinBudgetUSD  *float64      `json:"coin_budget_usd,omitempty"`
	USDBudget      *float64      `json:"usd_budget,omitempty"`
	USDRemaining   *float64      `json:"usd_remaining,omitempty"`
	USDBudgetCoins *float64      `json:"usd_budget_coins,omitempty"`
	// Native per-gateway spend where a metered backend reports it.
	Gateways []gatewayCost `json:"gateways"`
	// PriceTableDate / disclaimer travel with the payload so the UI can label
	// the estimate without hardcoding the date.
	PriceTableDate string `json:"price_table_date"`
	Disclaimer     string `json:"disclaimer"`
	// MergedPRs / ClosedIssues are hive-attributed counts for the primary repo
	// over the same persisted cost-history window the UI uses as the numerator.
	// The UI divides the displayed estimate by these counts to derive
	// cost-per-PR / cost-per-issue (issue #4110).
	// Zero means "no data yet" (collector hasn't run, GitHub is unreachable, or
	// no configured hive author has outcomes); the UI shows "—" rather than
	// treating it as a real zero denominator.
	MergedPRs        int    `json:"merged_prs"`
	ClosedIssues     int    `json:"closed_issues"`
	CountAuthor      string `json:"count_author,omitempty"`
	CountBasis       string `json:"count_basis,omitempty"`
	CountWindowStart int64  `json:"count_window_start,omitempty"`
	CountWindowEnd   int64  `json:"count_window_end,omitempty"`
	CountUpdatedAt   string `json:"count_updated_at,omitempty"`
	CountStatus      string `json:"count_status,omitempty"`
	CountStale       bool   `json:"count_stale,omitempty"`
	CountAvailable   bool   `json:"count_available"`
}

// costModelEntry is one row of the estimated per-model / per-agent breakdown.
// Source is "estimated" for priced models and "unpriced" for models absent
// from the price table (USD 0, UI shows "—").
type costModelEntry struct {
	Name        string  `json:"name"`
	USD         float64 `json:"usd"`
	Coins       float64 `json:"coins,omitempty"`
	Source      string  `json:"source"` // "estimated" | "unpriced"
	Input       int64   `json:"input"`
	Output      int64   `json:"output"`
	CacheRead   int64   `json:"cache_read"`
	CacheCreate int64   `json:"cache_create"`
}

type costEstimated struct {
	TotalUSD       float64          `json:"total_usd"`
	Coins          float64          `json:"coins,omitempty"`
	CoinLabel      string           `json:"coin_label,omitempty"`
	CoinBudget     *float64         `json:"coin_budget,omitempty"`
	CoinsRemaining *float64         `json:"coins_remaining,omitempty"`
	CoinUSD        float64          `json:"coin_usd,omitempty"`
	CoinBudgetUSD  *float64         `json:"coin_budget_usd,omitempty"`
	USDBudget      *float64         `json:"usd_budget,omitempty"`
	USDRemaining   *float64         `json:"usd_remaining,omitempty"`
	USDBudgetCoins *float64         `json:"usd_budget_coins,omitempty"`
	ByModel        []costModelEntry `json:"by_model"`
	ByAgent        []costModelEntry `json:"by_agent"`
	UnpricedModels []string         `json:"unpriced_models"`
	// WindowStart / WindowEnd bracket the token sessions represented by the
	// aggregate rows. They let the UI label "usage by sandbox" with the actual
	// observed span instead of leaving readers to guess.
	WindowStart int64 `json:"window_start,omitempty"`
	WindowEnd   int64 `json:"window_end,omitempty"`
	// BySession is the estimated cost for each individual agent session
	// (sandbox run), letting the UI show "usage by sandbox and cost per
	// session" rather than only per-model/per-agent aggregates. Each entry is
	// priced from its own token splits × list price, exactly like the
	// aggregate rows. Omitted (empty slice) when no session data is available.
	BySession []costSessionEntry `json:"by_session"`
}

// costSessionEntry is one session's estimated cost. A "session" is one agent
// run inside its sandbox, keyed by SessionID; Agent identifies which sandbox
// (agent) owned it and Model the model it used. Source is "estimated" for
// priced models and "unpriced" for models absent from the price table.
type costSessionEntry struct {
	SessionID string  `json:"session_id"`
	Agent     string  `json:"agent"`
	Model     string  `json:"model"`
	USD       float64 `json:"usd"`
	Coins     float64 `json:"coins,omitempty"`
	Source    string  `json:"source"` // "estimated" | "unpriced"
	Input     int64   `json:"input"`
	Output    int64   `json:"output"`
	CacheRead int64   `json:"cache_read"`
	// Messages and Started/LastActive give the UI enough to show session size
	// and timing without a second round-trip. Started is the earliest event
	// timestamp and LastActive the latest, both unix-milliseconds stamps
	// (0 when the scanner could not determine them).
	Messages   int   `json:"messages"`
	Started    int64 `json:"started,omitempty"`
	LastActive int64 `json:"last_active,omitempty"`
}

// maxCostSessions caps how many per-session rows the /api/cost payload carries,
// so a long-lived fleet with thousands of scanned sessions can't bloat the
// response. The newest-started sessions are kept, matching the Cost page's
// default session ordering.
const maxCostSessions = 200

// costEstimateDisclaimer moved to pkg/dashboard/collect (the repo-cost join
// stamps the identical sentence); aliased so every payload keeps one source.
const costEstimateDisclaimer = collect.CostEstimateDisclaimer

// handleCost serves GET /api/cost — the unified estimated + native cost view.
func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	resp := costResponse{
		PriceTableDate: tokens.PriceTableDate(),
		Disclaimer:     costEstimateDisclaimer,
		Gateways:       []gatewayCost{},
	}

	// --- Estimated cost from token counts ---
	resp.Estimated = s.estimatedCost()
	resp.Coins = resp.Estimated.Coins
	resp.CoinLabel = resp.Estimated.CoinLabel
	resp.CoinBudget = resp.Estimated.CoinBudget
	resp.CoinsRemaining = resp.Estimated.CoinsRemaining
	resp.CoinUSD = resp.Estimated.CoinUSD
	resp.CoinBudgetUSD = resp.Estimated.CoinBudgetUSD
	resp.USDBudget = resp.Estimated.USDBudget
	resp.USDRemaining = resp.Estimated.USDRemaining
	resp.USDBudgetCoins = resp.Estimated.USDBudgetCoins

	// --- Merged-PR / closed-issue counts (for cost-per-PR / cost-per-issue) ---
	if counts := s.costOutcomeCounts(); counts != nil {
		resp.CountAvailable = true
		resp.MergedPRs = counts.MergedPRs
		resp.ClosedIssues = counts.ClosedIssues
		resp.CountAuthor = counts.Author
		resp.CountBasis = counts.Basis
		resp.CountUpdatedAt = counts.UpdatedAt
		resp.CountStatus = counts.Status
		resp.CountStale = counts.Stale
		if counts.WindowStart != "" {
			if t, err := time.Parse(time.RFC3339, counts.WindowStart); err == nil {
				resp.CountWindowStart = t.UnixMilli()
			}
		}
	} else {
		resp.CountStatus = "counts unavailable (GitHub rate-limited)"
	}
	if !resp.CountAvailable && resp.CountWindowStart == 0 {
		resp.CountWindowStart = s.costCountWindowStart()
	}
	resp.CountWindowEnd = time.Now().UnixMilli()

	// --- Native cost per metered gateway ---
	if s.deps != nil && s.deps.Config != nil {
		for _, gw := range s.deps.Config.Governor.ResolvedGateways() {
			switch gw.Kind {
			case config.GatewayKindOpenRouter:
				resp.Gateways = append(resp.Gateways, s.openRouterCost(r.Context(), gw))
			case config.GatewayKindLiteLLM:
				resp.Gateways = append(resp.Gateways, s.liteLLMCost(r.Context(), gw))
			}
			// vLLM / llm-d / custom report no native spend; the estimate covers them.
		}
	}

	jsonResponse(w, resp)
}

func (s *Server) costOutcomeCounts() *ghpkg.PRIssueCounts {
	if s != nil && s.deps != nil && s.deps.MetricsCollector != nil {
		if counts := s.deps.MetricsCollector.GetPRIssueCounts(); prIssueCountsTotal(counts) > 0 && !counts.Stale {
			return counts
		}
	}
	if s != nil && s.deps != nil && s.deps.MetricsCollector != nil {
		if counts := s.deps.MetricsCollector.GetPRIssueCounts(); prIssueCountsTotal(counts) > 0 {
			return counts
		}
	}
	if counts := s.persistedCostOutcomeCounts(); prIssueCountsTotal(counts) > 0 {
		return counts
	}
	return nil
}

func (s *Server) persistedCostOutcomeCounts() *ghpkg.PRIssueCounts {
	if s == nil || s.audit == nil {
		return nil
	}
	c := s.audit.PRThroughputCounters()
	merged, closed, since := s.persistedCostOutcomeCountsForConfiguredRepos(c)
	if merged+closed == 0 {
		return nil
	}
	author := ""
	if s.deps != nil && s.deps.Config != nil {
		author = s.deps.Config.EffectiveAIAuthor()
	}
	return &ghpkg.PRIssueCounts{
		MergedPRs:    merged,
		ClosedIssues: closed,
		UpdatedAt:    since,
		Author:       author,
		Basis:        "persisted-pr-throughput",
		WindowStart:  since,
		Status:       "persisted",
		Stale:        true,
	}
}

func (s *Server) persistedCostOutcomeCountsForConfiguredRepos(c PRThroughputCounters) (merged, closed int, since string) {
	repos := s.costConfiguredRepos()
	if len(repos) == 0 || len(c.ByRepo) == 0 {
		return actorMatrixCount(c.ByActor, prThroughputKindPR, prThroughputRoleMerged, prThroughputActorHive),
			actorMatrixCount(c.ByActor, prThroughputKindIssue, prThroughputRoleClosed, prThroughputActorHive),
			c.Since
	}
	for _, repo := range repos {
		rc := c.forRepo(repo)
		merged += actorMatrixCount(rc.ByActor, prThroughputKindPR, prThroughputRoleMerged, prThroughputActorHive)
		closed += actorMatrixCount(rc.ByActor, prThroughputKindIssue, prThroughputRoleClosed, prThroughputActorHive)
		if since == "" || (rc.Since != "" && rc.Since < since) {
			since = rc.Since
		}
	}
	return merged, closed, since
}

func (s *Server) costConfiguredRepos() []string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return nil
	}
	project := s.deps.Config.Project
	seen := map[string]bool{}
	var out []string
	for _, repo := range project.Repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if !strings.Contains(repo, "/") && project.Org != "" {
			repo = config.QualifyRepo(project.Org, repo)
		}
		key := strings.ToLower(repo)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, repo)
	}
	return out
}

func actorMatrixCount(m PRThroughputActorMatrix, kind, role, actor string) int {
	if m == nil || m[kind] == nil || m[kind][role] == nil {
		return 0
	}
	return m[kind][role][actor]
}

func (s *Server) costCountWindowStart() int64 {
	history := s.CostHistory()
	if len(history) > 0 {
		return history[0].Timestamp
	}
	if est := s.estimatedCost(); est.WindowStart > 0 {
		return est.WindowStart
	}
	return 0
}

// estimatedCost computes the estimated per-model / per-agent breakdown (and the
// all-time cumulative total) from the current token summary. Factored out so
// both the /api/cost handler and the cost-history sampler produce the exact same
// figure. Returns a zero-value costEstimated (with non-nil slices) when no token
// data is available.
func (s *Server) estimatedCost() costEstimated {
	var est costEstimated
	if s.deps != nil && s.deps.Tokens != nil {
		if summary := s.deps.Tokens.Summary(); summary != nil {
			est = flattenEstimated(tokens.EstimateFromSummary(summary))
			est.BySession = estimatedSessions(summary)
			est.WindowStart, est.WindowEnd = estimatedWindow(summary)
			s.applyCoinEstimate(summary, &est)
		}
	}
	if est.ByModel == nil {
		est.ByModel = []costModelEntry{}
	}

	if est.ByAgent == nil {
		est.ByAgent = []costModelEntry{}
	}
	if est.UnpricedModels == nil {
		est.UnpricedModels = []string{}
	}
	if est.BySession == nil {
		est.BySession = []costSessionEntry{}
	}
	return est
}

func (s *Server) applyCoinEstimate(summary *tokens.AggregateSummary, est *costEstimated) {
	if summary == nil || est == nil || s.deps == nil || s.deps.Config == nil {
		return
	}
	coinCfg, ok := s.deps.Config.Governor.Budget.CoinConfig(tokens.BackendBob)
	if !ok {
		return
	}
	byAgent := map[string]float64{}
	for _, sess := range summary.Sessions {
		if !strings.EqualFold(sess.Backend, tokens.BackendBob) {
			continue
		}
		coins := coinCfg.TokensToCoins(sess.TotalTokens)
		est.Coins += coins
		byAgent[sess.Agent] += coins
		for i := range est.BySession {
			if est.BySession[i].SessionID == sess.SessionID {
				est.BySession[i].Coins = coins
				break
			}
		}
	}
	est.CoinLabel = coinCfg.LabelOrDefault()
	est.CoinUSD = coinCfg.CoinsToUSD(est.Coins)
	if coinCfg.Budget > 0 {
		budget := coinCfg.Budget
		remaining := budget - est.Coins
		if remaining < 0 {
			remaining = 0
		}
		est.CoinBudget = &budget
		est.CoinsRemaining = &remaining
		budgetUSD := coinCfg.CoinsToUSD(budget)
		est.CoinBudgetUSD = &budgetUSD
	}
	if s.deps.Config.Governor.Budget.USD > 0 {
		budget := s.deps.Config.Governor.Budget.USD
		remaining := budget - est.CoinUSD
		if remaining < 0 {
			remaining = 0
		}
		equivCoins := coinCfg.USDToCoins(budget)
		est.USDBudget = &budget
		est.USDRemaining = &remaining
		est.USDBudgetCoins = &equivCoins
	}
	for i := range est.ByAgent {
		est.ByAgent[i].Coins = byAgent[est.ByAgent[i].Name]
	}
}

func estimatedWindow(summary *tokens.AggregateSummary) (start, end int64) {
	if summary == nil {
		return 0, 0
	}
	for _, sess := range summary.Sessions {
		if sess.FirstActive > 0 && (start == 0 || sess.FirstActive < start) {
			start = sess.FirstActive
		}
		sessionEnd := sess.LastActive
		if sessionEnd == 0 {
			sessionEnd = sess.FirstActive
		}
		if sessionEnd > end {
			end = sessionEnd
		}
	}
	if end < start {
		end = start
	}
	return start, end
}

// estimatedSessions prices every scanned session individually so the UI can
// show cost per session (and, via the Agent field, group sessions by sandbox).
// It is a pure function of the summary — each session's own token splits are
// priced at list price, identically to the aggregate rows. The result is sorted
// by start time descending, then agent name ascending, and capped at
// maxCostSessions so the payload stays bounded on a large fleet. Returns an
// empty (non-nil) slice when there is no session data.
func estimatedSessions(summary *tokens.AggregateSummary) []costSessionEntry {
	if summary == nil {
		return []costSessionEntry{}
	}
	sessions := summary.Sessions
	out := make([]costSessionEntry, 0, len(sessions))
	for _, sess := range sessions {
		usd, exact := tokens.EstimateCostUSD(sess.Model, sess.InputTokens, sess.OutputTokens, sess.CacheRead, sess.CacheCreate)
		out = append(out, costSessionEntry{
			SessionID:  sess.SessionID,
			Agent:      sess.Agent,
			Model:      sess.Model,
			USD:        usd,
			Source:     sourceForPriced(exact),
			Input:      sess.InputTokens,
			Output:     sess.OutputTokens,
			CacheRead:  sess.CacheRead,
			Messages:   sess.Messages,
			Started:    sess.FirstActive,
			LastActive: sess.LastActive,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Started != out[j].Started {
			return out[i].Started > out[j].Started
		}
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].SessionID < out[j].SessionID
	})
	if len(out) > maxCostSessions {
		out = out[:maxCostSessions]
	}
	return out
}

// flattenEstimated converts the map-keyed EstimatedCost into sorted-agnostic
// slices with a per-row source tag, so the frontend can render a table without
// re-deriving "priced vs unpriced".
func flattenEstimated(est tokens.EstimatedCost) costEstimated {
	out := costEstimated{
		TotalUSD:       est.TotalUSD,
		ByModel:        make([]costModelEntry, 0, len(est.ByModel)),
		ByAgent:        make([]costModelEntry, 0, len(est.ByAgent)),
		UnpricedModels: est.UnpricedModels,
	}
	for name, mc := range est.ByModel {
		out.ByModel = append(out.ByModel, costModelEntry{
			Name:        name,
			USD:         mc.USD,
			Source:      sourceForPriced(mc.Priced),
			Input:       mc.Input,
			Output:      mc.Output,
			CacheRead:   mc.CacheRead,
			CacheCreate: mc.CacheCreate,
		})
	}
	for name, ac := range est.ByAgent {
		out.ByAgent = append(out.ByAgent, costModelEntry{
			Name:        name,
			USD:         ac.USD,
			Source:      sourceForPriced(ac.Priced),
			Input:       ac.Input,
			Output:      ac.Output,
			CacheRead:   ac.CacheRead,
			CacheCreate: ac.CacheCreate,
		})
	}
	return out
}

func sourceForPriced(priced bool) string {
	if priced {
		return "estimated"
	}
	return "unpriced"
}

// costHTTPClient builds an HTTP client that honors a gateway's optional private
// CA bundle (never disabling verification), mirroring how the LiteLLM key store
// builds its TLS config. A missing/unreadable bundle falls back to the system
// roots.
func costHTTPClient(caBundlePath string) *http.Client {
	client := &http.Client{Timeout: costProbeTimeout}
	path := strings.TrimSpace(caBundlePath)
	if path == "" {
		return client
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return client
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return client
	}
	client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	return client
}

// openRouterCost queries OpenRouter's GET {base}/key endpoint for real spend.
// The response shape is {"data": {"usage": <spent>, "limit": <credit cap or null>,
// "limit_remaining": <remaining or null>}}. usage is cumulative REAL spend.
func (s *Server) openRouterCost(ctx context.Context, gw config.GatewayConfig) gatewayCost {
	out := gatewayCost{Name: gw.Name, Kind: gw.Kind, Source: "native"}
	key := gw.ResolveAPIKey()
	if key == "" {
		out.Error = "no API key configured for this gateway"
		return out
	}

	url := strings.TrimRight(gw.Endpoint, "/") + openRouterKeyPath
	body, err := s.fetchCostJSON(ctx, url, key, gw.CABundle)
	if err != nil {
		out.Error = redactSecret(err.Error(), key)
		return out
	}

	var parsed struct {
		Data struct {
			Usage          *float64 `json:"usage"`
			Limit          *float64 `json:"limit"`
			LimitRemaining *float64 `json:"limit_remaining"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		out.Error = "unexpected response shape from OpenRouter /key"
		return out
	}
	out.SpentUSD = parsed.Data.Usage
	out.LimitUSD = parsed.Data.Limit
	out.RemainUSD = parsed.Data.LimitRemaining
	return out
}

// liteLLMCost queries a LiteLLM proxy's GET {base}/key/info endpoint for the
// calling key's accumulated spend. LiteLLM returns
// {"info": {"spend": <float>, "max_budget": <float or null>}}. We read spend as
// the native figure and derive remaining from max_budget when present. On any
// error (endpoint absent, unauthorized, unreachable) we return the error and
// the UI falls back to the estimate.
func (s *Server) liteLLMCost(ctx context.Context, gw config.GatewayConfig) gatewayCost {
	out := gatewayCost{Name: gw.Name, Kind: gw.Kind, Source: "native"}
	key := gw.ResolveAPIKey()
	if key == "" {
		out.Error = "no API key configured for this gateway"
		return out
	}

	url := strings.TrimRight(gw.Endpoint, "/") + litellmKeyInfoPath
	body, err := s.fetchCostJSON(ctx, url, key, gw.CABundle)
	if err != nil {
		out.Error = redactSecret(err.Error(), key)
		return out
	}

	// LiteLLM's /key/info nests the fields under "info"; be defensive about
	// both the nested and a flat shape some proxy versions return.
	var parsed struct {
		Info *struct {
			Spend     *float64 `json:"spend"`
			MaxBudget *float64 `json:"max_budget"`
		} `json:"info"`
		Spend     *float64 `json:"spend"`
		MaxBudget *float64 `json:"max_budget"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		out.Error = "unexpected response shape from LiteLLM /key/info"
		return out
	}
	spend, maxBudget := parsed.Spend, parsed.MaxBudget
	if parsed.Info != nil {
		if parsed.Info.Spend != nil {
			spend = parsed.Info.Spend
		}
		if parsed.Info.MaxBudget != nil {
			maxBudget = parsed.Info.MaxBudget
		}
	}
	if spend == nil {
		out.Error = "LiteLLM /key/info did not report a spend figure"
		return out
	}
	out.SpentUSD = spend
	out.LimitUSD = maxBudget
	if maxBudget != nil {
		rem := *maxBudget - *spend
		out.RemainUSD = &rem
	}
	return out
}

// fetchCostJSON performs a bounded, key-authenticated GET and returns the body
// bytes. Non-2xx responses become errors carrying a truncated, key-redacted
// slice of the body so the UI can show why the probe failed without leaking the
// key. Any transport error is returned verbatim (also redacted by the caller).
func (s *Server) fetchCostJSON(ctx context.Context, url, apiKey, caBundle string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, costProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := costHTTPClient(caBundle)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach gateway: %w", err)
	}
	defer closeHTTPBody(resp.Body)

	body, _ := io.ReadAll(io.LimitReader(resp.Body, costProbeMaxBody))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > costProbeMaxErrSnippet {
			snippet = snippet[:costProbeMaxErrSnippet]
		}
		return nil, fmt.Errorf("gateway returned HTTP %d: %s", resp.StatusCode, snippet)
	}
	return body, nil
}

// costProbeMaxErrSnippet bounds how much of an error body is surfaced to the UI.
const costProbeMaxErrSnippet = 256
