package dashboard

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Superseded PRs in the review queue (hivecommons/hive#11430). With
// supersession_sweep.close_contributor_prs on, the sweep posts a grace-window
// notice on a superseded human-authored PR and audits it as
// supersession-sweep-commented-grace on every pass until it closes the PR or
// a guard keeps it open. The queue derives "in the grace window" from those
// audit entries alone; no new state is stored.
const (
	supersessionAuditPrefix          = "supersession-"
	supersessionGraceAuditAction     = "supersession-sweep-commented-grace"
	supersessionConfirmCloseAction   = "supersession-review-confirm-close"
	supersessionKeepOpenAction       = "supersession-review-keep-open"
	supersessionAuditLookback        = 14 * 24 * time.Hour
	supersessionDefaultGitHubBaseURL = "https://github.com/"
)

// reviewQueueSupersessionAuditPath is where the queue scans the on-disk audit
// log; "" means the production path. Tests point it at a temp dir.
var reviewQueueSupersessionAuditPath = ""

func (s *Server) supersessionAuditEntries(now time.Time) []AuditEntry {
	if s == nil || s.audit == nil {
		return nil
	}
	since := now.Add(-supersessionAuditLookback)
	if s.audit.HasOnDiskLog(reviewQueueSupersessionAuditPath) {
		return s.audit.ActionsWithPrefixSince(since, supersessionAuditPrefix, reviewQueueSupersessionAuditPath)
	}
	return s.audit.RecentWithPrefixSince(since, supersessionAuditPrefix)
}

func (s *Server) supersessionGracePeriod() time.Duration {
	if s != nil && s.deps != nil && s.deps.Config != nil && s.deps.Config.SupersessionSweep.GracePeriod > 0 {
		return s.deps.Config.SupersessionSweep.GracePeriod
	}
	return ghpkg.DefaultSupersessionGracePeriod
}

func supersessionQualifyRepo(repo, org string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.Contains(repo, "/") || strings.TrimSpace(org) == "" {
		return repo
	}
	return strings.TrimSpace(org) + "/" + repo
}

// parseSupersessionAuditDetail reads the "k=v, k=v" detail the sweep's audit
// hook writes.
func parseSupersessionAuditDetail(detail string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(detail, ", ") {
		k, v, ok := strings.Cut(part, "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// supersededGraceStates replays supersession audit entries (oldest first)
// and returns the PRs whose latest entry is the grace notice, keyed by
// ghpkg.ReviewQueueKey. The window starts at the first grace entry of the
// current run of them; any other supersession entry for the PR (a keep, a
// close, or an operator action from the dashboard) ends it.
func supersededGraceStates(entries []AuditEntry, org string, grace time.Duration) map[string]ghpkg.ReviewQueueSuperseded {
	out := map[string]ghpkg.ReviewQueueSuperseded{}
	for _, e := range entries {
		repo := supersessionQualifyRepo(e.Repo, org)
		if repo == "" || e.Target <= 0 {
			continue
		}
		key := ghpkg.ReviewQueueKey(repo, e.Target)
		if e.Action != supersessionGraceAuditAction {
			delete(out, key)
			continue
		}
		if _, ok := out[key]; ok {
			continue
		}
		at, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			continue
		}
		fields := parseSupersessionAuditDetail(e.Detail)
		closerPR, _ := strconv.Atoi(fields["closer_pr"])
		if closerPR <= 0 {
			continue
		}
		info := ghpkg.ReviewQueueSuperseded{
			Reason:         ghpkg.ReviewQueueSupersededReason,
			Issue:          fields["issue"],
			CloserRepo:     repo,
			CloserPR:       closerPR,
			GraceStartedAt: at.UTC(),
			GraceEndsAt:    at.Add(grace).UTC(),
		}
		if issueRepo, _, ok := strings.Cut(fields["issue"], "#"); ok && issueRepo != "" {
			info.CloserRepo = supersessionQualifyRepo(issueRepo, org)
		}
		if strings.Contains(info.CloserRepo, "/") {
			info.CloserURL = fmt.Sprintf("%s%s/pull/%d", supersessionDefaultGitHubBaseURL, info.CloserRepo, closerPR)
		}
		out[key] = info
	}
	return out
}

// annotateSupersededReviewQueue marks queue entries that sit in the
// supersession grace window and adds a "superseded" reason to each.
func (s *Server) annotateSupersededReviewQueue(queue []ghpkg.ReviewQueueEntry, org string, now time.Time) {
	if len(queue) == 0 {
		return
	}
	states := supersededGraceStates(s.supersessionAuditEntries(now), org, s.supersessionGracePeriod())
	if len(states) == 0 {
		return
	}
	for i := range queue {
		info, ok := states[ghpkg.ReviewQueueKey(queue[i].Repo, queue[i].Number)]
		if !ok {
			continue
		}
		queue[i].Superseded = &info
		queue[i].Reasons = append(queue[i].Reasons, supersededReason(info, now))
	}
}

func supersededReason(info ghpkg.ReviewQueueSuperseded, now time.Time) string {
	closer := fmt.Sprintf("%s#%d", info.CloserRepo, info.CloserPR)
	if !now.Before(info.GraceEndsAt) {
		return fmt.Sprintf("%s: by %s; grace window ended %s, the sweep closes it on its next pass", info.Reason, closer, info.GraceEndsAt.Format(time.RFC3339))
	}
	return fmt.Sprintf("%s: by %s; the sweep closes it at %s unless someone replies", info.Reason, closer, info.GraceEndsAt.Format(time.RFC3339))
}

// supersededActionTarget validates a superseded-PR action request and
// returns the PR's owner/name and number. It writes the error response and
// returns ok=false on any failure.
func (s *Server) supersededActionTarget(w http.ResponseWriter, r *http.Request) (repo string, number int, ok bool) {
	if !requireOwnerRole(w, r) {
		return "", 0, false
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return "", 0, false
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		jsonError(w, "invalid pull request number", http.StatusBadRequest)
		return "", 0, false
	}
	owner := strings.TrimSpace(r.PathValue("owner"))
	repoName := strings.TrimSpace(r.PathValue("repo"))
	if owner == "" || repoName == "" || strings.Contains(owner, "/") || strings.Contains(repoName, "/") {
		jsonError(w, "invalid repository", http.StatusBadRequest)
		return "", 0, false
	}
	repo = owner + "/" + repoName
	if !s.prQueueRepoAllowed(repo) {
		jsonError(w, "repository is not managed by this hive", http.StatusForbidden)
		return "", 0, false
	}
	return repo, number, true
}

// handleReviewQueueSupersededClose serves
// POST /api/review/queue/{owner}/{repo}/{number}/superseded/close: an
// operator confirms the close of a PR in the supersession grace window
// before it ends. It posts the close comment, applies hive/superseded and
// closes the PR through the sweep's own close path.
func (s *Server) handleReviewQueueSupersededClose(w http.ResponseWriter, r *http.Request) {
	repo, number, ok := s.supersededActionTarget(w, r)
	if !ok {
		return
	}
	user := requestUser(r)
	commentUser := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	closerRef := "a different merged PR"
	org := ""
	if s.deps.Config != nil {
		org = s.deps.Config.Project.Org
	}
	if info, found := supersededGraceStates(s.supersessionAuditEntries(time.Now()), org, s.supersessionGracePeriod())[ghpkg.ReviewQueueKey(repo, number)]; found {
		closerRef = fmt.Sprintf("%s#%d", info.CloserRepo, info.CloserPR)
	}
	gh := s.deps.GHClient
	posted, err := gh.IssueCommentsContain(r.Context(), repo, number, ghpkg.SupersessionAutoCloseMarker)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !posted {
		if err := gh.CreateIssueComment(r.Context(), repo, number, ghpkg.RenderSupersessionOperatorCloseComment(closerRef, commentUser)); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	if err := gh.CloseSupersededPR(r.Context(), repo, number); err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.audit.LogRecord(user, supersessionConfirmCloseAction, auditDetail("repo", repo, "pr", strconv.Itoa(number), "closer", closerRef), "", repo, number)
	jsonResponse(w, map[string]any{
		"status": "closed",
		"repo":   repo,
		"number": number,
		"label":  ghpkg.SupersededLabel,
	})
}

// handleReviewQueueSupersededKeepOpen serves
// POST /api/review/queue/{owner}/{repo}/{number}/superseded/keep-open: an
// operator keeps a superseded PR open. It applies hive/keep-open, which the
// sweep honors by never auto-closing the PR.
func (s *Server) handleReviewQueueSupersededKeepOpen(w http.ResponseWriter, r *http.Request) {
	repo, number, ok := s.supersededActionTarget(w, r)
	if !ok {
		return
	}
	if err := s.deps.GHClient.KeepSupersededPROpen(r.Context(), repo, number); err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.audit.LogRecord(requestUser(r), supersessionKeepOpenAction, auditDetail("repo", repo, "pr", strconv.Itoa(number)), "", repo, number)
	jsonResponse(w, map[string]any{
		"status": "kept_open",
		"repo":   repo,
		"number": number,
		"label":  ghpkg.SupersessionKeepOpenLabel,
	})
}
