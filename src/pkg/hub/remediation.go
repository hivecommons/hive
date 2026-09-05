package hub

import (
	"fmt"
	"strings"
)

// Remediation hints (#5577): every non-green health verdict that matches a
// known failure signature carries a one-line "do this" + where to do it,
// rendered under the WHY chip on /fleet. The map below is the RFC's
// signature→action table; precedence is BY CONSTRUCTION — the base verdict
// returns on the first matching precondition (App broken, then provider
// limit, then budget, then blocked agents), so a budget-exhausted red can
// never be shadowed by the generic no-output red, and App-broken beats
// everything. A green verdict NEVER carries a hint: a healthy hive gets no
// instruction, and the detector ambers below only ever demote green, never
// mask a red.

// Remediation is the operator hint attached to a non-green HealthVerdict.
type Remediation struct {
	// Action is the one-line "do this" instruction.
	Action string `json:"action"`
	// Surface names WHERE the action happens ("spoke dashboard", "GitHub
	// App settings", ...) for rows whose Link cannot be resolved.
	Surface string `json:"surface,omitempty"`
	// Link is a direct URL to the surface when the hub can build one.
	Link string `json:"link,omitempty"`
}

// Verdict cause signatures. Set where the verdict's reason is decided so the
// remediation map switches on tokens, never on human-facing strings.
const (
	causeAppBroken           = "app-broken"
	causeLoginStuck          = "login-stuck"
	causeBudgetExhausted     = "budget-exhausted"
	causeBudgetMisconfigured = "budget-misconfigured"
	causeErrorStreak         = "error-streak"
	causeConsentWedge        = "consent-wedge"
	causeNoCadence           = "no-cadence"
	causeHoldStale           = "hold-stale"
	causeChannelLag          = "channel-lag"
	// The three families the 2026-09-02 fleet sweep found carrying no hint at
	// all (#5699). Each verdict already named its condition precisely; none
	// named a fix, so eleven non-green spokes rendered a WHY chip with nothing
	// under it. These are verdicts the sweep OBSERVED, not hypothetical ones.
	causeAgentsDown       = "agents-down"
	causeAllPaused        = "all-paused"
	causeProviderQuota    = "provider-quota"
	causeInferenceGateway = "inference-gateway"
)

// errorStreakRedThreshold is how many consecutive failed model calls turn a
// generic no-output red into the named "model calls failing" red. Three
// matches the spoke's own consecutive-failure convention (inference-auth
// latches at three): one failure is noise, three in a row is a pattern.
const errorStreakRedThreshold = 3

// applyDetectorStates layers the #5577 detector signals onto the base
// verdict:
//
//   - error-streak RED re-explains a GENERIC stale-output red (and only
//     that): turns ran, every model call died, and "no write in 4d" pointed
//     at the wrong thing (#5338). Precondition reds — App, provider, budget,
//     login — keep their reason; they gate the output stream upstream of the
//     model calls and already name a more fundamental fix.
//   - consent-wedge and no-cadence demote GREEN to amber: output may still
//     look acceptable, but a named agent is looping on a consent screen /
//     will never be kicked, and only an operator can fix either. They never
//     touch red or amber verdicts — a harder fault stays the headline.
//
// L1 (Inception) and non-reporting hives are exempt: no output is expected,
// so a config gap is not a health fault there (the base verdict's spine).
func applyDetectorStates(v HealthVerdict, e RegistryEntry) HealthVerdict {
	if !e.Online || e.ACMMLevel <= acmmInceptionMax {
		return v
	}

	if v.State == HealthStateRed && v.staleOutput {
		if name, streak := topErrorStreak(e.AgentErrorStreaks); streak >= errorStreakRedThreshold {
			v.cause = causeErrorStreak
			v.Reason = fmt.Sprintf("agent %s model calls failing (%d consecutive) — pin a working model", name, streak)
			return v
		}
	}

	if v.State != HealthStateGreen {
		return v
	}
	if len(e.ConsentWedged) > 0 {
		v.State = HealthStateAmber
		v.cause = causeConsentWedge
		v.Reason = fmt.Sprintf("agent(s) %s stuck at Copilot consent — restarting in a loop", nameList(e.ConsentWedged))
		return v
	}
	if len(e.NoCadenceAgents) > 0 {
		v.State = HealthStateAmber
		v.cause = causeNoCadence
		v.Reason = fmt.Sprintf("agent(s) %s enabled but never kicked — set cadences", nameList(e.NoCadenceAgents))
		return v
	}
	return v
}

// topErrorStreak returns the agent with the longest streak (ties broken by
// name so the verdict is deterministic across map iterations).
func topErrorStreak(streaks map[string]int) (string, int) {
	best, bestName := 0, ""
	for name, streak := range streaks {
		if streak > best || (streak == best && best > 0 && name < bestName) {
			best, bestName = streak, name
		}
	}
	return bestName, best
}

// channelLagMinCommits is how far behind its tracked channel's head a spoke
// must be before the digest-lag amber fires. One commit is a rollout in
// flight; two or more with no upgrade running means the channel moved and the
// spoke did not follow (the moving-tag/no-repull class).
const channelLagMinCommits = 2

// applyChannelLag demotes a GREEN verdict to amber when the entry carries the
// fleet-divergence channel info AND the spoke provably lags its channel:
// tracking "stable", measurably behind the stable-v4 head, and not currently
// upgrading. Called from handleMyHives — TrackedChannel and the behind-count
// live on MyHiveEntry, not the registry entry — and skipped entirely (per the
// RFC) when either signal is absent. Never touches red/amber: a harder fault
// stays the headline.
func applyChannelLag(v *HealthVerdict, trackedChannel string, commitsBehind *int, upgrading bool) {
	if v == nil || v.State != HealthStateGreen {
		return
	}
	if trackedChannel != "stable" || commitsBehind == nil || upgrading {
		return
	}
	if *commitsBehind < channelLagMinCommits {
		return
	}
	v.State = HealthStateAmber
	v.cause = causeChannelLag
	v.Reason = fmt.Sprintf("spoke lags its channel — %d commits behind %s", *commitsBehind, trackedChannel)
	attachRemediation(v, RegistryEntry{})
}

// attachRemediation fills v.Remediation from the cause signature — the RFC's
// signature→action table. Green never gets a hint; an unrecognized cause
// (provider limit, agents down, advisory stale, ...) gets none either, so a
// hint is only ever shown when it is specifically true.
func attachRemediation(v *HealthVerdict, e RegistryEntry) {
	if v.State == HealthStateGreen || v.State == HealthStateUnknown || v.cause == "" {
		return
	}
	dash := strings.TrimRight(strings.TrimSpace(e.DashboardURL), "/")
	link := func(path string) string {
		if dash == "" {
			return ""
		}
		return dash + path
	}
	switch v.cause {
	case causeAppBroken:
		// The install URL is cluster-scoped (GHE vs github.com), so
		// handleMyHives enriches Link afterwards via the cluster's GitHub
		// config; the hint itself never guesses a forge.
		v.Remediation = &Remediation{
			Action:  "Install or repair the GitHub App for this repo",
			Surface: "GitHub App settings",
		}
	case causeLoginStuck:
		v.Remediation = &Remediation{
			Action:  "Copilot device-flow login on the spoke dashboard",
			Surface: "spoke dashboard /login",
			Link:    link("/login"),
		}
	case causeBudgetExhausted, causeBudgetMisconfigured:
		v.Remediation = &Remediation{
			Action:  "Raise or reset the budget limit (Settings → Budget)",
			Surface: "spoke dashboard settings",
			Link:    link(""),
		}
	case causeErrorStreak:
		v.Remediation = &Remediation{
			Action:  "Pin a working model on the agent card",
			Surface: "spoke dashboard agent card",
			Link:    link(""),
		}
	case causeConsentWedge:
		v.Remediation = &Remediation{
			Action:  "Complete the Copilot consent flow on the live pod dashboard",
			Surface: "spoke dashboard",
			Link:    link(""),
		}
	case causeNoCadence:
		v.Remediation = &Remediation{
			Action:  "Set cadences on the agent card",
			Surface: "spoke dashboard agent card",
			Link:    link(""),
		}
	case causeHoldStale:
		v.Remediation = &Remediation{
			Action:  "Review the needs-human queue",
			Surface: "repo PR list",
			Link:    holdQueueLink(e),
		}
	case causeChannelLag:
		v.Remediation = &Remediation{
			Action:  "Spoke lags its channel — check auto-upgrade / force rollout",
			Surface: "hub fleet version controls",
		}
	case causeAgentsDown:
		// Three spokes at the sweep, one of them running supervisor-down for
		// two days. The fix is the agent card's restart, so the hint points
		// there rather than at the pod.
		v.Remediation = &Remediation{
			Action:  "Restart the agent from its card",
			Surface: "spoke dashboard agent card",
			Link:    link(""),
		}
	case causeAllPaused:
		// Seven spokes. Amber, not red: every agent is paused ON PURPOSE, so
		// the hint has to offer BOTH resolutions — resume, or say out loud that
		// the hive is mothballed — or it reads as an instruction to undo a
		// deliberate choice.
		v.Remediation = &Remediation{
			Action:  "Resume agents, or mark the hive idle if it is mothballed",
			Surface: "spoke dashboard",
			Link:    link(""),
		}
	case causeInferenceGateway:
		v.Remediation = &Remediation{
			Action:  "Fix or retest the failing gateway (Settings → Model Gateways)",
			Surface: "spoke dashboard settings",
			Link:    link(""),
		}
	case causeProviderQuota:
		// Re-authenticating does not help here and the operator's two real
		// options are on different surfaces: move the agent to a provider with
		// headroom (agent card) or raise the cap (provider account). Name both,
		// and link the one the hub can address.
		v.Remediation = &Remediation{
			Action:  "Rotate the agent to a provider with headroom, or raise the provider quota",
			Surface: "spoke dashboard agent card / provider account",
			Link:    link(""),
		}
	}
}

// holdQueueLink builds the PR-list URL for the hive's first repo — the queue
// the hold-labeled work is parked in. Empty when the entry has no org/repo
// (the hint's Surface still tells the operator where to look).
func holdQueueLink(e RegistryEntry) string {
	org := strings.TrimSpace(e.Org)
	if org == "" || len(e.Repos) == 0 {
		return ""
	}
	repo := strings.TrimSpace(e.Repos[0])
	if repo == "" {
		return ""
	}
	host := strings.TrimSpace(e.GitHubHost)
	if host == "" {
		host = "github.com"
	}
	path := repo
	if !strings.Contains(repo, "/") {
		path = org + "/" + repo
	}
	return "https://" + host + "/" + path + "/pulls"
}
