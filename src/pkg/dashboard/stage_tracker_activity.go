package dashboard

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// StageTrackerActivity keeps independent observations separate from the lane's
// native stage and next mover. Worker activity requires assignment AND worker;
// CI activity comes only from observed required checks.
type StageTrackerActivity struct {
	Head    string `json:"head"`
	State   string `json:"state"`
	Caption string `json:"caption"`
	// Detail is the longer, plain-language sentence behind Caption: what
	// the state means and what would move it. The chevron clamps Caption;
	// the tooltip carries Detail in full.
	Detail     string     `json:"detail,omitempty"`
	Actor      string     `json:"actor,omitempty"`
	Source     string     `json:"source"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	Since      *time.Time `json:"since,omitempty"`
	Blocker    string     `json:"blocker,omitempty"`
	URL        string     `json:"url,omitempty"`
	Active     bool       `json:"active,omitempty"`
	// Check counts on the current head for the CI track (hivecommons/hive#16484):
	// over the required checks when branch protection is known, otherwise
	// over every check run observed on the head. ChecksTotal == 0 means no
	// count could be made ("CI ?/?").
	ChecksPassed  int      `json:"checks_passed,omitempty"`
	ChecksTotal   int      `json:"checks_total,omitempty"`
	ChecksFailed  int      `json:"checks_failed,omitempty"`
	ChecksFailing []string `json:"checks_failing,omitempty"`
	// Perspective counts on the current head for the Review track: Total is
	// the swarm's expected set for this PR (configured perspectives plus any
	// extra one dispatched or reported for the head), Done the ones with a
	// verdict for the current head.
	PerspectivesDone  int                       `json:"perspectives_done,omitempty"`
	PerspectivesTotal int                       `json:"perspectives_total,omitempty"`
	Perspectives      []StageTrackerPerspective `json:"perspectives,omitempty"`
}

// StageTrackerPerspective describes the latest bundle verdict for one perspective
// on the current head, preserving dissent across reviewers. Summary is the
// first finding, when one was reported.
type StageTrackerPerspective struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
}

const trackerActivityFreshWindow = 2 * time.Minute

func trackerUnknownActivity(head, caption string) *StageTrackerActivity {
	return &StageTrackerActivity{Head: head, State: "unknown", Caption: caption, Source: "unavailable"}
}

func stageTrackerCIActivity(pr github.PullRequest, now time.Time) *StageTrackerActivity {
	out := trackerUnknownActivity(pr.HeadSHA, "CI unknown · no check results observed for this head yet")
	out.Detail = "hive has not yet seen which checks branch protection requires, or any check run or commit status for the PR's current commit."
	a := pr.CIActivity
	if a == nil || pr.HeadSHA == "" || !strings.EqualFold(a.Head, pr.HeadSHA) {
		return out
	}
	out.Source, out.ObservedAt = a.Source, a.ObservedAt
	if a.RequiredSource != "" {
		out.Source += "+required-" + a.RequiredSource
	}
	if a.StatusesKnown {
		out.Source += "+github-commit-statuses"
	}
	if !a.RequiredKnown || a.RequiredSource == "" || len(a.RequiredChecks) == 0 || len(a.Required) == 0 {
		stageTrackerCountHeadChecks(out, a)
		stageTrackerHeadChecksMotion(out, a)
		return out
	}
	type requiredObservation struct {
		name   string
		checks []github.PRActivityCheck
	}
	var observations []requiredObservation
	for _, name := range a.Required {
		found := false
		for _, binding := range a.RequiredChecks {
			if binding.Name == name {
				scoped := *a
				scoped.RequiredChecks = []github.PRRequiredCheck{binding}
				checks := scoped.ChecksForRequired(name)
				var runs, statuses []github.PRActivityCheck
				for _, check := range checks {
					if check.Kind == "commit_status" {
						statuses = append(statuses, check)
					} else {
						runs = append(runs, check)
					}
				}
				if len(statuses) > 0 && binding.AppKnown && (binding.AppID == nil || *binding.AppID == -1) {
					observations = append(observations, requiredObservation{name, statuses})
					if len(runs) > 0 {
						observations = append(observations, requiredObservation{name, runs})
					}
				} else {
					observations = append(observations, requiredObservation{name, checks})
				}
				found = true
			}
		}
		if !found {
			stageTrackerCountHeadChecks(out, a)
			stageTrackerHeadChecksMotion(out, a)
			return out
		}
	}
	out.State, out.Caption = "passing", "CI green"
	// Per required-check name: the leading state across its observations,
	// for the passed / failed counts.
	perName := map[string]string{}
	running, queued, queuedName := false, false, ""
	// Per required-check name: which have a final result, for the running count.
	settled := map[string]bool{}
	for _, name := range a.Required {
		settled[name] = true
	}
	for _, observation := range observations {
		name := observation.name
		check := github.PRActivityCheck{}
		ok := len(observation.checks) > 0
		if ok {
			check = observation.checks[0]
		}
		state := "unknown"
		switch {
		case len(observation.checks) > 1:
			state = "unknown"
		case !ok:
			if a.StatusesKnown {
				state = "missing"
			}
		case check.State == "queued" || check.State == "waiting" || check.State == "pending" || check.State == "requested":
			state = "queued"
			queued = true
			queuedName = name
		case check.State == "in_progress":
			state = "running"
			running = true
		case check.State == "completed":
			switch check.Conclusion {
			case "success":
				state = "passing"
			case "neutral", "skipped":
				state = "skipped"
			case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
				state = "failing"
			}
		}
		if state != "passing" && state != "failing" && state != "skipped" {
			settled[name] = false
		}
		// A red required check leads, then missing/unknown, then running/queued.
		rank := map[string]int{"passing": 0, "queued": 1, "running": 2, "skipped": 3, "unknown": 4, "missing": 5, "failing": 6}
		if previous, seen := perName[name]; !seen || rank[state] >= rank[previous] {
			perName[name] = state
		}
		if rank[state] >= rank[out.State] {
			out.State, out.Since, out.URL = state, check.Since, check.URL
			out.ObservedAt = a.ObservedAt
			if check.Kind == "commit_status" {
				out.ObservedAt = a.StatusesObservedAt
			}
			if state != "passing" {
				out.Blocker = name
			}
		}
	}
	statusesMissing := out.State == "passing" && a.NeedsCommitStatusEvidence() && !a.StatusesKnown
	if statusesMissing {
		out.State, out.Blocker = "unknown", "commit statuses not observed"
	}
	// Motion on the head leads every settled word: while any required
	// check still runs or waits for a runner the track is running, however
	// red, unreported or ambiguous its neighbours are — a ring that is not
	// yet full is a running ring, so the chevron stays active with its
	// count (hivecommons/hive#16631). The red names ride in ChecksFailing.
	switch {
	case running:
		out.State, out.Blocker = "running", ""
	case queued && out.State != "queued":
		out.State, out.Blocker = "queued", queuedName
	}
	done := 0
	for _, name := range a.Required {
		if settled[name] {
			done++
		}
	}
	total := len(a.Required)
	out.ChecksTotal = total
	for _, name := range a.Required {
		switch perName[name] {
		case "passing":
			out.ChecksPassed++
		case "failing":
			out.ChecksFailed++
			out.ChecksFailing = append(out.ChecksFailing, name)
		}
	}
	check := fmt.Sprintf("%q", out.Blocker)
	switch out.State {
	case "passing":
		out.Caption = "CI green · all " + plural(total, "required check") + " passed"
		out.Detail = "Every check that branch protection requires has reported success for the PR's current commit."
	case "failing":
		out.Caption = "CI failing · " + check + " red"
		out.Detail = "Required check " + check + " finished without success (failed, timed out, was cancelled, or needs action) on the PR's current commit. A fix or a re-run has to turn it green before CI can pass."
	case "running":
		out.Caption = fmt.Sprintf("CI running · %d/%d checks done", done, total)
		out.Detail = fmt.Sprintf("%d of the %s have a final result; the rest are still executing or waiting for a runner on the PR's current commit.", done, plural(total, "required check"))
		if out.ChecksFailed > 0 {
			out.Caption += fmt.Sprintf(" · %d failing", out.ChecksFailed)
			out.Detail += fmt.Sprintf(" %s already red: %s.", plural(out.ChecksFailed, "required check"), strings.Join(out.ChecksFailing, ", "))
		}
	case "queued":
		out.Caption = "CI queued · " + check + " waiting for a runner"
		out.Detail = "Required check " + check + " has been requested for the PR's current commit but has not started executing yet."
	case "missing":
		if wa := a.WorkflowApproval; wa != nil && wa.Pending > 0 {
			// Unreported because GitHub holds the runs behind its
			// approve-workflows gate: a person's click, not a push,
			// moves it (hivecommons/hive#16599).
			out.Blocker = stageTrackerWorkflowApprovalBlocker(wa.Pending)
			out.Caption = fmt.Sprintf("CI waiting · %s %s approval on GitHub", plural(wa.Pending, "workflow run"), pluralVerb(wa.Pending, "needs", "need"))
			out.Detail = fmt.Sprintf("%s for the PR's current commit %s waiting for a maintainer to approve workflows on GitHub (the gate GitHub applies to dependabot and first-time or fork contributors), so required check %s cannot report until someone approves them.", plural(wa.Pending, "workflow run"), pluralVerb(wa.Pending, "is", "are"), check)
			if wa.Actor != "" {
				out.Actor = wa.Actor
			}
			if wa.ObservedAt != nil {
				out.Since = wa.ObservedAt
			}
			break
		}
		out.Caption = "CI waiting · required check " + check + " hasn't reported for this head yet"
		out.Detail = "Branch protection requires a check named " + check + ", but no check run or commit status with that name exists for the PR's current commit. Usually the workflow that produces it has not been triggered; a push, re-run, or label can start it."
	case "unknown":
		if statusesMissing {
			out.Caption = "CI unknown · commit statuses not observed for this head"
			out.Detail = "A required check is reported as a commit status rather than a check run, and hive has not yet fetched commit statuses for the PR's current commit."
		} else {
			out.Caption = "CI unknown · " + check + " reported but with no conclusion"
			out.Detail = "Required check " + check + " exists for the PR's current commit but hive cannot read a clear result from it (several runs share the name, or the run ended with no conclusion)."
		}
	case "skipped":
		out.Caption = "CI skipped · check " + check + " skipped"
		out.Detail = "Required check " + check + " finished as skipped or neutral on the PR's current commit, which branch protection does not count as a pass."
	}
	// Running is active, full stop: the chevron has one fill. The poll's
	// age rides ObservedAt for the tooltip, never a dimmer state
	// (hivecommons/hive#16643).
	out.Active = out.State == "running" || out.State == "queued"
	return out
}

// stageTrackerWorkflowApprovalBlockerPrefix leads the CI blocker when the
// unreported checks are workflow runs waiting on GitHub's approve-workflows
// gate; the strip reads the count after the colon.
const stageTrackerWorkflowApprovalBlockerPrefix = "workflow-approval-required:"

func stageTrackerWorkflowApprovalBlocker(n int) string {
	return fmt.Sprintf("%s%d", stageTrackerWorkflowApprovalBlockerPrefix, n)
}

func pluralVerb(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// stageTrackerCountHeadChecks fills the check counts from every check run
// observed on the head when the required set is not known: one entry per
// check name, red if any run of that name is red, green only if its latest
// run succeeded.
func stageTrackerCountHeadChecks(out *StageTrackerActivity, a *github.PRCIActivity) {
	type tally struct {
		passing, failing bool
		since            time.Time
	}
	byName := map[string]*tally{}
	var order []string
	for _, check := range a.Checks {
		if check.Head != "" && !strings.EqualFold(check.Head, a.Head) {
			continue
		}
		t := byName[check.Name]
		if t == nil {
			t = &tally{}
			byName[check.Name] = t
			order = append(order, check.Name)
		}
		at := time.Time{}
		if check.Since != nil {
			at = *check.Since
		}
		red := check.State == "completed" && (check.Conclusion == "failure" || check.Conclusion == "timed_out" || check.Conclusion == "cancelled" || check.Conclusion == "action_required" || check.Conclusion == "startup_failure" || check.Conclusion == "stale")
		if red {
			t.failing = true
		}
		if !at.Before(t.since) {
			t.since, t.passing = at, check.State == "completed" && check.Conclusion == "success"
		}
	}
	out.ChecksTotal = len(order)
	for _, name := range order {
		t := byName[name]
		switch {
		case t.failing:
			out.ChecksFailed++
			out.ChecksFailing = append(out.ChecksFailing, name)
		case t.passing:
			out.ChecksPassed++
		}
	}
}

// stageTrackerHeadChecksMotion turns the required-set-unknown count into a
// running track while any check run on the head is still executing or
// queued: the ring is not full, so the chevron must be active with its
// count rather than a silent part-green glyph (hivecommons/hive#16631).
func stageTrackerHeadChecksMotion(out *StageTrackerActivity, a *github.PRCIActivity) {
	moving := 0
	for _, check := range a.Checks {
		if check.Head != "" && !strings.EqualFold(check.Head, a.Head) {
			continue
		}
		switch check.State {
		case "in_progress", "queued", "waiting", "pending", "requested":
			moving++
		}
	}
	if moving == 0 {
		return
	}
	done := out.ChecksTotal - moving
	if done < 0 {
		done = 0
	}
	out.State, out.Blocker = "running", ""
	out.Caption = fmt.Sprintf("CI running · %d/%d checks done", done, out.ChecksTotal)
	out.Detail = fmt.Sprintf("%d of the %s observed on the PR's current commit have a final result; the rest are still executing or waiting for a runner. Which of them branch protection requires is not yet known.", done, plural(out.ChecksTotal, "check"))
	if out.ChecksFailed > 0 {
		out.Caption += fmt.Sprintf(" · %d failing", out.ChecksFailed)
	}
	out.Active = true
}

// stageTrackerExpectedPerspectives is the swarm's perspective set for this
// hive: the configured list (built-ins when empty), falling back to the
// built-ins when the configuration cannot be resolved.
func stageTrackerExpectedPerspectives(cfg *config.Config) []review.Perspective {
	var names []string
	var prompts map[string]string
	if cfg != nil {
		names, prompts = cfg.Review.Perspectives, cfg.Review.PerspectivePrompts
	}
	if set, err := review.NewPerspectiveSet(names, prompts); err == nil && set.Len() > 0 {
		return set.List()
	}
	return append([]review.Perspective(nil), review.DefaultPerspectives...)
}

// stageTrackerCountPerspectives fills the Review track's done / total
// perspective counts for the PR's current head: total is the expected set
// plus any extra perspective dispatched or reported for the head; done the
// perspectives with a verdict for the head (the aggregate's, or the latest
// verified bundle verdict).
func stageTrackerCountPerspectives(out *StageTrackerActivity, pr github.PullRequest, bundle *evidence.Bundle, agg *review.Aggregate, dispatch review.DispatchState, qualify func(string) string, expected []review.Perspective) {
	if out == nil || pr.HeadSHA == "" {
		return
	}
	total := map[review.Perspective]bool{}
	done := map[review.Perspective]bool{}
	for _, p := range expected {
		total[p] = true
	}
	if agg != nil && trackerHeadMatches(pr.Repo, pr.Number, pr.HeadSHA, agg.Repo, agg.Number, agg.HeadSHA) {
		for p, v := range agg.Perspectives {
			total[p] = true
			if v != "" {
				done[p] = true
			}
		}
	}
	if bundle != nil && trackerHeadMatches(pr.Repo, pr.Number, pr.HeadSHA, bundle.Repo, bundle.Number, bundle.HeadSHA) {
		for _, verdict := range trackerLatestBundleVerdicts(bundle) {
			if verdict.Perspective == "" {
				continue
			}
			total[review.Perspective(verdict.Perspective)] = true
			if verdict.Verdict != "" {
				done[review.Perspective(verdict.Perspective)] = true
			}
		}
	}
	for _, pending := range dispatch.Pending {
		if pending.Perspective != "" && !pending.Decision && trackerHeadMatches(pr.Repo, pr.Number, pr.HeadSHA, qualify(pending.Repo), pending.Number, pending.HeadSHA) {
			total[pending.Perspective] = true
		}
	}
	out.PerspectivesTotal, out.PerspectivesDone = len(total), 0
	for p := range done {
		if total[p] {
			out.PerspectivesDone++
		}
	}
	// A swarm that is requested or queued has not started this round: no
	// perspective of it is done, whatever an earlier round left behind.
	// "Review 5/5 · swarm requested" contradicts itself (hivecommons/hive#16540).
	if out.State == "unassigned" {
		out.PerspectivesDone = 0
		if out.PerspectivesTotal == 0 {
			out.PerspectivesTotal = len(expected)
		}
	}
}

// stageTrackerReviewPendingState resolves the Review track for a PR the
// stage itself says is under review (050) when no verdict settled it: the
// swarm is in progress, so the track is "reviewing" (active) with the
// count of perspectives that posted for the current head — never
// "unknown", which left review-pending rows with no lit chevron
// (hivecommons/hive#16599). Without a head bundle or aggregate no
// perspective has posted: done is 0, whatever earlier rounds left behind.
func stageTrackerReviewPendingState(out *StageTrackerActivity, stage int, stageSince *time.Time) {
	if out == nil || out.State != "unknown" || stage != trackerNumberReviewPending {
		return
	}
	bundleSeen := strings.Contains(out.Source, "verified-review-bundle")
	if !bundleSeen {
		out.PerspectivesDone = 0
	}
	total := out.PerspectivesTotal
	done := out.PerspectivesDone
	if total > 0 && done >= total && bundleSeen {
		out.State = "adjudicating"
		out.Caption = fmt.Sprintf("Review %d/%d · verdicts in · aggregating", done, total)
		out.Detail = "Every perspective has posted a verdict for the PR's current commit; hive is combining them into the review decision."
	} else {
		out.State = "reviewing"
		out.Caption = fmt.Sprintf("Review %d/%d · in progress", done, total)
		out.Detail = fmt.Sprintf("The PR is at review pending: %d of %s have posted a verdict for the PR's current commit; the rest are still reviewing or waiting for a reviewer.", done, plural(total, "perspective"))
	}
	out.Active = true
	// The stage label is the observation; its time is when the label
	// landed (stable across rebuilds, so the payload ETag does not move).
	if out.ObservedAt == nil && stageSince != nil {
		at := *stageSince
		out.ObservedAt = &at
	}
	if out.Since == nil && stageSince != nil {
		at := *stageSince
		out.Since = &at
	}
	if out.Source == "" || out.Source == "unavailable" {
		out.Source = "stage-review-pending"
	} else {
		out.Source += "+stage-review-pending"
	}
}

func trackerHeadMatches(repo string, number int, head, observedRepo string, observedNumber int, observedHead string) bool {
	return number == observedNumber && head != "" && strings.EqualFold(head, observedHead) && strings.EqualFold(repo, observedRepo)
}

// trackerPrefersVerdict reports whether candidate should replace previous as
// the current verdict: dissent beats approval, otherwise the newest wins.
func trackerPrefersVerdict(previous, candidate evidence.Verdict) bool {
	previousApproves, candidateApproves := previous.Verdict == "approve", candidate.Verdict == "approve"
	if previousApproves != candidateApproves {
		return previousApproves
	}
	return candidate.RecordedAt.After(previous.RecordedAt)
}

// trackerLatestBundleVerdicts keeps one verdict per reviewer (perspective,
// backend, model): the newest recording, with dissent breaking exact ties.
func trackerLatestBundleVerdicts(bundle *evidence.Bundle) []evidence.Verdict {
	latest := map[string]evidence.Verdict{}
	for _, verdict := range bundle.Verdicts {
		key := verdict.Perspective + "\x00" + verdict.Backend + "\x00" + verdict.Model
		previous, ok := latest[key]
		if !ok || verdict.RecordedAt.After(previous.RecordedAt) ||
			(verdict.RecordedAt.Equal(previous.RecordedAt) && trackerPrefersVerdict(previous, verdict)) {
			latest[key] = verdict
		}
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]evidence.Verdict, 0, len(keys))
	for _, key := range keys {
		out = append(out, latest[key])
	}
	return out
}

func trackerReviewPerspectives(bundle *evidence.Bundle) []StageTrackerPerspective {
	latest := map[string]evidence.Verdict{}
	for _, verdict := range trackerLatestBundleVerdicts(bundle) {
		if verdict.Perspective == "" {
			continue
		}
		previous, ok := latest[verdict.Perspective]
		// Each backend/model has its own current verdict. An approval by
		// another reviewer must not hide dissent that still blocks the head.
		if !ok || trackerPrefersVerdict(previous, verdict) {
			latest[verdict.Perspective] = verdict
		}
	}
	names := make([]string, 0, len(latest))
	for name := range latest {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]StageTrackerPerspective, 0, len(names))
	for _, name := range names {
		v := latest[name]
		p := StageTrackerPerspective{Name: name, Verdict: v.Verdict}
		if len(v.Findings) > 0 {
			p.Summary = v.Findings[0].Summary
		}
		out = append(out, p)
	}
	return out
}

func trackerBundleApproves(bundle *evidence.Bundle, agg *review.Aggregate) bool {
	latest := trackerLatestBundleVerdicts(bundle)
	if len(latest) == 0 || len(agg.Perspectives) == 0 {
		return false
	}
	perspectives := map[string]bool{}
	for _, verdict := range latest {
		if verdict.Verdict != "approve" || verdict.Perspective == "" || verdict.RecordedAt.IsZero() {
			return false
		}
		perspectives[verdict.Perspective] = true
	}
	for perspective, verdict := range agg.Perspectives {
		if verdict != review.Verdict("approve") || !perspectives[string(perspective)] {
			return false
		}
	}
	return true
}

// The normal worker map has one seat per PR. Tracks instead inspect each agent,
// so a reviewer and fixer on the same PR cannot overwrite each other's evidence.
func trackerAssignedWorker(status *StatusPayload, repo string, number int, agent string, since, now time.Time) *time.Time {
	if status == nil || agent == "" {
		return nil
	}

	qualify := stageTrackerRepoQualifier(status)
	for _, worker := range status.Agents {
		action := worker.LastAction
		if strings.EqualFold(worker.Name, agent) && worker.Busy == "working" && !worker.Paused && !worker.AwaitingCI && action != nil &&
			action.Current && agentCausedAuditActions[action.Action] && action.Number == number && strings.EqualFold(qualify(action.Repo), repo) {
			if at, err := time.Parse(time.RFC3339Nano, action.At); err == nil && !at.Before(since) && !at.After(now) && now.Sub(at) <= trackerActivityFreshWindow {
				return &at
			}
		}
	}
	return nil
}

func trackerAssignedCodeWorker(status *StatusPayload, repo string, number int, head, agent string, since, now time.Time) *crux.CodeWork {
	if status == nil || agent == "" || head == "" {
		return nil
	}
	qualify := stageTrackerRepoQualifier(status)
	for _, worker := range status.Agents {
		observation := worker.CodeWork
		action := worker.LastAction
		if !strings.EqualFold(worker.Name, agent) || worker.Busy != "working" || worker.Paused || worker.AwaitingCI ||
			action == nil || !action.Current || !agentCausedAuditActions[action.Action] ||
			action.Number != number || !strings.EqualFold(qualify(action.Repo), repo) ||
			observation == nil || observation.Source != crux.CodeWorkSource || observation.Path == "" ||
			!strings.EqualFold(observation.Repo, repo) || !strings.EqualFold(observation.Head, head) ||
			observation.At.IsZero() || observation.At.Before(since) || observation.At.After(now) ||
			observation.KickAt.IsZero() || observation.KickAt.Before(since) || observation.At.Before(observation.KickAt) ||
			observation.ObservedAt.IsZero() || observation.ObservedAt.Before(observation.At) || observation.ObservedAt.After(now) ||
			now.Sub(observation.At) > trackerActivityFreshWindow || now.Sub(observation.ObservedAt) > trackerActivityFreshWindow {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, action.At)
		if err != nil || at.Before(observation.KickAt) || at.After(now) {
			continue
		}
		return observation
	}
	return nil
}

// trackerPerspectiveFindingLine renders a dissenting perspective as
// "<name> ✗|requires human[: <summary>]" for the review caption, or "" for
// anything else. The tracker-row-strip.js perspectiveLine() mirror rebuilds
// this exact string to avoid repeating the finding; keep the two in step.
func trackerPerspectiveFindingLine(perspective StageTrackerPerspective) string {
	var mark string
	switch perspective.Verdict {
	case "changes_requested", "reject":
		mark = "✗"
	case "requires_human":
		mark = "requires human"
	default:
		return ""
	}
	line := perspective.Name + " " + mark
	if perspective.Summary != "" {
		line += ": " + perspective.Summary
	}
	return line
}

func stageTrackerReviewActivity(pr github.PullRequest, bundle *evidence.Bundle, agg *review.Aggregate, dispatch review.DispatchState, status *StatusPayload, now time.Time) *StageTrackerActivity {
	out := trackerUnknownActivity(pr.HeadSHA, "Review unknown · current-head evidence unavailable")
	if agg != nil && !trackerHeadMatches(pr.Repo, pr.Number, pr.HeadSHA, agg.Repo, agg.Number, agg.HeadSHA) {
		agg = nil
	}
	qualify := stageTrackerRepoQualifier(status)
	if bundle != nil && trackerHeadMatches(pr.Repo, pr.Number, pr.HeadSHA, bundle.Repo, bundle.Number, bundle.HeadSHA) {
		out.Source, out.ObservedAt = "verified-review-bundle", timePtr(bundle.UpdatedAt)
		out.Perspectives = trackerReviewPerspectives(bundle)
		for _, verdict := range trackerLatestBundleVerdicts(bundle) {
			if !verdict.RecordedAt.IsZero() && (out.Since == nil || verdict.RecordedAt.After(*out.Since)) {
				at := verdict.RecordedAt
				out.Since = &at
			}
		}
		for _, posted := range bundle.PostedReviews {
			if strings.EqualFold(posted.Head, pr.HeadSHA) {
				out.URL = posted.URL
			}
		}
		if agg != nil && trackerHeadMatches(pr.Repo, pr.Number, pr.HeadSHA, agg.Repo, agg.Number, agg.HeadSHA) && len(bundle.Verdicts) > 0 {
			switch string(agg.Verdict) {
			case "approve":
				if agg.MergeEligible && trackerBundleApproves(bundle, agg) {
					out.State, out.Caption = "approved", "Review approved"
				} else {
					out.Caption = "Review unknown · bundle and aggregate do not establish approval"
				}
			case "changes_requested", "reject":
				out.State, out.Caption = "changes_requested", "Review findings · awaiting author"
				out.Blocker = stageTrackerFixerExhaustedSummary(agg)
			case "requires_human":
				out.State, out.Caption = "human_decision", "Review findings · needs owner sign-off"
				out.Blocker = agg.DecisionQuestion
			}
		}
		if out.State == "unknown" && (agg == nil || string(agg.Verdict) != "approve") {
			for _, verdict := range trackerLatestBundleVerdicts(bundle) {
				if verdict.Verdict == "changes_requested" || verdict.Verdict == "reject" {
					out.State, out.Caption = "changes_requested", "Review findings · awaiting author"
					if len(verdict.Findings) > 0 {
						out.Blocker = verdict.Findings[0].Summary
					}
					break
				}
			}
		}
		if out.State != "unknown" && agg != nil {
			var confirmed time.Time
			for _, recent := range dispatch.Recent {
				if recent.Agent != "" && !recent.Decision && !recent.Confirmed.IsZero() && !recent.Confirmed.After(now) &&
					trackerHeadMatches(pr.Repo, pr.Number, pr.HeadSHA, qualify(recent.Repo), recent.Number, recent.HeadSHA) &&
					recent.Confirmed.After(confirmed) {
					for _, receipt := range agg.ReportReceipts {
						if receipt.Agent == recent.Agent && receipt.Perspective == recent.Perspective &&
							receipt.Head == recent.Head && !receipt.Decided && !receipt.ReceivedAt.IsZero() &&
							!receipt.ReceivedAt.Before(recent.Confirmed) && !receipt.ReceivedAt.After(now) {
							confirmed, out.Actor = recent.Confirmed, recent.Agent
						}
					}
				}
			}
			if out.Actor != "" {
				out.Caption += " · " + out.Actor
			}
		}
	}
	for _, wait := range dispatch.Waits {
		if trackerHeadMatches(pr.Repo, pr.Number, pr.HeadSHA, qualify(wait.Repo), wait.Number, wait.HeadSHA) && out.State == "unknown" {
			out.State, out.Caption, out.Source = "unassigned", "Review queued · unassigned", "review-dispatch-wait"
			out.Since, out.ObservedAt, out.Blocker = timePtr(wait.FirstAt), timePtr(wait.LastAt), wait.Reason
		}
	}
	hadApproval := out.State == "approved"
	approvalAt := out.Since
	for _, pending := range dispatch.Pending {
		if !trackerHeadMatches(pr.Repo, pr.Number, pr.HeadSHA, qualify(pending.Repo), pending.Number, pending.HeadSHA) ||
			pending.Dispatched.IsZero() || pending.Dispatched.After(now) || now.Sub(pending.Dispatched) > review.DefaultPendingReviewTTL {
			continue
		}
		if out.State == "human_decision" {
			continue
		}
		settled := false
		if agg != nil && !agg.RecordedAt.After(now) {
			currentAggregate := *agg
			currentAggregate.ReportReceipts = nil
			for _, receipt := range agg.ReportReceipts {
				if !receipt.ReceivedAt.After(now) {
					currentAggregate.ReportReceipts = append(currentAggregate.ReportReceipts, receipt)
				}
			}
			currentPending := pending
			currentPending.Repo = qualify(pending.Repo)
			settled = review.PendingReviewCompleted(currentPending, review.Artifact{Items: []review.Aggregate{currentAggregate}})
		}
		if settled {
			continue
		}
		out.State, out.Caption, out.Actor, out.Source = "assigned", "Review assigned", pending.Agent, "review-dispatch"
		if pending.Agent == "" {
			out.State, out.Caption = "unassigned", "Review queued · unassigned"
		}
		if pending.Decision {
			out.Caption = "Review adjudication assigned"
			if pending.Agent == "" {
				out.Caption = "Review adjudication queued · unassigned"
			}
		}
		out.Since, out.ObservedAt = timePtr(pending.Dispatched), timePtr(dispatch.GeneratedAt)
		workerAt := trackerAssignedWorker(status, pr.Repo, pr.Number, pending.Agent, pending.Dispatched, now)
		out.Active = workerAt != nil
		if out.Active {
			out.State, out.Caption = "reviewing", "Review working"
			out.Source, out.ObservedAt = "review-dispatch+current-worker", workerAt
			if pending.Decision {
				out.State, out.Caption = "adjudicating", "Review adjudication working"
			}
		}
		if out.Actor != "" {
			out.Caption += " · " + out.Actor
		}
		if hadApproval {
			if approvalAt != nil && pending.Dispatched.After(*approvalAt) {
				out.Caption += " · re-review after approval"
			} else {
				out.Caption += " · another assignment not completed"
			}
		}
		if out.Active {
			break
		}
	}
	if out.State == "changes_requested" || out.State == "human_decision" {
		for _, perspective := range out.Perspectives {
			if line := trackerPerspectiveFindingLine(perspective); line != "" {
				out.Caption += " · " + line
				break
			}
		}
	}

	return out
}

func stageTrackerApplyCodeActivity(it *StageTrackerItem, dispatch review.DispatchState, status *StatusPayload, now time.Time) {
	activityCaption := it.CodeActivity != nil && it.CodeActivity.Caption != "" &&
		(it.Caption == it.CodeActivity.Caption || strings.HasPrefix(it.Caption, it.CodeActivity.Caption+" · "))
	it.DisplayColumn = 0
	out := trackerUnknownActivity(it.Head, "Code activity unknown")
	qualify := stageTrackerRepoQualifier(status)
	if it.Stage == trackerNumberNeedsAuthor || it.Stage == trackerNumberReturned {
		out.State, out.Caption, out.Actor, out.Source = "waiting_owner", "Waiting for owner · not observed coding", it.Author, "native-author-wait"
		if it.Author != "" {
			out.Caption = "Waiting for @" + it.Author + " · not observed coding"
		}
		out.Since = it.StageSince
		it.DisplayColumn = trackerNumberPRInProgress
	}
	if it.Stage == trackerNumberAdopted {
		out.State, out.Caption, out.Source = "adopted", "Adopted repair · activity unknown", "native-adoption"
		out.Since = it.StageSince
		it.DisplayColumn = trackerNumberPRInProgress
	}
	if it.Stage == trackerNumberFixing {
		out.State, out.Caption, out.Source = "waiting_fixer", "Waiting for fixer · no code work observed", "native-fix-request"
		out.Since = it.StageSince
		it.DisplayColumn = trackerNumberReviewPending
	}
	for _, fix := range dispatch.Fixes {
		if !trackerHeadMatches(it.Repo, it.Number, it.Head, qualify(fix.Repo), fix.Number, fix.HeadSHA) ||
			fix.Dispatched.IsZero() || fix.Dispatched.After(now) || now.Sub(fix.Dispatched) > review.DefaultPendingFixTTL {
			continue
		}
		out.State, out.Caption, out.Actor, out.Source = "assigned", "Fix task assigned · activity unknown", fix.Agent, "fix-dispatch"
		out.Since, out.ObservedAt = timePtr(fix.Dispatched), timePtr(dispatch.GeneratedAt)
		// A dispatch and PR administration are not source-work evidence.
		// Bind an observed successful, worktree-verified edit to the exact
		// actor/repo/head assignment; unsupported instrumentation is unknown.
		if codeWork := trackerAssignedCodeWorker(status, it.Repo, it.Number, it.Head, fix.Agent, fix.Dispatched, now); codeWork != nil {
			out.State, out.Caption, out.Active = "fixing", "Remediating · "+fix.Agent, true
			out.Source, out.ObservedAt = "fix-dispatch+"+codeWork.Source, timePtr(codeWork.At)
			it.DisplayColumn = trackerNumberPRInProgress
		}
		if status != nil {
			for _, worker := range status.Agents {
				action := worker.LastAction
				if strings.EqualFold(worker.Name, fix.Agent) && worker.Busy == "working" && !worker.Paused && worker.AwaitingCI &&
					action != nil && action.Current && agentCausedAuditActions[action.Action] && action.Number == it.Number && strings.EqualFold(qualify(action.Repo), it.Repo) {
					at, err := time.Parse(time.RFC3339Nano, action.At)
					if err != nil || at.Before(fix.Dispatched) || at.After(now) || now.Sub(at) > trackerActivityFreshWindow {
						continue
					}
					out.State, out.Caption = "waiting_ci", "Fix task idle · agent waiting for CI before coding"
					out.Detail = "The assigned fix agent is holding until CI reports on the PR's current commit; no code edits have been observed yet."
					out.Source, out.ObservedAt = "fix-dispatch+current-worker-ci-wait", &at
					if it.Stage != trackerNumberNeedsAuthor && it.Stage != trackerNumberReturned && it.Stage != trackerNumberAdopted {
						it.DisplayColumn = trackerNumberReviewPending
					}
					break
				}
			}
		}
		break
	}
	if it.Rework != nil && it.Rework.State == reworkStateInProgress && !out.Active {
		out.State, out.Caption, out.Actor, out.Source = "claimed", "Fix task claimed · activity unknown", it.Rework.Actor, "contributor-fix-claim"
		out.Since = it.Rework.Since
	}
	if it.Push != nil && (it.Push.State == pushStatePushed || it.Push.State == pushStateChecking) {
		// Current-head work observed after the push can coexist with checks;
		// a persistent push marker must not hide a later repair.
		workAfterPush := out.Active && (it.Push.State == pushStateChecking || it.Push.Since == nil ||
			(out.ObservedAt != nil && out.ObservedAt.After(*it.Push.Since)))
		if !workAfterPush {
			it.DisplayColumn = trackerNumberReviewPending
			out = &StageTrackerActivity{Head: it.Head, State: "pushed", Caption: "New head pushed · CI and re-review",
				Source: "recorded-push", Since: it.Push.Since}
			if it.Push.State == pushStateChecking {
				out.State, out.Caption, out.Source = "waiting_ci", "CI retry requested · no new commit pushed yet", "recorded-ci-retry"
				out.Detail = "A CI re-run was requested on the PR's current commit; the PR head has not changed, so the tracker waits for the re-run's result."
			}
		}
	}
	// Human decisions and superseded originals remain their native gates.
	if it.Stage == trackerNumberNeedsDecision || it.Stage >= trackerNumberAwaitingMerge || it.Superseded != nil {
		it.DisplayColumn = 0
	}
	if out.State == "waiting_fixer" && (strings.HasPrefix(it.Caption, "Remediation paused") || strings.HasPrefix(it.Caption, "Fixing paused")) {
		detail := strings.TrimPrefix(it.Caption, "Remediation paused")
		if detail == it.Caption {
			detail = strings.TrimPrefix(it.Caption, "Fixing paused")
		}
		out.Blocker = "Fix task blocked" + detail
		out.Source += "+fix-dispatch-status"
	}
	it.CodeActivity = out
	// Replace generic or previously projected activity, not independent
	// native facts such as a foreign bot conflict, hold or rebase episode.
	if it.Stage == trackerNumberFixing && it.Superseded == nil &&
		(it.Caption == "" || strings.HasPrefix(it.Caption, "Remediating") || strings.HasPrefix(it.Caption, "Remediation paused") || strings.HasPrefix(it.Caption, "Fixing") || activityCaption || out.Active) {
		it.Caption = out.Caption
		if out.Actor != "" && !out.Active {
			it.Caption += " · " + out.Actor
		}
	}
}
