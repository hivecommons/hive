package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/rebasemerge"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Activity-monitor row decisions (hivecommons/hive#13479 follow-up). The
// stage tracker lists items parked on a person — stage/045-needs-decision on
// an issue, stage/085 on a PR — and the row's "Decide…" menu lets the
// operator answer from the dashboard. Each verb maps onto a path the hive
// already honours for a maintainer on GitHub; none of them merges anything:
//
//   - approve     → on an issue, the same effect as `/hive approve` from a
//     maintainer with write access (Client.RecordOperatorIssueDecision):
//     park labels cleared, approved-direction applied, reply posted. On a
//     PR, the `/lgtm` and `/approve` commands posted as two attributed
//     comments, the approval recorded against the head as the answer to a
//     requires_human verdict, the hive's `needs-human` park and the
//     sentinel alert (label and evidence-bundle findings) released, and the
//     stage re-derived (Client.RecordOperatorPullRequestApprovalWith) — the
//     merge gate decides from there; nothing is reviewed or merged here.
//     The person's approval is the human review a requires_human verdict,
//     a supersession ask or a sentinel alert was waiting for; before this
//     the comments were read by no lane and the row stayed at stage/085
//     (hivecommons/hive#14821, #14822, crux#30).
//   - direction   → `/hive decision <comment>`; the comment is required. On
//     a hive-authored PR it is recorded as a human change request so the
//     author agent is kicked with it (Client.RecordOperatorPullRequestDecision);
//     on a contributor's clanker PR it is recorded the same way and carried
//     to that contributor's hive — the PR goes to 072 returned-to-contributor
//     (hivecommons/hive#15159). A PR a person opened answers 409.
//   - close-superseded / close-wontfix → one comment naming the operator
//     (and, when given, the superseding PR), then a not_planned close (a PR
//     takes the supersession sweep's own labelled close path).
//
// //   - adopt       → on a PR only: the owner's answer to "the author
//
//	maintains this repository; adopt it?" that
//	governor.labels.stage.adoption.maintainer_policy: ask parks a nudged
//	contributor PR with (hivecommons/hive#14524). One attributed comment,
//	then the `adoption/supersede` request label — the same path the
//	owner-delegate's adopt-supersede verdict takes — so the adoption
//	engine's next pass adopts the head (in place or by supersession)
//	exactly as it would a contributor who went offline.
//
// Authorisation is the repo-hold bar (canToggleRepoHold): a verified owner,
// or a signed-in GitHub user with write on the repository — which is exactly
// the permission the un-park sweep checks on a commenter.
const (
	repoItemDecisionApprove         = "approve"
	repoItemDecisionDirection       = "direction"
	repoItemDecisionCloseSuperseded = "close-superseded"
	repoItemDecisionCloseWontFix    = "close-wontfix"
	// close-read closes an advisory issue (triage/advisory, a dedupe
	// checklist) as completed once a maintainer has read it
	// (hivecommons/hive#15381): the row's "Close (read)" action.
	repoItemDecisionCloseRead = "close-read"
	repoItemDecisionAdopt     = "adopt"
	// Merge-gate answers on a PR parked at stage/090-ready-for-human-merge
	// (hivecommons/hive#15078). merge-now merges through the hive's App at
	// once, attributed to the operator; queue-merge records the operator's
	// approval for the hive's auto-merge sweep, which merges it on the next
	// tick once CI is green (soak and the self-merge ban still apply).
	repoItemDecisionMergeNow   = "merge-now"
	repoItemDecisionQueueMerge = "queue-merge"
	// re-author is the operator's explicit choice, offered on a 090 PR
	// whose head cannot merge as-is (unsigned commits → DCO), to have the
	// hive re-home the commits on a hive-owned branch and open a
	// superseding PR (hivecommons/hive#15136). It queues the same adoption
	// the Adopt decision does; it is never implied by an Approve.
	repoItemDecisionReauthor = "re-author"

	repoItemDecisionMaxBody    = 16 << 10
	repoItemDecisionMaxComment = 4000

	auditRepoItemDecisionApprove   = "repo_item_decision_approve"
	auditRepoItemDecisionDirection = "repo_item_decision_direction"
	auditRepoItemDecisionClose     = "repo_item_decision_close"
	auditRepoItemDecisionAdopt     = "repo_item_decision_adopt"
	auditRepoItemDecisionMerge     = "repo_item_decision_merge"
	auditRepoItemDecisionQueue     = "repo_item_decision_queue_merge"

	// repoItemDecisionAdoptMarker opens the attributed note an Adopt
	// decision leaves on the PR.
	repoItemDecisionAdoptMarker = "<!-- hive:adoption-requested"
	adoptRequestLabelColor      = "5319e7"
	adoptRequestLabelDesc       = "Owner-delegate request: adopt now in supersede mode"

	issueStateReasonNotPlanned = "not_planned"
	issueStateReasonCompleted  = "completed"
)

// repoItemDecisionChangeRequests is the store a PR direction is recorded in:
// the process-wide one the un-park sweep and review planner share. A test
// swaps it for an in-memory store.
var repoItemDecisionChangeRequests = func() review.HumanChangeRequestStore { return review.DefaultHumanChangeRequests() }

type repoItemDecisionRequest struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment"`
	// Type is "issue" or "pr"; it picks the close path. Unknown reads as issue.
	Type string `json:"type"`
	// SupersededBy names the pull request that replaces a close-superseded
	// item: "458", "#458", "owner/repo#458" or a GitHub URL. Optional.
	SupersededBy string `json:"superseded_by"`
	// Force with decision "adopt" is the operator's Adopt now: adopt at
	// once, bypassing the author's presence, the grace window and
	// maintainer_policy (api_repo_item_decision_reroute.go).
	Force bool `json:"force,omitempty"`
	// To with decision "reroute" is the target clanker's login, or "any".
	To string `json:"to,omitempty"`
}

// parseRepoItemSupersededBy resolves a "superseded by" reference the way an
// operator types it, defaulting a bare number to the item's own repo.
func parseRepoItemSupersededBy(raw, defaultRepo string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[j+1:]
		}
		rest = strings.TrimSuffix(rest, "/")
		parts := strings.Split(rest, "/")
		if len(parts) == 4 && (parts[2] == "pull" || parts[2] == "issues") {
			raw = parts[0] + "/" + parts[1] + "#" + parts[3]
		} else {
			return "", fmt.Errorf("superseded_by: unrecognised pull request URL")
		}
	}
	repo := defaultRepo
	numStr := strings.TrimPrefix(raw, "#")
	if i := strings.Index(raw, "#"); i > 0 {
		repo = raw[:i]
		numStr = raw[i+1:]
	}
	n, err := strconv.Atoi(strings.TrimSpace(numStr))
	if err != nil || n <= 0 {
		return "", fmt.Errorf("superseded_by: want a pull request number, owner/repo#number or URL")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("superseded_by: repository must be owner/repo")
	}
	return fmt.Sprintf("%s/%s#%d", owner, name, n), nil
}

type repoItemDecisionResponse struct {
	OK       bool   `json:"ok"`
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	Decision string `json:"decision"`
	Status   string `json:"status"`
	Label    string `json:"label,omitempty"`
	// Released lists what a PR approval cleared beyond posting the commands:
	// "approval-recorded", "needs-human", "sentinel-label",
	// "sentinel-acknowledged". Stage is the stage label the engine derived
	// on the spot, when it did.
	Released []string `json:"released,omitempty"`
	Stage    string   `json:"stage,omitempty"`
	// Result is the shared row-action outcome for the dashboard's toast.
	Result *RowActionResult `json:"result,omitempty"`
	// Head is the PR head an approve / direction was recorded against.
	Head string `json:"head,omitempty"`
	// AdmittedBy is the trust override an Approve (either variant) wrote
	// on the stage record for Head — "operator-approval:<login>" — so the
	// contributor lane merges the head as-is and the adoption engine
	// leaves the PR alone (hivecommons/hive#15136).
	AdmittedBy string `json:"admitted_by,omitempty"`
	// CommandsSkipped is set when an Approve did not post the `/lgtm` +
	// `/approve` comments: "prow-rejects-bot-commands" on a Prow repo where
	// the hive bot is not an approver, so they would only draw another
	// "NOT APPROVED" (dibs#510). The approval itself is still recorded.
	CommandsSkipped string `json:"commands_skipped,omitempty"`
	// Rebase is set when an approval landed on a PR that is behind or
	// conflicting with its base: the rebase episode the approval opened
	// (hivecommons/hive#15103), so the row reads "Rebasing · attempt n/2".
	Rebase *StageTrackerRebase `json:"rebase,omitempty"`
}

// admitPullRequestHead records user's Approve as the trust override for
// head on the stage record (hivecommons/hive#15136) and returns the
// admitted_by value written ("" when there is no record to write to —
// the lane then derives the same admission from the recorded approval).
func (s *Server) admitPullRequestHead(repoFull string, number int, head, user string) string {
	by := stagelabels.OperatorApprovalAdmission(user)
	if strings.TrimSpace(head) == "" || by == "" {
		return ""
	}
	store := s.stageStoreOrShared()
	key := stagelabels.Key(repoFull, number)
	store.NoteAdmission(key, head, by, time.Now())
	if rec, ok := store.Get(key); ok && rec.Admission(head) == by {
		return by
	}
	return ""
}

// repoItemApproveFacts is what a PR Approve has to say about itself:
// who approved, what the approval released (repoItemApprovalReleased plus
// "admitted"), the stage label the engine derived on the spot ("" when
// deferred to the sweep), whether the sentinel alert is still open, and
// how long the merge sweep soaks a head before merging.
type repoItemApproveFacts struct {
	User          string
	Released      []string
	Stage         string
	SentinelOpen  bool
	SentinelLabel string
	SoakMinutes   int
	// Prow says the PR is on a Prow-managed repo: the relayed `/approve`
	// came from the hive's account, which is not in OWNERS, so Prow ignores
	// it and the toast must not claim the PR is approved for Prow.
	Prow bool
	// CommandsSkipped: the `/lgtm` + `/approve` comments were not posted
	// because Prow already rejected the hive's commands on this PR
	// (dibs#510); the hive's own approval record is what counts.
	CommandsSkipped bool

	// ToContributor is the contributor whose hive a direction was carried
	// to (a clanker-authored PR, hivecommons/hive#15159); "" when the
	// hive's own author agent takes it.
	ToContributor string
}

// repoItemDecisionResult reads a recorded human decision into the shared
// row-action result.
func repoItemDecisionResult(repoFull string, number int, itemType, decision, comment, supersededBy, label string, released []string) *RowActionResult {
	return repoItemDecisionResultWith(repoFull, number, itemType, decision, comment, supersededBy, label, repoItemApproveFacts{Released: released})
}

func repoItemDecisionResultWith(repoFull string, number int, itemType, decision, comment, supersededBy, label string, facts repoItemApproveFacts) *RowActionResult {
	out := &RowActionResult{Target: rowActionTarget(repoFull, itemType, number)}
	switch decision {
	case repoItemDecisionApprove:
		out.Action = rowActionApproved
		if itemType == "pr" {
			repoItemApproveResult(out, number, facts)
		} else {
			out.Outcome = "`/hive approve` recorded; parking labels lifted, `" + label + "` applied."
			out.Next = "The hive un-parks the issue and carries on; nothing is merged."
		}
	case repoItemDecisionDirection:
		out.Action = rowActionDirected
		out.Outcome = "Direction recorded as `/hive decision`: " + strings.TrimSuffix(strings.TrimSpace(comment), ".") + "."
		if itemType == "pr" && strings.TrimSpace(facts.ToContributor) != "" {
			out.Next = "Returned to @" + strings.TrimPrefix(strings.TrimSpace(facts.ToContributor), "@") + "'s hive with your direction; the row moves when it pushes a new head."
		} else if itemType == "pr" {
			out.Next = "The author agent pushes a new head; the row moves when it lands."
		} else {
			out.Next = "The hive carries on with that direction; nothing is merged."
		}
	case repoItemDecisionCloseSuperseded:
		out.Action = rowActionClosed
		if supersededBy != "" {
			out.Outcome = "Closed as superseded by " + supersededBy + "."
		} else {
			out.Outcome = "Closed as superseded."
		}
		out.Next = "Nothing further to do; the row leaves the tracker on its next refresh."
	case repoItemDecisionCloseWontFix:
		out.Action = rowActionClosed
		out.Outcome = "Closed as won't fix (not planned)."
		out.Next = "Nothing further to do; the row leaves the tracker on its next refresh."
	case repoItemDecisionCloseRead:
		out.Action = rowActionClosed
		out.Outcome = "Closed as read (advisory acknowledged)."
		out.Next = "Nothing further to do; the row leaves the tracker on its next refresh."
	case repoItemDecisionAdopt:
		out.Action = rowActionAdopted
		out.Outcome = "Adoption queued: `" + label + "` applied."
		out.Next = "The hive opens its own PR carrying the author's commits and sign-off on its next cycle; the original is closed as superseded."
	case repoItemDecisionReauthor:
		out.Action = rowActionAdopted
		out.Outcome = "Re-authoring queued: `" + label + "` applied."
		out.Next = "The hive re-homes the commits on a hive-owned, signed-off branch and opens a superseding PR on its next cycle; this PR is closed when that one merges. Nothing is merged as-is."
	case repoItemDecisionMergeNow:
		out.Action = rowActionMerged
		out.Outcome = "Merged into `" + label + "` now via hive's identity, attributed to you."
		out.Next = "Nothing further to do; the row leaves the tracker on its next refresh."
	case repoItemDecisionQueueMerge:
		out.Action = rowActionQueued
		out.Outcome = "Your approval is recorded; `" + label + "` applied."
		out.Next = "Hive's merge sweep merges it on its next pass once CI is green and the head has soaked."
	}
	return out
}

func renderRepoItemDecisionCloseComment(verb, user, comment, supersededBy string) string {
	by := "an operator"
	if strings.TrimSpace(user) != "" {
		by = "@" + strings.TrimSpace(user)
	}
	if supersededBy != "" {
		verb += " by " + supersededBy
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Closed as %s by %s from the Hive dashboard.", verb, by)
	if c := strings.TrimSpace(comment); c != "" {
		fmt.Fprintf(&b, "\n\n%s", c)
	}
	return b.String()
}

func (s *Server) handleRepoItemDecision(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, repoName)
	if !allowed {
		jsonError(w, msg, status)
		return
	}
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if user == "" {
		jsonError(w, "GitHub user session required to record a decision", http.StatusUnauthorized)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	var body repoItemDecisionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, repoItemDecisionMaxBody)).Decode(&body); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	decision := strings.ToLower(strings.TrimSpace(body.Decision))
	comment := strings.TrimSpace(body.Comment)
	if len(comment) > repoItemDecisionMaxComment {
		jsonError(w, fmt.Sprintf("comment exceeds %d characters", repoItemDecisionMaxComment), http.StatusBadRequest)
		return
	}
	itemType := strings.ToLower(strings.TrimSpace(body.Type))
	if itemType != "pr" {
		itemType = "issue"
	}
	repoFull := owner + "/" + repoName
	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	gh := s.deps.GHClient
	numStr := strconv.Itoa(number)

	switch decision {
	case repoItemDecisionApprove, repoItemDecisionDirection:
		if decision == repoItemDecisionDirection && comment == "" {
			jsonError(w, "a decision needs the direction text", http.StatusBadRequest)
			return
		}
		if itemType == "pr" {
			s.handlePullRequestDecision(ctx, w, repoFull, number, user, decision, comment)
			return
		}
		text := ""
		if decision == repoItemDecisionDirection {
			text = comment
		}
		event, err := gh.RecordOperatorIssueDecision(ctx, repoFull, number, user, text)
		switch {
		case errors.Is(err, ghpkg.ErrIssueNotParked):
			jsonError(w, "issue is not waiting on a decision", http.StatusConflict)
			return
		case errors.Is(err, ghpkg.ErrDecisionTargetIsPR):
			jsonError(w, "this is a pull request; send type \"pr\" to decide it", http.StatusBadRequest)
			return
		case err != nil:
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		action := auditRepoItemDecisionApprove
		if decision == repoItemDecisionDirection {
			action = auditRepoItemDecisionDirection
		}
		s.audit.LogRecord(user, action, auditDetail("repo", repoFull, "number", numStr, "decision", event.Decision), "", repoFull, number)
		jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: "unparked", Label: ghpkg.HumanAckLabel,
			Result: repoItemDecisionResult(repoFull, number, itemType, decision, comment, "", ghpkg.HumanAckLabel, nil)})
	case repoItemDecisionCloseSuperseded, repoItemDecisionCloseWontFix, repoItemDecisionCloseRead:
		verb := "won't fix"
		stateReason := issueStateReasonNotPlanned
		supersededBy := ""
		if decision == repoItemDecisionCloseRead {
			if itemType != "issue" {
				jsonError(w, "close-read applies to issues", http.StatusBadRequest)
				return
			}
			verb, stateReason = "read (advisory acknowledged)", issueStateReasonCompleted
		}
		if decision == repoItemDecisionCloseSuperseded {
			verb = "superseded"
			ref, err := parseRepoItemSupersededBy(body.SupersededBy, repoFull)
			if err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			if ref == repoFull+"#"+numStr {
				jsonError(w, "superseded_by: an item cannot supersede itself", http.StatusBadRequest)
				return
			}
			supersededBy = ref
		}
		if err := gh.CreateIssueComment(ctx, repoFull, number, renderRepoItemDecisionCloseComment(verb, user, comment, supersededBy)); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		label := ""
		if itemType == "pr" && decision == repoItemDecisionCloseSuperseded {
			if err := gh.CloseSupersededPR(ctx, repoFull, number); err != nil {
				jsonError(w, err.Error(), http.StatusBadGateway)
				return
			}
			label = ghpkg.SupersededLabel
		} else {
			err := gh.CloseIssue(ctx, repoFull, number, ghpkg.IssueCloseOptions{
				OverrideReason:          "closed as " + verb + " by @" + user + " from the Hive dashboard",
				SuppressOverrideComment: true,
				StateReason:             stateReason,
				// A recorded human decision may close a tracking issue;
				// only the automated paths are refused.
				AllowTrackingIssue: true,
			})
			if err != nil {
				jsonError(w, err.Error(), http.StatusBadGateway)
				return
			}
		}
		s.audit.LogRecord(user, auditRepoItemDecisionClose, auditDetail("repo", repoFull, "number", numStr, "type", itemType, "reason", verb, "superseded_by", supersededBy), "", repoFull, number)
		jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: "closed", Label: label,
			Result: repoItemDecisionResult(repoFull, number, itemType, decision, comment, supersededBy, label, nil)})
	case repoItemDecisionReroute:
		if itemType != "pr" {
			jsonError(w, "reroute applies to pull requests", http.StatusBadRequest)
			return
		}
		s.handleRepoItemReroute(ctx, w, repoFull, number, user, comment, body.To)
	case repoItemDecisionAdopt, repoItemDecisionReauthor:
		if itemType != "pr" {
			jsonError(w, decision+" applies to pull requests", http.StatusBadRequest)
			return
		}
		if !s.stageAdoptionEnabled() {
			jsonError(w, "adoption requires governor.labels.stage.adoption to be enabled on this hive", http.StatusConflict)
			return
		}
		if body.Force && decision == repoItemDecisionAdopt {
			s.handleRepoItemAdoptNow(ctx, w, repoFull, number, user, comment)
			return
		}
		note := renderRepoItemDecisionAdoptComment(user, comment)
		if decision == repoItemDecisionReauthor {
			note = renderRepoItemDecisionReauthorComment(user, comment)
		}
		if err := gh.CreateIssueComment(ctx, repoFull, number, note); err != nil {
			jsonError(w, "posting adoption note: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := gh.EnsureIssueLabel(ctx, repoFull, delegatedecision.LabelAdoptSupersede, adoptRequestLabelColor, adoptRequestLabelDesc); err != nil {
			jsonError(w, "ensuring adoption request label: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := gh.AddLabels(ctx, repoFull, number, []string{delegatedecision.LabelAdoptSupersede}); err != nil {
			jsonError(w, "queuing adoption: "+err.Error(), http.StatusBadGateway)
			return
		}
		s.audit.LogRecord(user, auditRepoItemDecisionAdopt, auditDetail("repo", repoFull, "number", numStr, "type", itemType, "label", delegatedecision.LabelAdoptSupersede, "decision", decision), "", repoFull, number)
		jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: "adoption-queued", Label: delegatedecision.LabelAdoptSupersede,
			Result: repoItemDecisionResult(repoFull, number, itemType, decision, comment, "", delegatedecision.LabelAdoptSupersede, nil)})
	case repoItemDecisionMergeNow, repoItemDecisionQueueMerge:
		if itemType != "pr" {
			jsonError(w, decision+" applies to pull requests", http.StatusBadRequest)
			return
		}
		if !repoItemMergeDecisionAllowed(r) {
			jsonError(w, "merger or owner access required to merge or queue a pull request", http.StatusForbidden)
			return
		}
		s.handlePullRequestMergeDecision(ctx, w, repoFull, number, user, decision, comment)
	default:
		jsonError(w, "unknown decision; want approve, direction, close-superseded, close-wontfix, close-read, adopt, reroute, re-author, merge-now or queue-merge", http.StatusBadRequest)
	}
}

// repoItemMergeDecisionAllowed is the merge bar on top of the repo-hold bar:
// a verified owner or a merger (the same bar as POST .../queue-automerge),
// since both merge decisions end with a commit on the base branch.
func repoItemMergeDecisionAllowed(r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if isOwnerRole(role) {
		return r.Header.Get(ownerRoleVerifiedHeader) == "true"
	}
	return config.RoleAtLeast(role, config.RoleMerger)
}

// handlePullRequestMergeDecision answers merge-now / queue-merge on a PR the
// merge gate parked for a person (stage/090-ready-for-human-merge).
func (s *Server) handlePullRequestMergeDecision(ctx context.Context, w http.ResponseWriter, repoFull string, number int, user, decision, comment string) {
	gh := s.deps.GHClient
	numStr := strconv.Itoa(number)
	state, err := gh.GetPRState(ctx, repoFull, number)
	if err != nil {
		jsonError(w, "reading the pull request: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !state.MergedAt.IsZero() {
		jsonError(w, "pull request is already merged", http.StatusConflict)
		return
	}
	if !strings.EqualFold(state.State, "open") {
		jsonError(w, "pull request is already closed", http.StatusConflict)
		return
	}
	author, err := gh.GetPRAuthor(ctx, repoFull, number)
	if err != nil {
		jsonError(w, "reading the pull request author: "+err.Error(), http.StatusBadGateway)
		return
	}
	if strings.EqualFold(author, user) {
		jsonError(w, "you opened this pull request; another maintainer has to merge it", http.StatusForbidden)
		return
	}
	base := state.BaseRef
	if base == "" {
		base = "the base branch"
	}
	// Either variant is the person's trust override for this head: written
	// before the GitHub call so the adoption engine cannot take the PR over
	// between the approval landing and the merge (hivecommons/hive#15136).
	admitted := s.admitPullRequestHead(repoFull, number, state.HeadSHA, user)
	if decision == repoItemDecisionQueueMerge {
		if err := gh.QueuePRAutoMergeWithNote(ctx, repoFull, number, user, renderRepoItemDecisionQueueNote(user, comment)); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		label := gh.AutoMergeLabel()
		s.audit.LogRecord(user, auditRepoItemDecisionQueue, auditDetail("repo", repoFull, "number", numStr, "type", "pr", "head", state.HeadSHA, "author", author, "label", label, "admitted_by", admitted), "", repoFull, number)
		s.invalidateStageTrackerForWebhook()
		jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: "queued-for-merge", Label: label, Head: state.HeadSHA, AdmittedBy: admitted,
			Result: repoItemDecisionResult(repoFull, number, "pr", decision, comment, "", label, nil)})
		return
	}
	// A head GitHub reports behind or conflicting cannot merge (405): the
	// same click becomes "Rebase, then merge as maintainer" — approval on
	// record, rebase kicked, merge on green (hivecommons/hive#14822).
	if ms, err := gh.GetPRMergeState(ctx, repoFull, number); err == nil && ms.NeedsRebase() {
		s.armRebaseThenMerge(ctx, w, repoFull, number, user, comment, ms)
		return
	} else if err == nil && rebaseMergeStore().Armed(repoFull, number) {
		// The rebase landed but the follow-through has not merged yet: the
		// follow-through owns the merge; do not race it.
		prev, _ := rebaseMergeStore().Get(repoFull, number)
		jsonStatusResponse(w, http.StatusAccepted, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: repoItemStatusRebaseQd, Head: prev.ApprovedHead,
			Result: rebaseMergeQueuedResult(prev, true)})
		return
	}
	if err := gh.CreateIssueComment(ctx, repoFull, number, renderRepoItemDecisionMergeComment(user, comment)); err != nil {
		jsonError(w, "posting merge note: "+err.Error(), http.StatusBadGateway)
		return
	}
	// The head the operator saw is the head that merges: a push in between
	// makes GitHub refuse (409) rather than merge code nobody looked at.
	res, err := gh.MergePRVia(ctx, repoFull, number, "", state.HeadSHA, ghpkg.PRAuditPathDashboard)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !res.Merged {
		jsonError(w, "GitHub did not merge the pull request: "+strings.TrimSpace(res.Message), http.StatusConflict)
		return
	}
	s.audit.LogRecord(user, auditRepoItemDecisionMerge, auditDetail("repo", repoFull, "number", numStr, "type", "pr", "head", state.HeadSHA, "author", author, "base", state.BaseRef, "sha", res.SHA, "admitted_by", admitted), "", repoFull, number)
	s.invalidateStageTrackerForWebhook()
	jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: "merged", Head: state.HeadSHA, AdmittedBy: admitted,
		Result: repoItemDecisionResult(repoFull, number, "pr", decision, comment, "", base, nil)})
}

// renderRepoItemDecisionMergeComment is the attributed note left on the PR
// before the hive's App merges it for the operator.
func renderRepoItemDecisionMergeComment(user, comment string) string {
	body := fmt.Sprintf("Merged as maintainer by @%s from the Hive dashboard: the merge gate parked this pull request for a human because its author is not a trusted merger on this repository.", strings.TrimSpace(user))
	if c := strings.TrimSpace(comment); c != "" {
		body += "\n\n> " + strings.ReplaceAll(c, "\n", "\n> ")
	}
	return body
}

// renderRepoItemDecisionQueueNote is the note appended to the App's approval
// review when the operator lets the hive merge.
func renderRepoItemDecisionQueueNote(user, comment string) string {
	note := fmt.Sprintf("Approved from the Hive dashboard by @%s as the owner decision on a Ready-for-human-merge pull request; the hive's merge sweep merges it on green CI.", strings.TrimSpace(user))
	if c := strings.TrimSpace(comment); c != "" {
		note += "\n\n> " + strings.ReplaceAll(c, "\n", "\n> ")
	}
	return note
}

// renderRepoItemDecisionAdoptComment is the attributed note an Adopt
// decision leaves on the PR before the request label is applied.
func renderRepoItemDecisionAdoptComment(user, comment string) string {
	body := fmt.Sprintf("%s by=%s -->\n@%s asked the hive to adopt this pull request: its author maintains the repository and their clanker has been offline since the nudge, so adoption waited for an owner's decision (`governor.labels.stage.adoption.maintainer_policy: ask`). The adoption engine takes it from here on its next pass; the author's commits and sign-off are kept.", repoItemDecisionAdoptMarker, user, user)
	if comment != "" {
		body += "\n\n> " + strings.ReplaceAll(comment, "\n", "\n> ")
	}
	return body
}

// renderRepoItemDecisionReauthorComment is the attributed note a
// Re-author decision leaves on the PR before the adoption request label is
// applied (hivecommons/hive#15136).
func renderRepoItemDecisionReauthorComment(user, comment string) string {
	body := fmt.Sprintf("%s by=%s -->\n@%s asked the hive to re-author this pull request as a hive PR: its head cannot merge as-is (commits without a Signed-off-by), so the hive re-homes the commits on a hive-owned, signed-off branch and opens a superseding PR; this one is closed when that merges. The author's commits are preserved and attributed.", repoItemDecisionAdoptMarker, user, user)
	if c := strings.TrimSpace(comment); c != "" {
		body += "\n\n> " + strings.ReplaceAll(c, "\n", "\n> ")
	}
	return body
}

// handlePullRequestDecision answers approve / direction on a pull request.
func (s *Server) handlePullRequestDecision(ctx context.Context, w http.ResponseWriter, repoFull string, number int, user, decision, comment string) {
	gh := s.deps.GHClient
	numStr := strconv.Itoa(number)
	var (
		head string
		err  error
	)
	var released []string
	stage, commandsSkipped := "", ""
	sentinelOpen, prow := false, false
	toContributor := ""
	if decision == repoItemDecisionApprove {
		var event ghpkg.PullRequestApprovalEvent
		event, err = gh.RecordOperatorPullRequestApprovalWith(ctx, repoFull, number, user, ghpkg.PullRequestApprovalOptions{
			HumanChangeRequests: repoItemDecisionChangeRequests(),
			LiftNeedsHuman:      true,
			SentinelLabel:       s.sentinelLabel(),
		})
		head = event.HeadSHA
		released, stage, sentinelOpen, prow = repoItemApprovalReleased(event), event.Stage, event.SentinelLabelOpen, event.ProwManaged
		commandsSkipped = event.CommandsSkipped
	} else {
		var event ghpkg.PullRequestDecisionEvent
		event, err = gh.RecordOperatorPullRequestDecision(ctx, repoFull, number, user, comment, repoItemDecisionChangeRequests())
		head, toContributor = event.HeadSHA, event.ToContributor
	}
	switch {
	case errors.Is(err, ghpkg.ErrPullRequestNotFound):
		jsonError(w, "pull request not found", http.StatusNotFound)
		return
	case errors.Is(err, ghpkg.ErrPullRequestClosed):
		jsonError(w, "pull request is already closed", http.StatusConflict)
		return
	case errors.Is(err, ghpkg.ErrPullRequestNotHiveAuthored):
		jsonError(w, "the hive did not open this pull request, so it cannot carry a direction to its author; leave a changes-requested review instead", http.StatusConflict)
		return
	case errors.Is(err, ghpkg.ErrPullRequestHeadUnknown):
		jsonError(w, "could not read the pull request head; leave a changes-requested review instead", http.StatusBadGateway)
		return
	case err != nil:
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	action, status := auditRepoItemDecisionApprove, "approval-commented"
	admitted := ""
	if decision == repoItemDecisionDirection {
		action, status = auditRepoItemDecisionDirection, "direction-recorded"
		if toContributor != "" {
			status = "direction-returned-to-contributor"
		}
	} else {
		// The person's Approve is the trust override for this head: the
		// contributor lane merges it as-is and the adoption engine leaves
		// it alone while the admission stands (hivecommons/hive#15136).
		if admitted = s.admitPullRequestHead(repoFull, number, head, user); admitted != "" {
			released = append(released, "admitted")
		}
	}
	s.audit.LogRecord(user, action, auditDetail("repo", repoFull, "number", numStr, "type", "pr", "head", head, "decision", comment, "released", strings.Join(released, ","), "stage", stage, "admitted_by", admitted, "commands_skipped", commandsSkipped, "to_contributor", toContributor), "", repoFull, number)
	result := repoItemDecisionResultWith(repoFull, number, "pr", decision, comment, "", "", repoItemApproveFacts{
		User: user, Released: released, Stage: stage, SentinelOpen: sentinelOpen, SentinelLabel: s.sentinelLabel(), SoakMinutes: s.approveSoakMinutes(admitted != ""), Prow: prow, CommandsSkipped: commandsSkipped != "", ToContributor: toContributor,
	})
	var rebase *StageTrackerRebase
	if decision == repoItemDecisionApprove {
		// An approval on a PR that is behind or conflicting does not
		// reach the merge gate (it skips `conflicting` for good): say so,
		// and ask the fixer for the rebase now instead of promising the
		// gate "takes it from here" (hivecommons/hive#15103).
		if ms, err := gh.GetPRMergeState(ctx, repoFull, number); err == nil && ms.NeedsRebase() {
			rebase = s.queueRebaseOnApprove(ctx, repoFull, number, user, nonEmptyString(head, ms.HeadSHA), ms)
			approveNeedsRebaseResult(result, number, ms, rebase)
		}
	}
	// The row's park was just answered: serve the re-derived stage on the
	// next tracker read instead of the cached 085.
	s.invalidateStageTrackerForWebhook()
	jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: status, Released: released, Stage: stage, Head: head, AdmittedBy: admitted, CommandsSkipped: commandsSkipped, Rebase: rebase,
		Result: result})
}

// queueRebaseOnApprove opens the rebase episode for an approved PR that
// needs one: GitHub's update-branch on the spot for the hive's own PR that
// is merely behind, otherwise the fixer's rebase-resolve task on the next
// router pass (the episode carries the operator as requester so the router
// lists the PR whoever opened it, as long as the hive can push to it).
func (s *Server) queueRebaseOnApprove(ctx context.Context, repoFull string, number int, user, head string, ms ghpkg.PRMergeState) *StageTrackerRebase {
	gh := s.deps.GHClient
	now := rebaseMergeNow()
	a := rebaseAttemptsStore().Request(repoFull, number, head, user, now)
	how := "fixer rebase-resolve (next router pass)"
	if gh.IsHiveAuthorLogin(ms.Author) && !ms.Conflicting() {
		if err := gh.UpdateBranch(ctx, repoFull, number); err == nil {
			how = "update-branch"
		} else if s.logger != nil {
			s.logger.Warn("approve: update-branch failed; leaving the rebase to the fixer", "repo", repoFull, "number", number, "error", err)
		}
	}
	if s.logger != nil {
		s.logger.Info("approve on a PR that needs a rebase: rebase queued", "repo", repoFull, "number", number, "actor", user, "head", head, "base", ms.BaseRef, "mergeable_state", ms.MergeableState, "attempt", a.Count(), "max", rebasemerge.MaxRebaseAttempts, "kick", how)
	}
	return &StageTrackerRebase{Attempt: min(max(a.Count(), 1), rebasemerge.MaxRebaseAttempts), Max: rebasemerge.MaxRebaseAttempts, Exhausted: a.Exhausted(), Head: rebaseMergeShortSHA(a.Head()), Text: rebasemerge.AttemptText(a)}
}

// approveNeedsRebaseResult rewrites the approve toast for a PR that is
// behind or conflicting: what the approval did, what the hive does next,
// and the one-click upgrade to "Rebase, then merge as maintainer".
func approveNeedsRebaseResult(out *RowActionResult, number int, ms ghpkg.PRMergeState, rebase *StageTrackerRebase) {
	base := nonEmptyString(ms.BaseRef, "its base branch")
	attempt := ""
	if rebase != nil && !rebase.Exhausted {
		attempt = fmt.Sprintf(" (attempt %d/%d)", rebase.Attempt, rebase.Max)
	}
	if ms.Conflicting() {
		out.Outcome = fmt.Sprintf("Approved — #%d conflicts with %s; hive will rebase it next pass%s.", number, base, attempt)
	} else {
		out.Outcome = fmt.Sprintf("Approved — #%d is behind %s; hive is updating the branch%s.", number, base, attempt)
	}
	if rebase != nil && rebase.Exhausted {
		out.Outcome = fmt.Sprintf("Approved — #%d conflicts with %s and hive's %d rebase attempts are used up; a person has to resolve it.", number, base, rebase.Max)
		out.Next = "Resolve the conflict on the branch yourself, or adopt it into a fresh branch / close it as superseded from the row menu."
		return
	}
	out.Next = "The merge gate skips a conflicting PR, so your approval alone would not land it. Once the rebased head is green the gate takes it — or have hive merge it for you right after the rebase:"
	opt := decisionOption(decisionActionMergeNow, rebaseMergeFollowThroughLabel, true, "")
	opt.Consequence = rebaseMergeFollowThroughConsequence(number, ms.BaseRef)
	out.Options = []StageTrackerDecisionOption{opt}
}

// repoItemApprovalReleased names what a PR approval released, for the
// response and the audit line.
func repoItemApprovalReleased(event ghpkg.PullRequestApprovalEvent) []string {
	var out []string
	if event.Recorded {
		out = append(out, "approval-recorded")
	}
	if event.NeedsHumanLifted {
		out = append(out, "needs-human")
	}
	if event.SentinelLabelCleared {
		out = append(out, "sentinel-label")
	}
	if event.SentinelAcknowledged {
		out = append(out, "sentinel-acknowledged")
	}
	return out
}

// repoItemApproveResult words a PR Approve the way the owner reads it
// (hivecommons/dibs#510 read "`/lgtm` + `/approve` posted (released:
// approval-recorded, sentinel-label)" followed by the ⚠ sentinel warning
// it had just released and "the dashboard merges nothing"): the headline
// is the outcome in plain words, Next is the concrete next step and who
// merges, and what was posted rides Posted as the muted secondary line.
func repoItemApproveResult(out *RowActionResult, number int, facts repoItemApproveFacts) {
	released := map[string]bool{}
	for _, r := range facts.Released {
		released[r] = true
	}
	headline := "Approved #" + strconv.Itoa(number)
	if facts.Prow {
		headline = "Approval of #" + strconv.Itoa(number) + " recorded in hive"
	}
	if u := strings.TrimSpace(facts.User); u != "" {
		headline += " as @" + u
	}
	out.SentinelCleared = released["sentinel-label"]
	out.SentinelOpen = facts.SentinelOpen && !out.SentinelCleared
	switch {
	case out.SentinelCleared:
		headline += " · sentinel alert cleared by your review"
	case out.SentinelOpen:
		headline += " · sentinel alert still open"
	}
	out.Outcome = headline + "."
	if facts.CommandsSkipped {
		out.Posted = append(out.Posted, "`/lgtm` + `/approve` not posted (Prow does not accept the hive bot's commands on this repo)")
	} else {
		out.Posted = append(out.Posted, "`/lgtm` posted", "`/approve` posted")
	}
	if released["needs-human"] {
		out.Posted = append(out.Posted, "`needs-human` label removed")
	}
	if out.SentinelCleared {
		out.Posted = append(out.Posted, "`"+repoItemSentinelLabelOrDefault(facts.SentinelLabel)+"` label removed")
	}
	if released["admitted"] {
		out.Posted = append(out.Posted, "head admitted as-is on your approval")
	}
	switch {
	case out.SentinelOpen:
		out.Next = "The `" + repoItemSentinelLabelOrDefault(facts.SentinelLabel) + "` label is still on the PR: a maintainer must review the flagged head and take the label off on GitHub before hive's merge sweep will touch it."
	case facts.Stage == stagelabels.ReadyForHumanMerge.Label():
		out.Next = "Hive cannot merge this one (trust or role blocks it): merge it on GitHub yourself, or use Approve & merge as maintainer."
	default:
		out.Next = "Hive's merge sweep merges it on its next pass once CI is green and the head has soaked" + repoItemSoakNote(facts.SoakMinutes) + "."
	}
	if facts.Prow || facts.CommandsSkipped {
		// Prow reads `/approve` only from OWNERS; the hive's account is not
		// one, so the relayed command is "NOT APPROVED" there.
		out.Next = "Prow approval still needs an OWNERS reviewer: the relayed `/approve` came from hive's account, which Prow does not count. " + out.Next
	}
}

func repoItemSentinelLabelOrDefault(label string) string {
	if strings.TrimSpace(label) == "" {
		return config.DefaultSentinelLabel
	}
	return strings.TrimSpace(label)
}

func repoItemSoakNote(minutes int) string {
	if minutes <= 0 {
		return ""
	}
	return " (≈" + strconv.Itoa(minutes) + " min)"
}

// approveSoakMinutes is how long the merge sweep soaks an approved head:
// the contributor lane's soak when the approval admitted a foreign head,
// otherwise the sweep's minimum head age.
func (s *Server) approveSoakMinutes(admitted bool) int {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		if admitted {
			return config.DefaultContributorLaneSoakMinutes
		}
		return int(config.DefaultAutoMergeMinHeadAge / time.Minute)
	}
	if admitted {
		return s.deps.Config.AutoMerge.ContributorLane.EffectiveSoakMinutes()
	}
	return int(s.deps.Config.AutoMerge.EffectiveMinHeadAge() / time.Minute)
}
