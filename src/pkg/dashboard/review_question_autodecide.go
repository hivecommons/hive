package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Reviewer-question auto-decide (hivecommons/hive#16578 follow-up).
//
// A review perspective on the hive's own PR sometimes ends with a QUESTION
// rather than a finding — "should this merge ahead of the workflow edit?",
// "confirm the relay may lift the hold" — and the tracker parked the row at
// 085 "Needs human decision" on the wording alone (docs#618 sat with CI 4/4
// and Review 5/5 clean; hive#16456 likewise). The owner's verdict on those:
// "this is auto-merge material, not needs-human".
//
// This sweep classifies the ask (review.ClassifyDecisionQuestion): a
// POLICY SIGN-OFF stays the person's; a SCOPE / FOLLOW-UP QUESTION on a
// hive-authored PR with CI green and no unresolved finding runs the
// existing "Let hive decide" pipeline (runDelegatedDecision) with the
// hive as requester. An approve verdict admits the PR to the merge queue
// and files the question as a follow-up issue; a direction verdict routes
// the fixer; an owner-only / recommend-only verdict keeps the park, with
// the recommendation on the row. At most one auto-delegate runs per head
// (a new push asks again). Off switch: governor.review.auto_decide_questions.

const (
	// reviewQuestionAutodecideInterval paces the sweep between published
	// status snapshots.
	reviewQuestionAutodecideInterval = 2 * time.Minute
	// reviewQuestionAutodecideMaxPerSweep bounds the paid decisions one
	// sweep may start.
	reviewQuestionAutodecideMaxPerSweep = 3
	// reviewQuestionAutodecideCountWindow is the /api/status counter's window
	// (review_questions_autodecided_1h).
	reviewQuestionAutodecideCountWindow = time.Hour
	// reviewQuestionAutodecideRetention bounds the marker ledger.
	reviewQuestionAutodecideRetention = 30 * 24 * time.Hour
	// reviewQuestionFollowUpTitleMax caps the follow-up issue title.
	reviewQuestionFollowUpTitleMax = 120
	// reviewQuestionFollowUpLabel marks the follow-up issues this sweep files.
	reviewQuestionFollowUpLabel = "follow-up"
)

// reviewQuestionAutodecidePath is the once-per-head marker ledger.
var reviewQuestionAutodecidePath = "/data/review-question-autodecide.json"

// reviewQuestionAutodecideMarker records one auto-delegate on a head.
type reviewQuestionAutodecideMarker struct {
	Head    string    `json:"head"`
	At      time.Time `json:"at"`
	Class   string    `json:"class"`
	Reason  string    `json:"reason"`
	Verdict string    `json:"verdict,omitempty"`
	Rule    string    `json:"rule,omitempty"`
	// FollowUp is the follow-up issue filed on an approve verdict (0: none).
	FollowUp int `json:"follow_up,omitempty"`
}

// reviewQuestionAutodecide is the sweep's pacing state and marker ledger.
type reviewQuestionAutodecide struct {
	mu      sync.Mutex
	running bool
	lastAt  time.Time
	path    string
	loaded  bool
	items   map[string]reviewQuestionAutodecideMarker
}

func (st *reviewQuestionAutodecide) filePath() string {
	if st.path != "" {
		return st.path
	}
	return reviewQuestionAutodecidePath
}

// load reads the ledger once; an unreadable file starts empty.
func (st *reviewQuestionAutodecide) load() {
	if st.loaded {
		return
	}
	st.loaded = true
	if st.items == nil {
		st.items = map[string]reviewQuestionAutodecideMarker{}
	}
	data, err := os.ReadFile(st.filePath())
	if err != nil {
		return
	}
	var items map[string]reviewQuestionAutodecideMarker
	if json.Unmarshal(data, &items) == nil && items != nil {
		st.items = items
	}
}

func (st *reviewQuestionAutodecide) persist() error {
	data, err := json.MarshalIndent(st.items, "", "  ")
	if err != nil {
		return err
	}
	path := st.filePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// seen reports whether repo#number already auto-delegated on head.
func (st *reviewQuestionAutodecide) seen(repo string, number int, head string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.load()
	m, ok := st.items[operatorActionKey(repo, number)]
	return ok && strings.EqualFold(m.Head, head)
}

// mark records an auto-delegate on head, dropping markers past retention.
func (st *reviewQuestionAutodecide) mark(repo string, number int, m reviewQuestionAutodecideMarker) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.load()
	for k, old := range st.items {
		if m.At.Sub(old.At) > reviewQuestionAutodecideRetention {
			delete(st.items, k)
		}
	}
	st.items[operatorActionKey(repo, number)] = m
	return st.persist()
}

// update rewrites the marker for repo#number in place (verdict, follow-up).
func (st *reviewQuestionAutodecide) update(repo string, number int, fn func(*reviewQuestionAutodecideMarker)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.load()
	key := operatorActionKey(repo, number)
	m, ok := st.items[key]
	if !ok {
		return nil
	}
	fn(&m)
	st.items[key] = m
	return st.persist()
}

// countSince is the number of auto-delegates recorded within window of now.
func (st *reviewQuestionAutodecide) countSince(now time.Time, window time.Duration) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.load()
	n := 0
	for _, m := range st.items {
		if !m.At.IsZero() && now.Sub(m.At) <= window && !m.At.After(now) {
			n++
		}
	}
	return n
}

// reviewQuestionsAutodecided1h is the /api/status counter.
func (s *Server) reviewQuestionsAutodecided1h(now time.Time) int {
	if s == nil {
		return 0
	}
	return s.reviewQuestionAuto.countSince(now, reviewQuestionAutodecideCountWindow)
}

// reviewQuestionAutodecideEnabled is governor.review.auto_decide_questions
// (default on); unreadable config counts as on.
func (s *Server) reviewQuestionAutodecideEnabled() bool {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return config.GovernorReviewConfig{}.AutoDecideQuestionsEnabled()
	}
	return s.deps.Config.Governor.Review.AutoDecideQuestionsEnabled()
}

// reviewQuestionCandidate is one 085 row a reviewer's question parked.
type reviewQuestionCandidate struct {
	Repo     string
	Number   int
	Head     string
	Question string
	Reason   string
	Caption  string
	// HiveAuthored is the row's own authorship read (hive-authored or
	// adopted in place by the hive).
	HiveAuthored bool
}

// reviewQuestionCandidates picks the tracker rows the sweep may consider: a
// PR parked at 085 by a reviewer's question, on a known head, not on hold,
// CI neither red nor still running. Classification (fork, files, findings)
// happens per candidate afterwards.
func reviewQuestionCandidates(items []StageTrackerItem, qualify func(string) string) []reviewQuestionCandidate {
	var out []reviewQuestionCandidate
	for _, it := range items {
		if it.Kind != "pr" || it.Stage != trackerNumberNeedsDecision || it.Head == "" {
			continue
		}
		dn := it.DecisionNeeded
		if dn == nil || dn.AskedBy != decisionAskedByReviewer || strings.TrimSpace(dn.Question) == "" {
			continue
		}
		if it.Hold != nil || it.OnHold || it.ciRed || it.ciChecking {
			continue
		}
		repo := it.Repo
		if qualify != nil {
			repo = qualify(repo)
		}
		if !strings.Contains(repo, "/") {
			continue
		}
		out = append(out, reviewQuestionCandidate{
			Repo: repo, Number: it.Number, Head: it.Head, Question: strings.TrimSpace(dn.Question),
			Reason: it.Reason, Caption: it.Caption,
			HiveAuthored: it.AuthorKind == authorKindHive || it.AdoptedBy > 0,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})
	return out
}

// reviewQuestionUnresolvedFindings reports whether the head's aggregate
// still carries something a person or the fixer must answer: a backed
// decision finding, an in-scope finding, or a perspective that asked for
// changes. A bare question with none of these is what this sweep decides.
func reviewQuestionUnresolvedFindings(agg review.Aggregate) bool {
	if strings.TrimSpace(agg.DecisionFinding) != "" {
		return true
	}
	for _, f := range agg.Findings {
		if review.InScopeFinding(f.Finding) {
			return true
		}
	}
	for _, v := range agg.Perspectives {
		if v == review.VerdictChangesRequested || v == review.VerdictReject {
			return true
		}
	}
	return false
}

// maybeAutoDecideReviewQuestions starts a sweep in the background when one
// is due and none is running; it reports whether it started one.
// UpdateStatusIfFresh calls it on every published snapshot.
func (s *Server) maybeAutoDecideReviewQuestions(status *StatusPayload, now time.Time) bool {
	if s == nil || s.deps == nil || status == nil || !s.reviewQuestionAutodecideEnabled() {
		return false
	}
	if s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		return false
	}
	st := &s.reviewQuestionAuto
	st.mu.Lock()
	if st.running || (!st.lastAt.IsZero() && now.Sub(st.lastAt) < reviewQuestionAutodecideInterval) {
		st.mu.Unlock()
		return false
	}
	st.running = true
	st.lastAt = now
	st.mu.Unlock()
	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	go func() {
		defer func() {
			st.mu.Lock()
			st.running = false
			st.mu.Unlock()
		}()
		s.autoDecideReviewQuestions(ctx, status, now)
	}()
	return true
}

// autoDecideReviewQuestions runs one sweep synchronously.
func (s *Server) autoDecideReviewQuestions(ctx context.Context, status *StatusPayload, now time.Time) int {
	items := s.buildStageTracker(status, s.stageStoreOrShared(), now).Items
	cands := reviewQuestionCandidates(items, stageTrackerRepoQualifier(status))
	if len(cands) == 0 {
		return 0
	}
	var lookup func(string, int, string, bool) (review.Aggregate, bool)
	if art, err := review.LoadArtifact(""); err == nil {
		lookup = stageTrackerDecisionVerdictLookup(&art)
	}
	started := 0
	for _, c := range cands {
		if started >= reviewQuestionAutodecideMaxPerSweep || ctx.Err() != nil {
			break
		}
		if !s.delegateDecisionEnabled(c.Repo) || s.reviewQuestionAuto.seen(c.Repo, c.Number, c.Head) {
			continue
		}
		if s.autoDecideReviewQuestion(ctx, c, lookup, now) {
			started++
		}
	}
	return started
}

// autoDecideReviewQuestion classifies one candidate and, for a scope
// question, runs the delegated decision. It reports whether one ran.
func (s *Server) autoDecideReviewQuestion(ctx context.Context, c reviewQuestionCandidate, lookup func(string, int, string, bool) (review.Aggregate, bool), now time.Time) bool {
	in := review.DecisionQuestionInput{Question: c.Question, HiveAuthored: c.HiveAuthored, CIGreen: true}
	if lookup != nil {
		if agg, ok := lookup(c.Repo, c.Number, c.Head, true); ok {
			in.UnresolvedFindings = reviewQuestionUnresolvedFindings(agg)
			if agg.HiveAuthoredKnown {
				in.HiveAuthored = in.HiveAuthored || agg.HiveAuthored
			}
		}
	}
	fromFork, files, err := s.reviewQuestionPRFacts(ctx, c.Repo, c.Number)
	if err != nil {
		s.reviewQuestionLogger().Debug("reviewer question auto-decide skipped: PR read failed", "repo", c.Repo, "pr", c.Number, "error", err)
		return false
	}
	in.FromFork, in.Files = fromFork, files
	class, reason := review.ClassifyDecisionQuestion(in)
	if class != review.DecisionQuestionScope {
		s.reviewQuestionLogger().Debug("reviewer question kept for a person", "repo", c.Repo, "pr", c.Number, "head", c.Head, "class", string(class), "reason", reason)
		return false
	}
	hiveID := s.delegateHiveID()
	marker := reviewQuestionAutodecideMarker{Head: c.Head, At: now, Class: string(class), Reason: reason}
	if err := s.reviewQuestionAuto.mark(c.Repo, c.Number, marker); err != nil {
		s.reviewQuestionLogger().Warn("reviewer question auto-decide ledger write failed", "path", s.reviewQuestionAuto.filePath(), "error", err)
	}
	key := delegatedJobKey(c.Repo, c.Number)
	job := &delegatedDecisionJob{
		ID: key + "@" + strconv.FormatInt(now.UnixNano(), 36), Repo: c.Repo, Number: c.Number, Type: "pr",
		Status: delegatedJobStatusRunning, Requester: hiveID, StartedAt: now, Marker: delegatedecision.Marker(hiveID),
	}
	if _, ok := s.delegatedJobs.start(key, job, now); !ok {
		return false
	}
	s.delegatedJobs.plans.forget(c.Repo, c.Number)
	if err := s.operatorActions().Record(c.Repo, c.Number, OperatorAction{
		Action: operatorActionLetHiveDecide, Actor: hiveID, At: now, Outcome: operatorActionOutcomePending,
		HeadSHA: c.Head, Stage: trackerNumberNeedsDecision, Note: "auto: " + reason,
	}); err != nil {
		s.reviewQuestionLogger().Warn("operator action ledger write failed", "path", operatorActionsPath, "error", err)
	}
	s.runDelegatedDecision(ctx, key, c.Repo, c.Number, "pr", hiveID, false, delegatedDecisionRequest{
		Type: "pr", Reason: c.Reason, Caption: c.Caption, Head: c.Head,
	})
	done, _ := s.delegatedJobs.get(key)
	verdict, rule := "", ""
	if done.Decision != nil {
		verdict, rule = string(done.Decision.Verdict), done.Decision.Rule
	}
	followUp := 0
	if done.Decision != nil && done.Decision.Verdict == delegatedecision.VerdictApprove {
		followUp = s.fileReviewQuestionFollowUp(ctx, c)
	}
	_ = s.reviewQuestionAuto.update(c.Repo, c.Number, func(m *reviewQuestionAutodecideMarker) {
		m.Verdict, m.Rule, m.FollowUp = verdict, rule, followUp
	})
	s.reviewQuestionLogger().Info("reviewer question auto-decided",
		"repo", c.Repo, "pr", c.Number, "head", c.Head, "class", string(class), "reason", reason,
		"verdict", verdict, "rule", rule, "status", done.Status, "error", done.Error, "follow_up", followUp)
	s.invalidateStageTrackerCache()
	return true
}

// reviewQuestionPRFacts reads what the classifier needs from GitHub: whether
// the head lives on a fork, and the changed files with their patches.
func (s *Server) reviewQuestionPRFacts(ctx context.Context, repoFull string, number int) (bool, []review.DecisionQuestionFile, error) {
	client := s.deps.GHClient.GoGitHub()
	owner, name, ok := strings.Cut(repoFull, "/")
	if !ok || client == nil {
		return false, nil, errors.New("no GitHub client")
	}
	pr, _, err := client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return false, nil, err
	}
	fromFork := false
	if pr.GetHead().GetRepo() != nil && pr.GetBase().GetRepo() != nil {
		fromFork = !strings.EqualFold(pr.GetHead().GetRepo().GetFullName(), pr.GetBase().GetRepo().GetFullName())
	}
	var files []review.DecisionQuestionFile
	opt := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.PullRequests.ListFiles(ctx, owner, name, number, opt)
		if err != nil {
			return fromFork, nil, err
		}
		for _, f := range page {
			files = append(files, review.DecisionQuestionFile{Path: f.GetFilename(), Patch: f.GetPatch()})
			if prev := f.GetPreviousFilename(); prev != "" && prev != f.GetFilename() {
				files = append(files, review.DecisionQuestionFile{Path: prev})
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return fromFork, files, nil
}

// fileReviewQuestionFollowUp records the reviewer's question as a scanner
// issue linked to the PR once the hive admitted the PR as-is. It returns the
// issue number (0 when the gate refused or the create failed).
func (s *Server) fileReviewQuestionFollowUp(ctx context.Context, c reviewQuestionCandidate) int {
	if s.deps == nil || s.deps.GHClient == nil {
		return 0
	}
	title := reviewQuestionFollowUpTitle(c.Question)
	preview := "follow-up issue for reviewer question on " + c.Repo + "#" + strconv.Itoa(c.Number)
	if !s.deps.GHClient.WritebackAllowed(c.Repo, writeback.Item{HiveAuthored: true}, writeback.KindIssueFile, preview).Allowed {
		return 0
	}
	body := fmt.Sprintf("A review perspective on #%d ended with a question rather than a finding. "+
		"The hive decided the PR merges as-is (governor.review.auto_decide_questions); the question is kept here as follow-up work.\n\n"+
		"> %s\n\nSource: %s#%d (head `%s`).\n", c.Number, strings.ReplaceAll(c.Question, "\n", "\n> "), c.Repo, c.Number, c.Head)
	res, err := s.deps.GHClient.CreateIssue(ctx, ghpkg.CreateIssueRequest{ // writeback:gated
		Repo:   c.Repo,
		Title:  title,
		Body:   body,
		Labels: []string{reviewQuestionFollowUpLabel},
		Origin: config.OriginSourceGovernor,
	})
	if err != nil {
		s.reviewQuestionLogger().Warn("reviewer question follow-up issue not filed", "repo", c.Repo, "pr", c.Number, "error", err)
		return 0
	}
	return res.Number
}

// reviewQuestionFollowUpTitle is "follow-up: <question>" on one line,
// capped at reviewQuestionFollowUpTitleMax runes.
func reviewQuestionFollowUpTitle(question string) string {
	q := strings.Join(strings.Fields(question), " ")
	title := "follow-up: " + q
	if r := []rune(title); len(r) > reviewQuestionFollowUpTitleMax {
		title = strings.TrimSpace(string(r[:reviewQuestionFollowUpTitleMax-1])) + "…"
	}
	return title
}

func (s *Server) reviewQuestionLogger() *slog.Logger { return s.webhookLogger() }
