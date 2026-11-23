package prgate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Mode is auto_merge.pr_gate.mode.
type Mode string

const (
	ModeOff     Mode = "off"
	ModeShadow  Mode = "shadow"
	ModeEnforce Mode = "enforce"
)

// ParseMode maps a configured string to a Mode. Empty is off; anything unknown
// is reported so config validation can reject it (and runtime reads treat it
// as off).
func ParseMode(s string) (Mode, bool) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case "", ModeOff:
		return ModeOff, true
	case ModeShadow:
		return ModeShadow, true
	case ModeEnforce:
		return ModeEnforce, true
	}
	return ModeOff, false
}

// EnforceAvailable reports that `enforce` is built: a configured `enforce`
// posts the real required names, but only while the installation's grant
// check has positively confirmed Commit statuses write (see Gate.mode). Any
// other answer runs it as `shadow` or, with the grant known missing, posts
// nothing at all.
const EnforceAvailable = true

// Effective returns the mode the gate runs in for a configured one, before the
// grant check is applied.
func Effective(m Mode) Mode {
	if m == ModeEnforce && !EnforceAvailable {
		return ModeShadow
	}
	return m
}

const (
	// StateSuccess and StateFailure are commit status states.
	StateSuccess = "success"
	StateFailure = "failure"

	// StatusDescriptionMax is GitHub's cap on a commit status description.
	StatusDescriptionMax = 140

	maxExpectations = 512
	// maxSentEntries bounds the memory of statuses already posted. Past it the
	// memory restarts empty: the worst case is one repeated, harmless post.
	maxSentEntries = 4096
	maxInflight    = 8
)

var headSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Subject is the current state of a pull request as the gate needs it.
type Subject struct {
	HeadSHA string
	Title   string
	Draft   bool
	// State is GitHub's "open" or "closed".
	State string
}

// API is everything the gate needs from GitHub.
type API interface {
	// ReadSubject re-reads the PR from GitHub, never from a cache.
	ReadSubject(ctx context.Context, repo string, number int) (Subject, error)
	// PostStatus writes one commit status on sha.
	PostStatus(ctx context.Context, repo string, number int, sha, statusContext, state, description string) error
}

// Status is one commit status the gate wants on a head.
type Status struct {
	Context     string
	State       string
	Description string
}

// Plan returns the statuses a mode posts for a title. Off posts nothing.
// Shadow posts only the shadow-named context, whose description carries both
// verdicts so the operator can read them without a second status. Enforce
// posts the real required names in the order pr-verifier.yml settles them:
// PR Type and PR Issue carry the verdicts, then the wrapper `verify PR
// contents`, which is green by design. HandleEvent turns the wrapper red only
// when a verdict could not be recorded, exactly as the workflow job fails
// only then.
func Plan(mode Mode, title string) []Status {
	typeV, issueV := PRTypeVerdict(title), PRIssueVerdict(title)
	switch mode {
	case ModeShadow:
		return []Status{{ContextVerifyShadow, StateSuccess, clip(fmt.Sprintf("PR Type: %s; PR Issue: %s", typeV.Conclusion, issueV.Conclusion))}}
	case ModeEnforce:
		return []Status{
			{ContextPRType, stateOf(typeV), clip(typeV.Summary)},
			{ContextPRIssue, stateOf(issueV), clip(issueV.Summary)},
			{ContextVerify, StateSuccess, clip("PR contents verified by the hive: " + typeV.Conclusion + " / " + issueV.Conclusion)},
		}
	}
	return nil
}

// unrecordedWrapper is the wrapper status posted in place of the green one
// when a verdict could not be recorded on the head: a recording failure, not
// a verdict on the PR. Any later event or the backstop retries it.
func unrecordedWrapper(names []string) Status {
	return Status{ContextVerify, StateFailure, clip("The hive could not record " + strings.Join(names, " and ") +
		" on this head; add or remove a label to retry. Not a verdict on the PR.")}
}

func stateOf(v Verdict) string {
	if v.Conclusion == ConclusionSuccess {
		return StateSuccess
	}
	return StateFailure
}

func clip(s string) string {
	r := []rune(s)
	if len(r) <= StatusDescriptionMax {
		return s
	}
	return string(r[:StatusDescriptionMax])
}

// Outcome of one HandleEvent call.
type Outcome struct {
	// Skipped names why nothing was posted ("" when something was).
	Skipped string
	Posted  []string
	Err     error
}

// Skip reasons.
const (
	SkipModeOff    = "mode-off"
	SkipBadHead    = "event-head-not-a-sha"
	SkipNotOpen    = "pr-not-open"
	SkipDraft      = "pr-draft"
	SkipHeadMoved  = "head-moved"
	SkipUnchanged  = "already-posted"
	SkipReadFailed = "re-read-failed"
)

// ResultPosted, ResultForbidden and ResultError are the last post results.
const (
	ResultPosted    = "posted"
	ResultForbidden = "forbidden"
	ResultError     = "error"
)

// RepoStats is the per-repository picture the logs and the later health
// surfaces read.
type RepoStats struct {
	Mode           Mode
	Posted         int
	Forbidden      int
	Errors         int
	LastPost       string
	LastPostAt     time.Time
	LastPostHead   string
	Agreements     int
	Disagreements  int
	LastComparison string

	forbiddenUntil time.Time
}

type expectation struct {
	typeConclusion, issueConclusion string
	at                              time.Time
}

type posted struct{ state, description string }

// Gate posts the PR-contents gate. The zero value is not usable; use New.
type Gate struct {
	API    API
	ModeOf func(repo string) Mode
	Log    *slog.Logger
	Now    func() time.Time
	// Grant reads the installation's Commit statuses grant. Nil means no grant
	// check (a credential with no App installation).
	Grant GrantProvider

	mu       sync.Mutex
	sent     map[string]posted
	expect   map[string]expectation
	stats    map[string]*RepoStats
	prLocks  map[string]*prLock
	inflight int
	grant    grantState
}

type prLock struct {
	mu   sync.Mutex
	refs int
}

// New builds a Gate.
func New(api API, modeOf func(string) Mode, log *slog.Logger) *Gate {
	if log == nil {
		log = slog.Default()
	}
	return &Gate{API: api, ModeOf: modeOf, Log: log, Now: time.Now,
		sent: map[string]posted{}, expect: map[string]expectation{}, stats: map[string]*RepoStats{}, prLocks: map[string]*prLock{}}
}

// mode is the mode the gate actually runs in for repo: the configured one,
// forced off while the last grant check found the Commit statuses write grant
// missing (no status of any name can be written then), and with enforce run as
// shadow until a grant check has positively confirmed the write grant. It
// never promotes.
func (g *Gate) mode(repo string) Mode {
	if g == nil || g.ModeOf == nil {
		return ModeOff
	}
	m := Effective(g.ModeOf(repo))
	if m == ModeOff {
		return ModeOff
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.grant.info.Missing():
		return ModeOff
	case m == ModeEnforce && !g.enforceGrantedLocked():
		return ModeShadow
	}
	return m
}

// Active reports whether repo is configured for the gate (a mode other than
// off). A forced-off gate is still active: its events must reach the grant
// check, which is how it notices the grant arriving.
func (g *Gate) Active(repo string) bool { return Effective(g.ConfiguredMode(repo)) != ModeOff }

// TryAcquire reserves one of a bounded number of concurrent event slots; the
// caller releases it with Release. A burst beyond the bound is dropped and
// logged; the next event for the head covers it.
func (g *Gate) TryAcquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight >= maxInflight {
		return false
	}
	g.inflight++
	return true
}

// Release frees a slot taken by TryAcquire.
func (g *Gate) Release() {
	g.mu.Lock()
	g.inflight--
	g.mu.Unlock()
}

func (g *Gate) lockPR(key string) func() {
	g.mu.Lock()
	l := g.prLocks[key]
	if l == nil {
		l = &prLock{}
		g.prLocks[key] = l
	}
	l.refs++
	g.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		g.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(g.prLocks, key)
		}
		g.mu.Unlock()
	}
}

func (g *Gate) repoStats(repo string) *RepoStats {
	key := strings.ToLower(repo)
	s := g.stats[key]
	if s == nil {
		s = &RepoStats{}
		g.stats[key] = s
	}
	return s
}

// HandleEvent processes one pull_request delivery. eventHead is the head SHA
// the event carried; it is the commit the status goes on, and only if a fresh
// read of the PR still shows that head. Nothing is posted for a draft or
// closed PR. Already-posted identical statuses are not posted again.
func (g *Gate) HandleEvent(ctx context.Context, repo string, number int, eventHead string) Outcome {
	if Effective(g.ConfiguredMode(repo)) == ModeOff {
		return Outcome{Skipped: SkipModeOff}
	}
	if g.RepoForbidden(repo) {
		return Outcome{Skipped: SkipForbiddenCooldown}
	}
	g.RefreshGrant(ctx)
	mode := g.mode(repo)
	if mode == ModeOff {
		return Outcome{Skipped: SkipGrantMissing}
	}
	if !headSHARe.MatchString(eventHead) {
		return Outcome{Skipped: SkipBadHead}
	}
	defer g.lockPR(fmt.Sprintf("%s#%d", strings.ToLower(repo), number))()

	subject, err := g.API.ReadSubject(ctx, repo, number)
	if err != nil {
		g.Log.Warn("pr-gate: could not re-read the PR head, nothing posted", "repo", repo, "number", number, "error", err)
		return Outcome{Skipped: SkipReadFailed, Err: err}
	}
	switch {
	case subject.State != "open":
		return Outcome{Skipped: SkipNotOpen}
	case subject.Draft:
		return Outcome{Skipped: SkipDraft}
	case subject.HeadSHA != eventHead:
		return Outcome{Skipped: SkipHeadMoved}
	}

	// The verdict is recorded before the write: the workflow's own result for
	// this head can arrive at any moment and is compared against it.
	g.noteExpectation(repo, eventHead, subject.Title)
	out := Outcome{Skipped: SkipUnchanged}
	var unrecorded []string
	for _, st := range Plan(mode, subject.Title) {
		if st.Context == ContextVerify && len(unrecorded) > 0 {
			st = unrecordedWrapper(unrecorded)
		}
		key := strings.ToLower(repo) + "@" + eventHead + "|" + st.Context
		g.mu.Lock()
		prev, seen := g.sent[key]
		g.mu.Unlock()
		if seen && prev == (posted{st.State, st.Description}) {
			continue
		}
		if err := g.post(ctx, repo, number, eventHead, st); err != nil {
			out.Err = err
			out.Skipped = ""
			// A 403 means nothing more can be written. Any other failure
			// leaves this verdict unrecorded and the wrapper says so, as the
			// workflow job does.
			if errors.Is(err, ErrForbidden) || errors.Is(err, ErrContextNotAllowed) || st.Context == ContextVerify {
				break
			}
			unrecorded = append(unrecorded, st.Context)
			continue
		}
		g.mu.Lock()
		if len(g.sent) >= maxSentEntries {
			g.sent = map[string]posted{}
		}
		g.sent[key] = posted{st.State, st.Description}
		g.mu.Unlock()
		out.Posted = append(out.Posted, st.Context)
		out.Skipped = ""
	}
	return out
}

// post writes one status through the allowlist. A refusal is recorded once
// and never retried here.
func (g *Gate) post(ctx context.Context, repo string, number int, sha string, st Status) error {
	if err := PostableContext(st.Context); err != nil {
		return err
	}
	err := g.API.PostStatus(ctx, repo, number, sha, st.Context, st.State, st.Description)
	mode := g.mode(repo)
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.repoStats(repo)
	s.Mode = mode
	s.LastPostAt, s.LastPostHead = g.Now(), sha
	switch {
	case err == nil:
		s.Posted++
		s.LastPost = ResultPosted
		g.Log.Info("pr-gate: status posted", "repo", repo, "head", sha, "context", st.Context, "state", st.State, "mode", string(s.Mode))
	case errors.Is(err, ErrForbidden):
		s.Forbidden++
		s.LastPost = ResultForbidden
		s.forbiddenUntil = g.Now().Add(ForbiddenCooldown)
		g.invalidateGrantLocked()
		g.Log.Warn("pr-gate: status refused with 403, the installation has no Commit statuses write grant", "repo", repo, "head", sha, "context", st.Context, "error", err)
	default:
		s.Errors++
		s.LastPost = ResultError
		g.Log.Warn("pr-gate: status post failed", "repo", repo, "head", sha, "context", st.Context, "error", err)
	}
	return err
}

// ErrForbidden is what an API implementation wraps when GitHub refuses the
// status write with 403 (the App has no Commit statuses write grant).
var ErrForbidden = errors.New("pr gate: status write forbidden")

func (g *Gate) noteExpectation(repo, sha, title string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.expect) >= maxExpectations {
		var oldest string
		var oldestAt time.Time
		for k, e := range g.expect {
			if oldest == "" || e.at.Before(oldestAt) {
				oldest, oldestAt = k, e.at
			}
		}
		delete(g.expect, oldest)
	}
	g.expect[strings.ToLower(repo)+"@"+sha] = expectation{
		typeConclusion: PRTypeVerdict(title).Conclusion, issueConclusion: PRIssueVerdict(title).Conclusion, at: g.Now()}
}

// Comparison is the record written when the workflow's own result for a head
// arrives and the hive already holds its verdict for that head.
type Comparison struct {
	Repo, Head, Name, Hive, Workflow string
	Agree                            bool
}

// CompareWorkflow records the workflow's conclusion for one of the three gate
// check names on head against the hive's verdict. ok is false when the hive
// holds no verdict for that head (it never posted, or the record aged out).
func (g *Gate) CompareWorkflow(repo, head, name, workflowConclusion string) (Comparison, bool) {
	g.mu.Lock()
	e, found := g.expect[strings.ToLower(repo)+"@"+head]
	g.mu.Unlock()
	if !found {
		return Comparison{}, false
	}
	var hive string
	switch name {
	case ContextPRType:
		hive = e.typeConclusion
	case ContextPRIssue:
		hive = e.issueConclusion
	case ContextVerify:
		// The wrapper job is green whatever the verdicts are.
		hive = ConclusionSuccess
	default:
		return Comparison{}, false
	}
	c := Comparison{Repo: repo, Head: head, Name: name, Hive: hive, Workflow: workflowConclusion, Agree: hive == workflowConclusion}
	g.mu.Lock()
	s := g.repoStats(repo)
	if c.Agree {
		s.Agreements++
	} else {
		s.Disagreements++
	}
	s.LastComparison = fmt.Sprintf("%s: hive=%s workflow=%s agree=%t head=%s", name, hive, workflowConclusion, c.Agree, head)
	g.mu.Unlock()
	if c.Agree {
		g.Log.Info("pr-gate: comparison with the workflow result agrees", "repo", repo, "head", head, "check", name, "hive", hive, "workflow", workflowConclusion)
	} else {
		g.Log.Warn("pr-gate: comparison with the workflow result DISAGREES", "repo", repo, "head", head, "check", name, "hive", hive, "workflow", workflowConclusion)
	}
	return c, true
}

// Snapshot returns per-repository stats, sorted by repo, for logs and health.
func (g *Gate) Snapshot() map[string]RepoStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	repos := make([]string, 0, len(g.stats))
	for r := range g.stats {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	out := make(map[string]RepoStats, len(repos))
	for _, r := range repos {
		out[r] = *g.stats[r]
	}
	return out
}

var defaultGate atomic.Pointer[Gate]

// SetDefault registers the process's gate for readers that cannot be handed
// one (the merge lanes' held reason). Nil clears it.
func SetDefault(g *Gate) { defaultGate.Store(g) }

// DefaultHoldReason is HoldReason on the registered gate ("" when none).
func DefaultHoldReason(repo, statusContext string) string {
	return defaultGate.Load().HoldReason(repo, statusContext)
}
