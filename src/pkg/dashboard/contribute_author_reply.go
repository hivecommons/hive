package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// The issue author's answer ends the no-PR cooldown the question started.
//
// A contributor task that ends without a pull request books the short no-PR
// completion cooldown (completedNoPRCooldownHours, escalating per
// advanceNoPRStreakLocked). Often the task ended BECAUSE the agent asked the
// issue's author something and cannot go on until it is answered. The answer
// used to wake nothing: projectbluefin/gutenprint-printer-app#238 had its
// contributor ask for the exact heading wording at 3:39 PM, the issue's author
// answered at 3:40 PM, and the issue stayed withheld for the full four hours.
//
// The release here ends that cooldown early when all of these hold:
//
//   - the issue's latest completion is a no-PR idle (the idleCompletions
//     ledger has its record, no PR is on the completion) and its cooldown is
//     still running. A with-PR cooldown, a no-work / blocked / needs-decision
//     verdict, a failure or quarantine is never touched;
//   - during that task (from its start, when known, to just after the
//     completion) a comment carrying a hive marker was posted on the issue:
//     the `— hive:` trailer or the 🐝 **Hive Agent** footer, read by
//     ghpkg.HiveMarkerAgent. The newest such comment is the question;
//   - after it, the issue's author (the issue's own login, never a bot, and
//     not in a hive-marked comment) commented.
//
// One question earns one release. The question's comment ID is persisted in
// authorReplyReleases, so a restart, a chatty thread or a second reply cannot
// release the issue again; a later task that asks a NEW question may earn one
// more. A comment by anyone else releases nothing.
//
// Cost: the sweep looks only at issues under such a cooldown whose queue
// updated_at is newer than the completion and newer than the last read it
// made for them, and spends at most one comments read (normally one page, and
// a conditional 304 when nothing changed) per issue per new activity. An
// issue_comment webhook, where a hive has one, already refreshes that
// updated_at; there is no separate webhook path. With the completion cooldown
// switched off there is no cooldown to end and the sweep does nothing.
//
// The next task needs no extra prompt text: the assignment prompt already
// tells the agent to read the issue with its comments (#11443), and the
// answer is one of them.

// authorReplyReleasesFileName is the on-disk ledger, beside the no-PR streaks.
const authorReplyReleasesFileName = "author-reply-releases.json"

// authorReplyReleaseInterval is how often the sweep runs. It reads only the
// hub's own state and the queue snapshot unless an issue qualifies.
const authorReplyReleaseInterval = time.Minute

// authorReplyQuestionSlack is how long after the completion a question may
// carry as its GitHub creation time: the relay posts it before it reports the
// task complete, so this only absorbs clock skew between GitHub and the hub.
const authorReplyQuestionSlack = 2 * time.Minute

// authorReplyRetryAfterError is how long the sweep waits before reading an
// issue's comments again after a failed read.
const authorReplyRetryAfterError = 10 * time.Minute

// authorReplyMaxReadsPerPass caps the comments reads one pass spends.
const authorReplyMaxReadsPerPass = 20

// authorReplyReleaseRecord is the in-memory and on-disk shape of the last
// early release per work item.
type authorReplyReleaseRecord struct {
	QuestionID  int64     `json:"question_id"`
	QuestionAt  time.Time `json:"question_at"`
	ReplyAuthor string    `json:"reply_author,omitempty"`
	ReplyAt     time.Time `json:"reply_at"`
	ReleasedAt  time.Time `json:"released_at"`
}

// expired: a release is kept as long as an idle reason is, so it outlives
// every cooldown the no-PR streak can book.
func (r authorReplyReleaseRecord) expired(now time.Time) bool {
	return r.ReleasedAt.IsZero() || now.Sub(r.ReleasedAt) >= idleCompletionRetention
}

// authorReplyCheck is the sweep's read memo for one work item: the queue
// updated_at its last successful read covered, and when it may retry after a
// failed one.
type authorReplyCheck struct {
	updatedAt time.Time
	retryAt   time.Time
}

// authorReplyCandidate is one work item the sweep may read, captured under
// completedMu.
type authorReplyCandidate struct {
	key                string
	completedAt        time.Time
	taskStart          time.Time
	releasedQuestionID int64
	check              authorReplyCheck
}

// authorReplyTarget is a candidate the queue snapshot says has new activity.
type authorReplyTarget struct {
	authorReplyCandidate
	repo      string
	number    int
	author    string
	updatedAt time.Time
}

func (h *ContributeWSHub) authorReplyReleasesPath() string {
	if h != nil && h.authorReplyReleasesFile != "" {
		return h.authorReplyReleasesFile
	}
	return filepath.Join(filepath.Dir(h.noPRStreaksPath()), authorReplyReleasesFileName)
}

func (h *ContributeWSHub) loadAuthorReplyReleases() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(h.authorReplyReleasesPath())
	if err != nil {
		return
	}
	records := make(map[string]authorReplyReleaseRecord)
	if json.Unmarshal(data, &records) != nil {
		return
	}
	now := time.Now()
	for k, rec := range records {
		if rec.expired(now) || rec.QuestionID == 0 {
			continue
		}
		if h.authorReplyReleases == nil {
			h.authorReplyReleases = make(map[string]authorReplyReleaseRecord)
		}
		h.authorReplyReleases[k] = rec
	}
	h.logger.Info("[contribute-ws] loaded author-reply releases", "count", len(h.authorReplyReleases))
}

func (h *ContributeWSHub) saveAuthorReplyReleases() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	now := time.Now()
	h.completedMu.Lock()
	saved := make(map[string]authorReplyReleaseRecord, len(h.authorReplyReleases))
	for k, rec := range h.authorReplyReleases {
		if rec.expired(now) {
			delete(h.authorReplyReleases, k)
			continue
		}
		saved[k] = rec
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] author-reply releases marshal failed", "error", err)
		return
	}
	path := h.authorReplyReleasesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] author-reply releases directory creation failed", "error", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] author-reply releases write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] author-reply releases rename failed", "error", err)
	}
}

// authorReplyLister returns the comments read the sweep uses, or nil when the
// hub has no GitHub client.
func (h *ContributeWSHub) authorReplyLister() func(context.Context, string, int, time.Time) ([]ghpkg.IssueCommentEntry, error) {
	if h.authorReplyComments != nil {
		return h.authorReplyComments
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return nil
	}
	return h.server.deps.GHClient.IssueCommentsSince
}

// authorReplyReleaseLoop runs the sweep on its own clock, like
// blockedClaimsLoop, so a slow GitHub read never delays heartbeat reaping.
func (h *ContributeWSHub) authorReplyReleaseLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			h.sweepAuthorReplyReleases(ctx, time.Now())
			cancel()
		}
	}
}

// authorReplyPendingLocked lists the work items whose latest completion is a
// no-PR idle still inside its cooldown, and drops read memos for anything
// else. Callers hold completedMu.
func (h *ContributeWSHub) authorReplyPendingLocked(now time.Time) map[string]authorReplyCandidate {
	pending := make(map[string]authorReplyCandidate)
	for key, rec := range h.idleCompletions {
		completedAt, ok := h.completedTasks[key]
		if !ok || h.completedTaskPRURL[key] != "" {
			continue
		}
		if now.Sub(completedAt) > h.cooldownForLocked(key) {
			continue
		}
		// The idle record and the completion are booked together; a record
		// that does not belong to this completion explains nothing about it.
		if d := completedAt.Sub(rec.RecordedAt); d > time.Minute || d < -time.Minute {
			continue
		}
		cand := authorReplyCandidate{
			key:         key,
			completedAt: completedAt,
			taskStart:   rec.TaskStartedAt,
			check:       h.authorReplyChecked[key],
		}
		if rel, ok := h.authorReplyReleases[key]; ok && !rel.expired(now) {
			cand.releasedQuestionID = rel.QuestionID
		}
		pending[key] = cand
	}
	for key := range h.authorReplyChecked {
		if _, ok := pending[key]; !ok {
			delete(h.authorReplyChecked, key)
		}
	}
	return pending
}

// sweepAuthorReplyReleases ends the no-PR completion cooldown of every issue
// whose author answered the question the contributor asked during that task.
// It returns how many cooldowns it ended.
func (h *ContributeWSHub) sweepAuthorReplyReleases(ctx context.Context, now time.Time) int {
	if h == nil || h.server == nil || !h.cooldownEnabled() {
		return 0
	}
	list := h.authorReplyLister()
	if list == nil {
		return 0
	}
	h.completedMu.Lock()
	pending := h.authorReplyPendingLocked(now)
	h.completedMu.Unlock()
	if len(pending) == 0 {
		return 0
	}

	h.server.statusMu.RLock()
	status := h.server.status
	h.server.statusMu.RUnlock()
	if status == nil {
		return 0
	}
	var targets []authorReplyTarget
	for _, repo := range status.Repos {
		if len(repo.ActionableIssues) == 0 {
			continue
		}
		forEachActionableIssue(nil, "", repo.Full, repo.ActionableIssues, func(issue map[string]any, ref worksource.Ref) {
			if !ref.IsGitHubIssue() {
				return
			}
			cand, ok := pending[ref.Key()]
			if !ok {
				return
			}
			// Unknown activity reads nothing: the cooldown simply runs, as
			// it always did.
			updatedAt := issueUpdatedAtFromMap(issue)
			if updatedAt.IsZero() || !updatedAt.After(cand.completedAt) || !updatedAt.After(cand.check.updatedAt) {
				return
			}
			if now.Before(cand.check.retryAt) {
				return
			}
			author, _ := issue["author"].(string)
			if author = strings.TrimSpace(author); author == "" {
				return
			}
			targets = append(targets, authorReplyTarget{
				authorReplyCandidate: cand,
				repo:                 repo.Full,
				number:               ref.Number,
				author:               author,
				updatedAt:            updatedAt,
			})
		})
	}

	released := 0
	for i, t := range targets {
		if i >= authorReplyMaxReadsPerPass {
			break
		}
		comments, err := list(ctx, t.repo, t.number, t.taskStart)
		h.completedMu.Lock()
		if h.authorReplyChecked == nil {
			h.authorReplyChecked = make(map[string]authorReplyCheck)
		}
		if err != nil {
			check := t.check
			check.retryAt = now.Add(authorReplyRetryAfterError)
			h.authorReplyChecked[t.key] = check
		} else {
			h.authorReplyChecked[t.key] = authorReplyCheck{updatedAt: t.updatedAt}
		}
		h.completedMu.Unlock()
		if err != nil {
			h.logger.Warn("[contribute-ws] author-reply release: comments read failed",
				"repo", t.repo, "number", t.number, "error", err)
			continue
		}
		question, reply, ok := authorReplyAfterQuestion(comments, t.author, t.taskStart,
			t.completedAt.Add(authorReplyQuestionSlack), t.releasedQuestionID)
		if !ok {
			continue
		}
		if !h.releaseCooldownForAuthorReply(t.key, t.completedAt, question, reply, now) {
			continue
		}
		released++
		h.logger.Info("[contribute-ws] no-PR cooldown released: the issue's author answered the contributor's question",
			"repo", t.repo,
			"number", t.number,
			"question_comment_id", question.ID,
			"reply_author", reply.Author,
			"reply_at", reply.CreatedAt.UTC().Format(time.RFC3339))
	}
	return released
}

// releaseCooldownForAuthorReply ends key's completion cooldown when it is
// still the no-PR completion the sweep read for, and books the question as
// released. It reports whether it ended the cooldown.
func (h *ContributeWSHub) releaseCooldownForAuthorReply(key string, completedAt time.Time, question, reply ghpkg.IssueCommentEntry, now time.Time) bool {
	h.completedMu.Lock()
	t, ok := h.completedTasks[key]
	if !ok || !t.Equal(completedAt) || h.completedTaskPRURL[key] != "" {
		h.completedMu.Unlock()
		return false
	}
	if rel, ok := h.authorReplyReleases[key]; ok && !rel.expired(now) && question.ID <= rel.QuestionID {
		h.completedMu.Unlock()
		return false
	}
	delete(h.completedTasks, key)
	delete(h.completedTaskCooldown, key)
	delete(h.completedTaskPRURL, key)
	if h.authorReplyReleases == nil {
		h.authorReplyReleases = make(map[string]authorReplyReleaseRecord)
	}
	h.authorReplyReleases[key] = authorReplyReleaseRecord{
		QuestionID:  question.ID,
		QuestionAt:  question.CreatedAt.UTC(),
		ReplyAuthor: reply.Author,
		ReplyAt:     reply.CreatedAt.UTC(),
		ReleasedAt:  now.UTC(),
	}
	delete(h.authorReplyChecked, key)
	h.completedMu.Unlock()
	h.saveCompletedTasks()
	h.saveAuthorReplyReleases()
	return true
}

// authorReplyAfterQuestion finds the question and its answer in an issue's
// comments. The question is the newest hive-marked comment created inside
// [windowStart, windowEnd] (no lower bound when windowStart is zero) whose ID
// is newer than releasedQuestionID. The answer is the first comment after it
// by issueAuthor that is not a bot's and carries no hive marker.
func authorReplyAfterQuestion(comments []ghpkg.IssueCommentEntry, issueAuthor string, windowStart, windowEnd time.Time, releasedQuestionID int64) (question, reply ghpkg.IssueCommentEntry, ok bool) {
	issueAuthor = strings.TrimSpace(issueAuthor)
	if issueAuthor == "" {
		return question, reply, false
	}
	sorted := append([]ghpkg.IssueCommentEntry(nil), comments...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID < sorted[j].ID
	})
	qi := -1
	for i, cm := range sorted {
		if !windowStart.IsZero() && cm.CreatedAt.Before(windowStart) {
			continue
		}
		if cm.CreatedAt.After(windowEnd) {
			continue
		}
		if _, marked := ghpkg.HiveMarkerAgent(cm.Body); marked {
			qi = i
		}
	}
	if qi < 0 || sorted[qi].ID <= releasedQuestionID {
		return question, reply, false
	}
	question = sorted[qi]
	for _, cm := range sorted[qi+1:] {
		if cm.AuthorIsBot || !strings.EqualFold(strings.TrimSpace(cm.Author), issueAuthor) {
			continue
		}
		if _, marked := ghpkg.HiveMarkerAgent(cm.Body); marked {
			continue
		}
		return question, cm, true
	}
	return question, reply, false
}
