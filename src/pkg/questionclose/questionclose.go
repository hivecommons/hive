// Package questionclose closes question issues the hive has answered, unless
// the person who asked objects (hivecommons/hive#9584).
//
// The flow:
//
//  1. The scheduler hands every pass's classified issue list to Offer. Issues
//     carrying a question label (and no bug/enhancement/hold/human label) are
//     candidates. Offer never does I/O, so kick building never waits on it.
//  2. The scanner lane is told (KickSection) to answer a question with one
//     comment ending in the AnswerFooter: a hidden marker plus the opt-out
//     line "react 👎 to this comment and the issue will stay open".
//  3. Tick, on its own loop, reads each candidate's thread. When the LAST
//     comment is a marked answer posted by the hive's own identity (App bot
//     login or project.ai_author, never merely anyone pasting the marker), it
//     schedules a close at answer time + the configured window. The schedule
//     is persisted, so a restart does not lose or restart the clock.
//  4. At the deadline Tick re-reads the thread. A 👎 from the issue author on
//     the answer keeps the issue open and adds the human label. Otherwise the
//     issue is closed with state_reason "completed" and no further comment,
//     so the answer stays the hive's last word on it.
//
// A schedule is cancelled, not acted on, as soon as a pass sees any comment
// after the answer, an external close, the question label removed, or a
// bug/enhancement/hold/human label added. Everything is off unless
// governor.question_autoclose.enabled (or HIVE_QUESTION_AUTOCLOSE) says so.
package questionclose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	// AnswerMarker is the hidden token that identifies a hive answer comment.
	AnswerMarker = "<!-- hive-question-answer -->"
	// DefaultTickInterval is how often Run evaluates candidates and deadlines.
	DefaultTickInterval = 5 * time.Minute
	// recheckInterval throttles re-reading a candidate whose issue carries no
	// UpdatedAt (an older enumerator), so it is not read on every tick.
	recheckInterval = 30 * time.Minute
	// settledRetention is how long a finished schedule is remembered, so the
	// same answer comment is never scheduled twice.
	settledRetention = 30 * 24 * time.Hour
	// githubCallTimeout bounds each GitHub call a tick makes.
	githubCallTimeout = 30 * time.Second
	// reactionThumbsDown is GitHub's reaction content for 👎.
	reactionThumbsDown = "-1"
	// issueStateOpen is GitHub's state for an open issue.
	issueStateOpen = "open"
	// persistVersion is the on-disk schema version.
	persistVersion = 1
)

// Outcomes recorded when a schedule ends.
const (
	OutcomeClosed          = "closed"
	OutcomeKeptOpen        = "kept_open"
	OutcomeFollowUp        = "follow_up_comment"
	OutcomeClosedElsewhere = "closed_externally"
	OutcomeRelabelled      = "relabelled"
	OutcomeAnswerDeleted   = "answer_deleted"
	OutcomeOutOfScope      = "out_of_scope"
)

// excludedLabels are never auto-closed, whatever else they carry: bugs and
// enhancements are work, not questions. Hold labels are checked separately
// through the tracker so operator-configured hold labels count too.
var excludedLabels = map[string]struct{}{
	"bug":              {},
	"kind/bug":         {},
	"type/bug":         {},
	"type:bug":         {},
	"enhancement":      {},
	"kind/enhancement": {},
	"type/enhancement": {},
	"feature":          {},
	"kind/feature":     {},
	"type/feature":     {},
	"hold":             {},
}

// Tracker is the GitHub surface the manager needs. *github.Client satisfies it.
type Tracker interface {
	IssueThread(ctx context.Context, repo string, number int) (github.IssueThread, error)
	CommentReactors(ctx context.Context, repo string, commentID int64, content string) ([]string, error)
	CloseIssue(ctx context.Context, repo string, number int, opts github.IssueCloseOptions) error
	AddLabels(ctx context.Context, repo string, number int, labels []string) error
	IsHeldLabels(labels []string) bool
	// IsHiveAuthorLogin reports whether login is one of the hive's own
	// posting identities. Only those comments count as an answer.
	IsHiveAuthorLogin(login string) bool
}

// Settings is the resolved configuration.
type Settings struct {
	Enabled        bool
	Window         time.Duration
	QuestionLabels []string
	HumanLabel     string
}

// SettingsFromConfig resolves governor.question_autoclose, env overrides
// included.
func SettingsFromConfig(c config.QuestionAutocloseConfig) Settings {
	return Settings{
		Enabled:        c.IsEnabled(),
		Window:         c.EffectiveWindow(),
		QuestionLabels: c.EffectiveLabels(),
		HumanLabel:     c.EffectiveHumanLabel(),
	}
}

// Entry is one scheduled close.
type Entry struct {
	Repo            string    `json:"repo"`
	Issue           int       `json:"issue"`
	Author          string    `json:"author"`
	AnswerCommentID int64     `json:"answer_comment_id"`
	AnsweredAt      time.Time `json:"answered_at"`
	Deadline        time.Time `json:"deadline"`
}

// Key is the "repo#N" identity of the entry.
func (e Entry) Key() string { return key(e.Repo, e.Issue) }

// Settled records how a schedule ended.
type Settled struct {
	Repo            string    `json:"repo"`
	Issue           int       `json:"issue"`
	AnswerCommentID int64     `json:"answer_comment_id"`
	Outcome         string    `json:"outcome"`
	At              time.Time `json:"at"`
}

// Report counts what one Tick did.
type Report struct {
	Scheduled int
	Closed    int
	KeptOpen  int
	Cancelled int
}

type persisted struct {
	Version   int       `json:"version"`
	Scheduled []Entry   `json:"scheduled"`
	Settled   []Settled `json:"settled"`
}

type checkMark struct {
	at      time.Time
	updated time.Time
}

// Manager owns the durable schedule.
type Manager struct {
	settings Settings
	path     string
	tracker  func() Tracker
	logger   *slog.Logger
	now      func() time.Time

	mu          sync.Mutex
	scheduled   map[string]Entry
	settled     map[string]Settled
	candidates  map[string]github.Issue
	lastChecked map[string]checkMark
}

// New builds a manager persisting to path ("" keeps it in memory). A
// persisted file that cannot be parsed is reported, and the manager starts
// empty rather than failing boot. tracker is resolved per tick because the
// GitHub client can be rebuilt after boot.
func New(path string, settings Settings, tracker func() Tracker, logger *slog.Logger) (*Manager, error) {
	m := &Manager{
		settings:    settings,
		path:        path,
		tracker:     tracker,
		logger:      logger,
		now:         time.Now,
		scheduled:   map[string]Entry{},
		settled:     map[string]Settled{},
		candidates:  map[string]github.Issue{},
		lastChecked: map[string]checkMark{},
	}
	return m, m.load()
}

// Enabled reports whether the feature is on.
func (m *Manager) Enabled() bool { return m != nil && m.settings.Enabled }

// AnswerFooter is the text a hive answer comment must end with: the marker
// and the opt-out line, passed through the mention sanitizer like every other
// hive-authored GitHub text.
func AnswerFooter(window time.Duration) string {
	return advisory.NeutralizeMentions(AnswerMarker + "\n" +
		"If this doesn't answer your question, react 👎 to this comment and the issue will stay open. " +
		"Otherwise it closes automatically in " + formatWindow(window) + ".")
}

func formatWindow(d time.Duration) string {
	hours := int(d / time.Hour)
	if hours < 1 || d%time.Hour != 0 {
		return d.String()
	}
	if hours == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", hours)
}

// KickSection is the scanner-lane instruction block, or "" when disabled.
// issues is the scanner's work list; the question issues in it are named.
func (m *Manager) KickSection(issues []github.Issue) string {
	if !m.Enabled() {
		return ""
	}
	var b strings.Builder
	b.WriteString("QUESTION ISSUES (auto-close on answer is ON, #9584):\n")
	b.WriteString("  An issue that only asks a question (how do I..., is X supported?) and is not a bug report, enhancement or held item: label it `" + m.primaryQuestionLabel() + "` if it is not already, then post ONE answer comment.\n")
	b.WriteString("  End that comment with these lines exactly, and post nothing after it (Hive closes the issue itself; do not close it and do not add a closing comment):\n")
	b.WriteString("```\n" + AnswerFooter(m.settings.Window) + "\n```\n")
	var refs []string
	for _, is := range issues {
		if m.eligibleByLabels(is.Labels, nil) && is.Number > 0 {
			refs = append(refs, fmt.Sprintf("%s#%d", is.Repo, is.Number))
		}
	}
	if len(refs) > 0 {
		b.WriteString("  Question issues in your list: " + strings.Join(refs, ", ") + "\n")
	}
	return b.String()
}

// Offer records the current pass's issues as candidates. It does no I/O.
func (m *Manager) Offer(issues []github.Issue) {
	if !m.Enabled() {
		return
	}
	next := make(map[string]github.Issue, len(issues))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, is := range issues {
		if is.Repo == "" || is.Number <= 0 {
			continue
		}
		k := key(is.Repo, is.Number)
		_, scheduled := m.scheduled[k]
		if scheduled || m.hasQuestionLabel(is.Labels) {
			next[k] = is
		}
	}
	m.candidates = next
}

// Scheduled returns the live schedule, sorted by key.
func (m *Manager) Scheduled() []Entry {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, 0, len(m.scheduled))
	for _, e := range m.scheduled {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// SettledFor returns how the schedule for repo#number ended, if it did.
func (m *Manager) SettledFor(repo string, number int) (Settled, bool) {
	if m == nil {
		return Settled{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.settled[key(repo, number)]
	return s, ok
}

// Run ticks until ctx is done.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if !m.Enabled() {
		return
	}
	if interval <= 0 {
		interval = DefaultTickInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rep := m.Tick(ctx)
			if rep != (Report{}) {
				m.info("question-autoclose: tick", "scheduled", rep.Scheduled, "closed", rep.Closed,
					"kept_open", rep.KeptOpen, "cancelled", rep.Cancelled)
			}
		}
	}
}

// Tick reviews every scheduled close and every unscheduled candidate once.
func (m *Manager) Tick(ctx context.Context) Report {
	var rep Report
	if !m.Enabled() || m.tracker == nil {
		return rep
	}
	tr := m.tracker()
	if tr == nil {
		return rep
	}
	now := m.now()
	m.mu.Lock()
	m.pruneSettledLocked(now)
	entries := make([]Entry, 0, len(m.scheduled))
	for _, e := range m.scheduled {
		entries = append(entries, e)
	}
	cands := make(map[string]github.Issue, len(m.candidates))
	for k, is := range m.candidates {
		cands[k] = is
	}
	m.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key() < entries[j].Key() })

	for _, e := range entries {
		cand, seen := cands[e.Key()]
		m.review(ctx, tr, e, cand, seen, now, &rep)
	}
	keys := make([]string, 0, len(cands))
	for k := range cands {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m.discover(ctx, tr, cands[k], now, &rep)
	}
	return rep
}

// review decides one scheduled entry: cancel, wait, keep open or close.
func (m *Manager) review(ctx context.Context, tr Tracker, e Entry, cand github.Issue, seen bool, now time.Time, rep *Report) {
	if seen && !m.eligibleByLabels(cand.Labels, tr) {
		m.settle(e, OutcomeRelabelled, now, rep)
		return
	}
	due := !now.Before(e.Deadline)
	if !due && !(seen && m.activitySinceLastCheck(e.Key(), cand.UpdatedAt, now)) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, githubCallTimeout)
	defer cancel()
	thread, err := tr.IssueThread(cctx, e.Repo, e.Issue)
	if err != nil {
		m.warn("question-autoclose: could not read issue; will retry", "issue", e.Key(), "error", err)
		return
	}
	if outcome, cancelled := m.cancelReason(e, thread, tr); cancelled {
		m.settle(e, outcome, now, rep)
		return
	}
	if !due {
		return
	}
	reactors, err := tr.CommentReactors(cctx, e.Repo, e.AnswerCommentID, reactionThumbsDown)
	if err != nil {
		m.warn("question-autoclose: could not read reactions; will retry", "issue", e.Key(), "error", err)
		return
	}
	if containsFold(reactors, thread.Author) {
		if err := tr.AddLabels(cctx, e.Repo, e.Issue, []string{m.settings.HumanLabel}); err != nil {
			m.warn("question-autoclose: could not add human label; will retry", "issue", e.Key(), "error", err)
			return
		}
		m.settle(e, OutcomeKeptOpen, now, rep)
		return
	}
	err = tr.CloseIssue(cctx, e.Repo, e.Issue, github.IssueCloseOptions{
		StateReason:             github.IssueStateReasonCompleted,
		SuppressOverrideComment: true,
	})
	if errors.Is(err, github.ErrReporterConfirmationRequired) {
		m.settle(e, OutcomeOutOfScope, now, rep)
		return
	}
	if err != nil {
		m.warn("question-autoclose: close failed; will retry", "issue", e.Key(), "error", err)
		return
	}
	m.settle(e, OutcomeClosed, now, rep)
}

// cancelReason reports why a scheduled close must not happen, from a fresh
// read of the thread.
func (m *Manager) cancelReason(e Entry, thread github.IssueThread, tr Tracker) (string, bool) {
	if thread.IsPullRequest || thread.BugFamily {
		return OutcomeOutOfScope, true
	}
	if !strings.EqualFold(thread.State, issueStateOpen) {
		return OutcomeClosedElsewhere, true
	}
	if !m.eligibleByLabels(thread.Labels, tr) {
		return OutcomeRelabelled, true
	}
	idx := -1
	for i, c := range thread.Comments {
		if c.ID == e.AnswerCommentID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return OutcomeAnswerDeleted, true
	}
	if idx != len(thread.Comments)-1 {
		return OutcomeFollowUp, true
	}
	return "", false
}

// discover schedules a close for a candidate whose last comment is a hive
// answer. Candidates are re-read only when their issue shows new activity.
func (m *Manager) discover(ctx context.Context, tr Tracker, is github.Issue, now time.Time, rep *Report) {
	k := key(is.Repo, is.Number)
	m.mu.Lock()
	_, scheduled := m.scheduled[k]
	m.mu.Unlock()
	if scheduled || !m.eligibleByLabels(is.Labels, tr) || !m.activitySinceLastCheck(k, is.UpdatedAt, now) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, githubCallTimeout)
	defer cancel()
	thread, err := tr.IssueThread(cctx, is.Repo, is.Number)
	if err != nil {
		m.warn("question-autoclose: could not read candidate", "issue", k, "error", err)
		return
	}
	if thread.IsPullRequest || thread.BugFamily || !strings.EqualFold(thread.State, issueStateOpen) ||
		!m.eligibleByLabels(thread.Labels, tr) || len(thread.Comments) == 0 {
		return
	}
	// Only the hive's own posting identity can start the clock: the marker is
	// plain text, so anyone else pasting it must not get a question closed.
	last := thread.Comments[len(thread.Comments)-1]
	if !strings.Contains(last.Body, AnswerMarker) || strings.EqualFold(last.Author, thread.Author) ||
		!tr.IsHiveAuthorLogin(last.Author) {
		return
	}
	answeredAt := last.CreatedAt
	if answeredAt.IsZero() {
		answeredAt = now
	}
	e := Entry{
		Repo: is.Repo, Issue: is.Number, Author: thread.Author,
		AnswerCommentID: last.ID, AnsweredAt: answeredAt, Deadline: answeredAt.Add(m.settings.Window),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.settled[k]; ok && prev.AnswerCommentID == last.ID {
		return
	}
	if _, ok := m.scheduled[k]; ok {
		return
	}
	m.scheduled[k] = e
	rep.Scheduled++
	m.saveLocked()
	m.info("question-autoclose: scheduled", "issue", k, "deadline", e.Deadline.UTC().Format(time.RFC3339))
}

// activitySinceLastCheck reports whether an issue should be re-read: first
// sight, a newer UpdatedAt than the last read, or (UpdatedAt unknown) the
// recheck interval has passed. It records the read it allows.
func (m *Manager) activitySinceLastCheck(k string, updated, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	last, ok := m.lastChecked[k]
	allowed := !ok
	if ok {
		if updated.IsZero() {
			allowed = now.Sub(last.at) >= recheckInterval
		} else {
			allowed = updated.After(last.updated)
		}
	}
	if allowed {
		m.lastChecked[k] = checkMark{at: now, updated: updated}
	}
	return allowed
}

func (m *Manager) settle(e Entry, outcome string, now time.Time, rep *Report) {
	switch outcome {
	case OutcomeClosed:
		rep.Closed++
	case OutcomeKeptOpen:
		rep.KeptOpen++
	default:
		rep.Cancelled++
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.scheduled, e.Key())
	m.settled[e.Key()] = Settled{Repo: e.Repo, Issue: e.Issue, AnswerCommentID: e.AnswerCommentID, Outcome: outcome, At: now}
	m.saveLocked()
	m.info("question-autoclose: settled", "issue", e.Key(), "outcome", outcome)
}

func (m *Manager) pruneSettledLocked(now time.Time) {
	for k, s := range m.settled {
		if now.Sub(s.At) > settledRetention {
			delete(m.settled, k)
		}
	}
}

// eligibleByLabels: a question label, and no excluded, hold or human label.
// tr may be nil (no operator hold labels known); the built-in hold set still
// applies through github.HasHoldLabel.
func (m *Manager) eligibleByLabels(labels []string, tr Tracker) bool {
	if !m.hasQuestionLabel(labels) {
		return false
	}
	human := normalize(m.settings.HumanLabel)
	for _, l := range labels {
		n := normalize(l)
		if _, bad := excludedLabels[n]; bad || (human != "" && n == human) {
			return false
		}
	}
	if tr != nil {
		return !tr.IsHeldLabels(labels)
	}
	return !github.HasHoldLabel(labels)
}

// defaultQuestionLabel is what KickSection asks the scanner to apply when no
// question label is configured.
const defaultQuestionLabel = "question"

func (m *Manager) primaryQuestionLabel() string {
	for _, q := range m.settings.QuestionLabels {
		if t := strings.TrimSpace(q); t != "" {
			return t
		}
	}
	return defaultQuestionLabel
}

func (m *Manager) hasQuestionLabel(labels []string) bool {
	for _, l := range labels {
		n := normalize(l)
		for _, q := range m.settings.QuestionLabels {
			if n == normalize(q) {
				return true
			}
		}
	}
	return false
}

func (m *Manager) load() error {
	if m.path == "" {
		return nil
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("questionclose: parse %s: %w", m.path, err)
	}
	for _, e := range p.Scheduled {
		if e.Repo != "" && e.Issue > 0 && e.AnswerCommentID != 0 {
			m.scheduled[e.Key()] = e
		}
	}
	for _, s := range p.Settled {
		if s.Repo != "" && s.Issue > 0 {
			m.settled[key(s.Repo, s.Issue)] = s
		}
	}
	return nil
}

// saveLocked writes atomically (temp file, fsync, rename). A failure is
// logged; the in-memory schedule stands and the next change retries.
func (m *Manager) saveLocked() {
	if m.path == "" {
		return
	}
	if err := m.writeLocked(); err != nil {
		m.warn("question-autoclose: could not persist schedule", "path", m.path, "error", err)
	}
}

func (m *Manager) writeLocked() error {
	p := persisted{Version: persistVersion, Scheduled: make([]Entry, 0, len(m.scheduled)), Settled: make([]Settled, 0, len(m.settled))}
	for _, e := range m.scheduled {
		p.Scheduled = append(p.Scheduled, e)
	}
	for _, s := range m.settled {
		p.Settled = append(p.Settled, s)
	}
	sort.Slice(p.Scheduled, func(i, j int) bool { return p.Scheduled[i].Key() < p.Scheduled[j].Key() })
	sort.Slice(p.Settled, func(i, j int) bool {
		return key(p.Settled[i].Repo, p.Settled[i].Issue) < key(p.Settled[j].Repo, p.Settled[j].Issue)
	})
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".question-autoclose-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, m.path)
}

func (m *Manager) info(msg string, args ...any) {
	if m.logger != nil {
		m.logger.Info(msg, args...)
	}
}

func (m *Manager) warn(msg string, args ...any) {
	if m.logger != nil {
		m.logger.Warn(msg, args...)
	}
}

func key(repo string, number int) string { return fmt.Sprintf("%s#%d", repo, number) }

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func containsFold(list []string, want string) bool {
	if want == "" {
		return false
	}
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}
