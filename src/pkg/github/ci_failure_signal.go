package github

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// CIFailurePR is a PR a check_run / check_suite / status failure delivery
// resolved to, as the PR detail cache knew it at the moment of the event
// (captured before the delivery invalidates the entry).
type CIFailurePR struct {
	Number       int    `json:"number"`
	Author       string `json:"author,omitempty"`
	Title        string `json:"title,omitempty"`
	URL          string `json:"url,omitempty"`
	BaseRef      string `json:"base_ref,omitempty"`
	HiveAuthored bool   `json:"hive_authored"`
}

// WebhookCIFailure is the failed-check content of one check_run /
// check_suite / status delivery, carried on WebhookEventResult.CIFailure so
// the receiver can react now rather than on the next sweep.
type WebhookCIFailure struct {
	SuiteID    int64         `json:"suite_id,omitempty"`
	CheckRunID int64         `json:"check_run_id,omitempty"`
	HeadSHA    string        `json:"head_sha"`
	Check      string        `json:"check"`
	Conclusion string        `json:"conclusion"`
	DetailsURL string        `json:"details_url,omitempty"`
	PRs        []CIFailurePR `json:"prs,omitempty"`
}

// CIFailureSignal is one recorded "this check went red on this PR head"
// observation. Agent is the hive agent the PR is attributed to (empty for a
// contributor PR or an unattributed one).
type CIFailureSignal struct {
	Repo         string                     `json:"repo"`
	Number       int                        `json:"number"`
	HeadSHA      string                     `json:"head_sha"`
	Check        string                     `json:"check"`
	Conclusion   string                     `json:"conclusion,omitempty"`
	DetailsURL   string                     `json:"details_url,omitempty"`
	Title        string                     `json:"title,omitempty"`
	Author       string                     `json:"author,omitempty"`
	HiveAuthored bool                       `json:"hive_authored"`
	Agent        string                     `json:"agent,omitempty"`
	At           time.Time                  `json:"at"`
	Integration  *IntegrationFailureContext `json:"integration,omitempty"`
}

type IntegrationFailureContext struct {
	CandidateID string   `json:"candidate_id"`
	TestedSHA   string   `json:"tested_sha"`
	TestedTree  string   `json:"tested_tree"`
	BaseSHA     string   `json:"base_sha"`
	Members     []string `json:"ordered_members"`
	Policy      string   `json:"policy"`
	Producer    string   `json:"producer"`
	Attempt     int      `json:"attempt"`
	RunID       int64    `json:"run_id,omitempty"`
	SuiteID     int64    `json:"suite_id,omitempty"`
	Source      string   `json:"source,omitempty"`
}

// CIFailureSignalsStatus is the /api/status view of the store.
type CIFailureSignalsStatus struct {
	// Count is the number of distinct (repo, head, check) signals retained.
	Count int `json:"count"`
	// Last is the most recently recorded signal, nil when none.
	Last *CIFailureSignal `json:"last,omitempty"`
	// Kicks counts webhook-triggered agent kicks since boot.
	Kicks int `json:"kicks"`
	// Unmatched counts failure deliveries whose head SHA resolved to no open
	// PR; the sweep handles them from the dirty mark.
	Unmatched int `json:"unmatched"`
}

// ciFailureSignalTTL bounds how long a signal stays in the store after its
// last observation; a PR that stays red longer is re-listed from
// ci-failing.json by the sweep anyway.
const ciFailureSignalTTL = 2 * time.Hour

type ciFailureSignalKey struct {
	repo, headSHA, check string
}

// CIFailureSignalStore is the in-memory "ci failure signal" store: dedup per
// head SHA + check, per-PR-head kick cooldown, and a bounded TTL.
type CIFailureSignalStore struct {
	mu        sync.Mutex
	now       func() time.Time
	signals   map[ciFailureSignalKey]CIFailureSignal
	lastKick  map[string]time.Time
	last      *CIFailureSignal
	kicks     int
	unmatched int
}

func newCIFailureSignalStore() *CIFailureSignalStore {
	return &CIFailureSignalStore{
		now:      time.Now,
		signals:  map[ciFailureSignalKey]CIFailureSignal{},
		lastKick: map[string]time.Time{},
	}
}

var sharedCIFailureSignals = newCIFailureSignalStore()

// CIFailureSignals is the process-wide store the webhook receiver records
// into and the kick builder / sweep read from.
func CIFailureSignals() *CIFailureSignalStore { return sharedCIFailureSignals }

// ResetCIFailureSignalsForTest swaps in a fresh store on the given clock and
// returns the restore func.
func ResetCIFailureSignalsForTest(now func() time.Time) func() {
	old := sharedCIFailureSignals
	s := newCIFailureSignalStore()
	if now != nil {
		s.now = now
	}
	sharedCIFailureSignals = s
	return func() { sharedCIFailureSignals = old }
}

func ciFailureKey(sig CIFailureSignal) ciFailureSignalKey {
	return ciFailureSignalKey{
		repo:    canonicalPRDetailRepo(sig.Repo),
		headSHA: strings.ToLower(strings.TrimSpace(sig.HeadSHA)),
		check:   strings.TrimSpace(sig.Check),
	}
}

func ciFailureKickKey(repo string, number int, headSHA string) string {
	return canonicalPRDetailRepo(repo) + "#" + strconv.Itoa(number) + "@" + strings.ToLower(strings.TrimSpace(headSHA))
}

// Record stores sig and reports whether it is new for its (repo, head,
// check); a repeat observation refreshes the entry (and fills in a missing
// agent attribution) but returns false so callers do not re-kick.
func (s *CIFailureSignalStore) Record(sig CIFailureSignal) bool {
	if s == nil || sig.Repo == "" || sig.Number <= 0 || strings.TrimSpace(sig.HeadSHA) == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	if sig.At.IsZero() {
		sig.At = now
	}
	key := ciFailureKey(sig)
	prev, dup := s.signals[key]
	if dup {
		if prev.Agent != "" && sig.Agent == "" {
			sig.Agent = prev.Agent
		}
		if sig.Title == "" {
			sig.Title = prev.Title
		}
	}
	s.signals[key] = sig
	s.last = &sig
	return !dup
}

// SetAgent attributes every stored signal for repo#number to agent.
func (s *CIFailureSignalStore) SetAgent(repo string, number int, agent string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	canon := canonicalPRDetailRepo(repo)
	for k, sig := range s.signals {
		if k.repo == canon && sig.Number == number {
			sig.Agent = agent
			s.signals[k] = sig
		}
	}
}

// RecordUnmatched counts a failure delivery no open PR matched.
func (s *CIFailureSignalStore) RecordUnmatched() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.unmatched++
	s.mu.Unlock()
}

// AllowKick reports whether a webhook-triggered kick for repo#number at
// headSHA is outside the cooldown, and records it when so.
func (s *CIFailureSignalStore) AllowKick(repo string, number int, headSHA string, cooldown time.Duration) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	key := ciFailureKickKey(repo, number, headSHA)
	if last, ok := s.lastKick[key]; ok && cooldown > 0 && now.Sub(last) < cooldown {
		return false
	}
	s.lastKick[key] = now
	s.kicks++
	return true
}

// ForAgent lists the retained signals attributed to agent, oldest first.
func (s *CIFailureSignalStore) ForAgent(agent string) []CIFailureSignal {
	return s.filter(func(sig CIFailureSignal) bool { return agent != "" && sig.Agent == agent })
}

// ForPR lists the retained signals for repo#number, oldest first.
func (s *CIFailureSignalStore) ForPR(repo string, number int) []CIFailureSignal {
	canon := canonicalPRDetailRepo(repo)
	return s.filter(func(sig CIFailureSignal) bool {
		return sig.Number == number && canonicalPRDetailRepo(sig.Repo) == canon
	})
}

func (s *CIFailureSignalStore) filter(keep func(CIFailureSignal) bool) []CIFailureSignal {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	var out []CIFailureSignal
	for _, sig := range s.signals {
		if keep(sig) {
			out = append(out, sig)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Check < out[j].Check
	})
	return out
}

// Forget drops every signal for repo#number (the PR merged, closed, or its
// head moved on to a new SHA that is not this one).
func (s *CIFailureSignalStore) Forget(repo string, number int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	canon := canonicalPRDetailRepo(repo)
	for k, sig := range s.signals {
		if k.repo == canon && sig.Number == number {
			delete(s.signals, k)
		}
	}
}

// Snapshot is the /api/status view.
func (s *CIFailureSignalStore) Snapshot() CIFailureSignalsStatus {
	if s == nil {
		return CIFailureSignalsStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	out := CIFailureSignalsStatus{Count: len(s.signals), Kicks: s.kicks, Unmatched: s.unmatched}
	if s.last != nil {
		last := *s.last
		out.Last = &last
	}
	return out
}

func (s *CIFailureSignalStore) pruneLocked(now time.Time) {
	for k, sig := range s.signals {
		if now.Sub(sig.At) > ciFailureSignalTTL {
			delete(s.signals, k)
		}
	}
	for k, at := range s.lastKick {
		if now.Sub(at) > ciFailureSignalTTL {
			delete(s.lastKick, k)
		}
	}
}

// AuthorAgentForPR is the hive agent whose relay request opened repo#number
// per the audit log (agent_pr_created), or "" when none did.
func AuthorAgentForPR(repo string, number int) string {
	return auditAuthorAgentForPR(repo, number, reviewVerdictAuditPath, time.Now().UTC().Add(-reviewVerdictAuditLookback))
}

// webhookCIFailure extracts the failed-check content of a check_run /
// check_suite / status delivery, or nil when the delivery is not a failure
// the hive should react to (pending, success, neutral, skipped, cancelled).
func webhookCIFailure(event string, p webhookEventPayload) *WebhookCIFailure {
	switch event {
	case "check_run":
		if p.CheckRun == nil || p.Action != "completed" {
			return nil
		}
		switch p.CheckRun.Conclusion {
		case "failure", "timed_out":
		default:
			return nil
		}
		return &WebhookCIFailure{
			HeadSHA:    strings.TrimSpace(p.CheckRun.HeadSHA),
			Check:      strings.TrimSpace(p.CheckRun.Name),
			Conclusion: p.CheckRun.Conclusion,
			DetailsURL: firstNonEmpty(p.CheckRun.DetailsURL, p.CheckRun.HTMLURL),
		}
	case "check_suite":
		if p.CheckSuite == nil || p.Action != "completed" {
			return nil
		}
		switch p.CheckSuite.Conclusion {
		case "failure", "timed_out":
		default:
			return nil
		}
		name := "check suite"
		if app := strings.TrimSpace(p.CheckSuite.App.Name); app != "" {
			name = app + " check suite"
		}
		return &WebhookCIFailure{
			HeadSHA:    strings.TrimSpace(p.CheckSuite.HeadSHA),
			Check:      name,
			Conclusion: p.CheckSuite.Conclusion,
			DetailsURL: strings.TrimSpace(p.CheckSuite.URL),
		}
	case "status":
		switch p.State {
		case "failure", "error":
		default:
			return nil
		}
		return &WebhookCIFailure{
			HeadSHA:    strings.TrimSpace(p.SHA),
			Check:      strings.TrimSpace(p.Context),
			Conclusion: p.State,
			DetailsURL: strings.TrimSpace(p.TargetURL),
		}
	}
	return nil
}

// SeedPRDetailCacheForTest places a minimal open PR in the shared detail
// cache so webhook tests can resolve a head SHA without a GitHub call.
func SeedPRDetailCacheForTest(repo string, number int, author, title, headSHA, baseRef string) {
	sharedPRDetailCache.put(repo, number, &gh.PullRequest{
		Number:  gh.Ptr(number),
		State:   gh.Ptr("open"),
		Title:   gh.Ptr(title),
		HTMLURL: gh.Ptr("https://github.com/" + repo + "/pull/" + strconv.Itoa(number)),
		User:    &gh.User{Login: gh.Ptr(author)},
		Head:    &gh.PullRequestBranch{SHA: gh.Ptr(headSHA)},
		Base:    &gh.PullRequestBranch{Ref: gh.Ptr(baseRef)},
	})
}
