package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// "Waive SHA requirement" / "Restore SHA requirement" from the activity
// monitor's row menu (hivecommons/hive#18866): the override for the SHA-hold
// gate. Waiving adds the persistent `sha-waived` label (the SHA sweep never
// re-adds `needs-sha` while it is on), removes `needs-sha` so the issue is
// released at once, and posts one App-authored comment carrying a hidden
// marker that names who waived it, which is how the pill reads "SHA waived by
// <login>" after a restart. Restoring removes `sha-waived`; the next sweep
// holds the issue again if it still has no SHA and no exemption. Authorised
// exactly like the hold toggle (verified owner role, or write permission on
// the repo); audited under the dashboard user. Issues only.

const (
	shaWaiverAuditAction = "repo_item_sha_waiver"
	shaWaiverLabelColor  = "0e8a16"

	rowActionSHAWaived   = "sha-waived"
	rowActionSHARestored = "sha-restored"
)

type repoSHAWaiverRequest struct {
	Waived bool `json:"waived"`
}

type repoSHAWaiverResponse struct {
	OK           bool     `json:"ok"`
	Repo         string   `json:"repo"`
	Number       int      `json:"number"`
	Waived       bool     `json:"waived"`
	Added        []string `json:"added"`
	Removed      []string `json:"removed"`
	MinStatusSeq uint64   `json:"minStatusSeq"`
	// Result is the shared row-action outcome for the dashboard's toast.
	Result *RowActionResult `json:"result,omitempty"`
}

func (s *Server) handleRepoItemSHAWaiver(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, repoName)
	if !allowed {
		jsonError(w, msg, status)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	var body repoSHAWaiverRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	repoFull := owner + "/" + repoName
	if !s.watchesRepo(repoFull) {
		jsonError(w, "repository is not configured for this hive", http.StatusForbidden)
		return
	}
	issue, resp, err := s.deps.GHClient.GoGitHub().Issues.Get(ctx, owner, repoName, number)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			jsonError(w, "issue not found", http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if issue.IsPullRequest() {
		jsonError(w, "only issues carry the SHA requirement", http.StatusNotFound)
		return
	}
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.GetName())
	}
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	target := rowActionTarget(repoFull, rowActionTargetIssue, number)
	out := repoSHAWaiverResponse{OK: true, Repo: repoFull, Number: number, Waived: body.Waived, Added: []string{}, Removed: []string{}}

	if body.Waived {
		if shaLabelOn(labels, github.SHAWaivedLabel) {
			res := rowActionNoopResult(target, "the SHA requirement was already waived", time.Time{}, "")
			out.Result = &res
			jsonResponse(w, out)
			return
		}
		if err := s.deps.GHClient.EnsureIssueLabel(ctx, repoFull, github.SHAWaivedLabel, shaWaiverLabelColor, github.SHAWaivedLabelDescription); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := s.deps.GHClient.AddLabels(ctx, repoFull, number, []string{github.SHAWaivedLabel}); err != nil { // writeback:solicited a maintainer waived or restored the SHA requirement from the row menu
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Added = []string{github.SHAWaivedLabel}
		// Release the hold now rather than at the next sweep.
		for _, name := range []string{s.shaHoldLabelName(), config.DefaultSHAHoldTrustedLabel} {
			if shaLabelOn(labels, name) && !shaLabelOn(out.Removed, name) {
				if err := s.deps.GHClient.RemoveLabel(ctx, repoFull, number, name); err != nil { // writeback:solicited a maintainer waived or restored the SHA requirement from the row menu
					jsonError(w, err.Error(), http.StatusBadGateway)
					return
				}
				out.Removed = append(out.Removed, name)
			}
		}
		by := user
		if by == "" {
			by = "owner"
		}
		s.deps.GHClient.RecordSHAWaiver(repoFull, number, by)
		note := github.SHAWaivedMarker(by) + "\nThe commit-SHA requirement for this issue was waived by `" + by + "`. " +
			"The hive will work it without a SHA. A maintainer can restore the requirement from the dashboard."
		if err := s.deps.GHClient.CreateIssueComment(ctx, repoFull, number, note); err != nil && s.logger != nil { // writeback:solicited a maintainer waived or restored the SHA requirement from the row menu
			s.logger.Warn("SHA waiver: could not post the waiver note", "repo", repoFull, "issue", number, "error", err)
		}
		out.Result = &RowActionResult{Action: rowActionSHAWaived, Target: target,
			Outcome: "SHA requirement waived: `" + github.SHAWaivedLabel + "` added" + removedClause(out.Removed) + ".",
			Next:    "The hive may work this issue on its next pass; the SHA sweep will not hold it again until the requirement is restored."}
	} else {
		if !shaLabelOn(labels, github.SHAWaivedLabel) {
			res := rowActionNoopResult(target, "the SHA requirement was not waived", time.Time{}, "")
			out.Result = &res
			jsonResponse(w, out)
			return
		}
		if err := s.deps.GHClient.RemoveLabel(ctx, repoFull, number, github.SHAWaivedLabel); err != nil { // writeback:solicited a maintainer waived or restored the SHA requirement from the row menu
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Removed = []string{github.SHAWaivedLabel}
		s.deps.GHClient.RecordSHAWaiver(repoFull, number, "")
		out.Result = &RowActionResult{Action: rowActionSHARestored, Target: target,
			Outcome: "SHA requirement restored: `" + github.SHAWaivedLabel + "` removed.",
			Next:    "The next SHA sweep holds the issue again if it still has no commit SHA and no exemption."}
	}
	s.auditFromRequest(r, shaWaiverAuditAction, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "waived", strconv.FormatBool(body.Waived), "added", strings.Join(out.Added, ","), "removed", strings.Join(out.Removed, ",")), "")
	out.MinStatusSeq = s.refreshItemSnapshotNow(ctx, repoFull, number)
	jsonResponse(w, out)
}

func removedClause(removed []string) string {
	if len(removed) == 0 {
		return ""
	}
	return ", `" + strings.Join(removed, "`, `") + "` removed"
}

// refreshItemSnapshotNow re-reads one item and patches the status snapshot so the
// row reads its new labels at once. It is not specific to the SHA waiver: a
// row action that only changes labels can use it instead of copying the
// status-sequence bump (api_repo_hold.go applyRepoHoldToStatus moves hold
// items between lists and does something different). Best effort: the labels are already
// saved on GitHub, so a failed read only means the next poll shows them.
func (s *Server) refreshItemSnapshotNow(ctx context.Context, repo string, number int) uint64 {
	watched := repo
	if cfg := s.deps.Config; cfg != nil {
		for _, w := range cfg.Project.Repos {
			if strings.EqualFold(config.QualifyRepo(cfg.Project.Org, w), repo) {
				watched = w
				break
			}
		}
	}
	snap, err := s.deps.GHClient.FetchWebhookItemSnapshot(ctx, watched, number, false)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("SHA waiver: could not refresh the row", "repo", repo, "issue", number, "error", err)
		}
		return 0
	}
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{snap})
	s.statusMu.Lock()
	s.statusMutationEpoch++
	s.statusSeq++
	seq := s.statusSeq
	if s.status != nil {
		next := *s.status
		next.StatusSeq = seq
		s.status = &next
	}
	s.statusMu.Unlock()
	s.invalidateStageTrackerForWebhook()
	return seq
}
