package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// The "Let hive decide" preview (hivecommons/hive#15126). The owner pressed
// the button expecting an approval and got three superseding PRs: "I was
// not asked to supersede them". The SPA now asks for the plan first and
// shows a confirm popover — Hive would: …; Why: … — with [Let hive do
// that] [Approve and let hive merge] [Cancel]; nothing is written until a
// button is pressed, and the ledger records which one.

// delegatedDecisionPlan is the wire shape of POST …/decide-delegated/plan:
// the decision the hive WOULD record, in plain words, and the operator's
// alternatives. It never writes to GitHub or the ledger.
type delegatedDecisionPlan struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Type   string `json:"type"`
	// Title is the item's title, for the dialog's subject line.
	Title string `json:"title,omitempty"`
	// Would is the verdict the hive would record (adopt-supersede, approve,
	// recommend-only, …); Rule the policy rule behind it; Word the pill /
	// progress vocabulary for it ("Adopting · opening hive copy…").
	Would  string `json:"would"`
	Rule   string `json:"rule,omitempty"`
	Acting string `json:"acting,omitempty"`
	// Summary is the one decision sentence, verb first and without a
	// "Hive would:" prefix (the dialog's eyebrow says that): "Send #15332
	// back to its author with one instruction: make CI green, nothing else."
	Summary string `json:"summary"`
	// Why is the one plain reason: "Author @Karibhou is untrusted and
	// their clanker is offline."
	Why string `json:"why,omitempty"`
	// Consequences lists what follows — at most two short items.
	Consequences []string `json:"consequences"`
	// Ladder is the contributor-PR rung the decision landed on, its reason
	// and the rungs ruled out above it (hivecommons/hive#15289); the
	// dialog shows it under Details.
	Ladder *delegatedecision.Ladder `json:"ladder,omitempty"`
	// MergeableAsIs is true when "Approve and let hive merge" (and the
	// secondary merge-now) is an honest alternative: the ladder said
	// mergeable-as-is, or (no ladder) the head is green, conflict-free and
	// signed off. When false both merge alternatives are left out so the
	// dialog cannot contradict itself.
	MergeableAsIs bool `json:"mergeable_as_is"`
	// Alternatives are the other buttons, in the popover's option shape;
	// the SPA adds Cancel itself.
	Alternatives []StageTrackerDecisionOption `json:"alternatives"`
	// Writes is false: the plan has no side effects. Present so a reader
	// of the payload can tell it from a decision.
	Writes bool `json:"writes"`
	// Prior is set when the hive already recorded this decision: the plan
	// then says so and the primary button re-runs it.
	Prior *time.Time `json:"prior,omitempty"`
	Notes []string   `json:"notes,omitempty"`
}

// delegatedPlanAlternativeLabel is the preview's merge button: the
// operator approves and the hive merges on its next tick once CI is green.
const delegatedPlanAlternativeLabel = decisionQueueMergeLabel

func (s *Server) handleRepoItemDecideDelegatedPlan(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	repoFull := owner + "/" + repoName
	if !s.delegateDecisionEnabled(repoFull) {
		jsonError(w, "delegated decisions are disabled (decisions.delegate.enabled, or decisions.delegate.repos.<repo>.enabled for this repository)", http.StatusForbidden)
		return
	}
	if allowed, status, msg := s.canToggleRepoHold(r, owner, repoName); !allowed {
		jsonError(w, msg, status)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	var body delegatedDecisionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, delegatedDecisionMaxBody)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	itemType := strings.ToLower(strings.TrimSpace(body.Type))
	if itemType != "pr" {
		itemType = "issue"
	}
	mergerRole := config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleMerger) &&
		(!isOwnerRole(r.Header.Get("X-Hive-Role")) || r.Header.Get(ownerRoleVerifiedHeader) == "true")
	ctx := r.Context()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	plan, err := s.planDelegatedDecision(ctx, repoFull, number, itemType, mergerRole, body)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, plan)
}

// planDelegatedDecision runs the same gather + decide as
// decideDelegatedOne and stops there: nothing is executed, commented,
// audited or recorded.
func (s *Server) planDelegatedDecision(ctx context.Context, repoFull string, number int, itemType string, mergerRole bool, req delegatedDecisionRequest) (*delegatedDecisionPlan, error) {
	in, err := s.gatherDelegatedInput(ctx, repoFull, number, itemType, req)
	if err != nil {
		return nil, err
	}
	plan := &delegatedDecisionPlan{Repo: repoFull, Number: number, Type: itemType, Title: strings.TrimSpace(in.Title)}
	var d delegatedecision.Decision
	if out, ok := s.delegatedHumanMergeOutcome(repoFull, number, itemType, "", mergerRole, in, req); ok {
		// The 090 shortcut: the plan is the shortcut's own answer, with
		// its options as the alternatives (hivecommons/hive#15078).
		d = *out.Decision
		plan.Notes = out.Notes
		if out.Result != nil {
			plan.Alternatives = out.Result.Options
		}
	} else {
		var notes []string
		d, notes = s.decideDelegated(ctx, in)
		plan.Notes = notes
		if o, ok := s.delegatedOrphanedAdoption(ctx, repoFull, number, in, time.Now()); ok {
			d = o.decision("#" + strconv.Itoa(number))
			plan.Notes = append(plan.Notes, "adoption orphaned: hive's copy #"+strconv.Itoa(o.Dead)+" is closed without merging")
		}
		if prior := s.delegatedPriorDecisionState(ctx, repoFull, number, d.Rule); prior.Found {
			at := prior.At
			plan.Prior = &at
		}
	}
	plan.Would, plan.Rule = string(d.Verdict), d.Rule
	if verb, ok := decidedVerbs[plan.Would]; ok {
		plan.Acting = verb.Acting
	}
	plan.Summary, plan.Consequences = delegatedPlanWording(in, d)
	plan.Why = delegatedPlanWhy(in, d)
	plan.Ladder = d.Ladder
	plan.MergeableAsIs = delegatedPlanMergeableAsIs(in, d)
	if plan.Alternatives == nil {
		plan.Alternatives = delegatedPlanAlternatives(in, d, mergerRole)
	}
	kept := plan.Alternatives[:0]
	for _, alt := range plan.Alternatives {
		if alt.Action == decisionActionMergeNow || alt.Action == decisionActionQueueMerge {
			if !plan.MergeableAsIs {
				continue
			}
			if alt.Action == decisionActionQueueMerge {
				alt.Label = delegatedPlanAlternativeLabel
			}
		}
		kept = append(kept, alt)
	}
	plan.Alternatives = kept
	return plan, nil
}

// delegatedPlanWording is the one decision sentence and the (at most
// two) "then" items for the decision the hive would record. Operator
// feedback on the preview (hivecommons/hive#15332's dialog: "this needs
// to be exceedingly simple to understand and act on"): verb first, plain
// words, no "Hive would:" prefix — the dialog's eyebrow says that once —
// and nothing a reader has to know hive's vocabulary for.
func delegatedPlanWording(in delegatedecision.Input, d delegatedecision.Decision) (string, []string) {
	ref := "#" + strconv.Itoa(in.Number)
	target := 0
	for _, a := range d.Actions {
		if a.Target > 0 {
			target = a.Target
		}
	}
	switch {
	case d.Verdict == delegatedecision.VerdictAdoptSupersede:
		return "Re-open " + ref + " as hive's own PR with the same commits and merge that; close " + ref + " as superseded.",
			[]string{
				"Hive opens its copy of " + ref + ", lets it soak, and merges it.",
				ref + " is closed as superseded; the author keeps the credit.",
			}
	case d.Verdict == delegatedecision.VerdictApprove:
		return "Approve " + ref + " as it is and let the merge queue merge it.",
			[]string{"Hive approves the current commit.", ref + " merges once CI is green and it has soaked."}
	case d.Verdict == delegatedecision.VerdictCloseDuplicate && target > 0:
		return "Close " + ref + " as a duplicate of #" + strconv.Itoa(target) + ".", []string{ref + " is closed with a comment pointing at #" + strconv.Itoa(target) + "."}
	case d.Verdict == delegatedecision.VerdictCloseSuperseded && target > 0:
		return "Close " + ref + " as superseded by #" + strconv.Itoa(target) + ".", []string{ref + " is closed with a comment pointing at #" + strconv.Itoa(target) + "."}
	case d.Verdict == delegatedecision.VerdictCloseDuplicate, d.Verdict == delegatedecision.VerdictCloseSuperseded:
		return "Close " + ref + ".", []string{ref + " is closed with a comment saying why."}
	case d.Verdict == delegatedecision.VerdictDirection:
		return "Send " + ref + " back to its author with one instruction: " + delegatedPlanInstruction(in, d) + ".",
			[]string{"Hive posts that instruction on " + ref + ".", "The author pushes a fix; the row updates when it lands."}
	case d.Verdict == delegatedecision.VerdictRefused:
		return "Do nothing: " + delegatedPlanPlain(d.Rationale) + ".", []string{"Nothing changes; " + ref + " stays waiting for you."}
	case d.Verdict == delegatedecision.VerdictOwnerOnly:
		return "Leave this to the owner.", []string{"Hive labels " + ref + " needs-owner and holds it.", "Only the owner can grant this."}
	case d.Verdict == delegatedecision.VerdictOperatorAction:
		return "Tell the spoke owner what to do: " + delegatedPlanPlain(strings.TrimPrefix(strings.TrimSpace(d.Text), "operator action — ")) + ".",
			[]string{"Hive posts that to the spoke owner and labels " + ref + " needs-owner; no agent is sent.", ref + " stays where it is and clears once the report stops recurring."}
	case d.Verdict == delegatedecision.VerdictNotNow:
		return "Decline " + ref + " for now and close it as not planned.",
			[]string{"Hive posts why: no incident or recurrence justifies the cost yet.", ref + " is closed as not planned; re-propose it with evidence."}
	default:
		text := delegatedPlanPlain(d.Text)
		if text == "" {
			text = delegatedecision.VerdictPlain(d.Verdict)
		}
		return "Recommend only: " + text + ".", []string{"Hive posts a comment and nothing else.", ref + " waits for you to decide."}
	}
}

// delegatedPlanInstruction is the direction in the author's terms: the
// fix list from the head's facts ("make CI green, nothing else"), or the
// decision's own text when the direction is a reviewer's question.
func delegatedPlanInstruction(in delegatedecision.Input, d delegatedecision.Decision) string {
	var fix []string
	if in.Conflicting {
		fix = append(fix, "rebase onto the base branch")
	}
	if in.DCOMissing {
		fix = append(fix, "add the sign-off")
	}
	if !in.CIGreen {
		fix = append(fix, "make CI green")
	}
	if len(fix) == 0 || strings.TrimSpace(in.Question) != "" {
		if text := delegatedPlanPlain(d.Text); text != "" {
			return text
		}
	}
	if len(fix) == 0 {
		return "address the review"
	}
	return strings.Join(fix, ", ") + ", nothing else"
}

// delegatedPlanWhy is the one plain reason behind the decision, kept
// short enough to read at a glance: the author's standing when that is
// what decided it, otherwise the head's blockers, otherwise the first
// clause of the rule's rationale with its issue references removed.
func delegatedPlanWhy(in delegatedecision.Input, d delegatedecision.Decision) string {
	var parts []string
	if in.Kind == delegatedecision.KindPR && !in.TrustedAuthor && !in.HiveAuthored && d.Verdict != delegatedecision.VerdictDirection {
		who := "the author"
		if a := strings.TrimSpace(in.Author); a != "" {
			who = "author @" + a
		}
		parts = append(parts, who+" is untrusted")
	}
	if !in.ContributorOnline && d.Verdict != delegatedecision.VerdictDirection && in.Kind == delegatedecision.KindPR {
		parts = append(parts, "their clanker is offline")
	}
	why := strings.Join(parts, " and ")
	if why == "" && d.Verdict == delegatedecision.VerdictDirection {
		why = delegatedPlanBlockers(in)
	}
	if why == "" {
		why = delegatedPlanPlain(d.Rationale)
	}
	if why == "" && d.Rule != "" {
		why = delegatedecision.RulePlain(d.Rule)
	}
	if why == "" {
		return ""
	}
	return strings.ToUpper(why[:1]) + why[1:] + "."
}

// delegatedPlanBlockers names what keeps a head from merging, in plain
// words: "CI is red", "it conflicts with the base branch", "the sign-off
// is missing".
func delegatedPlanBlockers(in delegatedecision.Input) string {
	var b []string
	if !in.CIGreen {
		b = append(b, "CI is red")
	}
	if in.Conflicting {
		b = append(b, "it conflicts with the base branch")
	}
	if in.DCOMissing {
		b = append(b, "the sign-off is missing")
	}
	return strings.Join(b, " and ")
}

// delegatedPlanIssueRef strips the "(hivecommons/hive#NNNN)" provenance
// a rule's rationale carries for the log; the preview reads better
// without it.
var delegatedPlanIssueRef = regexp.MustCompile(`\s*\([^()]*#\d+\)`)

// delegatedPlanPlain is a rationale or decision text reduced to its
// first clause, without issue references or a trailing full stop.
func delegatedPlanPlain(text string) string {
	text = delegatedPlanIssueRef.ReplaceAllString(strings.TrimSpace(text), "")
	if i := strings.Index(text, "; "); i > 0 {
		text = text[:i]
	}
	return strings.TrimSuffix(strings.TrimSpace(text), ".")
}

// delegatedPlanMergeableAsIs says whether "Approve and let hive merge" is an
// honest alternative: the ladder's own verdict when it ran, otherwise the
// head's facts. A preview that says the head is not mergeable must not
// offer to merge it (hivecommons/hive#15332's dialog).
func delegatedPlanMergeableAsIs(in delegatedecision.Input, d delegatedecision.Decision) bool {
	if in.Kind != delegatedecision.KindPR {
		return false
	}
	if d.Ladder != nil {
		return d.Ladder.Decision == delegatedecision.RuleMergeableAsIs
	}
	return !in.Conflicting && in.CIGreen && !in.DCOMissing
}

// delegatedPlanAlternatives are the other buttons: approve and let hive
// merge (queue-merge, the lead, for a merger), merge now as maintainer,
// send back, close as superseded.
func delegatedPlanAlternatives(in delegatedecision.Input, d delegatedecision.Decision, mergerRole bool) []StageTrackerDecisionOption {
	if in.Kind != delegatedecision.KindPR {
		return []StageTrackerDecisionOption{
			decisionOption(decisionActionDirection, "Give direction", true, ""),
		}
	}
	opts := delegatedHumanMergeOptions(in, in.Number, mergerRole)
	if d.Verdict == delegatedecision.VerdictApprove {
		// The hive would approve anyway, so "Approve and let hive merge" is
		// the primary button under another name; merge-now is the one
		// alternative left that does something different.
		for i := range opts {
			if opts[i].Action == decisionActionQueueMerge {
				opts = append(opts[:i], opts[i+1:]...)
				break
			}
		}
	}
	return opts
}
