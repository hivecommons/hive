package dashboard

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Superseded PR recommendations are independent of automatic closure. Legacy
// grace entries still start a countdown; detection entries only offer a decision.
const (
	supersessionAuditPrefix          = "supersession-"
	supersessionDetectedAuditAction  = ghpkg.SupersessionSweepAuditPrefix + ghpkg.SupersessionActionDetectedContributor
	supersessionGraceAuditAction     = ghpkg.SupersessionSweepAuditPrefix + ghpkg.SupersessionActionCommentedGrace
	supersessionCommentedAuditAction = ghpkg.SupersessionSweepAuditPrefix + ghpkg.SupersessionActionCommentedContributor
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

// supersededGraceStates replays audit entries oldest first. Detection records
// survive a plain contributor notice, while legacy grace-only records retain
// their original reset semantics. Close and keep-open actions end both states.
func supersededGraceStates(entries []AuditEntry, org string, grace time.Duration) map[string]ghpkg.ReviewQueueSuperseded {
	out := map[string]ghpkg.ReviewQueueSuperseded{}
	detected := map[string]bool{}
	for _, e := range entries {
		repo := supersessionQualifyRepo(e.Repo, org)
		if repo == "" || e.Target <= 0 {
			continue
		}
		key := ghpkg.ReviewQueueKey(repo, e.Target)
		switch e.Action {
		case supersessionDetectedAuditAction, supersessionGraceAuditAction:
		case supersessionCommentedAuditAction:
			if detected[key] {
				info := out[key]
				info.GraceStartedAt, info.GraceEndsAt = time.Time{}, time.Time{}
				out[key] = info
				continue
			}
			fallthrough
		default:
			delete(out, key)
			delete(detected, key)
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
			Reason:     ghpkg.ReviewQueueSupersededReason,
			Issue:      fields["issue"],
			CloserRepo: repo,
			CloserPR:   closerPR,
		}
		// Records written before the sweep recorded its outcome carry no
		// reason and keep the close recommendation (hivecommons/hive#14176).
		if fields["reason"] == ghpkg.SupersessionOutcomeExtraFiles {
			info.Reason = ghpkg.ReviewQueueSupersededExtraFilesReason
			for _, f := range strings.Split(fields["extra_files"], ";") {
				if f = strings.TrimSpace(f); f != "" {
					info.ExtraFiles = append(info.ExtraFiles, f)
				}
			}
		}
		if issueRepo, _, ok := strings.Cut(fields["issue"], "#"); ok && issueRepo != "" {
			info.CloserRepo = supersessionQualifyRepo(issueRepo, org)
		}
		if strings.Contains(info.CloserRepo, "/") {
			info.CloserURL = fmt.Sprintf("%s%s/pull/%d", supersessionDefaultGitHubBaseURL, info.CloserRepo, closerPR)
		}
		if previous, ok := out[key]; ok && previous.CloserRepo == info.CloserRepo && previous.CloserPR == info.CloserPR && previous.Issue == info.Issue {
			info.GraceStartedAt, info.GraceEndsAt = previous.GraceStartedAt, previous.GraceEndsAt
		}
		if e.Action == supersessionDetectedAuditAction {
			detected[key] = true
		} else if info.GraceStartedAt.IsZero() {
			info.GraceStartedAt, info.GraceEndsAt = at.UTC(), at.Add(grace).UTC()
		}
		out[key] = info
	}
	return out
}

// supersededReviewStates also accounts for an operator disabling auto-close
// after a grace notice was recorded, so the UI never advertises a disabled close.
func (s *Server) supersededReviewStates(org string, now time.Time) map[string]ghpkg.ReviewQueueSuperseded {
	if s == nil {
		return map[string]ghpkg.ReviewQueueSuperseded{}
	}
	states := supersededGraceStates(s.supersessionAuditEntries(now), org, s.supersessionGracePeriod())
	autoClose := s.deps != nil && s.deps.Config != nil && s.deps.Config.SupersessionSweep.CloseContributorPRs
	if !autoClose {
		for key, info := range states {
			info.GraceStartedAt, info.GraceEndsAt = time.Time{}, time.Time{}
			states[key] = info
		}
	}
	return states
}

// SupersessionStatusOption gives the inbox the same recommendations as the queue.
func (s *Server) SupersessionStatusOption() StatusOption {
	org := ""
	if s != nil && s.deps != nil && s.deps.Config != nil {
		org = s.deps.Config.Project.Org
	}
	states := s.supersededReviewStates(org, time.Now())
	return func(o *statusOptions) {
		o.supersededPR = func(repo string, number int) (ghpkg.ReviewQueueSuperseded, bool) {
			info, ok := states[ghpkg.ReviewQueueKey(supersessionQualifyRepo(repo, org), number)]
			return info, ok
		}
	}
}

// annotateSupersededReviewQueue adds supersession recommendations and any
// active grace window to the queue.
func (s *Server) annotateSupersededReviewQueue(queue []ghpkg.ReviewQueueEntry, org string, now time.Time) {
	if len(queue) == 0 {
		return
	}
	states := s.supersededReviewStates(org, now)
	if len(states) == 0 {
		return
	}
	for i := range queue {
		info, ok := states[ghpkg.ReviewQueueKey(queue[i].Repo, queue[i].Number)]
		if !ok || labelSet(queue[i].Labels)[ghpkg.SupersessionKeepOpenLabel] {
			continue
		}
		queue[i].Superseded = &info
		queue[i].Reasons = append(queue[i].Reasons, supersededReason(info, now))
	}
}

func supersededReason(info ghpkg.ReviewQueueSuperseded, now time.Time) string {
	closer := fmt.Sprintf("%s#%d", info.CloserRepo, info.CloserPR)
	if info.Reason == ghpkg.ReviewQueueSupersededExtraFilesReason {
		return fmt.Sprintf("%s: by %s; this PR adds files it did not touch (%s); review the extra change, keep open recommended", info.Reason, closer, supersededExtraFilesSummary(info.ExtraFiles))
	}
	if info.GraceEndsAt.IsZero() {
		return fmt.Sprintf("%s: by %s; close as superseded or keep open", info.Reason, closer)
	}
	if !now.Before(info.GraceEndsAt) {
		return fmt.Sprintf("%s: by %s; grace window ended %s, the sweep closes it on its next pass", info.Reason, closer, info.GraceEndsAt.Format(time.RFC3339))
	}
	return fmt.Sprintf("%s: by %s; the sweep closes it at %s unless someone replies", info.Reason, closer, info.GraceEndsAt.Format(time.RFC3339))
}

// supersededExtraFilesShown caps the extra files named in one line of text.
const supersededExtraFilesShown = 3

// supersededExtraFilesSummary names the files a superseded PR adds beyond the
// merged PR, e.g. "CONTRIBUTING.md" or "a.go, b.go, c.go and 2 more".
func supersededExtraFilesSummary(files []string) string {
	if len(files) == 0 {
		return "files not recorded"
	}
	if len(files) <= supersededExtraFilesShown {
		return strings.Join(files, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(files[:supersededExtraFilesShown], ", "), len(files)-supersededExtraFilesShown)
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
// operator confirms the close of a superseded PR. It posts the close comment,
// applies hive/superseded and closes the PR through the sweep's own close path.
func (s *Server) handleReviewQueueSupersededClose(w http.ResponseWriter, r *http.Request) {
	repo, number, ok := s.supersededActionTarget(w, r)
	if !ok {
		return
	}
	user := requestUser(r)
	commentUser := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	org := ""
	if s.deps.Config != nil {
		org = s.deps.Config.Project.Org
	}
	info, found := supersededGraceStates(s.supersessionAuditEntries(time.Now()), org, s.supersessionGracePeriod())[ghpkg.ReviewQueueKey(repo, number)]
	if !found {
		jsonError(w, "no supersession state recorded for this pull request", http.StatusNotFound)
		return
	}
	closerRef := fmt.Sprintf("%s#%d", info.CloserRepo, info.CloserPR)
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
