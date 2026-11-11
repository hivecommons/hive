package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// bpdItem is one issue or pull request the Depends-on fake serves.
type bpdItem struct {
	isPR     bool
	state    string
	merged   bool
	draft    bool
	body     string
	author   string
	labels   []string
	comments []string
	events   []map[string]any
}

// bpdFake serves the PR listing, items, label events and comments the
// blocked PR sweep reads, and records its writes. Label writes add the App's
// labeled/unlabeled events, as GitHub would.
type bpdFake struct {
	mu      sync.Mutex
	items   map[string]*bpdItem // "owner/repo#N"
	posted  []string            // "owner/repo#N: body"
	added   []string            // "owner/repo#N"
	removed []string            // "owner/repo#N"
	tick    int
}

func newBPDFake() *bpdFake { return &bpdFake{items: map[string]*bpdItem{}} }

func (f *bpdFake) put(ref string, it *bpdItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it.author == "" {
		it.author = "alice"
	}
	f.items[ref] = it
}

// event appends a `blocked` label event on ref by actor.
func (f *bpdFake) event(ref, kind, actor string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventLocked(ref, kind, actor)
}

func (f *bpdFake) eventLocked(ref, kind, actor string) {
	f.tick++
	it := f.items[ref]
	it.events = append(it.events, map[string]any{
		"event": kind, "created_at": fmt.Sprintf("2026-11-10T10:%02d:%02dZ", f.tick/60, f.tick%60),
		"actor": map[string]string{"login": actor}, "label": map[string]string{"name": BlockedLabel},
	})
}

func (f *bpdFake) item(ref string) *bpdItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.items[ref]
}

func (f *bpdFake) issueJSON(owner, repo string, n int, it *bpdItem) map[string]any {
	labels := make([]map[string]string, 0, len(it.labels))
	for _, l := range it.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	out := map[string]any{
		"number": n, "state": it.state, "draft": it.draft, "body": it.body,
		"user": map[string]string{"login": it.author}, "labels": labels,
		"created_at": "2026-11-01T00:00:00Z",
	}
	if it.isPR {
		pr := map[string]any{"url": fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d", owner, repo, n)}
		if it.merged {
			pr["merged_at"] = "2026-11-09T00:00:00Z"
		}
		out["pull_request"] = pr
	}
	return out
}

func (f *bpdFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
		if len(parts) < 3 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		owner, repo := parts[0], parts[1]
		if parts[2] == "pulls" && len(parts) == 3 && r.Method == http.MethodGet {
			out := []map[string]any{}
			for ref, it := range f.items {
				prefix := owner + "/" + repo + "#"
				if !strings.HasPrefix(ref, prefix) || !it.isPR || it.state != "open" {
					continue
				}
				n, _ := strconv.Atoi(strings.TrimPrefix(ref, prefix))
				out = append(out, f.issueJSON(owner, repo, n, it))
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		if parts[2] != "issues" || len(parts) < 4 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n, _ := strconv.Atoi(parts[3])
		ref := fmt.Sprintf("%s/%s#%d", owner, repo, n)
		it, ok := f.items[ref]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		switch {
		case len(parts) == 4 && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(f.issueJSON(owner, repo, n, it))
		case len(parts) == 5 && parts[4] == "events" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(it.events)
		case len(parts) == 5 && parts[4] == "comments" && r.Method == http.MethodGet:
			out := make([]map[string]any, 0, len(it.comments))
			for i, c := range it.comments {
				out = append(out, map[string]any{"id": i + 1, "body": c, "user": map[string]string{"login": testHiveAppBotLogin}})
			}
			_ = json.NewEncoder(w).Encode(out)
		case len(parts) == 5 && parts[4] == "comments" && r.Method == http.MethodPost:
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			it.comments = append(it.comments, body.Body)
			f.posted = append(f.posted, ref+": "+body.Body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1000 + len(f.posted), "body": body.Body})
		case len(parts) == 5 && parts[4] == "labels" && r.Method == http.MethodPost:
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			for _, l := range labels {
				if !issueHasLabel(it.labels, l) {
					it.labels = append(it.labels, l)
				}
			}
			f.added = append(f.added, ref)
			f.eventLocked(ref, "labeled", testHiveAppBotLogin)
			_, _ = io.WriteString(w, `[]`)
		case len(parts) == 6 && parts[4] == "labels" && r.Method == http.MethodDelete:
			kept := it.labels[:0]
			for _, l := range it.labels {
				if !strings.EqualFold(l, parts[5]) {
					kept = append(kept, l)
				}
			}
			it.labels = kept
			f.removed = append(f.removed, ref)
			f.eventLocked(ref, "unlabeled", testHiveAppBotLogin)
			_, _ = io.WriteString(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *bpdFake) client(t *testing.T) *Client {
	t.Helper()
	c := newTestClient(t, f.server(t), "acme", []string{"widget"})
	c.SetAppBotLogin(testHiveAppBotLogin)
	return c
}

func (f *bpdFake) postedOn(ref string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.posted {
		if body, ok := strings.CutPrefix(p, ref+": "); ok {
			out = append(out, body)
		}
	}
	return out
}

func (f *bpdFake) counts() (posted, added, removed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posted), len(f.added), len(f.removed)
}

func sweepBPD(t *testing.T, c *Client) BlockedPRSweepResult {
	t.Helper()
	res, err := c.SweepBlockedPRDependencies(context.Background(), BlockedSweepOptions{Repos: []string{"acme/widget"}})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return res
}

func bpdOpenPR(body string) *bpdItem { return &bpdItem{isPR: true, state: "open", body: body} }

// A PR whose Depends-on names an open PR in its own repo and one in another
// repo gets `blocked` and one marker comment per target; a second pass
// writes nothing.
func TestSweepBlockedPRDependencies_PlacesSameAndCrossRepoOnce(t *testing.T) {
	f := newBPDFake()
	f.put("acme/widget#10", bpdOpenPR("Adds the client.\n\nDepends-on: #11, acme/gadget#5"))
	f.put("acme/widget#11", bpdOpenPR("the same-repo prerequisite"))
	f.put("acme/gadget#5", bpdOpenPR("the upstream change"))
	c := f.client(t)
	t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 10) })

	res := sweepBPD(t, c)
	if res.Placed != 1 || res.Marked != 2 || res.Errors != 0 {
		t.Fatalf("first pass = %+v, want 1 placed and 2 marked", res)
	}
	if !issueHasLabel(f.item("acme/widget#10").labels, BlockedLabel) {
		t.Fatalf("labels = %v, want blocked", f.item("acme/widget#10").labels)
	}
	posted := f.postedOn("acme/widget#10")
	if len(posted) != 2 || !strings.Contains(posted[0], BlockedOnMarker("acme/gadget", 5)) || !strings.Contains(posted[1], BlockedOnMarker("acme/widget", 11)) {
		t.Fatalf("marker comments = %q", posted)
	}
	if got := BlockedOnSeenReason("acme/widget", 10); got != "blocked on acme/gadget#5" {
		t.Fatalf("BlockedOnSeenReason = %q", got)
	}
	for _, p := range f.postedOn("acme/widget#11") {
		t.Fatalf("the target must not be written: %q", p)
	}

	beforePosted, beforeAdded, _ := f.counts()
	res = sweepBPD(t, c)
	afterPosted, afterAdded, afterRemoved := f.counts()
	if res.Placed+res.Marked+res.Unblocked != 0 || afterPosted != beforePosted || afterAdded != beforeAdded || afterRemoved != 0 {
		t.Fatalf("second pass must be a no-op: %+v posted %d→%d added %d→%d removed %d", res, beforePosted, afterPosted, beforeAdded, afterAdded, afterRemoved)
	}
}

// A target the App cannot read places nothing; a draft, a PR naming only an
// issue or a closed PR, and a PR a person already blocked are left alone.
func TestSweepBlockedPRDependencies_PlacesNothingWhenItShouldNot(t *testing.T) {
	f := newBPDFake()
	f.put("acme/widget#20", bpdOpenPR("Depends-on: other-org/secret#3"))
	draft := bpdOpenPR("Depends-on: #11")
	draft.draft = true
	f.put("acme/widget#21", draft)
	f.put("acme/widget#22", bpdOpenPR("Depends-on: #12, #13"))
	personBlocked := bpdOpenPR("Depends-on: #11")
	personBlocked.labels = []string{BlockedLabel}
	f.put("acme/widget#23", personBlocked)
	f.event("acme/widget#23", "labeled", "maintainer")
	f.put("acme/widget#11", bpdOpenPR("open prerequisite"))
	f.put("acme/widget#12", &bpdItem{state: "open", body: "an issue, not a PR"})
	f.put("acme/widget#13", &bpdItem{isPR: true, state: "closed", merged: true})
	c := f.client(t)

	res := sweepBPD(t, c)
	posted, added, removed := f.counts()
	if res.Placed != 0 || posted != 0 || added != 0 || removed != 0 {
		t.Fatalf("nothing must be written: %+v posted=%d added=%d removed=%d", res, posted, added, removed)
	}
	if res.Errors != 0 {
		t.Fatalf("an unreadable target is logged, not an error: %+v", res)
	}
	key := "acme/widget#20|other-org/secret#3"
	blockedPRUnreadable.Lock()
	logged := blockedPRUnreadable.m[key]
	blockedPRUnreadable.Unlock()
	if !logged {
		t.Fatalf("the unreadable target must be remembered as logged (%s)", key)
	}
}

// A person who removes the hive's `blocked` releases the PR for the targets
// already marked; a newly listed open target re-arms it.
func TestSweepBlockedPRDependencies_PersonReleaseIsRespected(t *testing.T) {
	f := newBPDFake()
	f.put("acme/widget#30", bpdOpenPR("Depends-on: acme/gadget#6"))
	f.put("acme/gadget#6", bpdOpenPR("upstream"))
	f.put("acme/gadget#7", bpdOpenPR("another upstream"))
	c := f.client(t)
	t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 30) })

	if res := sweepBPD(t, c); res.Placed != 1 {
		t.Fatalf("first pass = %+v", res)
	}
	// A maintainer merges it anyway: removes the hive's label.
	f.mu.Lock()
	f.items["acme/widget#30"].labels = nil
	f.eventLocked("acme/widget#30", "unlabeled", "maintainer")
	f.mu.Unlock()

	res := sweepBPD(t, c)
	if res.Placed != 0 || res.Released != 1 || issueHasLabel(f.item("acme/widget#30").labels, BlockedLabel) {
		t.Fatalf("released PR re-blocked for the same refs: %+v labels=%v", res, f.item("acme/widget#30").labels)
	}

	// The author adds a second dependency: a new target re-arms the hold.
	f.mu.Lock()
	f.items["acme/widget#30"].body = "Depends-on: acme/gadget#6\nDepends-on: acme/gadget#7"
	f.mu.Unlock()
	res = sweepBPD(t, c)
	if res.Placed != 1 || res.Marked != 1 || !issueHasLabel(f.item("acme/widget#30").labels, BlockedLabel) {
		t.Fatalf("a new target must re-arm: %+v labels=%v", res, f.item("acme/widget#30").labels)
	}
	posted := f.postedOn("acme/widget#30")
	if last := posted[len(posted)-1]; !strings.Contains(last, BlockedOnMarker("acme/gadget", 7)) {
		t.Fatalf("new marker = %q", last)
	}
}

// blockedPRFixture is a PR the hive already blocked on its marked targets.
func blockedPRFixture(t *testing.T, f *bpdFake, ref, body string, targets ...string) {
	t.Helper()
	it := bpdOpenPR(body)
	it.labels = []string{BlockedLabel}
	for _, target := range targets {
		repo, num, _ := strings.Cut(target, "#")
		n, _ := strconv.Atoi(num)
		it.comments = append(it.comments, blockedPRDependsOnMarkerComment(repo, n))
	}
	f.put(ref, it)
	f.event(ref, "labeled", testHiveAppBotLogin)
}

// The lift: all targets merged → lifted with the note; one still open →
// kept; a target dropped from the body → lifted; closed unmerged → lifted
// with the clause; a person's `blocked` → never lifted.
func TestSweepBlockedPRDependencies_Lift(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		targets    []string
		state      map[string]*bpdItem
		personSet  bool
		wantLift   bool
		wantClause string
	}{
		{name: "all merged", body: "Depends-on: #41, acme/gadget#8", targets: []string{"acme/widget#41", "acme/gadget#8"},
			state:    map[string]*bpdItem{"acme/widget#41": {isPR: true, state: "closed", merged: true}, "acme/gadget#8": {isPR: true, state: "closed", merged: true}},
			wantLift: true, wantClause: "Unblocked: acme/widget#41 merged; acme/gadget#8 merged."},
		{name: "one still open", body: "Depends-on: #41, acme/gadget#8", targets: []string{"acme/widget#41", "acme/gadget#8"},
			state: map[string]*bpdItem{"acme/widget#41": {isPR: true, state: "closed", merged: true}, "acme/gadget#8": {isPR: true, state: "open"}}},
		{name: "target dropped from the body", body: "No dependencies any more.", targets: []string{"acme/gadget#8"},
			state:    map[string]*bpdItem{"acme/gadget#8": {isPR: true, state: "open"}},
			wantLift: true, wantClause: "acme/gadget#8 is no longer listed under `Depends-on:`"},
		{name: "closed unmerged", body: "Depends-on: acme/gadget#8", targets: []string{"acme/gadget#8"},
			state:    map[string]*bpdItem{"acme/gadget#8": {isPR: true, state: "closed"}},
			wantLift: true, wantClause: "acme/gadget#8 closed without merging"},
		{name: "person-applied blocked", body: "Depends-on: acme/gadget#8", targets: []string{"acme/gadget#8"},
			state:     map[string]*bpdItem{"acme/gadget#8": {isPR: true, state: "closed", merged: true}},
			personSet: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBPDFake()
			ref := "acme/widget#" + strconv.Itoa(40+i*100)
			blockedPRFixture(t, f, ref, tc.body, tc.targets...)
			if tc.personSet {
				f.event(ref, "labeled", "maintainer")
			}
			for target, it := range tc.state {
				f.put(target, it)
			}
			c := f.client(t)
			n, _ := strconv.Atoi(strings.TrimPrefix(ref, "acme/widget#"))
			t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", n) })

			res := sweepBPD(t, c)
			blocked := issueHasLabel(f.item(ref).labels, BlockedLabel)
			if tc.wantLift == blocked || (res.Unblocked == 1) != tc.wantLift {
				t.Fatalf("lift=%v: res=%+v labels=%v", tc.wantLift, res, f.item(ref).labels)
			}
			posted := f.postedOn(ref)
			if !tc.wantLift {
				if len(posted) != 0 {
					t.Fatalf("a kept block must post nothing: %q", posted)
				}
				return
			}
			if len(posted) != 1 || !strings.Contains(posted[0], blockedUnblockedMarker) || !strings.Contains(posted[0], tc.wantClause) {
				t.Fatalf("unblocked note = %q, want %q", posted, tc.wantClause)
			}
			// The note is posted once: a second pass finds no label to lift.
			if res := sweepBPD(t, c); res.Unblocked != 0 || len(f.postedOn(ref)) != 1 {
				t.Fatalf("second pass must not lift again: %+v", res)
			}
		})
	}
}

// With the write-back gate off for the repository nothing is written.
func TestSweepBlockedPRDependencies_WritebackGateOffWritesNothing(t *testing.T) {
	f := newBPDFake()
	f.put("acme/widget#50", bpdOpenPR("Depends-on: acme/gadget#9"))
	f.put("acme/gadget#9", bpdOpenPR("upstream"))
	blockedPRFixture(t, f, "acme/widget#51", "Depends-on: acme/gadget#10", "acme/gadget#10")
	f.put("acme/gadget#10", &bpdItem{isPR: true, state: "closed", merged: true})
	c := f.client(t)
	c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"acme/widget": {Mode: writeback.RepoModeOff}}}, writeback.Deps{}, ""))
	t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 50); forgetBlockedOnSeen("acme/widget", 51) })

	res := sweepBPD(t, c)
	posted, added, removed := f.counts()
	if posted != 0 || added != 0 || removed != 0 || res.Suppressed != 2 {
		t.Fatalf("gate off must write nothing: %+v posted=%d added=%d removed=%d", res, posted, added, removed)
	}
}

// Without the App bot login the sweep cannot tell its own label from a
// person's, so it does nothing.
func TestSweepBlockedPRDependencies_NoAppLoginDoesNothing(t *testing.T) {
	f := newBPDFake()
	f.put("acme/widget#60", bpdOpenPR("Depends-on: acme/gadget#11"))
	f.put("acme/gadget#11", bpdOpenPR("upstream"))
	c := newTestClient(t, f.server(t), "acme", []string{"widget"})
	if res := sweepBPD(t, c); res.PRs != 0 {
		t.Fatalf("res = %+v", res)
	}
	if posted, added, _ := f.counts(); posted+added != 0 {
		t.Fatalf("wrote without an App identity: posted=%d added=%d", posted, added)
	}
}

// RecordVerdictBlocker and the PR sweep share one marker writer, so both
// write the marker ParseBlockedOnMarker and the sweep read.
func TestBlockedPRDependsOnMarkerCommentCarriesTheSharedMarker(t *testing.T) {
	body := blockedPRDependsOnMarkerComment("acme/gadget", 5)
	if repo, pr, ok := ParseBlockedOnMarker(body); !ok || repo != "acme/gadget" || pr != 5 {
		t.Fatalf("marker = %s#%d ok=%v in %q", repo, pr, ok, body)
	}
	if strings.Contains(body, "@") {
		t.Fatalf("the marker comment must mention no one: %q", body)
	}
	got := blockedPRMarkedTargets(nil)
	if len(got) != 0 {
		t.Fatalf("no comments, no targets: %v", got)
	}
}

// `hive-open-issue comment --blocked-on-pr owner/repo#n` parks the issue on
// a PR in another repository: the watcher reads that repo, not the issue's.
func TestIssueRequestWatcherBlockedOnPRCrossRepo(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var commentBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/12":
			_, _ = io.WriteString(w, `{"number":12,"state":"open","labels":[]}`)
		case r.Method == "GET" && r.URL.Path == "/repos/acme/gadget/pulls/5":
			_, _ = io.WriteString(w, `{"number":5,"state":"open"}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/12/comments":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/12/comments":
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			commentBody = payload.Body
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":9}`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/12/labels":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = io.WriteString(w, `{"name":"blocked"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)
	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 12, Agent: "scanner", BlockedOnPR: 5, BlockedOnPRRepo: "acme/gadget",
		Body: "Waits on the upstream change.",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Fatalf("request not consumed (stat err=%v); calls=%v", err, calls)
	}
	mu.Lock()
	defer mu.Unlock()
	if repo, pr, ok := ParseBlockedOnMarker(commentBody); !ok || repo != "acme/gadget" || pr != 5 {
		t.Fatalf("marker = %s#%d ok=%v in %q (calls=%v)", repo, pr, ok, commentBody, calls)
	}
}

// The human-merge-path `hold` (#17609, #17622) and the Depends-on `blocked`
// are separate labels with separate lifts: a hive `blocked` and its marker
// comment are not another source of the `hold`, so the hold still lifts when
// its own reason goes, and the lanes keep refusing the PR on `blocked`.
func TestOtherHoldSourcesForPRIgnoresDependsOnBlock(t *testing.T) {
	app := &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}
	comments := []*gh.IssueComment{
		{Body: gh.Ptr(HumanMergePathComment([]string{".github/workflows/release.yml"})), User: app},
		{Body: gh.Ptr(blockedPRDependsOnMarkerComment("acme/gadget", 5)), User: app},
	}
	_, c := newHoldSourceTest(t)
	if got := c.OtherHoldSourcesForPR("acme/widget", 42, []string{"hold", BlockedLabel}, comments); len(got) != 0 {
		t.Fatalf("other hold sources = %v, want none: `blocked` is not a hold source", got)
	}
}
