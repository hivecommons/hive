package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
// that] [Approve & merge as-is] [Cancel]; nothing is written until a
// button is pressed, and the ledger records which one.

// delegatedDecisionPlan is the wire shape of POST …/decide-delegated/plan:
// the decision the hive WOULD record, in plain words, and the operator's
// alternatives. It never writes to GitHub or the ledger.
type delegatedDecisionPlan struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Type   string `json:"type"`
	// Would is the verdict the hive would record (adopt-supersede, approve,
	// recommend-only, …); Rule the policy rule behind it; Word the pill /
	// progress vocabulary for it ("Adopting · opening hive copy…").
	Would  string `json:"would"`
	Rule   string `json:"rule,omitempty"`
	Acting string `json:"acting,omitempty"`
	// Summary is one line: "Hive would: re-author #15029 as a hive PR with
	// the same commits and merge it; #15029 will be closed as superseded."
	Summary string `json:"summary"`
	// Why is the plain reason: "author @Karibhou is untrusted and their
	// clanker is offline."
	Why string `json:"why,omitempty"`
	// Consequences lists what follows, one per line.
	Consequences []string `json:"consequences"`
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

// delegatedPlanAlternativeLabel is the preview's approve-as-is button.
const delegatedPlanAlternativeLabel = "Approve & merge as-is"

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
	plan := &delegatedDecisionPlan{Repo: repoFull, Number: number, Type: itemType}
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
	if plan.Alternatives == nil {
		plan.Alternatives = delegatedPlanAlternatives(in, d, mergerRole)
	}
	for i := range plan.Alternatives {
		if plan.Alternatives[i].Action == decisionActionMergeNow {
			plan.Alternatives[i].Label = delegatedPlanAlternativeLabel
		}
	}
	return plan, nil
}

// delegatedPlanWording is the summary line and the consequences for the
// decision the hive would record.
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
		return "Hive would: re-author " + ref + " as a hive PR with the same commits and merge it; " + ref + " will be closed as superseded.",
			[]string{
				"hive opens a hive-authored copy of " + ref + " with the same commits and sign-off",
				"the copy rebases, soaks and merges unattended on the adoption pass",
				ref + " is closed as superseded once the copy merges; the author keeps commit attribution",
			}
	case d.Verdict == delegatedecision.VerdictApprove:
		return "Hive would: approve " + ref + " at its current head and let the merge gate merge it.",
			[]string{"hive records approve at the current head", "the merge sweep merges " + ref + " once CI is green and the head has soaked"}
	case d.Verdict == delegatedecision.VerdictCloseDuplicate && target > 0:
		return "Hive would: close " + ref + " as a duplicate of #" + strconv.Itoa(target) + ".", []string{ref + " is closed with a comment pointing at #" + strconv.Itoa(target)}
	case d.Verdict == delegatedecision.VerdictCloseSuperseded && target > 0:
		return "Hive would: close " + ref + " as superseded by #" + strconv.Itoa(target) + ".", []string{ref + " is closed with a comment pointing at #" + strconv.Itoa(target)}
	case d.Verdict == delegatedecision.VerdictCloseDuplicate, d.Verdict == delegatedecision.VerdictCloseSuperseded:
		return "Hive would: close " + ref + ".", []string{ref + " is closed with a comment saying why"}
	case d.Verdict == delegatedecision.VerdictDirection:
		return "Hive would: send " + ref + " back with direction: " + strings.TrimSuffix(strings.TrimSpace(d.Text), ".") + ".",
			[]string{"hive posts the direction on " + ref, "the author's clanker pushes a new commit; the row moves when it lands"}
	case d.Verdict == delegatedecision.VerdictRefused:
		return "Hive would: refuse — " + strings.TrimSuffix(strings.TrimSpace(d.Rationale), ".") + ".", []string{"nothing changes; the item stays parked for you"}
	case d.Verdict == delegatedecision.VerdictOwnerOnly:
		return "Hive would: leave this to the owner.", []string{"hive labels it needs-owner and holds it", "only the owner can grant this"}
	default:
		text := strings.TrimSuffix(strings.TrimSpace(d.Text), ".")
		if text == "" {
			text = delegatedecision.VerdictPlain(d.Verdict)
		}
		return "Hive would: recommend only — " + text + ".", []string{"nothing is written beyond a comment; the item stays parked until you decide"}
	}
}

// delegatedPlanWhy is the plain reason behind the plan: "author @Karibhou
// is untrusted and their clanker is offline."
func delegatedPlanWhy(in delegatedecision.Input, d delegatedecision.Decision) string {
	var parts []string
	if in.Kind == delegatedecision.KindPR && !in.TrustedAuthor && !in.HiveAuthored {
		who := "the author"
		if a := strings.TrimSpace(in.Author); a != "" {
			who = "author @" + a
		}
		parts = append(parts, who+" is untrusted")
	}
	if !in.ContributorOnline {
		parts = append(parts, "their clanker is offline")
	}
	why := strings.Join(parts, " and ")
	if why == "" {
		why = strings.TrimSuffix(strings.TrimSpace(d.Rationale), ".")
	}
	if why == "" && d.Rule != "" {
		why = delegatedecision.RulePlain(d.Rule)
	}
	if why == "" {
		return ""
	}
	return strings.ToUpper(why[:1]) + why[1:] + "."
}

// delegatedPlanAlternatives are the other buttons: approve & merge as-is
// (merge-now, in-tracker, for a merger) or approve and let hive merge,
// send back, close as superseded.
func delegatedPlanAlternatives(in delegatedecision.Input, d delegatedecision.Decision, mergerRole bool) []StageTrackerDecisionOption {
	if in.Kind != delegatedecision.KindPR {
		return []StageTrackerDecisionOption{
			decisionOption(decisionActionDirection, "Give direction", true, ""),
		}
	}
	opts := delegatedHumanMergeOptions(in, in.Number, mergerRole)
	if d.Verdict == delegatedecision.VerdictApprove {
		// The hive would approve anyway: the as-is merge is the same thing
		// done now.
		for i := range opts {
			if opts[i].Action == decisionActionQueueMerge {
				opts = append(opts[:i], opts[i+1:]...)
				break
			}
		}
	}
	return opts
}
