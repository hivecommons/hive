package dashboard

import (
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// generalAdvancedBody is the body of PUT /api/config/governor/general-advanced.
// Every field is a pointer, so an absent key leaves that setting untouched.
// It is a named type so the admin MCP's governor.general_settings operation
// can be checked against it: TestGeneralAdvancedBodyMatchesAdminMCPOp fails
// when a field is added here and not there.
type generalAdvancedBody struct {
	EvalIntervalS      *int    `json:"eval_interval_s"`
	AttributionTrailer *bool   `json:"attribution_trailer"`
	ExplainMode        *string `json:"explain_mode"`
	// APIBudgetPacing is governor.api_budget_pacing: spread the hive's GitHub
	// calls over the rate-limit window. The loop reads it every cycle.
	APIBudgetPacing *bool `json:"api_budget_pacing"`
	governorLoopSettings
}

// apiBudgetPacingStateFor is the pacing state the general-advanced settings
// report next to the toggle. The loop publishes it each time it recomputes
// its interval, so right after a save it can still describe the old setting;
// then the saved value is shown with a note that it has not applied yet.
func apiBudgetPacingStateFor(cfg *config.Config) github.APIBudgetPacing {
	state := github.APIBudgetPacingSnapshot()
	if want := cfg.Governor.APIBudgetPacing; state.Enabled != want {
		state = github.APIBudgetPacing{Enabled: want, Factor: 1, EffectiveIntervalS: state.EffectiveIntervalS,
			Reason: "saved; applies from the next eval cycle"}
	}
	return state
}
