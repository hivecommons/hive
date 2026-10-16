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
}

func newAdoptAPI() *adoptAPI {
	return &adoptAPI{refs: map[string]string{}, nextPR: 90, superseders: map[int]map[string]any{}}
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
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/pulls/"):
		var n int
		_, _ = sscanInt(strings.TrimPrefix(path, "/repos/acme/widget/pulls/"), &n)
		payload, ok := a.superseders[n]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		_ = json.NewEncoder(w).Encode(payload)
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

func (a *adoptAPI) setSuperseder(n int, state string, merged bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.superseders[n] = map[string]any{"number": n, "state": state, "merged": merged}
}

func adoptPR(number int, head string, labels ...string) *gh.PullRequest {
	pr := nudgePR(number, head, labels...)
	pr.Base = &gh.PullRequestBranch{Ref: gh.Ptr("v5")}
	pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("mallory/widget")}
	pr.Body = gh.Ptr("Fixes the widget.\n\nCloses #12\nfixes #13\n")
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
	api, closeAPI := seededStageAPI(t)
	adopt := newAdoptAPI()
	api.extra = adopt.handle
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: outcome}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, policy)
	return &adoptHarness{t: t, api: api, adopt: adopt, clock: clock, sink: sink, e: e}, closeAPI
}

func (h *adoptHarness) observe(pr *gh.PullRequest, reason string) StageLabelResult {
	h.t.Helper()
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
	h.e.noteStageAuthor("acme", "widget", pr.GetNumber(), stagelabels.AuthorContributor)
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
	return StageLabelPolicy{Enabled: true, ContributorNudge: true, Adoption: true, GraceOnline: 2 * time.Hour, GraceOffline: time.Hour}
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
	if adoptionDue(rec, "aaa", p, now.Add(59*time.Minute)) {
		t.Fatal("due before grace")
	}
	if !adoptionDue(rec, "aaa", p, now.Add(time.Hour)) {
		t.Fatal("not due at grace")
	}
	if adoptionDue(rec, "bbb", p, now.Add(2*time.Hour)) {
		t.Fatal("due for a head that was never nudged")
	}
	rec.AdoptedHead = "aaa"
	if adoptionDue(rec, "aaa", p, now.Add(3*time.Hour)) {
		t.Fatal("due twice for one head")
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
	labels := []string{"stage/072-returned-to-contributor", "author/contributor"}

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
	if len(comments) != 1 || !strings.Contains(comments[0], "by=90 head=aaa") || !strings.Contains(comments[0], "@mallory") ||
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
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "author/contributor"), "check-failing:unit")
	if h.api.writes() != 0 || len(h.adopt.pulls) != 1 {
		t.Fatalf("churn after adoption: added=%v removed=%v comments=%v pulls=%d", h.api.added, h.api.removed, h.api.comments, len(h.adopt.pulls))
	}
}

// Offline / external author: comment-only nudge, 1h grace, then 074 and the
// superseding PR. Once the superseder merges the original is closed.
func TestAdoptionOfflineGraceThenSupersederMergedClosesOriginal(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()

	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "author/contributor"}
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
	res := h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "author/contributor"), "check-failing:unit")
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
	labels := []string{"stage/072-returned-to-contributor", "author/contributor"}
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	h.adopt.setSuperseder(90, "closed", false)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "author/contributor"), "check-failing:unit")
	if len(h.adopt.prEdits) != 0 {
		t.Fatalf("original closed although superseder was abandoned: %v", h.adopt.prEdits)
	}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "author/contributor"), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/072-returned-to-contributor" {
		t.Fatalf("fallback to 072: added=%v removed=%v", h.api.added, h.api.removed)
	}
}

// A new head from the author before the grace re-arms the nudge and is never
// adopted; a green head leaves 072 entirely.
func TestAdoptionNewHeadFromAuthorNotAdopted(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "author/contributor"}
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
	h.e.noteStageAuthor("acme", "widget", 7, stagelabels.AuthorContributor)
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
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "author/contributor"), "check-failing:unit")
	if len(h.adopt.pulls) != 0 || len(adoptionComments(h.api)) != 0 {
		t.Fatalf("adopted with adoption off: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
}

// Self-authored (App) PRs are never adopted: they are in 070, not 072.
func TestAdoptionSkipsSelfAuthored(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	rec := stagelabels.Record{NudgeHead: "aaa", NudgeCount: 1, FirstNudgeAt: now.Add(-5 * time.Hour)}
	if !adoptionDue(rec, "aaa", adoptPolicy(), now) {
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
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/070-fixing" {
		t.Fatalf("self PR stage: %v", h.api.added)
	}
	h.clock.advance(5 * time.Hour)
	h.api.reset()
	h.e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", adoptPR(8, "aaa", "stage/070-fixing"), "check-failing:unit", false, nil)
	if _, err := h.e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.adopt.pulls) != 0 || len(h.api.comments) != 0 {
		t.Fatalf("self PR touched: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
}
