package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// adoptAPI layers the git-ref / pull-request routes the adoption needs on
// top of stageAPI and records what the hive did.
type adoptAPI struct {
	mu          sync.Mutex
	refs        map[string]string // refs/heads/x → sha
	refUpdates  []string
	pulls       []map[string]any
	nextPR      int
	superseders map[int]map[string]any // number → GET payload
	prEdits     []string
	commits     map[string]adoptCommit
}

type adoptCommit struct {
	login    string // GitHub login of author/committer, or git:<name> for an unlinked identity
	verified bool
}

func newAdoptAPI() *adoptAPI {
	return &adoptAPI{refs: map[string]string{}, nextPR: 90, superseders: map[int]map[string]any{}, commits: map[string]adoptCommit{}}
}

func (a *adoptAPI) handle(w http.ResponseWriter, r *http.Request) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/repos/acme/widget/git/refs":
		var body struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, exists := a.refs[body.Ref]; exists {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Reference already exists"})
			return true
		}
		a.refs[body.Ref] = body.SHA
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": body.Ref, "object": map[string]string{"sha": body.SHA}})
		return true
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/repos/acme/widget/git/refs/"):
		var body struct {
			SHA string `json:"sha"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		ref := "refs/" + strings.TrimPrefix(path, "/repos/acme/widget/git/refs/")
		a.refs[ref] = body.SHA
		a.refUpdates = append(a.refUpdates, ref+"="+body.SHA)
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": ref, "object": map[string]string{"sha": body.SHA}})
		return true
	case r.Method == http.MethodPost && path == "/repos/acme/widget/pulls":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.pulls = append(a.pulls, body)
		n := a.nextPR
		a.nextPR++
		a.superseders[n] = map[string]any{"number": n, "state": "open", "merged": false}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "html_url": "https://github.com/acme/widget/pull/90"})
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/pulls/") && !strings.HasSuffix(path, "/commits"):
		var n int
		_, _ = sscanInt(strings.TrimPrefix(path, "/repos/acme/widget/pulls/"), &n)
		payload, ok := a.superseders[n]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		_ = json.NewEncoder(w).Encode(payload)
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/commits/"):
		sha := strings.TrimPrefix(path, "/repos/acme/widget/commits/")
		commit, ok := a.commits[sha]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		verification := map[string]any{"verified": commit.verified}
		if name, ok := strings.CutPrefix(commit.login, "git:"); ok {
			// Unlinked identity: bare git author/committer name, no GitHub user.
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "author": nil, "committer": nil, "commit": map[string]any{"author": map[string]any{"name": name}, "committer": map[string]any{"name": name}, "verification": verification}})
			return true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "author": map[string]any{"login": commit.login}, "committer": map[string]any{"login": commit.login}, "commit": map[string]any{"verification": verification}})
		return true
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/repos/acme/widget/pulls/"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.prEdits = append(a.prEdits, strings.TrimPrefix(path, "/repos/acme/widget/pulls/")+":"+body["state"].(string))
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		return true
	}
	return false
}

func sscanInt(s string, n *int) (int, error) {
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		v = v*10 + int(c-'0')
	}
	*n = v
	return 1, nil
}

// allowMaintainerEdits makes GET pulls/n report maintainer_can_modify.
func (a *adoptAPI) allowMaintainerEdits(n int, allow bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.superseders[n] = map[string]any{"number": n, "state": "open", "merged": false, "maintainer_can_modify": allow}
}

func (a *adoptAPI) setCommitAuthor(sha, login string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commits[sha] = adoptCommit{login: login}
}

func (a *adoptAPI) setVerifiedCommitAuthor(sha, login string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commits[sha] = adoptCommit{login: login, verified: true}
}

func (a *adoptAPI) setSuperseder(n int, state string, merged bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.superseders[n] = map[string]any{"number": n, "state": state, "merged": merged}
}

func adoptPR(number int, head string, labels ...string) *gh.PullRequest {
	pr := nudgePR(number, head, labels...)
	pr.Base = &gh.PullRequestBranch{Ref: gh.Ptr("v5")}
	pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("mallory/widget")}
	pr.Body = gh.Ptr("Fixes the widget.\n\nCloses #12\nfixes #13\n\n— hive: backend=pi model=gpt-6\n")
	return pr
}

func adoptionComments(api *stageAPI) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, AdoptionMarkerPrefix) {
			out = append(out, c)
		}
	}
	return out
}

type adoptHarness struct {
	t     *testing.T
	api   *stageAPI
	adopt *adoptAPI
	clock *contributorLaneClock
	sink  *fakeNudgeSink
	e     *Engine
}

func newAdoptHarness(t *testing.T, outcome ContributorNudgeOutcome, policy StageLabelPolicy) (*adoptHarness, func()) {
	t.Helper()
	sink := &fakeNudgeSink{outcome: outcome}
	return newAdoptHarnessWith(t, sink, sink, policy)
}

// newAdoptHarnessWith wires an arbitrary sink (wrapping fake) into the engine.
func newAdoptHarnessWith(t *testing.T, fake *fakeNudgeSink, sink ContributorNudgeSink, policy StageLabelPolicy) (*adoptHarness, func()) {
	t.Helper()
	api, closeAPI := seededStageAPI(t)
	adopt := newAdoptAPI()
	api.extra = adopt.handle
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, policy)
	return &adoptHarness{t: t, api: api, adopt: adopt, clock: clock, sink: fake, e: e}, closeAPI
}

func (h *adoptHarness) observe(pr *gh.PullRequest, reason string) StageLabelResult {
	h.t.Helper()
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
	h.e.noteStageRole("acme", "widget", pr.GetNumber(), stagelabels.RoleContributor)
	res, err := h.e.ApplyStageLabels(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func (h *adoptHarness) addedLabels() []string {
	var out []string
	for _, a := range h.api.added {
		out = append(out, a...)
	}
	return out
}

func adoptPolicy() StageLabelPolicy {
	return StageLabelPolicy{Enabled: true, ContributorNudge: true, Adoption: true, AdoptionStrictAttribution: true, GraceOnline: 2 * time.Hour, GraceOffline: time.Hour, HiveID: "acme-hive"}
}

func TestAdoptionGraceSelection(t *testing.T) {
	p := adoptPolicy()
	if got := adoptionGrace(stagelabels.Record{NudgeOnline: true}, p); got != 2*time.Hour {
		t.Fatalf("online grace %v", got)
	}
	if got := adoptionGrace(stagelabels.Record{}, p); got != time.Hour {
		t.Fatalf("offline grace %v", got)
	}
	if got := adoptionGrace(stagelabels.Record{NudgeOnline: true}, StageLabelPolicy{}); got != 2*time.Hour {
		t.Fatalf("default online grace %v", got)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	rec := stagelabels.Record{NudgeHead: "aaa", NudgeCount: 1, FirstNudgeAt: now}
	none := ContributorPresence{}
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(59*time.Minute), none, false); due {
		t.Fatal("due before grace")
	}
	if due, reason := adoptionDue(rec, "aaa", p, now.Add(time.Hour), none, false); !due || reason != AdoptReasonGrace {
		t.Fatalf("not due at grace: %v %q", due, reason)
	}
	if due, _ := adoptionDue(rec, "bbb", p, now.Add(2*time.Hour), none, false); due {
		t.Fatal("due for a head that was never nudged")
	}
	// Live presence: online relay keeps the online grace; an offline relay is
	// adopted AdoptAfterOffline after its last heartbeat (not the nudge);
	// an unregistered author after AdoptAfterUnregistered.
	online := ContributorPresence{Registered: true, Online: true}
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(time.Hour), online, true); due {
		t.Fatal("online relay adopted before the online grace")
	}
	if due, reason := adoptionDue(rec, "aaa", p, now.Add(2*time.Hour), online, true); !due || reason != AdoptReasonGrace {
		t.Fatalf("online relay at grace: %v %q", due, reason)
	}
	offline := ContributorPresence{Registered: true, LastSeen: now.Add(10 * time.Minute)}
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(24*time.Minute), offline, true); due {
		t.Fatal("offline relay adopted before 15m since its last heartbeat")
	}
	if due, reason := adoptionDue(rec, "aaa", p, now.Add(25*time.Minute), offline, true); !due || reason != AdoptReasonOffline {
		t.Fatalf("offline relay: %v %q", due, reason)
	}
	p.AdoptAfterOffline = 5 * time.Minute
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(15*time.Minute), offline, true); !due {
		t.Fatal("after_offline knob ignored")
	}
	unregistered := ContributorPresence{}
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(14*time.Minute), unregistered, true); due {
		t.Fatal("unregistered author adopted before 15m")
	}
	if due, reason := adoptionDue(rec, "aaa", p, now.Add(15*time.Minute), unregistered, true); !due || reason != AdoptReasonUnregistered {
		t.Fatalf("unregistered author: %v %q", due, reason)
	}
	// Records nudged before FirstNudgeAt existed carry only LastNudgeAt;
	// the grace counts from there instead of never becoming due.
	legacy := stagelabels.Record{NudgeHead: "aaa", NudgeCount: 1, LastNudgeAt: now}
	if due, _ := adoptionDue(legacy, "aaa", StageLabelPolicy{}, now.Add(24*time.Minute), offline, true); due {
		t.Fatal("legacy record adopted before the offline wait")
	}
	if due, reason := adoptionDue(legacy, "aaa", StageLabelPolicy{}, now.Add(40*time.Minute), offline, true); !due || reason != AdoptReasonOffline {
		t.Fatalf("legacy record not adopted: %v %q", due, reason)
	}
	if due, _ := adoptionDue(stagelabels.Record{NudgeHead: "aaa", NudgeCount: 1}, "aaa", StageLabelPolicy{}, now.Add(24*time.Hour), offline, true); due {
		t.Fatal("record with no nudge time adopted")
	}
	p.AdoptAfterUnregistered = time.Hour
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(time.Hour), unregistered, true); !due {
		t.Fatal("after_unregistered knob ignored")
	}
	rec.AdoptedHead = "aaa"
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(3*time.Hour), none, false); due {
		t.Fatal("due twice for one head")
	}
	rec.AdoptedHead = ""
	rec.AdoptMode = stagelabels.AdoptModeInPlace
	if due, _ := adoptionDue(rec, "ccc", p, now.Add(3*time.Hour), none, false); due {
		t.Fatal("due while adopted in place")
	}
	if got := adoptionCloses("Closes #12\nfixes acme/other#3\nnot closes #9 here\n"); len(got) != 2 || got[0] != "Closes #12" || got[1] != "fixes acme/other#3" {
		t.Fatalf("closes lines %q", got)
	}
}

// Online author: nudge + 2h grace; nothing is adopted before the grace, then
// the hive supersedes the PR, comments once and moves it to 074.
func TestAdoptionOnlineGraceThenAdopt(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "spawn", Online: true}, adoptPolicy())
	defer done()

	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	if len(nudgeComments(h.api)) != 1 || len(adoptionComments(h.api)) != 0 || len(h.adopt.pulls) != 0 {
		t.Fatalf("first cycle: comments=%v pulls=%d", h.api.comments, len(h.adopt.pulls))
	}
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}

	// Just under the online grace: still the contributor's.
	h.api.reset()
	h.clock.advance(2*time.Hour - time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if h.api.writes() != 0 || len(h.adopt.pulls) != 0 {
		t.Fatalf("adopted before the online grace: added=%v pulls=%d", h.api.added, len(h.adopt.pulls))
	}

	// Grace elapsed: branch pushed at the contributor head, App PR opened,
	// one comment, stage flips to 074.
	h.clock.advance(time.Minute)
	res := h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if got := h.adopt.refs["refs/heads/hive/adopt-7"]; got != "aaa" {
		t.Fatalf("adopt branch sha=%q refs=%v", got, h.adopt.refs)
	}
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("pulls=%v", h.adopt.pulls)
	}
	pull := h.adopt.pulls[0]
	if pull["head"] != "hive/adopt-7" || pull["base"] != "v5" || pull["title"] != "fix: widget" {
		t.Fatalf("superseding PR=%v", pull)
	}
	body, _ := pull["body"].(string)
	for _, want := range []string{
		"Supersedes #7 (adopted after CI went red and the contributor was unavailable; original commits by @mallory preserved)",
		"Closes #12", "fixes #13", "fork `mallory/widget`", "rebase this branch on `v5`", "`check-failing:unit`", "Do not open another PR",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("superseding body missing %q:\n%s", want, body)
		}
	}
	comments := adoptionComments(h.api)
	if len(comments) != 1 || !strings.Contains(comments[0], "by=acme-hive reason=grace-elapsed mode=supersede superseded_by=90 head=aaa") || !strings.Contains(comments[0], "@mallory") ||
		!strings.Contains(comments[0], "red for 2h") || !strings.Contains(comments[0], "relay nudge went unanswered") || !strings.Contains(comments[0], "#90") {
		t.Fatalf("adoption comment: %v", h.api.comments)
	}
	if res.Comments != 1 {
		t.Fatalf("res=%+v", res)
	}
	// The stage flips on the next cycle (the record is adopted at this head).
	h.api.reset()
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	added := h.addedLabels()
	if len(added) != 1 || added[0] != "stage/074-adopted-by-hive" || len(h.api.removed) != 1 || h.api.removed[0] != "stage/072-returned-to-contributor" {
		t.Fatalf("074 transition: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if len(h.sink.resolved) != 1 || h.sink.resolved[0] != 7 {
		t.Fatalf("fix queue not resolved on adoption: %v", h.sink.resolved)
	}
	// Steady state: no churn, one adoption per head, superseder polled at most every 10m.
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if h.api.writes() != 0 || len(h.adopt.pulls) != 1 {
		t.Fatalf("churn after adoption: added=%v removed=%v comments=%v pulls=%d", h.api.added, h.api.removed, h.api.comments, len(h.adopt.pulls))
	}
}

// Offline / external author: comment-only nudge, 1h grace, then 074 and the
// superseding PR. Once the superseder merges the original is closed.
func TestAdoptionOfflineGraceThenSupersederMergedClosesOriginal(t *testing.T) {
	// A sink without Presence: the nudge-time graces apply (the author's
	// `— hive:` trailer still makes the PR a clanker's).
	fake := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "comment"}}
	h, done := newAdoptHarnessWith(t, fake, nudgeOnlySink{fake}, adoptPolicy())
	defer done()

	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(59 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 0 {
		t.Fatal("adopted before the offline grace")
	}
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("offline grace did not adopt: pulls=%d", len(h.adopt.pulls))
	}
	if c := adoptionComments(h.api); len(c) != 1 || !strings.Contains(c[0], "red for 1h") || !strings.Contains(c[0], "no relay session was reachable") {
		t.Fatalf("adoption comment: %v", h.api.comments)
	}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/074-adopted-by-hive" {
		t.Fatalf("074: %v", h.api.added)
	}

	// Superseder merges: close note + close the original (polled after 10m).
	h.adopt.setSuperseder(90, "closed", true)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	res := h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("original not closed: %v", h.adopt.prEdits)
	}
	if len(h.api.comments) != 1 || !strings.HasPrefix(h.api.comments[0], adoptionCloseMarker) || !strings.Contains(h.api.comments[0], "#90") || res.Comments != 1 {
		t.Fatalf("close note: %v res=%+v", h.api.comments, res)
	}
}

// Superseder closed unmerged: the adoption is forgotten and the PR falls
// back to 072 (a maintainer declined the takeover).
func TestAdoptionSupersederClosedUnmergedReturnsTo072(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	h.adopt.setSuperseder(90, "closed", false)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(h.adopt.prEdits) != 0 {
		t.Fatalf("original closed although superseder was abandoned: %v", h.adopt.prEdits)
	}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/072-returned-to-contributor" {
		t.Fatalf("fallback to 072: added=%v removed=%v", h.api.added, h.api.removed)
	}
}

// A new head from the author before the grace re-arms the nudge and is never
// adopted; a green head leaves 072 entirely.
func TestAdoptionNewHeadFromAuthorNotAdopted(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	// The author is pushing, so their relay is online: the 2h online grace.
	h.sink.presence = &ContributorPresence{Registered: true, Online: true}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(50 * time.Minute)
	h.api.reset()
	// Still red at a new head: fresh nudge, grace restarts.
	h.observe(adoptPR(7, "bbb", labels...), "check-failing:unit")
	if len(nudgeComments(h.api)) != 1 || !strings.Contains(nudgeComments(h.api)[0], "head=bbb n=1") || len(h.adopt.pulls) != 0 {
		t.Fatalf("new head: comments=%v pulls=%d", h.api.comments, len(h.adopt.pulls))
	}
	h.api.reset()
	h.clock.advance(50 * time.Minute) // 100m since first nudge, 50m since the second head's
	h.observe(adoptPR(7, "bbb", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 0 || h.api.writes() != 0 {
		t.Fatalf("adopted a freshly re-nudged head: pulls=%d writes=%d", len(h.adopt.pulls), h.api.writes())
	}
	// Author goes green: back on the normal path, no adoption ever.
	h.api.reset()
	h.clock.advance(3 * time.Hour)
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", adoptPR(7, "ccc", labels...), "contributor-lane:no-verdict", false, nil)
	h.e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	if _, err := h.e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/050-review-pending" || len(h.adopt.pulls) != 0 {
		t.Fatalf("green head: added=%v pulls=%d", h.api.added, len(h.adopt.pulls))
	}
	if len(h.sink.resolved) != 1 {
		t.Fatalf("resolved=%v", h.sink.resolved)
	}
}

// Adoption disabled: the PR stays in 072 past every grace.
func TestAdoptionDisabledStaysReturned(t *testing.T) {
	p := adoptPolicy()
	p.Adoption = false
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.api.reset()
	h.clock.advance(5 * time.Hour)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(h.adopt.pulls) != 0 || len(adoptionComments(h.api)) != 0 {
		t.Fatalf("adopted with adoption off: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
}

// Self-authored (App) PRs are never adopted: they are in 070, not 072.
func TestAdoptionSkipsSelfAuthored(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	rec := stagelabels.Record{NudgeHead: "aaa", NudgeCount: 1, FirstNudgeAt: now.Add(-5 * time.Hour)}
	if due, _ := adoptionDue(rec, "aaa", adoptPolicy(), now, ContributorPresence{}, false); !due {
		t.Fatal("precondition")
	}
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	pr := adoptPR(8, "aaa")
	pr.User.Login = gh.Ptr(testHiveAppBotLogin)
	h.e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "check-failing:unit", false, nil)
	if _, err := h.e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The App's own PR is author/hive even though its body carries the
	// relay-style trailer: the App login wins (authorkind rule 1).
	if added := h.addedLabels(); strings.Join(added, " ") != "author/hive stage/070-fixing" {
		t.Fatalf("self PR stage: %v", h.api.added)
	}
	h.clock.advance(5 * time.Hour)
	h.api.reset()
	pr = adoptPR(8, "aaa", "stage/070-fixing", "author/hive")
	pr.User.Login = gh.Ptr(testHiveAppBotLogin)
	h.e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "check-failing:unit", false, nil)
	if _, err := h.e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.adopt.pulls) != 0 || len(h.api.comments) != 0 {
		t.Fatalf("self PR touched: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
}

// hivecommons/hive#12527: the contributor's relay registered here, answered
// nothing and dropped off. Adoption follows AdoptAfterOffline (15m default)
// counted from its last heartbeat — not the nudge-time graces.
func TestAdoptionRelayOfflineAdoptsAfterOfflineWindow(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	start := h.clock.at
	h.sink.presence = &ContributorPresence{Registered: true, Online: true}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	if len(nudgeComments(h.api)) != 1 {
		t.Fatalf("nudge: %v", h.api.comments)
	}
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}

	// Relay went away 20m after the nudge.
	h.sink.presence = &ContributorPresence{Registered: true, LastSeen: start.Add(20 * time.Minute)}
	h.api.reset()
	h.clock.advance(34 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if h.api.writes() != 0 || len(h.adopt.pulls) != 0 {
		t.Fatalf("adopted before 15m offline: added=%v comments=%v", h.api.added, h.api.comments)
	}
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	comments := adoptionComments(h.api)
	if len(h.adopt.pulls) != 1 || len(comments) != 1 ||
		!strings.Contains(comments[0], "by=acme-hive reason=contributor-offline mode=supersede superseded_by=90 head=aaa") ||
		!strings.Contains(comments[0], "your hive has been offline since the nudge") {
		t.Fatalf("offline adoption: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/074-adopted-by-hive" {
		t.Fatalf("074: %v", h.api.added)
	}
}

// An author with no contributor profile here (another hive posted the PR)
// is adopted after AdoptAfterUnregistered.
func TestAdoptionUnregisteredAuthor(t *testing.T) {
	p := adoptPolicy()
	p.AdoptAfterUnregistered = 90 * time.Minute
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.sink.presence = &ContributorPresence{}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(89 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 0 {
		t.Fatal("adopted before after_unregistered")
	}
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if c := adoptionComments(h.api); len(c) != 1 || !strings.Contains(c[0], "reason=contributor-unregistered") || !strings.Contains(c[0], "no hive of yours is registered here") {
		t.Fatalf("unregistered adoption: %v", h.api.comments)
	}
}

// In-place adoption: the PR allows maintainer edits, so the hive takes over
// the PR's own branch (no superseding PR), flips to 074, keeps the adoption
// across its own pushes and yields when the contributor pushes.
func TestAdoptionInPlaceWhenMaintainerCanModify(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(30 * time.Minute)
	res := h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
		t.Fatalf("superseded instead of adopting in place: pulls=%v refs=%v", h.adopt.pulls, h.adopt.refs)
	}
	comments := adoptionComments(h.api)
	if len(comments) != 1 || res.Comments != 1 ||
		!strings.Contains(comments[0], "by=acme-hive reason=contributor-offline mode=in-place head=aaa") ||
		!strings.Contains(comments[0], "@mallory") || !strings.Contains(comments[0], "stage/074-adopted-by-hive") {
		t.Fatalf("in-place comment: %v", h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !rec.AdoptedInPlace() || rec.AdoptReason != AdoptReasonOffline || rec.AdoptedBy != 0 {
		t.Fatalf("record: %+v", rec)
	}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/074-adopted-by-hive" || len(h.api.removed) != 1 || h.api.removed[0] != "stage/072-returned-to-contributor" {
		t.Fatalf("074 transition: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if len(h.sink.resolved) != 1 || h.sink.resolved[0] != 7 {
		t.Fatalf("fix queue not resolved: %v", h.sink.resolved)
	}
	adopted := []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}

	// The hive's fixer pushes: still red, adoption follows the new head, no churn.
	h.adopt.setCommitAuthor("bbb", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", adopted...), "check-failing:unit")
	if h.api.writes() != 0 {
		t.Fatalf("churn after hive push: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() || rec.AdoptedHead != "bbb" {
		t.Fatalf("adoption did not follow the hive's head: %+v", rec)
	}

	// A fixer agent pushes with a bare git identity (no GitHub login): the
	// hive keeps ownership only when it recorded this exact pushed head.
	h.adopt.setCommitAuthor("bb2", "git:scanner")
	h.e.stageStoreOrShared().MarkHivePushedHead(stageKey("acme", "widget", 7), "bb2")
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bb2", adopted...), "check-failing:unit")
	if h.api.writes() != 0 {
		t.Fatalf("churn after fixer push: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() || rec.AdoptedHead != "bb2" {
		t.Fatalf("adoption did not follow the fixer's head: %+v", rec)
	}

	// The contributor is back and pushes: one yield note, adoption forgotten,
	// the ordinary 072 flow (fresh nudge for the new head) resumes.
	h.adopt.setCommitAuthor("ccc", "mallory")
	h.api.reset()
	h.clock.advance(time.Minute)
	res = h.observe(adoptPR(7, "ccc", adopted...), "check-failing:unit")
	var yields int
	for _, c := range h.api.comments {
		if strings.HasPrefix(c, AdoptionYieldMarker) {
			yields++
		}
	}
	if yields != 1 || len(nudgeComments(h.api)) != 1 || res.Comments != 2 {
		t.Fatalf("yield: comments=%v res=%+v", h.api.comments, res)
	}
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/072-returned-to-contributor" {
		t.Fatalf("back to 072: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); rec.AdoptedInPlace() || rec.YieldedHead != "ccc" {
		t.Fatalf("adoption not released: %+v", rec)
	}
	// Same head again: the yield note is not repeated.
	h.api.reset()
	h.observe(adoptPR(7, "ccc", labels...), "check-failing:unit")
	if len(h.api.comments) != 0 {
		t.Fatalf("yield repeated: %v", h.api.comments)
	}
}

func TestAdoptionYieldsOnForgeableCommitIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity string
	}{
		{name: "forged scanner author name", identity: "git:scanner"},
		{name: "forged app login in git name", identity: "git:" + testHiveAppBotLogin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			h.adopt.allowMaintainerEdits(7, true)
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			h.clock.advance(30 * time.Minute)
			h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
			h.e.stageStoreOrShared().MarkHivePushedHead(stageKey("acme", "widget", 7), "prior-hive-head")
			h.adopt.setCommitAuthor("bbb", tc.identity)

			h.api.reset()
			h.clock.advance(time.Minute)
			res := h.observe(adoptPR(7, "bbb", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
			if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); rec.AdoptedInPlace() || rec.YieldedHead != "bbb" {
				t.Fatalf("forgeable identity kept adoption: %+v", rec)
			}
			var yields int
			for _, c := range h.api.comments {
				if strings.HasPrefix(c, AdoptionYieldMarker) {
					yields++
				}
			}
			if yields != 1 || res.Comments == 0 {
				t.Fatalf("yield comments=%d res=%+v comments=%v", yields, res, h.api.comments)
			}
		})
	}
}

func TestAdoptionKeepsOnAppLinkedOrRecordedHead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*adoptHarness, string)
	}{
		{name: "app committer login", setup: func(h *adoptHarness, head string) {
			h.adopt.setVerifiedCommitAuthor(head, testHiveAppBotLogin)
		}},
		{name: "recorded hive-pushed head", setup: func(h *adoptHarness, head string) {
			h.adopt.setCommitAuthor(head, "git:scanner")
			h.e.stageStoreOrShared().MarkHivePushedHead(stageKey("acme", "widget", 7), head)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			h.adopt.allowMaintainerEdits(7, true)
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			h.clock.advance(30 * time.Minute)
			h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
			tc.setup(h, "bbb")

			h.api.reset()
			h.clock.advance(time.Minute)
			h.observe(adoptPR(7, "bbb", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
			if h.api.writes() != 0 {
				t.Fatalf("churn after hive-evidenced push: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
			}
			if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() || rec.AdoptedHead != "bbb" {
				t.Fatalf("adoption did not follow hive-evidenced head: %+v", rec)
			}
		})
	}
}

func TestAdoptionKeepsWhenHeadCommitLookupFails(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "missing", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if h.api.writes() != 0 {
		t.Fatalf("churn after unknown pusher: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() || rec.AdoptedHead != "missing" {
		t.Fatalf("adoption did not stay on unknown pusher: %+v", rec)
	}
}

func TestAdoptionStrictAttributionSafetyValveBeforeRepoEvidence(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
	h.adopt.setCommitAuthor("bbb", "git:scanner")

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if h.api.writes() != 0 {
		t.Fatalf("safety valve churned: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() || rec.AdoptedHead != "bbb" {
		t.Fatalf("safety valve did not keep adoption before repo evidence: %+v", rec)
	}
}

// Adoption is an ownership state, not a CI state (hivecommons/hive#12736):
// once the hive owns a PR in place, its own pushes going pending, green or
// red again all read 074 — never 050/060/072 — until the merge gate takes
// the PR; a contributor push releases the adoption even on a green head.
func TestAdoptionPersistsWhileHiveOwnsThePR(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	returned := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", returned...), "check-failing:unit")
	h.observe(adoptPR(7, "aaa", returned...), "check-failing:unit")
	key := stageKey("acme", "widget", 7)
	if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.AdoptedInPlace() || rec.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("not adopted in place at 074: %+v", rec)
	}
	adopted := []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}
	stageIs := func(want stagelabels.Stage, step string) {
		t.Helper()
		if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != want {
			t.Fatalf("%s: stage %s, want %s (added=%v removed=%v)", step, rec.Stage, want, h.api.added, h.api.removed)
		}
	}

	// The hive's fixer pushes and CI is pending: still 074, nothing written.
	h.adopt.setCommitAuthor("bbb", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", adopted...), "check-pending")
	stageIs(stagelabels.AdoptedByHive, "ci pending")
	if h.api.writes() != 0 {
		t.Fatalf("ci pending churn: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}

	// Green, awaiting a verdict (the lane says 050): still 074.
	h.api.reset()
	h.observe(adoptPR(7, "bbb", adopted...), contributorLaneReasonNoVerdict)
	stageIs(stagelabels.AdoptedByHive, "green no-verdict")
	if h.api.writes() != 0 {
		t.Fatalf("green churn: added=%v removed=%v", h.api.added, h.api.removed)
	}

	// Approved but CI still running (the lane says 060): still 074.
	h.api.reset()
	h.e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"})
	h.observe(adoptPR(7, "bbb", adopted...), "check-pending")
	stageIs(stagelabels.AdoptedByHive, "approved ci pending")

	// Red again: 074, not 072 — no nudge to the contributor the hive stood in for.
	h.api.reset()
	h.e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{})
	h.observe(adoptPR(7, "bbb", adopted...), "check-failing:unit")
	stageIs(stagelabels.AdoptedByHive, "red again")
	if h.api.writes() != 0 || len(nudgeComments(h.api)) != 0 {
		t.Fatalf("red churn: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}

	// The label drifted to 050 (the bug this test pins): the reconciler
	// writes 074 back and removes 050.
	h.api.reset()
	h.observe(adoptPR(7, "bbb", "stage/050-review-pending", "role/contributor", "author/clanker"), contributorLaneReasonNoVerdict)
	stageIs(stagelabels.AdoptedByHive, "drifted label")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/074-adopted-by-hive" || len(h.api.removed) != 1 || h.api.removed[0] != "stage/050-review-pending" {
		t.Fatalf("heal 050 → 074: added=%v removed=%v", h.api.added, h.api.removed)
	}

	// The merge gate takes it: approved and soaking is 100, the adoption
	// record stays (it is released by the yield / close paths, not here).
	h.api.reset()
	h.e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"})
	h.observe(adoptPR(7, "bbb", adopted...), contributorLaneReasonSoaking+"(5m)")
	stageIs(stagelabels.MergeQueue, "merge gate")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/100-merge-queue" || len(h.api.removed) != 1 || h.api.removed[0] != "stage/074-adopted-by-hive" {
		t.Fatalf("074 → 100: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.AdoptedInPlace() {
		t.Fatalf("adoption dropped at the merge gate: %+v", rec)
	}

	// The contributor pushes over a green head: the hive yields even though
	// the lane never saw red — one yield note, then the normal pipeline.
	h.adopt.setCommitAuthor("ccc", "mallory")
	h.api.reset()
	h.e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{})
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "ccc", "stage/100-merge-queue", "role/contributor", "author/clanker"), contributorLaneReasonNoVerdict)
	stageIs(stagelabels.ReviewPending, "released")
	var yields int
	for _, c := range h.api.comments {
		if strings.HasPrefix(c, AdoptionYieldMarker) {
			yields++
		}
	}
	if yields != 1 {
		t.Fatalf("yield on green head: comments=%v", h.api.comments)
	}
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/050-review-pending" {
		t.Fatalf("released → 050: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.AdoptedInPlace() || rec.YieldedHead != "ccc" {
		t.Fatalf("adoption not released: %+v", rec)
	}
}

// A superseding adoption is bound to the adopted head: the original going
// green at that head stays 074 (the superseder carries it); a new head from
// the author is the author's again.
func TestAdoptionSupersedePersistsAtAdoptedHead(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.sink.presence = &ContributorPresence{Registered: true, Online: false}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	returned := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(2 * time.Hour)
	h.observe(adoptPR(7, "aaa", returned...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("not superseded: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
	key := stageKey("acme", "widget", 7)
	h.api.reset()
	h.observe(adoptPR(7, "aaa", "stage/050-review-pending", "role/contributor", "author/clanker"), contributorLaneReasonNoVerdict)
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("green at adopted head: stage %s", rec.Stage)
	}
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/074-adopted-by-hive" || len(h.api.removed) != 1 || h.api.removed[0] != "stage/050-review-pending" {
		t.Fatalf("heal 050 → 074: added=%v removed=%v", h.api.added, h.api.removed)
	}
	h.api.reset()
	h.observe(adoptPR(7, "bbb", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), contributorLaneReasonNoVerdict)
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.ReviewPending {
		t.Fatalf("author's new head: stage %s", rec.Stage)
	}
}

// In-place requested but the fork forbids maintainer edits: supersede.
func TestAdoptionInPlaceFallsBackToSupersede(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, false)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("expected superseding PR: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
	if c := adoptionComments(h.api); len(c) != 1 || !strings.Contains(c[0], "mode=supersede") {
		t.Fatalf("comment: %v", h.api.comments)
	}
}

// hold / do-not-merge parks the PR: adoption is due but never performed.
func TestAdoptionBlockedByHold(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.api.reset()
	h.clock.advance(5 * time.Hour)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker", "hold"), "check-failing:unit")
	if len(h.adopt.pulls) != 0 || len(adoptionComments(h.api)) != 0 {
		t.Fatalf("adopted a held PR: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
	if got := adoptionBlocked([]string{"size/L", "do-not-merge/hold"}); got != "do-not-merge/hold" {
		t.Fatalf("adoptionBlocked=%q", got)
	}
}

// The 072 interrupt is for author/clanker only: a human's PR (no hive
// trailer, no profile) sits at 071 needs-author and a hive-authored PR (the
// App's own login) at 070 fixing, with neither nudge nor adoption.
func TestAdoptionAndNudgeOnlyForClankers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  string
		mutate func(pr *gh.PullRequest)
	}{
		{"human", "stage/071-needs-author", func(pr *gh.PullRequest) { pr.Body = gh.Ptr("Fixes the widget.\n") }},
		{"hive", "stage/070-fixing", func(pr *gh.PullRequest) {
			// The App's own PR; an agent/<lane> stamp on a PR the App did
			// not author is a stray (#12545) and is not hive evidence.
			pr.User.Login = gh.Ptr(testHiveAppBotLogin)
			pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr("agent/scanner")})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
			defer done()
			h.sink.presence = &ContributorPresence{}
			pr := adoptPR(7, "aaa")
			tc.mutate(pr)
			h.observe(pr, "check-failing:unit")
			if added := strings.Join(h.addedLabels(), " "); !strings.Contains(added, tc.stage) || strings.Contains(added, "stage/072-returned-to-contributor") {
				t.Fatalf("%s: %v", tc.stage, h.api.added)
			}
			if len(h.api.comments) != 0 || len(h.sink.nudges) != 0 {
				t.Fatalf("interrupt fired for %s author: comments=%v nudges=%d", tc.name, h.api.comments, len(h.sink.nudges))
			}
			h.api.reset()
			h.clock.advance(6 * time.Hour)
			pr = adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker")
			tc.mutate(pr)
			h.observe(pr, "check-failing:unit")
			if len(h.api.comments) != 0 || len(h.adopt.pulls) != 0 || strings.Contains(strings.Join(h.addedLabels(), " "), "stage/074") {
				t.Fatalf("adopted a %s PR: added=%v comments=%v pulls=%d", tc.name, h.api.added, h.api.comments, len(h.adopt.pulls))
			}
		})
	}
}
