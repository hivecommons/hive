package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
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
//     author agent is kicked with it (Client.RecordOperatorPullRequestDecision).
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
	repoItemDecisionAdopt           = "adopt"

	repoItemDecisionMaxBody    = 16 << 10
	repoItemDecisionMaxComment = 4000

	auditRepoItemDecisionApprove   = "repo_item_decision_approve"
	auditRepoItemDecisionDirection = "repo_item_decision_direction"
	auditRepoItemDecisionClose     = "repo_item_decision_close"
	auditRepoItemDecisionAdopt     = "repo_item_decision_adopt"

	// repoItemDecisionAdoptMarker opens the attributed note an Adopt
	// decision leaves on the PR.
	repoItemDecisionAdoptMarker = "<!-- hive:adoption-requested"
	adoptRequestLabelColor      = "5319e7"
	adoptRequestLabelDesc       = "Owner-delegate request: adopt now in supersede mode"

	issueStateReasonNotPlanned = "not_planned"
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
}

// repoItemDecisionResult reads a recorded human decision into the shared
// row-action result.
func repoItemDecisionResult(repoFull string, number int, itemType, decision, comment, supersededBy, label string, released []string) *RowActionResult {
	out := &RowActionResult{Target: rowActionTarget(repoFull, itemType, number)}
	switch decision {
	case repoItemDecisionApprove:
		out.Action = rowActionApproved
		if itemType == "pr" {
			out.Outcome = "`/lgtm` + `/approve` posted"
			if len(released) > 0 {
				out.Outcome += " (released: " + strings.Join(released, ", ") + ")"
			}
			out.Outcome += "."
			out.Next = "The merge gate takes it from here; the dashboard merges nothing."
		} else {
			out.Outcome = "`/hive approve` recorded; parking labels lifted, `" + label + "` applied."
			out.Next = "The hive un-parks the issue and carries on; nothing is merged."
		}
	case repoItemDecisionDirection:
		out.Action = rowActionDirected
		out.Outcome = "Direction recorded as `/hive decision`: " + strings.TrimSuffix(strings.TrimSpace(comment), ".") + "."
		if itemType == "pr" {
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
	case repoItemDecisionAdopt:
		out.Action = rowActionAdopted
		out.Outcome = "Adoption queued: `" + label + "` applied."
		out.Next = "The hive opens its own PR carrying the author's commits and sign-off on its next cycle; the original is closed as superseded."
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
	case repoItemDecisionCloseSuperseded, repoItemDecisionCloseWontFix:
		verb := "won't fix"
		supersededBy := ""
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
				StateReason:             issueStateReasonNotPlanned,
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
	case repoItemDecisionAdopt:
		if itemType != "pr" {
			jsonError(w, "adopt applies to pull requests", http.StatusBadRequest)
			return
		}
		if !s.stageAdoptionEnabled() {
			jsonError(w, "adoption requires governor.labels.stage.adoption to be enabled on this hive", http.StatusConflict)
			return
		}
		if err := gh.CreateIssueComment(ctx, repoFull, number, renderRepoItemDecisionAdoptComment(user, comment)); err != nil {
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
		s.audit.LogRecord(user, auditRepoItemDecisionAdopt, auditDetail("repo", repoFull, "number", numStr, "type", itemType, "label", delegatedecision.LabelAdoptSupersede), "", repoFull, number)
		jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: "adoption-queued", Label: delegatedecision.LabelAdoptSupersede,
			Result: repoItemDecisionResult(repoFull, number, itemType, decision, comment, "", delegatedecision.LabelAdoptSupersede, nil)})
	default:
		jsonError(w, "unknown decision; want approve, direction, close-superseded, close-wontfix or adopt", http.StatusBadRequest)
	}
}

// renderRepoItemDecisionAdoptComment is the attributed note an Adopt
// decision leaves on the PR before the request label is applied.
func renderRepoItemDecisionAdoptComment(user, comment string) string {
	body := fmt.Sprintf("%s by=%s -->\n@%s asked the hive to adopt this pull request: its author maintains the repository and their hive has been offline since the nudge, so adoption waited for an owner's decision (`governor.labels.stage.adoption.maintainer_policy: ask`). The adoption engine takes it from here on its next pass; the author's commits and sign-off are kept.", repoItemDecisionAdoptMarker, user, user)
	if comment != "" {
		body += "\n\n> " + strings.ReplaceAll(comment, "\n", "\n> ")
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
	stage := ""
	if decision == repoItemDecisionApprove {
		var event ghpkg.PullRequestApprovalEvent
		event, err = gh.RecordOperatorPullRequestApprovalWith(ctx, repoFull, number, user, ghpkg.PullRequestApprovalOptions{
			HumanChangeRequests: repoItemDecisionChangeRequests(),
			LiftNeedsHuman:      true,
			SentinelLabel:       s.sentinelLabel(),
		})
		head = event.HeadSHA
		released, stage = repoItemApprovalReleased(event), event.Stage
	} else {
		var event ghpkg.PullRequestDecisionEvent
		event, err = gh.RecordOperatorPullRequestDecision(ctx, repoFull, number, user, comment, repoItemDecisionChangeRequests())
		head = event.HeadSHA
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
	if decision == repoItemDecisionDirection {
		action, status = auditRepoItemDecisionDirection, "direction-recorded"
	}
	s.audit.LogRecord(user, action, auditDetail("repo", repoFull, "number", numStr, "type", "pr", "head", head, "decision", comment, "released", strings.Join(released, ","), "stage", stage), "", repoFull, number)
	// The row's park was just answered: serve the re-derived stage on the
	// next tracker read instead of the cached 085.
	s.invalidateStageTrackerForWebhook()
	jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: status, Released: released, Stage: stage, Head: head,
		Result: repoItemDecisionResult(repoFull, number, "pr", decision, comment, "", "", released)})
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
