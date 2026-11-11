package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Workflow-model simulate (hivecommons/hive#17620, RFC phase 3c): "paste or
// pick a real issue/PR and see which state the model puts it in and why,
// using the same Facts the engine would use". Read-only, open to any
// dashboard user like GET on the model.

// workflowSimulateTimeout bounds the forge reads behind a repo+number fill.
const workflowSimulateTimeout = 30 * time.Second

// workflowSimulateRequest is the body of POST .../simulate. Facts is the
// JSON mirror of stageengine.Facts; when it is absent and Repo+Number are
// set the handler fills what one forge read can tell and marks the rest
// unknown. When both are present Facts wins and the forge is not read.
type workflowSimulateRequest struct {
	Kind   string         `json:"kind"`
	Facts  *SimulateFacts `json:"facts,omitempty"`
	Repo   string         `json:"repo,omitempty"`
	Number int            `json:"number,omitempty"`
}

// SimulateFacts is the wire form of stageengine.Facts. Every field is
// optional; the zero value is a closed, unlabeled item by an unknown
// author with no kind-specific evidence. Scalar enumerations use the
// rules-vocabulary spellings (author.kind, pr.checks, pr.review, pr.dco in
// pkg/workflowmodel.Facts) so what the operator types is what a rule tests.
type SimulateFacts struct {
	Labels []string            `json:"labels"`
	Open   bool                `json:"open"`
	Merged bool                `json:"merged"`
	Draft  bool                `json:"draft"`
	Author SimulateAuthor      `json:"author"`
	Park   SimulatePark        `json:"park"`
	Origin string              `json:"origin,omitempty"`
	Issue  *SimulateIssueFacts `json:"issue,omitempty"`
	PR     *SimulatePRFacts    `json:"pr,omitempty"`
}

// SimulateAuthor mirrors stageengine.AuthorFacts.
type SimulateAuthor struct {
	Login        string `json:"login,omitempty"`
	Kind         string `json:"kind,omitempty"` // hive | clanker | human | unknown
	SelfAuthored bool   `json:"selfAuthored,omitempty"`
	Role         string `json:"role,omitempty"` // trusted | contributor | newcomer | untrusted | ""
}

// SimulatePark mirrors stageengine.ParkFacts.
type SimulatePark struct {
	Holds              []string `json:"holds,omitempty"`
	NeedsHuman         bool     `json:"needsHuman,omitempty"`
	DecisionLabel      string   `json:"decisionLabel,omitempty"`
	PossiblyExists     bool     `json:"possiblyExists,omitempty"`
	Sentinel           bool     `json:"sentinel,omitempty"`
	HumanOnlyReason    string   `json:"humanOnlyReason,omitempty"`
	ApprovedDirection  bool     `json:"approvedDirection,omitempty"`
	DecisionQuestion   string   `json:"decisionQuestion,omitempty"`
	DecisionSuperseded bool     `json:"decisionSuperseded,omitempty"`
}

// SimulateIssueFacts mirrors the issue-side stageengine.IssueFacts the
// derivers and the rules vocabulary read.
type SimulateIssueFacts struct {
	Bucket                 string `json:"bucket,omitempty"` // actionable | claimed | in_progress | claimed_by_pr | filtered | reporter_triage
	FilteredReason         string `json:"filteredReason,omitempty"`
	Claimed                bool   `json:"claimed,omitempty"`
	ClaimHolder            string `json:"claimHolder,omitempty"`
	ClaimPRStarted         bool   `json:"claimPRStarted,omitempty"`
	LikelyDone             bool   `json:"likelyDone,omitempty"`
	Stalled                bool   `json:"stalled,omitempty"`
	CoveringPR             int    `json:"coveringPR,omitempty"`
	CoveringPRStage        int    `json:"coveringPRStage,omitempty"`
	OpenChildren           int    `json:"openChildren,omitempty"`
	HoldLabel              string `json:"holdLabel,omitempty"`
	BlockedReason          string `json:"blockedReason,omitempty"`
	NeedsHumanLabel        string `json:"needsHumanLabel,omitempty"`
	Blocker                string `json:"blocker,omitempty"`
	ApprovedDirectionLabel string `json:"approvedDirectionLabel,omitempty"`
	UmbrellaLabel          string `json:"umbrellaLabel,omitempty"`
	DiscussionLabel        string `json:"discussionLabel,omitempty"`
	TriageAwaiting         string `json:"triageAwaiting,omitempty"`
	TriageAwaitingLabel    string `json:"triageAwaitingLabel,omitempty"`
}

// SimulatePRFacts mirrors the PR-side stageengine.PRFacts the derivers and
// the rules vocabulary read. Lane is optional; without one the built-in
// deriver is fed stageengine.SimulatedLane and the response says so.
type SimulatePRFacts struct {
	HeadSHA          string        `json:"headSHA,omitempty"`
	HeadFresh        bool          `json:"headFresh,omitempty"`
	BaseRef          string        `json:"baseRef,omitempty"`
	Conflicting      bool          `json:"conflicting,omitempty"`
	ChangesRequested bool          `json:"changesRequested,omitempty"`
	Checks           string        `json:"checks,omitempty"` // green | red | pending | missing | unknown
	Review           string        `json:"review,omitempty"` // pending | approved | changes_requested | requires_human | unavailable
	DCO              string        `json:"dco,omitempty"`    // yes | no | unknown
	DCOGate          bool          `json:"dcoGate,omitempty"`
	HumanApprover    string        `json:"humanApprover,omitempty"`
	Lane             *SimulateLane `json:"lane,omitempty"`
}

// SimulateLane is one automerge lane opinion (stageengine.LaneFact).
type SimulateLane struct {
	Lane   string `json:"lane"`
	Reason string `json:"reason,omitempty"`
}

// SimulateDecision is the wire form of the stageengine.Decision.
type SimulateDecision struct {
	State    string `json:"state"`
	Column   string `json:"column"`
	Stage    int    `json:"stage"`
	Label    string `json:"label"`
	Reason   string `json:"reason"`
	Abstain  bool   `json:"abstain"`
	Covered  bool   `json:"covered"`
	Terminal bool   `json:"terminal"`
}

// SimulateResponse is the body of a 200 from .../simulate.
type SimulateResponse struct {
	Decision SimulateDecision        `json:"decision"`
	Trace    []stageengine.RuleTrace `json:"trace"`
	Facts    SimulateFacts           `json:"facts"`
	Filled   []string                `json:"filled"`
	Unknown  []string                `json:"unknown"`
	Notes    []string                `json:"notes,omitempty"`
}

func (s *Server) handleWorkflowSimulate(w http.ResponseWriter, r *http.Request) {
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	var req workflowSimulateRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	req.Repo = strings.TrimSpace(req.Repo)
	m, _, err := st.Get(r.PathValue("name"), r.PathValue("version"))
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	profile, diags, cerr := workflowmodel.Compile(m)
	if cerr != nil {
		jsonError(w, fmt.Sprintf("%s@%s does not compile: %d diagnostics", m.Metadata.Name, m.Spec.Meta.Version, len(diags)), http.StatusUnprocessableEntity)
		return
	}

	var filled, unknown, notes []string
	facts := req.Facts
	if facts == nil {
		if req.Repo == "" || req.Number <= 0 {
			jsonError(w, "set facts, or repo (owner/repo) and number to fetch them", http.StatusBadRequest)
			return
		}
		if !strings.Contains(req.Repo, "/") {
			jsonError(w, "repo must be owner/repo", http.StatusBadRequest)
			return
		}
		if s.deps == nil || s.deps.WorkflowItemSnapshot == nil {
			jsonError(w, "fetching an item needs a forge client; set facts by hand instead", http.StatusUnprocessableEntity)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), workflowSimulateTimeout)
		defer cancel()
		snap, err := s.deps.WorkflowItemSnapshot(ctx, req.Repo, req.Number)
		if err != nil {
			jsonError(w, fmt.Sprintf("fetch %s#%d: %v", req.Repo, req.Number, err), http.StatusBadGateway)
			return
		}
		if req.Kind == "" {
			req.Kind = snap.Kind
		} else if req.Kind != snap.Kind {
			jsonError(w, fmt.Sprintf("%s#%d is a %s, not a %s", req.Repo, req.Number, snap.Kind, req.Kind), http.StatusBadRequest)
			return
		}
		facts, filled, unknown = simulateFactsFromSnapshot(snap)
	} else if req.Kind == "" {
		jsonError(w, "kind must be issue or pr", http.StatusBadRequest)
		return
	}

	owner, repo := "sim", "sim"
	if i := strings.IndexByte(req.Repo, '/'); i > 0 {
		owner, repo = req.Repo[:i], req.Repo[i+1:]
	}
	ref, err := stageengine.SimulateRef(req.Kind, owner, repo, req.Number)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	norm := normalizeSimulateFacts(req.Kind, *facts)
	ef := norm.toEngine(ref)
	if ref.Kind == stagelabels.PRItem && len(ef.PR.Lanes) == 0 {
		lane := stageengine.SimulatedLane(ef)
		ef.PR.Lanes = []stageengine.LaneFact{lane}
		norm.PR.Lane = &SimulateLane{Lane: lane.Lane, Reason: lane.Reason}
		notes = append(notes, fmt.Sprintf("no lane observation supplied; simulated lane %s with reason %q from the head facts", lane.Lane, lane.Reason))
	}
	dec, trace := stageengine.Explain(profile, ref.Kind, ef)
	if trace == nil {
		trace = []stageengine.RuleTrace{}
	}
	if filled == nil {
		filled = []string{}
	}
	if unknown == nil {
		unknown = []string{}
	}
	jsonResponse(w, SimulateResponse{
		Decision: simulateDecision(profile, dec),
		Trace:    trace,
		Facts:    norm,
		Filled:   filled,
		Unknown:  unknown,
		Notes:    notes,
	})
}

// simulateDecision maps the engine's Decision onto the profile: the state
// that carries the stage number and its column.
func simulateDecision(p *workflowmodel.Profile, d stageengine.Decision) SimulateDecision {
	out := SimulateDecision{Reason: d.Reason, Abstain: d.Abstain, Terminal: d.Terminal}
	if d.Abstain {
		out.Covered = !strings.HasPrefix(d.Reason, stageengine.ReasonNotCovered)
		return out
	}
	out.Covered = true
	if d.Stage != stagelabels.None {
		out.Stage = d.Stage.Number()
		out.Label = d.Stage.Label()
		if st, ok := p.StateForNumber(out.Stage); ok {
			out.State = st
			out.Column = p.ColumnOf[st]
		}
	}
	return out
}

// simulateFactsFromSnapshot fills a SimulateFacts from one forge read and
// names what it could and could not fill.
func simulateFactsFromSnapshot(snap ghpkg.WorkflowItemSnapshot) (*SimulateFacts, []string, []string) {
	f := &SimulateFacts{Labels: snap.Labels, Open: snap.Open, Merged: snap.Merged, Draft: snap.Draft}
	if f.Labels == nil {
		f.Labels = []string{}
	}
	f.Author.Login = snap.Author
	f.Author.Kind = "unknown"
	f.Author.Role = stagelabels.RoleOf(snap.Labels).String()
	f.Park.Holds = stagelabels.Holds(snap.Labels)
	f.Park.NeedsHuman = hasLabelFold(snap.Labels, stageengine.NeedsHumanLabel)
	f.Origin = originLabel(snap.Labels)
	filled := []string{"labels", "open", "author.login", "author.role", "park.holds", "park.needsHuman", "origin"}
	unknown := []string{"author.kind"}
	if snap.Kind == "pr" {
		f.PR = &SimulatePRFacts{HeadSHA: snap.HeadSHA, BaseRef: snap.BaseRef, Review: "pending", DCO: "unknown", Checks: "unknown"}
		filled = append(filled, "merged", "draft", "pr.headSHA", "pr.baseRef")
		if snap.ConflictKnown {
			f.PR.Conflicting = snap.Conflicting
			filled = append(filled, "pr.conflicting")
		} else {
			unknown = append(unknown, "pr.conflicting")
		}
		if snap.ChecksKnown {
			f.PR.Checks = stageengine.ChecksName(stagelabels.CheckSummary{Known: true, Total: snap.Checks.Total, Green: snap.Checks.Green, Pending: snap.Checks.Pending, Red: snap.Checks.Red, Missing: snap.Checks.MissingRequired})
			switch snap.Checks.DCO {
			case 1:
				f.PR.DCO = "yes"
			case -1:
				f.PR.DCO = "no"
			}
			filled = append(filled, "pr.checks", "pr.dco")
		} else {
			unknown = append(unknown, "pr.checks", "pr.dco")
		}
		unknown = append(unknown, "pr.review", "pr.changesRequested", "pr.lane")
	} else {
		f.Issue = &SimulateIssueFacts{Bucket: "actionable"}
		if !snap.Open {
			f.Issue.Bucket = ""
		}
		unknown = append(unknown, "issue.bucket", "issue.claimed", "issue.coveringPR", "issue.stalled")
	}
	sort.Strings(unknown)
	return f, filled, unknown
}

func hasLabelFold(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

func originLabel(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(strings.ToLower(l), "origin/") {
			return l
		}
	}
	return ""
}

// normalizeSimulateFacts fills the kind-specific pointer the deriver
// needs, lower-cases the enumerations and drops the other kind's evidence.
func normalizeSimulateFacts(kind string, f SimulateFacts) SimulateFacts {
	if f.Labels == nil {
		f.Labels = []string{}
	}
	f.Author.Kind = stageengine.AuthorKindName(stageengine.ParseAuthorKind(f.Author.Kind))
	f.Author.Role = strings.ToLower(strings.TrimSpace(f.Author.Role))
	if f.Park.Holds == nil {
		f.Park.Holds = stagelabels.Holds(f.Labels)
	}
	if kind == "pr" {
		f.Issue = nil
		if f.PR == nil {
			f.PR = &SimulatePRFacts{}
		}
		f.PR.Checks = stageengine.ChecksName(stageengine.ParseChecks(f.PR.Checks))
		f.PR.Review = simulateReviewName(f.PR.Review)
		f.PR.DCO = stageengine.DCOName(stageengine.ParseDCO(f.PR.DCO))
	} else {
		f.PR = nil
		if f.Issue == nil {
			f.Issue = &SimulateIssueFacts{}
		}
		f.Issue.Bucket = strings.ToLower(strings.TrimSpace(f.Issue.Bucket))
	}
	return f
}

// simulateReviewName keeps requires_human distinct (stageengine.ReviewName
// folds it into pending for the rules vocabulary, but the built-in deriver
// parks on it).
func simulateReviewName(s string) string {
	st := stageengine.ParseReview(s)
	if st == stagelabels.VerdictRequiresHuman {
		return "requires_human"
	}
	return stageengine.ReviewName(st)
}

func simulateRole(s string) stagelabels.Role {
	for _, r := range []stagelabels.Role{stagelabels.RoleTrusted, stagelabels.RoleContributor, stagelabels.RoleNewcomer, stagelabels.RoleUntrusted} {
		if r.String() == s {
			return r
		}
	}
	return stagelabels.RoleNone
}

// toEngine converts the wire facts into the Facts the deriver reads.
func (f SimulateFacts) toEngine(ref stageengine.Ref) stageengine.Facts {
	out := stageengine.Facts{
		Ref:        ref,
		ObservedAt: time.Now().UTC(),
		Labels:     append([]string(nil), f.Labels...),
		Open:       f.Open,
		Merged:     f.Merged,
		Draft:      f.Draft,
		Origin:     f.Origin,
		Author: stageengine.AuthorFacts{
			Login:        f.Author.Login,
			Kind:         stageengine.ParseAuthorKind(f.Author.Kind),
			SelfAuthored: f.Author.SelfAuthored,
			Role:         simulateRole(f.Author.Role),
		},
		Park: stageengine.ParkFacts{
			Holds:              append([]string(nil), f.Park.Holds...),
			NeedsHuman:         f.Park.NeedsHuman,
			DecisionLabel:      f.Park.DecisionLabel,
			PossiblyExists:     f.Park.PossiblyExists,
			Sentinel:           f.Park.Sentinel,
			HumanOnlyReason:    f.Park.HumanOnlyReason,
			ApprovedDirection:  f.Park.ApprovedDirection,
			DecisionQuestion:   f.Park.DecisionQuestion,
			DecisionSuperseded: f.Park.DecisionSuperseded,
		},
	}
	if f.Issue != nil {
		is := &stageengine.IssueFacts{
			Bucket:                 stageengine.IssueBucket(f.Issue.Bucket),
			FilteredReason:         f.Issue.FilteredReason,
			LikelyDone:             f.Issue.LikelyDone,
			Stalled:                f.Issue.Stalled,
			OpenChildren:           f.Issue.OpenChildren,
			HoldLabel:              f.Issue.HoldLabel,
			BlockedReason:          f.Issue.BlockedReason,
			NeedsHumanLabel:        f.Issue.NeedsHumanLabel,
			Blocker:                f.Issue.Blocker,
			ApprovedDirectionLabel: f.Issue.ApprovedDirectionLabel,
			UmbrellaLabel:          f.Issue.UmbrellaLabel,
			DiscussionLabel:        f.Issue.DiscussionLabel,
			TriageAwaiting:         f.Issue.TriageAwaiting,
			TriageAwaitingLabel:    f.Issue.TriageAwaitingLabel,
		}
		if f.Issue.Claimed || f.Issue.ClaimHolder != "" {
			is.Claim = &stageengine.ClaimFact{Holder: f.Issue.ClaimHolder, PRStarted: f.Issue.ClaimPRStarted}
		}
		if f.Issue.CoveringPR > 0 {
			is.CoveringPR = &stageengine.Ref{Owner: ref.Owner, Repo: ref.Repo, Number: f.Issue.CoveringPR, Kind: stagelabels.PRItem}
			if st, ok := stageByNumberForSimulate(f.Issue.CoveringPRStage); ok {
				is.CoveringPRStage = st
			}
		}
		out.Issue = is
	}
	if f.PR != nil {
		review := stageengine.ParseReview(f.PR.Review)
		pr := &stageengine.PRFacts{
			HeadSHA:          f.PR.HeadSHA,
			HeadFresh:        f.PR.HeadFresh,
			BaseRef:          f.PR.BaseRef,
			Conflicting:      f.PR.Conflicting,
			ChangesRequested: f.PR.ChangesRequested || review == stagelabels.VerdictChangesRequested,
			Checks:           stageengine.ParseChecks(f.PR.Checks),
			DCO:              stageengine.ParseDCO(f.PR.DCO),
			DCOGate:          f.PR.DCOGate,
			Review:           stageengine.ReviewFacts{State: review, HumanApprover: f.PR.HumanApprover},
		}
		// The built-in deriver reads the raw verdict string behind the
		// folded state (VerdictBlocks / VerdictNeedsHuman), so spell it.
		switch review {
		case stagelabels.VerdictApproved:
			pr.Review.VerdictKnown, pr.Review.Verdict = true, "approve"
		case stagelabels.VerdictChangesRequested:
			pr.Review.VerdictKnown, pr.Review.Verdict = true, "changes_requested"
		case stagelabels.VerdictRequiresHuman:
			pr.Review.VerdictKnown, pr.Review.Verdict = true, "requires_human"
		}
		if f.PR.Lane != nil && f.PR.Lane.Lane != "" {
			pr.Lanes = []stageengine.LaneFact{{Lane: f.PR.Lane.Lane, Reason: f.PR.Lane.Reason, Role: out.Author.Role, Merged: f.Merged}}
		}
		out.PR = pr
	}
	return out
}

func stageByNumberForSimulate(n int) (stagelabels.Stage, bool) {
	if n <= 0 {
		return stagelabels.None, false
	}
	for _, def := range stagelabels.Table {
		if def.Number == n {
			return def.Stage, true
		}
	}
	return stagelabels.None, false
}
