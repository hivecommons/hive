package stageengine

// Golden fixtures for the PR-side stage derivation (ADR-0025, PR-A,
// hivecommons/hive#13006).
//
// These freeze TODAY's behaviour of ClassifyStage / ResolveStage — and the
// pure post-steps ApplyStageLabels applies to the resolved opinion (author
// kind split of 07x, the normalized-fixing guard, RouteHuman, the DCO
// override and the 074 adoption yield) — byte-for-byte, so the central stage
// engine can prove parity before it takes the derivation over (plan:
// src/docs/stage-engine-plan.md, PR-B/PR-F). The corpus is generated
// combinatorially (lane × reason × verdict, lane × reason × author kind,
// lane × reason × modifier, lane precedence pairs, human-route evidence,
// DCO, adoption) and completed by hand-picked regressions from recent PRs.
//
// Regenerate with
//
//	go test ./pkg/stageengine -run TestStageGoldens -update
//
// and commit the result; a change in the goldens IS a semantic change and
// needs its own review. Without -update the test fails on any difference,
// and on a corpus that no longer matches the generator.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

var updateStageGoldens = flag.Bool("update", false, "rewrite the stage golden fixtures from the current derivation")

const stageGoldenDir = "testdata/stage_goldens"

// stageGoldenVerdict is the serialised ContributorLaneVerdict.
type stageGoldenVerdict struct {
	Configured         bool     `json:"configured,omitempty"`
	Approved           bool     `json:"approved,omitempty"`
	Verdict            string   `json:"verdict,omitempty"`
	DecisionRequested  bool     `json:"decision_requested,omitempty"`
	DecisionQuestion   string   `json:"decision_question,omitempty"`
	HumanSummaries     []string `json:"human_summaries,omitempty"`
	HiveAuthoredKnown  bool     `json:"hive_authored_known,omitempty"`
	HiveAuthored       bool     `json:"hive_authored,omitempty"`
	FixCycleCapReached bool     `json:"fix_cycle_cap_reached,omitempty"`
}

func (v *stageGoldenVerdict) toVerdict() ContributorLaneVerdict {
	if v == nil {
		return ContributorLaneVerdict{}
	}
	return ContributorLaneVerdict{Configured: v.Configured, Approved: v.Approved, Verdict: v.Verdict, DecisionRequested: v.DecisionRequested, DecisionQuestion: v.DecisionQuestion, HumanSummaries: v.HumanSummaries, HiveAuthoredKnown: v.HiveAuthoredKnown, HiveAuthored: v.HiveAuthored, FixCycleCapReached: v.FixCycleCapReached}
}

// stageGoldenDetail is the serialised stageDetail (the fields ClassifyStage
// and the post-steps read). A nil Verdict means VerdictKnown == false.
type stageGoldenDetail struct {
	Verdict           *stageGoldenVerdict `json:"verdict,omitempty"`
	HumanApprover     string              `json:"human_approver,omitempty"`
	HumanApprovalHead string              `json:"human_approval_head,omitempty"`
	Role              string              `json:"role,omitempty"`
	RedChecks         []string            `json:"red_checks,omitempty"`
	RedChecksHead     string              `json:"red_checks_head,omitempty"`
	DCOGate           bool                `json:"dco_gate,omitempty"`
	DCOGateHead       string              `json:"dco_gate_head,omitempty"`
}

// stageGoldenObservation is the serialised stageObservation.
type stageGoldenObservation struct {
	Lane             string            `json:"lane"`
	Reason           string            `json:"reason"`
	HeadSHA          string            `json:"head_sha,omitempty"`
	Labels           []string          `json:"labels,omitempty"`
	Merged           bool              `json:"merged,omitempty"`
	Err              bool              `json:"err,omitempty"`
	SharedCIIncident int               `json:"shared_ci_incident,omitempty"`
	Detail           stageGoldenDetail `json:"detail"`
}

// stageGoldenRecord is the part of the previous stagelabels.Record the
// post-steps read (PreserveNormalizedFixingStage, AdoptionLive).
type stageGoldenRecord struct {
	Stage        string `json:"stage,omitempty"`
	Reason       string `json:"reason,omitempty"`
	ActivityHead string `json:"activity_head,omitempty"`
	AdoptedHead  string `json:"adopted_head,omitempty"`
	AdoptMode    string `json:"adopt_mode,omitempty"`
}

// stageGoldenChecks is the head check digest humanEvidence reads
// (hgithub.CommitChecksSummary) plus whether it could be read.
type stageGoldenChecks struct {
	Total   int      `json:"total,omitempty"`
	Green   int      `json:"green,omitempty"`
	Pending int      `json:"pending,omitempty"`
	Red     []string `json:"red,omitempty"`
	DCO     int      `json:"dco,omitempty"`
	Known   bool     `json:"known,omitempty"`
}

// stageGoldenRoute is the non-observation evidence humanEvidence gathers for
// a human-routed PR: the contributor-lane verdict for the head (nil = the
// verdict source has nothing, i.e. an unconfigured reviewer), the check
// digest, the sentinel label name and the soak.
type stageGoldenRoute struct {
	Verdict       *stageGoldenVerdict `json:"verdict,omitempty"`
	Checks        stageGoldenChecks   `json:"checks"`
	SentinelLabel string              `json:"sentinel_label,omitempty"`
	SoakMinutes   int                 `json:"soak_minutes,omitempty"`
	SoakElapsedM  int                 `json:"soak_elapsed_minutes,omitempty"`
}

// stageGoldenDCO is the commit check result ApplyStageLabels folds in.
type stageGoldenDCO struct {
	Total            int    `json:"total,omitempty"`
	Unsigned         int    `json:"unsigned,omitempty"`
	SquashUnsafeHive int    `json:"squash_unsafe_hive,omitempty"`
	Policy           string `json:"policy,omitempty"` // "" (detect) | required | not-required
	AuthorIsApp      bool   `json:"author_is_app,omitempty"`
}

// stageGoldenInput is one fixture's input: the lane observations sorted by
// precedence as StageTracker.drain hands them over, the author kind
// stageAuthorKind detects, and the optional post-step inputs.
type stageGoldenInput struct {
	Observations []stageGoldenObservation `json:"observations"`
	AuthorKind   string                   `json:"author_kind,omitempty"`
	Prev         *stageGoldenRecord       `json:"prev,omitempty"`
	Route        *stageGoldenRoute        `json:"route,omitempty"`
	DCO          *stageGoldenDCO          `json:"dco,omitempty"`
}

// stageGoldenWant is ResolveStage's answer (Stage…Lane) and the stage the
// post-steps leave (Final*). Skipped is set when the resolved rank is none
// and ApplyStageLabels would not touch the PR.
type stageGoldenWant struct {
	Stage        string   `json:"stage"`
	Rank         int      `json:"rank"`
	Reason       string   `json:"reason"`
	Role         string   `json:"role"`
	Terminal     bool     `json:"terminal,omitempty"`
	Human        bool     `json:"human,omitempty"`
	Lane         string   `json:"lane"`
	Skipped      bool     `json:"skipped,omitempty"`
	FinalStage   string   `json:"final_stage,omitempty"`
	FinalReason  string   `json:"final_reason,omitempty"`
	FinalDCO     string   `json:"final_dco,omitempty"`
	RouteReasons []string `json:"route_reasons,omitempty"`
}

type stageGoldenCase struct {
	Name  string           `json:"name"`
	Note  string           `json:"note,omitempty"`
	Input stageGoldenInput `json:"input"`
	Want  stageGoldenWant  `json:"want"`
}

func stageGoldenStage(s string) stagelabels.Stage {
	for _, d := range stagelabels.Table {
		if d.Stage.String() == s {
			return d.Stage
		}
	}
	return stagelabels.None
}

var stageGoldenRoles = map[string]stagelabels.Role{
	"": stagelabels.RoleNone, "trusted": stagelabels.RoleTrusted, "contributor": stagelabels.RoleContributor,
	"newcomer": stagelabels.RoleNewcomer, "untrusted": stagelabels.RoleUntrusted,
}

func stageGoldenRoleString(r stagelabels.Role) string {
	if r == stagelabels.RoleNone {
		return "none"
	}
	return r.String()
}

func dcoGoldenString(d stagelabels.DCOState) string {
	switch d {
	case stagelabels.DCOYes:
		return "yes"
	case stagelabels.DCONo:
		return "no"
	}
	return "unknown"
}

func (o stageGoldenObservation) toObservation() stageObservation {
	obs := stageObservation{
		Lane: o.Lane, Reason: o.Reason, HeadSHA: o.HeadSHA, Labels: o.Labels, Merged: o.Merged, Err: o.Err,
		SharedCIIncident: o.SharedCIIncident,
		Display:          "hivecommons/hive", Owner: "hivecommons", Repo: "hive", Number: 1,
		Detail: stageDetail{
			HumanApprover: o.Detail.HumanApprover, HumanApprovalHead: o.Detail.HumanApprovalHead,
			Role: stageGoldenRoles[o.Detail.Role], RedChecks: o.Detail.RedChecks, RedChecksHead: o.Detail.RedChecksHead,
			DCOGate: o.Detail.DCOGate, DCOGateHead: o.Detail.DCOGateHead,
		},
	}
	if o.Detail.Verdict != nil {
		obs.Detail.VerdictKnown = true
		obs.Detail.Verdict = o.Detail.Verdict.toVerdict()
	}
	return obs
}

// runStageGolden evaluates one fixture with the current code. It mirrors the
// per-PR order of ApplyStageLabels: ResolveStage on the raw observations
// (author kind not yet known), the 07x split once it is, the normalized
// fixing guard, routeHuman (humanEvidence + stagelabels.RouteHuman), the
// DCO override and the adoption yield. The engine-dependent steps that need
// a forge read or live policy (missingStageLanes, stageAuthorKind, the
// stageDCO commit walk, settleInPlaceAdoption) are represented by their
// inputs, never re-implemented.
func runStageGolden(in stageGoldenInput) stageGoldenWant {
	w, _ := runStageGoldenWithFacts(in)
	return w
}

// runStageGoldenWithFacts is runStageGolden that also returns the Facts
// ApplyStageLabels would hand the stage engine for the fixture: FactsFromPRObservations
// over the raw observations plus the PRPassFacts the pass records at each
// post-step. TestStageGoldensEngineParity derives them and expects Final*.
func runStageGoldenWithFacts(in stageGoldenInput) (stageGoldenWant, Facts) {
	list := make([]stageObservation, 0, len(in.Observations))
	for _, o := range in.Observations {
		list = append(list, o.toObservation())
	}
	facts := FactsFromPRObservations(list)
	verdict, role, obs := ResolveStage(list)
	w := stageGoldenWant{
		Stage: verdict.Stage.String(), Rank: verdict.Rank, Reason: verdict.Reason, Role: stageGoldenRoleString(role),
		Terminal: verdict.Terminal, Human: verdict.Human, Lane: obs.Lane,
	}
	if verdict.Rank == stageRankNone {
		w.Skipped = true
		return w, facts
	}
	obs.AuthorKind = authorkind.Kind(in.AuthorKind)
	pass := &PRPassFacts{AuthorKind: obs.AuthorKind}
	facts.PR.Pass = pass
	if verdict.Stage == stagelabels.NeedsAuthor || verdict.Stage == stagelabels.ReturnedToContributor {
		verdict.Stage = FixingStage(obs)
	}
	obs.LaneReason = obs.Reason
	if verdict.Reason != "" {
		obs.Reason = verdict.Reason
	}
	var prevRec stagelabels.Record
	hadPrev := in.Prev != nil
	if hadPrev {
		prevRec = stagelabels.Record{Stage: stageGoldenStage(in.Prev.Stage), Reason: in.Prev.Reason, ActivityHead: in.Prev.ActivityHead, AdoptedHead: in.Prev.AdoptedHead, AdoptMode: in.Prev.AdoptMode}
		pass.Prior = &PriorStage{Stage: prevRec.Stage, Reason: prevRec.Reason, ActivityHead: prevRec.ActivityHead}
	}
	if replacement, ok := PreserveNormalizedFixingStage(prevRec, hadPrev, obs, verdict); ok {
		verdict = replacement
		obs.Reason = replacement.Reason
	}
	if verdict.Human && !verdict.Terminal {
		evidence := goldenHumanEvidence(obs, verdict.Stage, in.Route)
		pass.Route = &evidence
		route := stagelabels.RouteHuman(evidence)
		verdict.Stage = route.Stage
		if route.LaneReason != "" {
			obs.Reason = route.LaneReason
		}
		w.RouteReasons = route.Reasons
	}
	if !verdict.Terminal && verdict.Stage != stagelabels.None && in.DCO != nil {
		obs.Detail.DCO = dcoState(in.DCO.Total, in.DCO.Unsigned)
		required := obs.Detail.DCOGate && (obs.Detail.DCOGateHead == "" || obs.HeadSHA == "" || obs.Detail.DCOGateHead == obs.HeadSHA)
		switch in.DCO.Policy {
		case "required":
			required = true
		case "not-required":
			required = false
		}

		w.FinalDCO = dcoGoldenString(obs.Detail.DCO)
		pass.DCO, pass.DCOKnown = obs.Detail.DCO, true
		pass.DCORequired = required
		pass.SquashUnsafeHive, pass.AuthorIsApp = in.DCO.SquashUnsafeHive, in.DCO.AuthorIsApp
		if obs.Detail.DCO == stagelabels.DCONo && required {
			verdict.Stage = FixingStage(obs)
			verdict.Rank = stageRankOwned
			obs.Reason = stagelabels.LaneReasonDCOMissing
		} else if in.DCO.SquashUnsafeHive > 0 && !in.DCO.AuthorIsApp {
			// No merge commit is elected here (no role/trusted lane in
			// these fixtures). Mixed authorship is the hive's doing and
			// parks for a maintainer; an all-App head goes to its author.
			verdict.Stage = FixingStage(obs)
			if in.DCO.Total > in.DCO.SquashUnsafeHive && !(hadPrev && prevRec.AdoptionLive(obs.HeadSHA)) {
				verdict.Stage = stagelabels.NeedsDecision
				pass.SignoffHiveCaused = true
			}
			verdict.Rank = stageRankOwned
			obs.Reason = stagelabels.LaneReasonHiveSignoffAttribution
		}
	}
	if !verdict.Terminal && hadPrev && prevRec.AdoptionLive(obs.HeadSHA) && verdict.Stage.YieldsToAdoption() {
		verdict.Stage = stagelabels.AdoptedByHive
		pass.AdoptionLive = true
	}
	if verdict.Terminal {
		w.FinalStage = "terminal"
		return w, facts
	}
	w.FinalStage, w.FinalReason = verdict.Stage.String(), obs.Reason
	return w, facts
}

// goldenHumanEvidence is humanEvidence with its forge reads and live policy
// replaced by the fixture's route block (nil = checks unreadable, no
// verdict source, default sentinel label, no soak).
func goldenHumanEvidence(obs stageObservation, stage stagelabels.Stage, r *stageGoldenRoute) stagelabels.Evidence {
	if r == nil {
		r = &stageGoldenRoute{}
	}
	cause := stagelabels.CauseTrust
	if stage == stagelabels.NeedsDecision {
		cause = stagelabels.CauseDecision
	}
	contributorVerdict := normalizeContributorLaneVerdict(r.Verdict.toVerdict())
	e := stagelabels.Evidence{
		Head:             obs.HeadSHA,
		Verdict:          humanVerdict(contributorVerdict),
		Conflict:         humanConflict(obs),
		Holds:            stagelabels.Holds(obs.Labels),
		Cause:            cause,
		CauseReason:      decisionCause(obs.Reason),
		DecisionQuestion: strings.TrimSpace(contributorVerdict.DecisionQuestion),
		HumanSummaries:   append([]string(nil), contributorVerdict.HumanSummaries...),
		SelfAuthored:     obs.Lane == stageLaneSelf,
		AuthorKind:       obs.AuthorKind,
	}
	sentinel := r.SentinelLabel
	if sentinel == "" {
		sentinel = defaultSentinelLabel
	}
	e.Sentinel = HasLabelFold(obs.Labels, sentinel)
	summary := stageGoldenChecks{Total: r.Checks.Total, Green: r.Checks.Green, Pending: r.Checks.Pending, Red: r.Checks.Red, DCO: r.Checks.DCO}
	known := r.Checks.Known
	e.Checks = stagelabels.CheckSummary{Total: summary.Total, Green: summary.Green, Pending: summary.Pending, Red: summary.Red, Known: known}
	switch summary.DCO {
	case 1:
		e.DCO = stagelabels.DCOYes
	case -1:
		e.DCO = stagelabels.DCONo
	}
	e.HeadFresh = known && summary.Total == 0
	if r.SoakMinutes > 0 {
		e.Soak = time.Duration(r.SoakMinutes) * time.Minute
		if known && summary.Total > 0 && summary.Pending == 0 && len(summary.Red) == 0 {
			e.SoakElapsed = time.Duration(r.SoakElapsedM) * time.Minute
		}
	}
	return e
}

// ── corpus ──────────────────────────────────────────────────────────────

const goldenHead = "abc1234def"

// stageGoldenReasons is every lane reason ClassifyStage distinguishes, in a
// fixed order, including one representative per prefix family and a
// reason no map knows.
var stageGoldenReasons = []string{
	"",
	"closed", "gone", "label-removed", "draft",
	// hand-offs
	"not-app-authored", trustedBotLaneSkipReason, "repo-auto-merge-disabled", contributorLaneReasonAppAuthored,
	contributorLaneReasonNoAuthor, "missing-head-sha", "fetch-pr", "fetch-pr-recheck",
	// admission (weak opinions)
	"untrusted-author-role", contributorLaneReasonNoClaim, contributorLaneReasonClaimAuthor, contributorLaneReasonClaimOtherHive,
	contributorLaneReasonNoLinkedIssue, contributorLaneReasonClaimTier, contributorLaneReasonClaimUnverified, contributorLaneReasonAuthorAssociation,
	// workflow approval
	workflowApprovalRequiredPrefix + "ci.yml",
	// needs-human
	contributorLaneReasonWorkflowPath, contributorLaneReasonTestsRemoved, contributorLaneReasonNoReviewer, contributorLaneReasonReporterTrustHold,
	contributorLaneReasonSentinelBlocked, contributorLaneReasonNoAppBotLogin, contributorLaneReasonFilesEvidence,
	autoMergeReasonHumanMergePath, autoMergeReasonHumanMergePathEvidence, autoMergeReasonIntentUnauthorized, autoMergeReasonIntentEvidence,
	autoMergeReasonWorkflowPermission, autoMergeReasonForkWorkflowUpdate,
	"approval-desk-withheld", "author-permission-missing", "fork-non-member", "held", "label:hold", "label:needs-human", "exempt-label", "sentinel",
	autoMergeReasonNoAppBotLogin, "self-authorization-hold-released", "excluded-label:do-not-merge",
	"linked-issue:hold:#551:hold",
	// fixing
	"changes-requested", contributorLaneReasonChangesRequested, "conflicting", "not-mergeable", "label:needs-rebase", "label:rebase-me",
	// CI red
	"required-check-failing:unit", "check-failing:lint", "status-failure", "check-runs", "check-timed_out",
	// CI pending / inconclusive
	"ci-unverified", "check-pending", "required-check-pending:unit", "status-pending", "pending: unit", "required-check-missing:unit", "check-neutral", "check-skipped",
	// merge queue
	contributorLaneReasonSoaking + "(remaining=30m)", "head-changed-since-eval", "merge-failed", "merge-not-applied", "required-checks-expected",
	"behind", "updated-branch", "update-branch", autoMergeReasonBehindBudgetExhausted, "release-hold:v1.2",
	// review pending
	contributorLaneReasonNoVerdict,
	// unmapped
	contributorLaneReasonRegistryTrusted, "something-unmapped",
}

var stageGoldenLanes = []string{stageLaneSelf, stageLaneTrusted, stageLaneContributor}

type namedVerdict struct {
	name    string
	verdict *stageGoldenVerdict
	// approver/head set the trusted human approval on the detail.
	approver, approvalHead string
}

const goldenConcreteSummary = "correctness requires human: LaneQueuedWork is plumbed but never populated; add the producer in AgentActivityFor."

var stageGoldenVerdicts = []namedVerdict{
	{name: "none"},
	{name: "approve", verdict: &stageGoldenVerdict{Configured: true, Approved: true, Verdict: "approve"}},
	{name: "changes_requested", verdict: &stageGoldenVerdict{Configured: true, Verdict: "changes_requested"}},
	{name: "reject", verdict: &stageGoldenVerdict{Configured: true, Verdict: "reject"}},
	{name: "requires_human", verdict: &stageGoldenVerdict{Configured: true, Verdict: "requires_human"}},
	{name: "requires_human-concrete", verdict: &stageGoldenVerdict{Configured: true, Verdict: "requires_human", HumanSummaries: []string{goldenConcreteSummary}}},
	{name: "requires_human-policy-owned", verdict: &stageGoldenVerdict{Configured: true, Verdict: "requires_human", HiveAuthoredKnown: true, HumanSummaries: []string{goldenConcreteSummary}}},
	{name: "requires_human-human-approved", verdict: &stageGoldenVerdict{Configured: true, Verdict: "requires_human"}, approver: "clubanderson", approvalHead: goldenHead},
	{name: "requires_human-human-approved-stale", verdict: &stageGoldenVerdict{Configured: true, Verdict: "requires_human"}, approver: "clubanderson", approvalHead: "0ld0000000"},
	{name: "decision_requested", verdict: &stageGoldenVerdict{Configured: true, Approved: true, Verdict: "approve", DecisionRequested: true, DecisionQuestion: "@clubanderson should decide."}},
	{name: "unconfigured", verdict: &stageGoldenVerdict{}},
	{name: "incomplete", verdict: &stageGoldenVerdict{Configured: true, Verdict: "incomplete"}},
}

func (v namedVerdict) detail(role string) stageGoldenDetail {
	return stageGoldenDetail{Verdict: v.verdict, HumanApprover: v.approver, HumanApprovalHead: v.approvalHead, Role: role}
}

var stageGoldenAuthorKinds = []string{"", string(authorkind.Human), string(authorkind.Clanker), string(authorkind.Hive)}

func goldenKindName(k string) string {
	if k == "" {
		return "unknown"
	}
	return k
}

func goldenReasonName(r string) string {
	if r == "" {
		return "empty"
	}
	return strings.NewReplacer("/", "_", "(", "", ")", "", " ", "_", ":", "=").Replace(r)
}

// defaultKindForLane is the author kind the per-lane crosses use when kind
// is not the dimension under test.
func defaultKindForLane(lane string) string {
	switch lane {
	case stageLaneSelf:
		return string(authorkind.Hive)
	case stageLaneTrusted:
		return string(authorkind.Human)
	}
	return string(authorkind.Clanker)
}

func generateStageGoldens() map[string][]stageGoldenCase {
	out := map[string][]stageGoldenCase{}
	add := func(file string, c stageGoldenCase) {
		c.Want = runStageGolden(c.Input)
		out[file] = append(out[file], c)
	}
	single := func(lane, reason string, d stageGoldenDetail) []stageGoldenObservation {
		return []stageGoldenObservation{{Lane: lane, Reason: reason, HeadSHA: goldenHead, Detail: d}}
	}

	// 1. lane × reason × verdict. The contributor lane is the only one whose
	// classification reads the verdict shape in depth, so the other lanes
	// run the three verdicts that reach distinct branches.
	for _, lane := range stageGoldenLanes {
		verdicts := stageGoldenVerdicts
		if lane != stageLaneContributor {
			verdicts = stageGoldenVerdicts[:3]
		}
		for _, reason := range stageGoldenReasons {
			for _, v := range verdicts {
				add("lane-reason-verdict.json", stageGoldenCase{
					Name:  fmt.Sprintf("%s/%s/verdict=%s", lane, goldenReasonName(reason), v.name),
					Input: stageGoldenInput{Observations: single(lane, reason, v.detail("")), AuthorKind: defaultKindForLane(lane)},
				})
			}
		}
	}

	// 2. lane × reason × author kind (verdict none; also with approve for
	// the awaiting-admission path).
	for _, lane := range stageGoldenLanes {
		for _, reason := range stageGoldenReasons {
			for _, kind := range stageGoldenAuthorKinds {
				add("lane-reason-authorkind.json", stageGoldenCase{
					Name:  fmt.Sprintf("%s/%s/kind=%s", lane, goldenReasonName(reason), goldenKindName(kind)),
					Input: stageGoldenInput{Observations: single(lane, reason, stageGoldenDetail{}), AuthorKind: kind},
				})
			}
		}
	}

	// 3. lane × reason × modifier.
	type modifier struct {
		name string
		mod  func(o *stageGoldenObservation)
	}
	approve := &stageGoldenVerdict{Configured: true, Approved: true, Verdict: "approve"}
	modifiers := []modifier{
		{"merged", func(o *stageGoldenObservation) { o.Merged = true }},
		{"err", func(o *stageGoldenObservation) { o.Err = true }},
		{"err+approve", func(o *stageGoldenObservation) { o.Err = true; o.Detail.Verdict = approve }},
		{"label=needs-rebase", func(o *stageGoldenObservation) { o.Labels = []string{"Needs-Rebase"} }},
		{"label=author_hive", func(o *stageGoldenObservation) { o.Labels = []string{"author/hive"} }},
		{"redcheck=same-head", func(o *stageGoldenObservation) {
			o.Detail.RedChecks = []string{"docs-link-check", "lint"}
			o.Detail.RedChecksHead = goldenHead
		}},
		{"redcheck=stale-head", func(o *stageGoldenObservation) {
			o.Detail.RedChecks = []string{"lint"}
			o.Detail.RedChecksHead = "0ld0000000"
		}},
		{"redcheck+approve", func(o *stageGoldenObservation) {
			o.Detail.RedChecks = []string{"lint"}
			o.Detail.RedChecksHead = goldenHead
			o.Detail.Verdict = approve
		}},
		{"sharedci", func(o *stageGoldenObservation) { o.SharedCIIncident = 9100 }},
		{"sharedci+redcheck", func(o *stageGoldenObservation) {
			o.SharedCIIncident = 9100
			o.Detail.RedChecks = []string{"lint"}
			o.Detail.RedChecksHead = goldenHead
		}},
		{"sharedci+approve", func(o *stageGoldenObservation) { o.SharedCIIncident = 9100; o.Detail.Verdict = approve }},
		{"no-head", func(o *stageGoldenObservation) { o.HeadSHA = "" }},
		{"role=trusted", func(o *stageGoldenObservation) { o.Detail.Role = "trusted" }},
	}
	for _, lane := range stageGoldenLanes {
		for _, reason := range stageGoldenReasons {
			for _, m := range modifiers {
				obs := single(lane, reason, stageGoldenDetail{})
				m.mod(&obs[0])
				add("lane-reason-modifier.json", stageGoldenCase{
					Name:  fmt.Sprintf("%s/%s/mod=%s", lane, goldenReasonName(reason), m.name),
					Input: stageGoldenInput{Observations: obs, AuthorKind: defaultKindForLane(lane)},
				})
			}
		}
	}

	// 4. Lane precedence: trusted-author × contributor pairs (plus the self
	// lane on top) × role evidence per lane.
	trustedReasons := []string{"untrusted-author-role", "not-app-authored", trustedBotLaneSkipReason, contributorLaneReasonNoVerdict, "check-failing:unit", "author-permission-missing", "held", "", "something-unmapped"}
	contributorReasons := []string{contributorLaneReasonNoClaim, contributorLaneReasonClaimTier, contributorLaneReasonClaimUnverified, contributorLaneReasonNoVerdict, contributorLaneReasonSoaking + "(remaining=5m)", contributorLaneReasonAppAuthored, contributorLaneReasonChangesRequested, contributorLaneReasonWorkflowPath, contributorLaneReasonRegistryTrusted, "conflicting", ""}
	roles := []string{"", "trusted", "contributor", "newcomer", "untrusted"}
	for _, tr := range trustedReasons {
		for _, cr := range contributorReasons {
			for _, trole := range []string{"", "trusted"} {
				for _, crole := range roles {
					obs := []stageGoldenObservation{
						{Lane: stageLaneTrusted, Reason: tr, HeadSHA: goldenHead, Detail: stageGoldenDetail{Role: trole}},
						{Lane: stageLaneContributor, Reason: cr, HeadSHA: goldenHead, Detail: stageGoldenDetail{Role: crole}},
					}
					add("precedence.json", stageGoldenCase{
						Name:  fmt.Sprintf("trusted=%s,role=%s/contributor=%s,role=%s", goldenReasonName(tr), goldenKindName(trole), goldenReasonName(cr), goldenKindName(crole)),
						Input: stageGoldenInput{Observations: obs, AuthorKind: string(authorkind.Human)},
					})
				}
			}
		}
	}
	for _, sr := range []string{"check-failing:unit", contributorLaneReasonNoVerdict, "not-app-authored", "draft", "closed", "head-changed-since-eval"} {
		for _, cr := range []string{contributorLaneReasonAppAuthored, contributorLaneReasonNoClaim, contributorLaneReasonChangesRequested} {
			obs := []stageGoldenObservation{
				{Lane: stageLaneSelf, Reason: sr, HeadSHA: goldenHead, Detail: stageGoldenDetail{Role: "trusted"}},
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role", HeadSHA: goldenHead},
				{Lane: stageLaneContributor, Reason: cr, HeadSHA: goldenHead},
			}
			add("precedence.json", stageGoldenCase{
				Name:  fmt.Sprintf("self=%s/trusted=untrusted-author-role/contributor=%s", goldenReasonName(sr), goldenReasonName(cr)),
				Input: stageGoldenInput{Observations: obs, AuthorKind: string(authorkind.Hive)},
			})
		}
	}

	// 5. Human route: every human-routed reason × route evidence × verdict.
	humanReasons := []string{
		"untrusted-author-role", contributorLaneReasonNoClaim, contributorLaneReasonClaimTier, contributorLaneReasonClaimUnverified,
		contributorLaneReasonAuthorAssociation, workflowApprovalRequiredPrefix + "ci.yml", contributorLaneReasonWorkflowPath,
		contributorLaneReasonTestsRemoved, contributorLaneReasonNoReviewer, contributorLaneReasonReporterTrustHold,
		contributorLaneReasonSentinelBlocked, autoMergeReasonHumanMergePath, autoMergeReasonIntentUnauthorized,
		"approval-desk-withheld", "held", "label:hold", "exempt-label", "sentinel", "excluded-label:do-not-merge", "fork-non-member",
	}
	type routeShape struct {
		name   string
		route  *stageGoldenRoute
		labels []string
	}
	green := stageGoldenChecks{Total: 3, Green: 3, Known: true}
	routeShapes := []routeShape{
		{name: "noroute", route: nil},
		{name: "green", route: &stageGoldenRoute{Checks: green}},
		{name: "green-dco-yes", route: &stageGoldenRoute{Checks: stageGoldenChecks{Total: 3, Green: 3, Known: true, DCO: 1}}},
		{name: "green-dco-no", route: &stageGoldenRoute{Checks: stageGoldenChecks{Total: 3, Green: 3, Known: true, DCO: -1}}},
		{name: "pending", route: &stageGoldenRoute{Checks: stageGoldenChecks{Total: 3, Green: 1, Pending: 2, Known: true}}},
		{name: "red", route: &stageGoldenRoute{Checks: stageGoldenChecks{Total: 3, Green: 2, Red: []string{"unit"}, Known: true}}},
		{name: "fresh", route: &stageGoldenRoute{Checks: stageGoldenChecks{Known: true}}},
		{name: "unknown", route: &stageGoldenRoute{Checks: stageGoldenChecks{}}},
		{name: "green-soaking", route: &stageGoldenRoute{Checks: green, SoakMinutes: 30, SoakElapsedM: 10}},
		{name: "green-soaked", route: &stageGoldenRoute{Checks: green, SoakMinutes: 30, SoakElapsedM: 45}},
		{name: "green-hold-label", route: &stageGoldenRoute{Checks: green}, labels: []string{"do-not-merge/wip"}},
		{name: "green-sentinel", route: &stageGoldenRoute{Checks: green}, labels: []string{defaultSentinelLabel}},
		{name: "green-custom-sentinel", route: &stageGoldenRoute{Checks: green, SentinelLabel: "ops/alert"}, labels: []string{"ops/alert"}},
		{name: "green-needs-rebase", route: &stageGoldenRoute{Checks: green}, labels: []string{"needs-rebase"}},
	}
	routeVerdicts := []namedVerdict{
		{name: "none"},
		{name: "approve", verdict: &stageGoldenVerdict{Configured: true, Approved: true, Verdict: "approve"}},
		{name: "changes_requested", verdict: &stageGoldenVerdict{Configured: true, Verdict: "changes_requested"}},
		{name: "requires_human", verdict: &stageGoldenVerdict{Configured: true, Verdict: "requires_human", DecisionQuestion: "Ship behind a flag?"}},
		{name: "requires_human-summary", verdict: &stageGoldenVerdict{Configured: true, Verdict: "requires_human", HumanSummaries: []string{"needs a product call"}}},
		{name: "requires_human-concrete", verdict: &stageGoldenVerdict{Configured: true, Verdict: "requires_human", HumanSummaries: []string{goldenConcreteSummary}}},
		{name: "pending", verdict: &stageGoldenVerdict{Configured: true, Verdict: ""}},
	}
	// The full verdict cross runs on green checks; every other route shape
	// takes the three verdicts that change its outcome, and the shapes that
	// end in a repair stage are repeated for a clanker author (the 07x
	// split).
	coreVerdicts := []namedVerdict{routeVerdicts[0], routeVerdicts[1], routeVerdicts[3]}
	fixingShapes := map[string]bool{"red": true, "green-dco-no": true, "green-needs-rebase": true}
	for _, reason := range humanReasons {
		for _, rs := range routeShapes {
			verdicts := coreVerdicts
			if rs.name == "green" {
				verdicts = routeVerdicts
			}
			for _, rv := range verdicts {
				kinds := []string{string(authorkind.Human)}
				if fixingShapes[rs.name] {
					kinds = append(kinds, string(authorkind.Clanker))
				}
				for _, kind := range kinds {
					var route *stageGoldenRoute
					if rs.route != nil {
						cp := *rs.route
						cp.Verdict = rv.verdict
						route = &cp
					} else if rv.verdict != nil {
						continue
					}
					obs := single(stageLaneContributor, reason, stageGoldenDetail{})
					obs[0].Labels = rs.labels
					add("human-route.json", stageGoldenCase{
						Name:  fmt.Sprintf("%s/route=%s/verdict=%s/kind=%s", goldenReasonName(reason), rs.name, rv.name, kind),
						Input: stageGoldenInput{Observations: obs, AuthorKind: kind, Route: route},
					})
				}
			}
		}
	}

	// 6. DCO override.
	dcoReasons := []string{contributorLaneReasonNoVerdict, contributorLaneReasonSoaking + "(remaining=5m)", "check-pending", "check-failing:unit", "draft"}
	// The gate-detail cross only matters for the shapes whose outcome depends
	// on the gate observation; the rest run without one.
	gateSensitive := map[string]bool{"signed": true, "unsigned-detect-gate": true}
	for _, lane := range stageGoldenLanes {
		for _, reason := range dcoReasons {
			for _, kind := range []string{string(authorkind.Human), string(authorkind.Clanker), string(authorkind.Hive)} {
				for _, d := range []struct {
					name string
					dco  stageGoldenDCO
				}{
					{"signed", stageGoldenDCO{Total: 2}},
					{"unsigned-detect-gate", stageGoldenDCO{Total: 2, Unsigned: 1}},
					{"unsigned-required", stageGoldenDCO{Total: 2, Unsigned: 1, Policy: "required"}},
					{"unsigned-not-required", stageGoldenDCO{Total: 2, Unsigned: 1, Policy: "not-required"}},
					{"empty", stageGoldenDCO{}},
					{"squash-unsafe", stageGoldenDCO{Total: 2, SquashUnsafeHive: 1}},
					{"squash-unsafe-app", stageGoldenDCO{Total: 2, SquashUnsafeHive: 1, AuthorIsApp: true}},
					{"squash-unsafe-hive-only", stageGoldenDCO{Total: 1, SquashUnsafeHive: 1}},
					{"unsigned+squash-unsafe-required", stageGoldenDCO{Total: 3, Unsigned: 1, SquashUnsafeHive: 1, Policy: "required"}},
				} {
					gates := []struct {
						name string
						det  stageGoldenDetail
					}{
						{"nogate", stageGoldenDetail{}},
						{"gate", stageGoldenDetail{DCOGate: true, DCOGateHead: goldenHead}},
						{"gate-stale", stageGoldenDetail{DCOGate: true, DCOGateHead: "0ld0000000"}},
					}
					if !gateSensitive[d.name] {
						gates = gates[:1]
					}
					for _, gate := range gates {
						dco := d.dco
						add("dco.json", stageGoldenCase{
							Name:  fmt.Sprintf("%s/%s/kind=%s/dco=%s/%s", lane, goldenReasonName(reason), kind, d.name, gate.name),
							Input: stageGoldenInput{Observations: single(lane, reason, gate.det), AuthorKind: kind, DCO: &dco},
						})
					}
				}
			}
		}
	}

	// 7. Adoption (hivecommons/hive#12737): a live adoption keeps 074 across
	// every stage before the merge gate; a stale or in-place record behaves
	// per Record.AdoptionLive.
	adoptionReasons := []string{contributorLaneReasonNoVerdict, "check-pending", "check-failing:unit", "conflicting", contributorLaneReasonChangesRequested, contributorLaneReasonSoaking + "(remaining=5m)", "head-changed-since-eval", "draft", "closed", contributorLaneReasonNoClaim, contributorLaneReasonWorkflowPath, ""}
	prevShapes := []struct {
		name string
		prev *stageGoldenRecord
	}{
		{"none", nil},
		{"supersede-live", &stageGoldenRecord{Stage: "072-returned-to-contributor", AdoptedHead: goldenHead, AdoptMode: stagelabels.AdoptModeSupersede}},
		{"supersede-stale", &stageGoldenRecord{Stage: "074-adopted-by-hive", AdoptedHead: "0ld0000000", AdoptMode: stagelabels.AdoptModeSupersede}},
		{"inplace-live", &stageGoldenRecord{Stage: "074-adopted-by-hive", AdoptedHead: "0ld0000000", AdoptMode: stagelabels.AdoptModeInPlace}},
		{"yielded", &stageGoldenRecord{Stage: "072-returned-to-contributor", AdoptMode: stagelabels.AdoptModeInPlace}},
	}
	for _, reason := range adoptionReasons {
		for _, ps := range prevShapes {
			for _, v := range []namedVerdict{stageGoldenVerdicts[0], stageGoldenVerdicts[1], stageGoldenVerdicts[2]} {
				for _, kind := range []string{string(authorkind.Human), string(authorkind.Clanker)} {
					add("adoption.json", stageGoldenCase{
						Name:  fmt.Sprintf("%s/prev=%s/verdict=%s/kind=%s", goldenReasonName(reason), ps.name, v.name, kind),
						Input: stageGoldenInput{Observations: single(stageLaneContributor, reason, v.detail("")), AuthorKind: kind, Prev: ps.prev, Route: &stageGoldenRoute{Checks: green}},
					})
				}
			}
		}
	}

	// 8. Hand-picked regressions.
	humanDecisionRequested := &stageGoldenVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, DecisionQuestion: "@clubanderson should this land before the freeze?"}
	requiresHumanConcrete := &stageGoldenVerdict{Configured: true, Verdict: "requires_human", HumanSummaries: []string{goldenConcreteSummary}}
	for _, c := range []stageGoldenCase{
		{
			Name: "regression/12713-12722-human-pr-red-ci-is-071",
			Note: "hivecommons/hive#12722: a human-authored contributor PR with red CI is 071 needs-author, not 072.",
			Input: stageGoldenInput{Observations: []stageGoldenObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role", HeadSHA: goldenHead},
				{Lane: stageLaneContributor, Reason: "required-check-failing:unit", HeadSHA: goldenHead, Detail: stageGoldenDetail{Role: "contributor"}},
			}, AuthorKind: string(authorkind.Human)},
		},
		{
			Name: "regression/12713-12722-clanker-pr-red-ci-is-072",
			Note: "hivecommons/hive#12722: the same PR by a clanker is 072 returned-to-contributor.",
			Input: stageGoldenInput{Observations: []stageGoldenObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role", HeadSHA: goldenHead},
				{Lane: stageLaneContributor, Reason: "required-check-failing:unit", HeadSHA: goldenHead, Detail: stageGoldenDetail{Role: "contributor"}},
			}, AuthorKind: string(authorkind.Clanker)},
		},
		{
			Name: "regression/12713-12722-hive-pr-red-ci-is-070",
			Note: "hivecommons/hive#12722: the App's own PR stays 070 fixing whatever lane saw the red.",
			Input: stageGoldenInput{Observations: []stageGoldenObservation{
				{Lane: stageLaneSelf, Reason: "required-check-failing:unit", HeadSHA: goldenHead, Detail: stageGoldenDetail{Role: "trusted"}},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonAppAuthored, HeadSHA: goldenHead},
			}, AuthorKind: string(authorkind.Hive)},
		},
		{
			Name: "regression/12722-author-hive-label-on-trusted-lane-is-070",
			Note: "A trusted-author lane PR carrying author/hive (another hive's App) reworks at 070 and shows role/trusted.",
			Input: stageGoldenInput{Observations: []stageGoldenObservation{
				{Lane: stageLaneTrusted, Reason: "check-failing:lint", HeadSHA: goldenHead, Labels: []string{"author/hive"}},
			}, AuthorKind: string(authorkind.Hive)},
		},
		{
			Name: "regression/12737-adopted-pr-keeps-074-while-pending",
			Note: "hivecommons/hive#12737: a superseded contributor PR whose CI went pending stays at 074 while the adoption is live.",
			Input: stageGoldenInput{Observations: single(stageLaneContributor, "check-pending", stageGoldenDetail{}), AuthorKind: string(authorkind.Clanker),
				Prev: &stageGoldenRecord{Stage: "074-adopted-by-hive", Reason: "check-failing:unit", AdoptedHead: goldenHead, AdoptMode: stagelabels.AdoptModeSupersede}},
		},
		{
			Name: "regression/12737-adopted-pr-keeps-074-when-approved",
			Note: "hivecommons/hive#12737: an approve verdict does not release a live adoption (060 yields to 074).",
			Input: stageGoldenInput{Observations: single(stageLaneContributor, "check-pending", stageGoldenVerdicts[1].detail("")), AuthorKind: string(authorkind.Clanker),
				Prev: &stageGoldenRecord{Stage: "074-adopted-by-hive", AdoptedHead: goldenHead, AdoptMode: stagelabels.AdoptModeSupersede}},
		},
		{
			Name: "regression/12737-merge-gate-takes-adopted-pr",
			Note: "hivecommons/hive#12737: at the merge gate (100) the adoption gives way.",
			Input: stageGoldenInput{Observations: single(stageLaneContributor, contributorLaneReasonSoaking+"(remaining=1m)", stageGoldenVerdicts[1].detail("")), AuthorKind: string(authorkind.Clanker),
				Prev: &stageGoldenRecord{Stage: "074-adopted-by-hive", AdoptedHead: goldenHead, AdoptMode: stagelabels.AdoptModeSupersede}},
		},
		{
			Name: "regression/12737-new-contributor-head-releases-supersede-adoption",
			Note: "A superseding adoption is tied to the adopted head; a new head reads as a plain 072.",
			Input: stageGoldenInput{Observations: single(stageLaneContributor, "check-failing:unit", stageGoldenDetail{}), AuthorKind: string(authorkind.Clanker),
				Prev: &stageGoldenRecord{Stage: "074-adopted-by-hive", AdoptedHead: "0ld0000000", AdoptMode: stagelabels.AdoptModeSupersede}},
		},
		{
			Name: "regression/12856-pr-needs-human-is-085-never-045",
			Note: "hivecommons/hive#12856: the PR side keeps parking at 085; 045 is issue-only.",
			Input: stageGoldenInput{Observations: single(stageLaneContributor, "label:needs-human", stageGoldenDetail{}), AuthorKind: string(authorkind.Human),
				Route: &stageGoldenRoute{Checks: green, Verdict: &stageGoldenVerdict{Configured: true, Approved: true, Verdict: "approve"}}},
		},
		{
			Name:  "regression/12856-pr-wearing-045-is-reclassified-by-lane",
			Note:  "hivecommons/hive#12856: a PR found wearing the issue park label is staged from the lane evidence alone (the label is not a reason).",
			Input: stageGoldenInput{Observations: []stageGoldenObservation{{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, HeadSHA: goldenHead, Labels: []string{"stage/045-needs-decision"}}}, AuthorKind: string(authorkind.Human)},
		},
		{
			Name: "regression/12403-registry-trusted-author-not-flipped-to-090",
			Note: "hivecommons/hive#12403: the trusted-author lane's role check must not flip a registry-trusted author to 090 while the contributor lane has no classified opinion.",
			Input: stageGoldenInput{Observations: []stageGoldenObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role", HeadSHA: goldenHead},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonRegistryTrusted, HeadSHA: goldenHead, Detail: stageGoldenDetail{Role: "trusted"}},
			}, AuthorKind: string(authorkind.Human)},
		},
		{
			Name: "regression/12417-requires-human-verdict-hands-off-not-review-pending",
			Note: "hivecommons/hive#12417/#12438: a requires_human verdict folded into no-verdict by the lane still hands the PR to a person.",
			Input: stageGoldenInput{Observations: single(stageLaneTrusted, contributorLaneReasonNoVerdict, stageGoldenVerdicts[4].detail("")), AuthorKind: string(authorkind.Human),
				Route: &stageGoldenRoute{Checks: green, Verdict: stageGoldenVerdicts[4].verdict}},
		},
		{
			Name: "regression/15202-human-author-decision-requested-conflicting-is-071",
			Note: "hivecommons/hive#15202 (seen on #15157): a decision-requested verdict on a conflicting head a person pushed themselves goes to 071 Needs author with the rebase, not to 085 and never to adoption.",
			Input: stageGoldenInput{Observations: single(stageLaneContributor, "conflicting", stageGoldenDetail{Verdict: humanDecisionRequested, Role: "trusted"}), AuthorKind: string(authorkind.Human),
				Route: &stageGoldenRoute{Checks: green, Verdict: humanDecisionRequested}},
		},
		{
			Name: "regression/15202-human-author-decision-requested-green-is-071-with-question",
			Note: "hivecommons/hive#15202: the same verdict on a green head routes the reviewer's question to the author (071), not to the decision queue (085).",
			Input: stageGoldenInput{Observations: single(stageLaneContributor, contributorLaneReasonNoVerdict, stageGoldenDetail{Verdict: humanDecisionRequested, Role: "trusted"}), AuthorKind: string(authorkind.Human),
				Route: &stageGoldenRoute{Checks: green, Verdict: humanDecisionRequested}},
		},
		{
			Name:  "regression/concrete-requires-human-normalizes-to-changes-requested",
			Note:  "A requires_human verdict whose summary names a concrete remediation is 07x with reviewer-verdict:changes_requested.",
			Input: stageGoldenInput{Observations: single(stageLaneSelf, contributorLaneReasonNoVerdict, stageGoldenDetail{Verdict: requiresHumanConcrete}), AuthorKind: string(authorkind.Hive)},
		},
		{
			Name: "regression/normalized-fixing-guard-blocks-raw-human-flap",
			Note: "PreserveNormalizedFixingStage: the raw requires_human must not flap a PR already at 070/changes_requested on the same head back to 085.",
			Input: stageGoldenInput{Observations: single(stageLaneSelf, "label:needs-human", stageGoldenDetail{Verdict: requiresHumanConcrete}), AuthorKind: string(authorkind.Hive),
				Prev: &stageGoldenRecord{Stage: "070-fixing", Reason: stagelabels.LaneReasonChangesRequested, ActivityHead: goldenHead}},
		},
		{
			Name:  "regression/12538-red-optional-check-never-merge-queue",
			Note:  "hivecommons/hive#12538 (#12457): a red non-required check on the head is the repair stage, never 100.",
			Input: stageGoldenInput{Observations: single(stageLaneSelf, contributorLaneReasonSoaking+"(remaining=10m)", stageGoldenDetail{RedChecks: []string{"docs-link-check"}, RedChecksHead: goldenHead}), AuthorKind: string(authorkind.Hive)},
		},
		{
			Name:  "regression/13020-soaking-is-095-not-merge-queue",
			Note:  "hivecommons/hive#13020: a lane-admitted head inside its soak window is 095 pre-merge-soak; 100 merge-queue is reached only once the soak is complete.",
			Input: stageGoldenInput{Observations: single(stageLaneSelf, contributorLaneReasonSoaking+"(remaining=12m0s)", stageGoldenDetail{}), AuthorKind: string(authorkind.Hive)},
		},
		{
			Name:  "regression/13020-soak-complete-is-merge-queue",
			Note:  "hivecommons/hive#13020: soak done and only GitHub (required-checks-expected) in the way → 100 merge-queue.",
			Input: stageGoldenInput{Observations: single(stageLaneSelf, "required-checks-expected", stageGoldenDetail{}), AuthorKind: string(authorkind.Hive)},
		},
		{
			Name:  "regression/12606-shared-ci-red-is-review-pending",
			Note:  "hivecommons/hive#12606: red CI deferred to a shared-ci incident is the hive's, not the author's.",
			Input: stageGoldenInput{Observations: []stageGoldenObservation{{Lane: stageLaneContributor, Reason: "required-check-failing:unit", HeadSHA: goldenHead, SharedCIIncident: 9100}}, AuthorKind: string(authorkind.Clanker)},
		},
		{
			Name: "regression/12542-unsigned-head-in-dco-repo-is-072",
			Note: "hivecommons/hive#12542: an unsigned head where the repo enforces DCO goes straight back to its author.",
			Input: stageGoldenInput{Observations: single(stageLaneContributor, contributorLaneReasonNoVerdict, stageGoldenDetail{DCOGate: true, DCOGateHead: goldenHead}), AuthorKind: string(authorkind.Clanker),
				DCO: &stageGoldenDCO{Total: 3, Unsigned: 1}},
		},
		{
			Name:  "regression/trusted-human-approval-clears-requires-human-same-head",
			Note:  "A trusted human's APPROVED review on the current head resolves requires_human into the pre-merge soak (095) with the approver in the reason.",
			Input: stageGoldenInput{Observations: single(stageLaneContributor, contributorLaneReasonSoaking+"(remaining=30m)", stageGoldenVerdicts[7].detail("contributor")), AuthorKind: string(authorkind.Human)},
		},
		{
			Name: "regression/12528-trust-only-handoff-green-approved-is-090",
			Note: "hivecommons/hive#12528: only trust blocks it and everything else is in place → 090 ready-for-human-merge.",
			Input: stageGoldenInput{Observations: []stageGoldenObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role", HeadSHA: goldenHead},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonClaimTier, HeadSHA: goldenHead, Detail: stageGoldenDetail{Role: "newcomer"}},
			}, AuthorKind: string(authorkind.Human), Route: &stageGoldenRoute{Checks: stageGoldenChecks{Total: 4, Green: 4, Known: true, DCO: 1}, Verdict: &stageGoldenVerdict{Configured: true, Approved: true, Verdict: "approve"}}},
		},
	} {
		add("regressions.json", c)
	}

	for file := range out {
		sort.Slice(out[file], func(i, j int) bool { return out[file][i].Name < out[file][j].Name })
	}
	return out
}

func marshalStageGoldenCases(cases []stageGoldenCase) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, c := range cases {
		b, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		if i < len(cases)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("]\n")
	return buf.Bytes(), nil
}

func TestStageGoldens(t *testing.T) {
	generated := generateStageGoldens()
	if *updateStageGoldens {
		if err := os.RemoveAll(stageGoldenDir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(stageGoldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		total := 0
		for file, cases := range generated {
			b, err := marshalStageGoldenCases(cases)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stageGoldenDir, file), b, 0o644); err != nil {
				t.Fatal(err)
			}
			total += len(cases)
		}
		t.Logf("wrote %d stage goldens across %d files", total, len(generated))
		return
	}

	files, err := filepath.Glob(filepath.Join(stageGoldenDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no fixtures under %s; run with -update", stageGoldenDir)
	}
	if len(files) != len(generated) {
		t.Errorf("fixture dir holds %d files, generator produces %d; run with -update", len(files), len(generated))
	}
	seen := map[string]bool{}
	total := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cases []stageGoldenCase
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		file := filepath.Base(path)
		gen := map[string]stageGoldenCase{}
		for _, c := range generated[file] {
			gen[c.Name] = c
		}
		if len(gen) != len(cases) {
			t.Errorf("%s: fixture holds %d cases, generator produces %d; run with -update", file, len(cases), len(gen))
		}
		for _, c := range cases {
			total++
			if seen[c.Name] {
				t.Errorf("%s: duplicate case %q", file, c.Name)
			}
			seen[c.Name] = true
			g, ok := gen[c.Name]
			if !ok {
				t.Errorf("%s: case %q is not produced by the generator; run with -update", file, c.Name)
				continue
			}
			if gb, cb := goldenJSON(t, g.Input), goldenJSON(t, c.Input); gb != cb {
				t.Errorf("%s: case %q input drifted from the generator:\n fixture   %s\n generator %s", file, c.Name, cb, gb)
			}
			got := runStageGolden(c.Input)
			if gb, cb := goldenJSON(t, got), goldenJSON(t, c.Want); gb != cb {
				t.Errorf("%s: case %q\n got  %s\n want %s", file, c.Name, gb, cb)
			}
		}
		if want, err := marshalStageGoldenCases(cases); err != nil || !bytes.Equal(want, raw) {
			t.Errorf("%s: not in canonical form; run with -update", file)
		}
	}
	t.Logf("replayed %d stage goldens", total)
}

func goldenJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestStageGoldensEngineParity is the shadow gate in miniature: for every
// golden the engine's PR deriver, given the Facts the pass submits (lane
// observations plus PRPassFacts), must land on the fixture's final stage
// and reason — what ApplyStageLabels writes — not merely ResolveStage's
// opinion. A fixture the pass skips must be an Abstain.
func TestStageGoldensEngineParity(t *testing.T) {
	deriver := PRDeriver()
	total := 0
	for file, cases := range generateStageGoldens() {
		for _, c := range cases {
			total++
			want, facts := runStageGoldenWithFacts(c.Input)
			got := deriver.Derive(facts)
			if want.Skipped {
				if !got.Abstain {
					t.Errorf("%s: %q: pass skips the PR, engine derived %s (%s)", file, c.Name, got.Stage, got.Reason)
				}
				continue
			}
			if got.Abstain {
				t.Errorf("%s: %q: engine abstained (%s), pass wrote %s", file, c.Name, got.Reason, want.FinalStage)
				continue
			}
			if want.FinalStage == "terminal" {
				if !got.Terminal {
					t.Errorf("%s: %q: pass is terminal, engine derived %s", file, c.Name, got.Stage)
				}
				continue
			}
			if got.Terminal || got.Stage.String() != want.FinalStage || got.Reason != want.FinalReason {
				t.Errorf("%s: %q: engine %s (%s), pass wrote %s (%s)", file, c.Name, got.Stage, got.Reason, want.FinalStage, want.FinalReason)
			}
			if want.FinalDCO != "" && dcoGoldenString(got.DCO) != want.FinalDCO {
				t.Errorf("%s: %q: engine DCO %s, pass wrote %s", file, c.Name, dcoGoldenString(got.DCO), want.FinalDCO)
			}
		}
	}
	t.Logf("engine reproduced %d stage goldens", total)
}

// TestStageGoldensCoverEveryPRStage guards the corpus: every PR-side stage,
// None and the terminal outcome are reached by at least one fixture, and no
// fixture derives an issue-only stage for a PR.
func TestStageGoldensCoverEveryPRStage(t *testing.T) {
	reached := map[string]bool{}
	for _, cases := range generateStageGoldens() {
		for _, c := range cases {
			reached[c.Want.FinalStage] = true
		}
	}
	want := []string{stagelabels.None.String(), "terminal"}
	for _, d := range stagelabels.Table {
		if d.PR {
			want = append(want, d.Stage.String())
		}
	}
	for _, s := range want {
		if !reached[s] {
			t.Errorf("no golden reaches %s", s)
		}
	}
	for _, d := range stagelabels.Table {
		if !d.PR && reached[d.Stage.String()] {
			t.Errorf("a golden derived issue-only stage %s for a PR", d.Stage)
		}
	}
}

// Fixture-only gathering policy snapshots; derivation stays in the engine.
func humanVerdict(v ContributorLaneVerdict) stagelabels.VerdictState {
	v = normalizeContributorLaneVerdict(v)
	switch {
	case !v.Configured:
		return stagelabels.VerdictUnavailable
	case v.Approved:
		return stagelabels.VerdictApproved
	case VerdictBlocks(stageDetail{Verdict: v, VerdictKnown: true}):
		return stagelabels.VerdictChangesRequested
	case VerdictNeedsHuman(stageDetail{Verdict: v, VerdictKnown: true}):
		return stagelabels.VerdictRequiresHuman
	}
	return stagelabels.VerdictPending
}

func decisionCause(reason string) string {
	switch {
	case reason == "held", reason == "label:hold", reason == "label:needs-human", reason == "exempt-label",
		strings.HasPrefix(reason, "excluded-label:"), reason == "sentinel", reason == contributorLaneReasonSentinelBlocked:
		return ""
	case reason == contributorLaneReasonWorkflowPath:
		return prstage.CauseWorkflowPathManual
	case reason == contributorLaneReasonTestsRemoved:
		return prstage.CauseTestsRemoved
	case reason == contributorLaneReasonNoReviewer, reason == StageReasonVerdictRequiresHuman, reason == StageReasonDecisionRequested:
		// RouteHuman renders the verdict itself from the evidence.
		return ""
	case reason == contributorLaneReasonReporterTrustHold:
		return prstage.CauseReporterTrustHold
	case reason == autoMergeReasonHumanMergePath, reason == autoMergeReasonHumanMergePathEvidence:
		return prstage.CauseHumanMergePath
	case reason == autoMergeReasonIntentUnauthorized, reason == autoMergeReasonIntentEvidence:
		return prstage.CauseIntentUnauthorized
	case reason == autoMergeReasonWorkflowPermission:
		return "GitHub refused to bring the branch up to date: the base changed `.github/workflows/` files and the token the hive used lacks the Workflows permission, so no hive agent can push that update. A person must merge or rebase the base into the branch and push, or grant the App Workflows: write if the installation lacks it"
	case reason == autoMergeReasonForkWorkflowUpdate:
		return "GitHub refused to bring the branch up to date: the head branch is in a fork and the base changed `.github/workflows/` files, which the Hive App cannot push to a fork. The author, or a maintainer with push access when \"Allow edits by maintainers\" is on, must run `gh pr update-branch` on this PR or merge the base into the branch and push"
	case strings.HasPrefix(reason, workflowApprovalUntrustedPrefix):
		return prstage.CauseWorkflowApprovalUntrusted()
	case strings.HasPrefix(reason, workflowApprovalWorkflowMissingPrefix):
		return prstage.CauseWorkflowApprovalWorkflowMissing()
	case strings.HasPrefix(reason, workflowApprovalRequiredPrefix):
		return prstage.CauseWorkflowApprovalRequired()
	case reason == "approval-desk-withheld":
		return prstage.CauseApprovalDeskWithheld
	case strings.HasPrefix(reason, "linked-issue:"):
		if kind, issue, label, ok := prstage.ParseLinkedIssueReason(reason); ok {
			ref := "#" + strconv.Itoa(issue)
			if kind == prstage.LinkedIssueReasonHoldPrefix {
				return "issue " + ref + ", which this PR closes, is on hold (`" + label + "`); remove `" + label + "` from " + ref + " to release this PR"
			}
			return "issue " + ref + ", which this PR closes, is parked for a decision (`" + label + "`); once it is decided, remove `" + label + "` from " + ref + " to release this PR"
		}
	case reason == "":
		return ""
	}
	return prstage.CauseLaneReported(reason)
}

var humanConflictReasons = map[string]bool{"conflicting": true, "not-mergeable": true, "label:needs-rebase": true}

// humanConflict mirrors automerge's: the lane's own conflict reason counts
// for a person's PR even when a decision-requested verdict took the stage
// reason (hivecommons/hive#15202).
func humanConflict(obs stageObservation) bool {
	return humanConflictReasons[obs.Reason] || HasLabelFold(obs.Labels, "needs-rebase") ||
		obs.AuthorKind == authorkind.Human && humanConflictReasons[obs.LaneReason]
}

const defaultSentinelLabel = "sentinel-alert"

func dcoState(total, unsigned int) stagelabels.DCOState {
	if total == 0 {
		return stagelabels.DCOUnknown
	}
	if unsigned > 0 {
		return stagelabels.DCONo
	}
	return stagelabels.DCOYes
}

const contributorLaneReasonRegistryTrusted = "contributor-lane:" + "registry-trusted"
const workflowApprovalUntrustedPrefix = "workflow-approval-required:untrusted-commenter:"
const workflowApprovalWorkflowMissingPrefix = "workflow-approval-required:missing-workflow:"
