package dashboard

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// GET /api/gh-rate-limits takes two optional parameters. Without them the
// answer is github.RateLimitInfo as it always was, so the dashboard's poll
// and the quota report script read the same bytes as before.
//
//   - ?top=N sets how many top_consumers rows come back: a whole number from
//     1 up, where anything above ghRateLimitsMaxTop is answered with
//     ghRateLimitsMaxTop rows. The default stays ghRateLimitsDefaultTop.
//   - ?usage=1 adds the fields ghRateLimitsUsageView lists. The admin MCP's
//     github_api_usage tool reads this view.
//
// Neither parameter makes a GitHub call: both read the accounting the hive's
// own transports recorded.
const (
	ghRateLimitsDefaultTop = 10
	// ghRateLimitsMaxTop equals the admin MCP's row cap (adminmcp.MaxResultLimit).
	ghRateLimitsMaxTop = 50
)

type ghRateLimitsOptions struct {
	top   int
	usage bool
}

// ghRateLimitsOptionsFrom reads ?top and ?usage. A top that is not a whole
// number of at least 1 is an error, which the handler answers with 400.
func ghRateLimitsOptionsFrom(r *http.Request) (ghRateLimitsOptions, error) {
	opts := ghRateLimitsOptions{top: ghRateLimitsDefaultTop}
	if r == nil {
		return opts, nil
	}
	q := r.URL.Query()
	opts.usage = queryFlagEnabled(q.Get("usage"))
	raw := strings.TrimSpace(q.Get("top"))
	if raw == "" {
		return opts, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return opts, fmt.Errorf("top must be a whole number from 1 to %d, got %q", ghRateLimitsMaxTop, raw)
	}
	opts.top = min(n, ghRateLimitsMaxTop)
	return opts, nil
}

// ghAPIBudgetThresholds are the configured floors the API budget mode is
// derived from, plus the floor below which the proxy refuses agent reads.
type ghAPIBudgetThresholds struct {
	// Reserve is github.api_reserve: below it the mode is conserve.
	Reserve int `json:"reserve"`
	// Critical is github.api_critical: below it the mode is critical.
	Critical int `json:"critical"`
	// AgentReserveFloor is github.agent_reserve_floor.
	AgentReserveFloor int `json:"agent_reserve_floor"`
}

// ghRateLimitsUsageView is the ?usage=1 answer: github.RateLimitInfo with its
// fields unchanged and in place, plus what a reader needs to answer "who is
// spending the budget" without the dashboard.
type ghRateLimitsUsageView struct {
	*github.RateLimitInfo
	// TopConsumersTotal is how many caller + method + endpoint rows the
	// window holds; top_consumers is cut when it is larger than that list.
	TopConsumersTotal int `json:"top_consumers_total"`
	// ByCaller is the same window summed per caller, never cut.
	ByCaller []github.RESTCallerTotal `json:"by_caller"`
	// WindowTotals sums every request in the window.
	WindowTotals ghRateLimitsWindowTotals `json:"window_totals"`
	// WindowSeconds is how far back top_consumers, by_caller and
	// window_totals look.
	WindowSeconds int `json:"window_seconds"`
	// CoreResetInSeconds is the time left until core.reset by this hive's
	// clock; 0 when the reset has passed or is unknown.
	CoreResetInSeconds int `json:"core_reset_in_seconds"`
	// APIBudgetThresholds are the configured floors behind api_budget.mode.
	APIBudgetThresholds ghAPIBudgetThresholds `json:"api_budget_thresholds"`
	// AgentCaps is the status payload's ghRateLimits.agent_caps: each agent's
	// proxy cap and its calls in the current hour.
	AgentCaps map[string]any `json:"agent_caps"`
}

type ghRateLimitsWindowTotals struct {
	Requests    int `json:"requests"`
	Charged     int `json:"charged"`
	NotModified int `json:"not_modified"`
	RateLimited int `json:"rate_limited"`
}

// ghRateLimitsView shapes one RateLimits reading for the request's options.
// With neither parameter it returns limits itself.
func (s *Server) ghRateLimitsView(limits *github.RateLimitInfo, opts ghRateLimitsOptions, now time.Time) any {
	if limits == nil {
		return limits
	}
	if !opts.usage {
		if opts.top != ghRateLimitsDefaultTop {
			limits.TopConsumers = github.RESTTopConsumers(opts.top)
		}
		return limits
	}
	usage := github.RESTUsageSnapshot(opts.top)
	limits.TopConsumers = usage.TopConsumers
	var cfg *config.Config
	if s != nil && s.deps != nil {
		cfg = s.deps.Config
	}
	view := ghRateLimitsUsageView{
		RateLimitInfo:     limits,
		TopConsumersTotal: usage.Consumers,
		ByCaller:          usage.ByCaller,
		WindowTotals: ghRateLimitsWindowTotals{
			Requests:    usage.Totals.Requests,
			Charged:     usage.Totals.Charged,
			NotModified: usage.Totals.NotModified,
			RateLimited: usage.Totals.RateLimited,
		},
		WindowSeconds: int(github.RESTAccountingWindow() / time.Second),
		APIBudgetThresholds: ghAPIBudgetThresholds{
			Reserve:           config.DefaultGitHubAPIReserve,
			Critical:          config.DefaultGitHubAPICritical,
			AgentReserveFloor: cfg.GitHubAgentReserveFloor(),
		},
		AgentCaps: buildAgentGitHubCaps(cfg),
	}
	if cfg != nil {
		if cfg.GitHub.APIReserve > 0 {
			view.APIBudgetThresholds.Reserve = cfg.GitHub.APIReserve
		}
		if cfg.GitHub.APICritical > 0 {
			view.APIBudgetThresholds.Critical = cfg.GitHub.APICritical
		}
	}
	if left := limits.Core.Reset.Sub(now); !limits.Core.Reset.IsZero() && left > 0 {
		view.CoreResetInSeconds = int(left / time.Second)
	}
	return view
}
