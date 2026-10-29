package dashboard

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// auditActionCIFailureKick is the activity-log action for a webhook
// CI-failure signal that kicked an agent or nudged the contributor lane.
const auditActionCIFailureKick = "ci_failure_kick"

// CIFailureKickOutcome is what the governor-side CIFailureKick hook reports
// for one hive-authored PR signal.
type CIFailureKickOutcome struct {
	// Agent is the agent the PR was attributed to ("" when unattributed).
	Agent string `json:"agent,omitempty"`
	// Kicked is true when a targeted kick was dispatched.
	Kicked bool `json:"kicked"`
	// Reason names why no kick went out (paused, cooldown, …).
	Reason string `json:"reason,omitempty"`
}

// ciFailureReaction is the per-PR record the webhook response carries.
type ciFailureReaction struct {
	Number  int    `json:"number"`
	Check   string `json:"check"`
	Route   string `json:"route"`
	Agent   string `json:"agent,omitempty"`
	Outcome string `json:"outcome"`
}

// reactToCIFailure turns a failed-check delivery into "ci failure signals"
// and reacts to each one now rather than on the next sweep: a hive-authored
// PR goes to the CIFailureKick hook (targeted kick of the owning agent); a
// contributor PR goes to SweepNudge (the contributor lane / stage 072 path).
// Agents never watch or poll CI themselves — this out-of-band signal, or
// the sweep as fallback, is the only detection. Guards: config flag, repo
// pause, dedup per head+check, and the per-head kick cooldown.
func (s *Server) reactToCIFailure(res github.WebhookEventResult) []ciFailureReaction {
	f := res.CIFailure
	if f == nil || s == nil || s.deps == nil {
		return nil
	}
	store := github.CIFailureSignals()
	if len(f.PRs) == 0 {
		store.RecordUnmatched()
		return nil
	}
	// owner/name as the payload reports it: the sweep trigger, the repo
	// pause predicate and the signal store all accept that form.
	repo := res.Repo
	if repo == "" {
		repo = res.ConfiguredRepo
	}
	cfg := s.deps.Config
	enabled := cfg == nil || cfg.CIFailureWebhookKick()
	repoPaused := cfg != nil && cfg.IsRepoPaused(repo)
	cooldown := cfg.CIFailureKickCooldown()
	now := time.Now().UTC()

	var out []ciFailureReaction
	for _, pr := range f.PRs {
		sig := github.CIFailureSignal{
			Repo: repo, Number: pr.Number, HeadSHA: f.HeadSHA, Check: f.Check,
			Conclusion: f.Conclusion, DetailsURL: f.DetailsURL, Title: pr.Title,
			Author: pr.Author, HiveAuthored: pr.HiveAuthored, At: now,
		}
		r := ciFailureReaction{Number: pr.Number, Check: f.Check, Route: "contributor"}
		if pr.HiveAuthored {
			r.Route = "agent"
		}
		fresh := store.Record(sig)
		switch {
		case !fresh:
			r.Outcome = "duplicate"
		case !enabled:
			r.Outcome = "disabled"
		case repoPaused:
			r.Outcome = "repo paused"
		case pr.HiveAuthored:
			r.Agent, r.Outcome = s.kickForCIFailure(sig)
		default:
			r.Outcome = s.nudgeContributorForCIFailure(sig, cooldown)
		}
		out = append(out, r)
	}
	return out
}

func (s *Server) kickForCIFailure(sig github.CIFailureSignal) (agent, outcome string) {
	if s.deps.CIFailureKick == nil {
		return "", "no kick hook"
	}
	res := s.deps.CIFailureKick(sig)
	if res.Agent != "" {
		github.CIFailureSignals().SetAgent(sig.Repo, sig.Number, res.Agent)
	}
	if !res.Kicked {
		if res.Reason == "" {
			res.Reason = "not kicked"
		}
		return res.Agent, res.Reason
	}
	s.logCIFailureReaction(sig, fmt.Sprintf("kicked agent %s", res.Agent), res.Agent)
	return res.Agent, "kicked"
}

func (s *Server) nudgeContributorForCIFailure(sig github.CIFailureSignal, cooldown time.Duration) string {
	if s.deps.SweepNudge == nil {
		return "no sweep nudge"
	}
	if !github.CIFailureSignals().AllowKick(sig.Repo, sig.Number, sig.HeadSHA, cooldown) {
		return "cooldown"
	}
	s.deps.SweepNudge(sig.Repo, sig.Number)
	s.logCIFailureReaction(sig, "nudged contributor lane", "")
	return "nudged"
}

// logCIFailureReaction writes the one-line activity entry
// "CI failed on #N (check) — kicked agent X".
func (s *Server) logCIFailureReaction(sig github.CIFailureSignal, what, agent string) {
	if s.audit == nil {
		return
	}
	line := fmt.Sprintf("CI failed on #%d (%s) — %s", sig.Number, sig.Check, what)
	if s.logger != nil {
		s.logger.Info("github webhook: "+line, "repo", sig.Repo, "head", shortSHA(sig.HeadSHA), "details", sig.DetailsURL)
	}
	s.audit.LogRecord("system", auditActionCIFailureKick, line, agent, sig.Repo, sig.Number)
}
