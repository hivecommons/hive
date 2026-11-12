package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	// commentBy names, by index into comments, the comments someone other
	// than the App bot wrote: {login, author_association}.
	commentBy map[int][2]string
	// commentsStatus, when set, is the HTTP status the comment listing
	// answers with instead of the comments.
	commentsStatus int
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

// commentAs appends a comment on ref written by login, whose
// author_association is association.
func (f *bpdFake) commentAs(ref, body, login, association string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.items[ref]
	if it.commentBy == nil {
		it.commentBy = map[int][2]string{}
	}
	it.commentBy[len(it.comments)] = [2]string{login, association}
	it.comments = append(it.comments, body)
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
			if it.commentsStatus != 0 {
				w.WriteHeader(it.commentsStatus)
				_, _ = io.WriteString(w, `{"message":"unavailable"}`)
				return
			}
			out := make([]map[string]any, 0, len(it.comments))
			for i, c := range it.comments {
				cm := map[string]any{"id": i + 1, "body": c, "user": map[string]string{"login": testHiveAppBotLogin}}
				if by, ok := it.commentBy[i]; ok {
					cm["user"] = map[string]string{"login": by[0]}
					cm["author_association"] = by[1]
				}
				out = append(out, cm)
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

// bpdSuccessorTodayClause is the clause the lift note has always carried for
// a target that closed without merging.
const bpdSuccessorTodayClause = "acme/widget#71 closed without merging, so check that this PR still makes sense without it"

// bpdClosedPR is a pull request closed without merging.
func bpdClosedPR() *bpdItem { return &bpdItem{isPR: true, state: "closed"} }

// bpdSuperseded makes ref a pull request closed without merging whose
// maintainer said "Superseded by #next".
func bpdSuperseded(f *bpdFake, ref string, next int) {
	f.put(ref, bpdClosedPR())
	f.commentAs(ref, fmt.Sprintf("Superseded by #%d.", next), "maintainer", "MEMBER")
}

// A maintainer re-does the pull request a PR depends on: the target closes
// unmerged with "Superseded by #72". The hold moves to #72 with one comment,
// stays there on later passes without another write, and lifts when #72
// merges with a note naming both. Before, the first pass lifted the hold.
func TestSweepBlockedPRDependencies_FollowsSuccessorUntilItMerges(t *testing.T) {
	f := newBPDFake()
	const body = "Adds the client.\n\nDepends-on: #71"
	blockedPRFixture(t, f, "acme/widget#70", body, "acme/widget#71")
	bpdSuperseded(f, "acme/widget#71", 72)
	f.put("acme/widget#72", bpdOpenPR("the maintainer's redo"))
	c := f.client(t)
	t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 70) })

	res := sweepBPD(t, c)
	if res.Unblocked != 0 || res.Marked != 1 || res.Errors != 0 || !issueHasLabel(f.item("acme/widget#70").labels, BlockedLabel) {
		t.Fatalf("first pass must keep the hold and mark the successor: %+v labels=%v", res, f.item("acme/widget#70").labels)
	}
	posted := f.postedOn("acme/widget#70")
	if len(posted) != 1 {
		t.Fatalf("want one successor comment, got %q", posted)
	}
	for _, want := range []string{
		BlockedOnMarker("acme/widget", 72),
		"<!-- hive:depends-on-successor pr=acme/widget#72 replaces=acme/widget#71 -->",
		"acme/widget#71, which this pull request lists under `Depends-on:`, was not merged.",
		"Update the `Depends-on:` line when convenient.",
	} {
		if !strings.Contains(posted[0], want) {
			t.Fatalf("successor comment lacks %q: %q", want, posted[0])
		}
	}
	if n, ok := ParseSupersededBy(posted[0]); ok {
		t.Fatalf("the successor comment must not read as a supersession of the PR that carries it (parsed #%d): %q", n, posted[0])
	}
	if got := BlockedOnSeenReason("acme/widget", 70); got != "blocked on acme/widget#72" {
		t.Fatalf("BlockedOnSeenReason = %q, want the successor", got)
	}
	if got := f.item("acme/widget#70").body; got != body {
		t.Fatalf("the PR body must never be edited: %q", got)
	}
	if got := f.postedOn("acme/widget#71"); len(got)+len(f.postedOn("acme/widget#72")) != 0 {
		t.Fatalf("neither the target nor its successor may be written: %q", got)
	}

	beforePosted, beforeAdded, beforeRemoved := f.counts()
	res = sweepBPD(t, c)
	afterPosted, afterAdded, afterRemoved := f.counts()
	if res.Placed+res.Marked+res.Unblocked+res.Errors != 0 || afterPosted != beforePosted || afterAdded != beforeAdded || afterRemoved != beforeRemoved {
		t.Fatalf("second pass must write nothing: %+v posted %d→%d added %d→%d removed %d→%d", res, beforePosted, afterPosted, beforeAdded, afterAdded, beforeRemoved, afterRemoved)
	}
	if !issueHasLabel(f.item("acme/widget#70").labels, BlockedLabel) {
		t.Fatal("second pass must still hold on the successor")
	}

	f.mu.Lock()
	f.items["acme/widget#72"].state, f.items["acme/widget#72"].merged = "closed", true
	f.mu.Unlock()
	res = sweepBPD(t, c)
	if res.Unblocked != 1 || issueHasLabel(f.item("acme/widget#70").labels, BlockedLabel) {
		t.Fatalf("the successor merged, so the hold must lift: %+v labels=%v", res, f.item("acme/widget#70").labels)
	}
	posted = f.postedOn("acme/widget#70")
	note := posted[len(posted)-1]
	for _, want := range []string{blockedUnblockedMarker, "Unblocked: acme/widget#71 was not merged and was replaced by acme/widget#72; acme/widget#72 merged."} {
		if !strings.Contains(note, want) {
			t.Fatalf("lift note lacks %q: %q", want, note)
		}
	}
}

// Every statement the successor lookup trusts moves the hold, and a chain is
// followed to the open pull request at its end, up to three replacements
// away (the limit followSupersededChain applies for an issue blocked on a
// PR).
func TestSweepBlockedPRDependencies_HoldsOnDeclaredSuccessor(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *bpdFake)
		want  int
	}{
		{name: "collaborator comment", want: 72, setup: func(f *bpdFake) {
			f.put("acme/widget#71", bpdClosedPR())
			f.commentAs("acme/widget#71", "superseded by #72", "helper", "COLLABORATOR")
			f.put("acme/widget#72", bpdOpenPR("redo"))
		}},
		{name: "the closed target's author", want: 72, setup: func(f *bpdFake) {
			f.put("acme/widget#71", bpdClosedPR())
			f.commentAs("acme/widget#71", "Superseded by #72.", "alice", "NONE")
			f.put("acme/widget#72", bpdOpenPR("redo"))
		}},
		{name: "the closed target's own body", want: 72, setup: func(f *bpdFake) {
			closed := bpdClosedPR()
			closed.body = "Superseded by #72."
			f.put("acme/widget#71", closed)
			f.put("acme/widget#72", bpdOpenPR("redo"))
		}},
		{name: "the hive's adoption marker", want: 72, setup: func(f *bpdFake) {
			closed := bpdClosedPR()
			closed.comments = []string{"<!-- hive:adopted by=hive reason=offline mode=supersede superseded_by=72 head=abc -->"}
			f.put("acme/widget#71", closed)
			f.put("acme/widget#72", bpdOpenPR("the hive's copy"))
		}},
		{name: "newest trusted statement wins over an older one", want: 73, setup: func(f *bpdFake) {
			f.put("acme/widget#71", bpdClosedPR())
			f.commentAs("acme/widget#71", "Superseded by #72.", "maintainer", "MEMBER")
			f.commentAs("acme/widget#71", "Superseded by #73.", "maintainer", "OWNER")
			f.commentAs("acme/widget#71", "Superseded by #99.", "mallory", "NONE")
			f.put("acme/widget#72", bpdOpenPR("first redo"))
			f.put("acme/widget#73", bpdOpenPR("second redo"))
			f.put("acme/widget#99", bpdOpenPR("a stranger's pull request"))
		}},
		{name: "chain of two", want: 73, setup: func(f *bpdFake) {
			bpdSuperseded(f, "acme/widget#71", 72)
			bpdSuperseded(f, "acme/widget#72", 73)
			f.put("acme/widget#73", bpdOpenPR("redo of the redo"))
		}},
		{name: "chain at the hop limit", want: 74, setup: func(f *bpdFake) {
			bpdSuperseded(f, "acme/widget#71", 72)
			bpdSuperseded(f, "acme/widget#72", 73)
			bpdSuperseded(f, "acme/widget#73", 74)
			f.put("acme/widget#74", bpdOpenPR("third redo"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBPDFake()
			blockedPRFixture(t, f, "acme/widget#70", "Depends-on: #71", "acme/widget#71")
			tc.setup(f)
			c := f.client(t)
			t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 70) })

			res := sweepBPD(t, c)
			if res.Unblocked != 0 || res.Marked != 1 || res.Errors != 0 || !issueHasLabel(f.item("acme/widget#70").labels, BlockedLabel) {
				t.Fatalf("the hold must move to #%d: %+v labels=%v", tc.want, res, f.item("acme/widget#70").labels)
			}
			posted := f.postedOn("acme/widget#70")
			link := fmt.Sprintf("<!-- hive:depends-on-successor pr=acme/widget#%d replaces=acme/widget#71 -->", tc.want)
			if len(posted) != 1 || !strings.Contains(posted[0], BlockedOnMarker("acme/widget", tc.want)) || !strings.Contains(posted[0], link) {
				t.Fatalf("successor comment = %q, want the marker and link for #%d", posted, tc.want)
			}
			if res := sweepBPD(t, c); res.Marked+res.Unblocked != 0 || len(f.postedOn("acme/widget#70")) != 1 {
				t.Fatalf("second pass must write nothing: %+v posted=%q", res, f.postedOn("acme/widget#70"))
			}
		})
	}
}

// Whatever does not name an open pull request through a trusted statement
// releases the hold exactly as before, with the note it always had: a
// stranger's line, a successor that merged or closed, an issue, a missing
// number, the held PR itself, a chain longer than the hop limit, and the
// hive's own bookkeeping about a different blocker.
func TestSweepBlockedPRDependencies_ReleasesAsBeforeWithoutAnOpenSuccessor(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *bpdFake)
	}{
		{name: "no statement at all", setup: func(f *bpdFake) {
			f.put("acme/widget#71", bpdClosedPR())
		}},
		{name: "a commenter with no association who is not the author", setup: func(f *bpdFake) {
			f.put("acme/widget#71", bpdClosedPR())
			f.commentAs("acme/widget#71", "Superseded by #72.", "mallory", "NONE")
			f.put("acme/widget#72", bpdOpenPR("a stranger's pull request"))
		}},
		{name: "a past contributor who is not the author", setup: func(f *bpdFake) {
			f.put("acme/widget#71", bpdClosedPR())
			f.commentAs("acme/widget#71", "Superseded by #72.", "drive-by", "CONTRIBUTOR")
			f.put("acme/widget#72", bpdOpenPR("someone else's pull request"))
		}},
		{name: "the successor already merged", setup: func(f *bpdFake) {
			bpdSuperseded(f, "acme/widget#71", 72)
			f.put("acme/widget#72", &bpdItem{isPR: true, state: "closed", merged: true})
		}},
		{name: "the successor closed with no successor of its own", setup: func(f *bpdFake) {
			bpdSuperseded(f, "acme/widget#71", 72)
			f.put("acme/widget#72", bpdClosedPR())
		}},
		{name: "the successor is an issue", setup: func(f *bpdFake) {
			bpdSuperseded(f, "acme/widget#71", 72)
			f.put("acme/widget#72", &bpdItem{state: "open", body: "an issue, not a pull request"})
		}},
		{name: "the successor does not exist", setup: func(f *bpdFake) {
			bpdSuperseded(f, "acme/widget#71", 72)
		}},
		{name: "the successor is the held pull request itself", setup: func(f *bpdFake) {
			bpdSuperseded(f, "acme/widget#71", 70)
		}},
		{name: "two closed pull requests naming each other", setup: func(f *bpdFake) {
			bpdSuperseded(f, "acme/widget#71", 72)
			bpdSuperseded(f, "acme/widget#72", 71)
		}},
		{name: "a chain longer than the hop limit", setup: func(f *bpdFake) {
			bpdSuperseded(f, "acme/widget#71", 72)
			bpdSuperseded(f, "acme/widget#72", 73)
			bpdSuperseded(f, "acme/widget#73", 74)
			bpdSuperseded(f, "acme/widget#74", 75)
			f.put("acme/widget#75", bpdOpenPR("four replacements away"))
		}},
		{name: "the hive's bookkeeping about another blocker", setup: func(f *bpdFake) {
			closed := bpdClosedPR()
			closed.comments = []string{BlockedOnMarker("acme/widget", 72) + "\nRe-targeted: blocker #5 was superseded by #72."}
			f.put("acme/widget#71", closed)
			f.put("acme/widget#72", bpdOpenPR("what the closed target itself waited on"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBPDFake()
			blockedPRFixture(t, f, "acme/widget#70", "Depends-on: #71", "acme/widget#71")
			tc.setup(f)
			c := f.client(t)
			t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 70) })

			res := sweepBPD(t, c)
			if res.Unblocked != 1 || res.Marked != 0 || res.Errors != 0 || issueHasLabel(f.item("acme/widget#70").labels, BlockedLabel) {
				t.Fatalf("the hold must lift as before: %+v labels=%v", res, f.item("acme/widget#70").labels)
			}
			posted := f.postedOn("acme/widget#70")
			if len(posted) != 1 || !strings.Contains(posted[0], blockedUnblockedMarker) || !strings.Contains(posted[0], "Unblocked: "+bpdSuccessorTodayClause+".") {
				t.Fatalf("lift note = %q, want only the note it always had", posted)
			}
			if strings.Contains(posted[0], "hive:depends-on-successor") {
				t.Fatalf("no successor may be marked: %q", posted[0])
			}
		})
	}
}

// A successor holds only in place of a listed target that is still closed
// unmerged, and only when the hive itself linked it: dropping the target
// from the body releases both, a target that merged after all releases its
// successor, and a link marker a stranger posted holds nothing.
func TestSweepBlockedPRDependencies_SuccessorStopsHoldingWithItsTarget(t *testing.T) {
	succComment := blockedPRSuccessorMarkerComment(blockedPRTarget{repo: "acme/widget", number: 72}, blockedPRTarget{repo: "acme/widget", number: 71})
	cases := []struct {
		name    string
		body    string
		setup   func(f *bpdFake)
		clauses []string
	}{
		{name: "target dropped from the body while held on its successor", body: "No dependencies any more.",
			setup: func(f *bpdFake) {
				f.item("acme/widget#70").comments = append(f.item("acme/widget#70").comments, succComment)
				f.put("acme/widget#71", bpdClosedPR())
			},
			clauses: []string{"acme/widget#71 is no longer listed under `Depends-on:`", "acme/widget#72 no longer stands in for a listed dependency"}},
		{name: "target reopened and merged while held on its successor", body: "Depends-on: #71",
			setup: func(f *bpdFake) {
				f.item("acme/widget#70").comments = append(f.item("acme/widget#70").comments, succComment)
				f.put("acme/widget#71", &bpdItem{isPR: true, state: "closed", merged: true})
			},
			clauses: []string{"acme/widget#71 merged", "acme/widget#72 no longer stands in for a listed dependency"}},
		{name: "a stranger's link marker", body: "Depends-on: #71",
			setup: func(f *bpdFake) {
				f.commentAs("acme/widget#70", succComment, "mallory", "NONE")
				f.put("acme/widget#71", bpdClosedPR())
			},
			clauses: []string{bpdSuccessorTodayClause, "acme/widget#72 is no longer listed under `Depends-on:`"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBPDFake()
			blockedPRFixture(t, f, "acme/widget#70", tc.body, "acme/widget#71")
			f.put("acme/widget#72", bpdOpenPR("still open"))
			tc.setup(f)
			c := f.client(t)
			t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 70) })

			res := sweepBPD(t, c)
			if res.Unblocked != 1 || res.Marked != 0 || issueHasLabel(f.item("acme/widget#70").labels, BlockedLabel) {
				t.Fatalf("the hold must lift: %+v labels=%v", res, f.item("acme/widget#70").labels)
			}
			posted := f.postedOn("acme/widget#70")
			if len(posted) != 1 || !strings.Contains(posted[0], "Unblocked: "+strings.Join(tc.clauses, "; ")+".") {
				t.Fatalf("lift note = %q, want clauses %q", posted, tc.clauses)
			}
		})
	}
}

// People still win. A person's `blocked` is neither lifted nor given a
// successor marker, and a person who removed the hive's `blocked` while the
// PR was held on a successor does not get it back.
func TestSweepBlockedPRDependencies_SuccessorLeavesPeopleAlone(t *testing.T) {
	succComment := blockedPRSuccessorMarkerComment(blockedPRTarget{repo: "acme/widget", number: 72}, blockedPRTarget{repo: "acme/widget", number: 71})
	t.Run("person-applied blocked", func(t *testing.T) {
		f := newBPDFake()
		blockedPRFixture(t, f, "acme/widget#70", "Depends-on: #71", "acme/widget#71")
		f.event("acme/widget#70", "labeled", "maintainer")
		bpdSuperseded(f, "acme/widget#71", 72)
		f.put("acme/widget#72", bpdOpenPR("redo"))
		c := f.client(t)

		res := sweepBPD(t, c)
		posted, added, removed := f.counts()
		if res.Marked+res.Unblocked+res.Placed != 0 || posted+added+removed != 0 || !issueHasLabel(f.item("acme/widget#70").labels, BlockedLabel) {
			t.Fatalf("a person's blocked must be left alone: %+v posted=%d added=%d removed=%d", res, posted, added, removed)
		}
	})
	t.Run("person removed the hive's blocked while held on the successor", func(t *testing.T) {
		f := newBPDFake()
		blockedPRFixture(t, f, "acme/widget#70", "Depends-on: #71", "acme/widget#71")
		f.item("acme/widget#70").comments = append(f.item("acme/widget#70").comments, succComment)
		f.mu.Lock()
		f.items["acme/widget#70"].labels = nil
		f.eventLocked("acme/widget#70", "unlabeled", "maintainer")
		f.mu.Unlock()
		f.put("acme/widget#71", bpdClosedPR())
		f.put("acme/widget#72", bpdOpenPR("redo"))
		c := f.client(t)

		for pass := 1; pass <= 2; pass++ {
			res := sweepBPD(t, c)
			posted, added, removed := f.counts()
			if res.Placed+res.Marked != 0 || posted+added+removed != 0 || issueHasLabel(f.item("acme/widget#70").labels, BlockedLabel) {
				t.Fatalf("pass %d: the hive must not put its blocked back for a successor: %+v posted=%d added=%d removed=%d", pass, res, posted, added, removed)
			}
		}
	})
}

// A successor the sweep may not mark (the write-back gate refuses) or could
// not look up (the closed target's comments are unreadable) keeps the hold
// for the pass and writes nothing; the next pass looks again.
func TestSweepBlockedPRDependencies_SuccessorKeepsHoldWhenItCannotWriteOrRead(t *testing.T) {
	t.Run("write-back gate off", func(t *testing.T) {
		f := newBPDFake()
		blockedPRFixture(t, f, "acme/widget#70", "Depends-on: #71", "acme/widget#71")
		bpdSuperseded(f, "acme/widget#71", 72)
		f.put("acme/widget#72", bpdOpenPR("redo"))
		c := f.client(t)
		c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"acme/widget": {Mode: writeback.RepoModeOff}}}, writeback.Deps{}, ""))
		t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 70) })

		res := sweepBPD(t, c)
		posted, added, removed := f.counts()
		if posted+added+removed != 0 || res.Suppressed != 1 || res.Marked+res.Unblocked != 0 || !issueHasLabel(f.item("acme/widget#70").labels, BlockedLabel) {
			t.Fatalf("gate off must write nothing and keep the hold: %+v posted=%d added=%d removed=%d", res, posted, added, removed)
		}
	})
	t.Run("closed target's comments unreadable", func(t *testing.T) {
		resetBlockedSuccessorLookups(t)
		f := newBPDFake()
		blockedPRFixture(t, f, "acme/widget#70", "Depends-on: #71", "acme/widget#71")
		closed := bpdClosedPR()
		closed.commentsStatus = http.StatusBadGateway
		f.put("acme/widget#71", closed)
		c := f.client(t)
		t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 70) })

		res := sweepBPD(t, c)
		posted, added, removed := f.counts()
		if posted+added+removed != 0 || res.Errors != 1 || res.Unblocked != 0 || !issueHasLabel(f.item("acme/widget#70").labels, BlockedLabel) {
			t.Fatalf("an unreadable successor statement must keep the hold: %+v posted=%d added=%d removed=%d", res, posted, added, removed)
		}
		// The read works again: no statement, so the hold lifts as before.
		f.mu.Lock()
		f.items["acme/widget#71"].commentsStatus = 0
		f.mu.Unlock()
		if res := sweepBPD(t, c); res.Unblocked != 1 || !strings.Contains(strings.Join(f.postedOn("acme/widget#70"), "\n"), bpdSuccessorTodayClause) {
			t.Fatalf("once readable, the hold must lift with the usual note: %+v posted=%q", res, f.postedOn("acme/widget#70"))
		}
	})
}

// The successor comment carries the marker every reader of the hold already
// understands plus the link the sweep reads back, mentions no one, and its
// link is read only from the hive's own comments.
func TestBlockedPRSuccessorMarkerCommentRoundTrip(t *testing.T) {
	succ, old := blockedPRTarget{repo: "acme/gadget", number: 12}, blockedPRTarget{repo: "acme/gadget", number: 11}
	body := blockedPRSuccessorMarkerComment(succ, old)
	if repo, pr, ok := ParseBlockedOnMarker(body); !ok || repo != "acme/gadget" || pr != 12 {
		t.Fatalf("blocked-on marker = %s#%d ok=%v in %q", repo, pr, ok, body)
	}
	if strings.Contains(body, "@") {
		t.Fatalf("the successor comment must mention no one: %q", body)
	}
	if _, ok := ParseSupersededBy(body); ok {
		t.Fatalf("the successor comment must not parse as a supersession: %q", body)
	}
	if _, ok := blockedSuccessorStatement(body); ok {
		t.Fatalf("the successor comment is bookkeeping, not a statement: %q", body)
	}
	c := newBPDFake().client(t)
	own := &gh.IssueComment{Body: gh.Ptr(body), User: &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}}
	other := &gh.IssueComment{Body: gh.Ptr(body), User: &gh.User{Login: gh.Ptr("mallory")}, AuthorAssociation: gh.Ptr("MEMBER")}
	links := c.blockedSuccessorLinks([]*gh.IssueComment{other, own, nil})
	if len(links) != 1 || links[0].successor != succ || links[0].replaces != old {
		t.Fatalf("links = %+v, want the hive's one link", links)
	}
	if got := blockedPRMarkedTargets([]*gh.IssueComment{own}); len(got) != 1 || got[0] != succ {
		t.Fatalf("marked targets = %+v, want only the successor", got)
	}
}

// resetBlockedSuccessorLookups empties the record of failing successor
// lookups now and when the test ends, so tests leak nothing into each other
// whatever order they run in.
func resetBlockedSuccessorLookups(t *testing.T) {
	t.Helper()
	wipe := func() {
		blockedSuccessorLookups.Lock()
		blockedSuccessorLookups.m = map[string]blockedSuccessorLookupState{}
		blockedSuccessorLookups.Unlock()
	}
	wipe()
	t.Cleanup(wipe)
}

// blockedSuccessorLookupRecorded reports whether a failing lookup is on
// record for the held item and its closed blocker.
func blockedSuccessorLookupRecorded(repo string, number int, blocker string) bool {
	blockedSuccessorLookups.Lock()
	defer blockedSuccessorLookups.Unlock()
	_, ok := blockedSuccessorLookups.m[blockedSuccessorLookupKey(repo, number, blocker)]
	return ok
}

// blockedSuccessorClock pins the clock the lookup grace is measured on and
// returns the function that moves it forward. It also resets the record of
// failing lookups.
func blockedSuccessorClock(t *testing.T) func(time.Duration) {
	t.Helper()
	resetBlockedSuccessorLookups(t)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	prev := blockedSuccessorNow
	blockedSuccessorNow = func() time.Time { return now }
	t.Cleanup(func() { blockedSuccessorNow = prev })
	return func(d time.Duration) { now = now.Add(d) }
}

// blockedSuccessorGaveUpLog is what the one give-up warning says.
const blockedSuccessorGaveUpLog = "successor lookup for a closed blocker kept failing for 1h0m0s; releasing as before"

// A comment list GitHub definitely refuses (404) names no successor: the
// hold is released on the first pass with the note it always had, and
// nothing is kept on record.
func TestSweepBlockedPRDependencies_SuccessorLookupRefusedReleasesAtOnce(t *testing.T) {
	resetBlockedSuccessorLookups(t)
	f := newBPDFake()
	blockedPRFixture(t, f, "acme/widget#80", "Depends-on: #81", "acme/widget#81")
	closed := bpdClosedPR()
	closed.commentsStatus = http.StatusNotFound
	f.put("acme/widget#81", closed)
	c := f.client(t)
	t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 80) })

	res := sweepBPD(t, c)
	if res.Unblocked != 1 || res.Errors != 0 || issueHasLabel(f.item("acme/widget#80").labels, BlockedLabel) {
		t.Fatalf("a refused lookup must release on the first pass: %+v labels=%v", res, f.item("acme/widget#80").labels)
	}
	posted := f.postedOn("acme/widget#80")
	if len(posted) != 1 || !strings.Contains(posted[0], "Unblocked: acme/widget#81 closed without merging, so check that this PR still makes sense without it.") {
		t.Fatalf("lift note = %q, want the note it always had", posted)
	}
	if blockedSuccessorLookupRecorded("acme/widget", 80, "acme/widget#81") {
		t.Fatal("a refused lookup must leave nothing on record")
	}
}

// A lookup that fails for a reason that may pass (500) keeps the hold, but
// only within the grace: once it has failed for longer, the hold is released
// with the note it always had and the give-up is logged once. Before the
// bound, such a failure would have held the PR for as long as it lasted.
func TestSweepBlockedPRDependencies_SuccessorLookupFailureIsBounded(t *testing.T) {
	advance := blockedSuccessorClock(t)
	f := newBPDFake()
	blockedPRFixture(t, f, "acme/widget#82", "Depends-on: #83", "acme/widget#83")
	closed := bpdClosedPR()
	closed.commentsStatus = http.StatusInternalServerError
	f.put("acme/widget#83", closed)
	c := f.client(t)
	var logs bytes.Buffer
	c.logger = slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 82) })

	for pass, wait := range []time.Duration{0, 30 * time.Minute} {
		advance(wait)
		res := sweepBPD(t, c)
		posted, added, removed := f.counts()
		if res.Errors != 1 || res.Unblocked != 0 || posted+added+removed != 0 || !issueHasLabel(f.item("acme/widget#82").labels, BlockedLabel) {
			t.Fatalf("pass %d inside the grace must keep the hold and write nothing: %+v posted=%d added=%d removed=%d", pass+1, res, posted, added, removed)
		}
	}
	if !blockedSuccessorLookupRecorded("acme/widget", 82, "acme/widget#83") || strings.Contains(logs.String(), blockedSuccessorGaveUpLog) {
		t.Fatalf("inside the grace the failure is on record and nothing is given up: %s", logs.String())
	}

	advance(blockedSuccessorLookupGrace)
	res := sweepBPD(t, c)
	if res.Unblocked != 1 || issueHasLabel(f.item("acme/widget#82").labels, BlockedLabel) {
		t.Fatalf("past the grace the hold must be released as before: %+v labels=%v", res, f.item("acme/widget#82").labels)
	}
	posted := f.postedOn("acme/widget#82")
	if len(posted) != 1 || !strings.Contains(posted[0], "Unblocked: acme/widget#83 closed without merging, so check that this PR still makes sense without it.") {
		t.Fatalf("lift note = %q, want the note it always had", posted)
	}
	if got := strings.Count(logs.String(), blockedSuccessorGaveUpLog); got != 1 {
		t.Fatalf("the give-up must be logged once, got %d in:\n%s", got, logs.String())
	}
	if blockedSuccessorLookupRecorded("acme/widget", 82, "acme/widget#83") {
		t.Fatal("the record must go with the released hold")
	}

	advance(5 * time.Minute)
	res = sweepBPD(t, c)
	if res.Unblocked+res.Placed+res.Marked != 0 || len(f.postedOn("acme/widget#82")) != 1 || strings.Count(logs.String(), blockedSuccessorGaveUpLog) != 1 {
		t.Fatalf("a pass after the release must do nothing new: %+v posted=%q", res, f.postedOn("acme/widget#82"))
	}
}

// A lookup that failed once and then works is followed as usual: the hold
// moves to the open successor and the failure is forgotten.
func TestSweepBlockedPRDependencies_SuccessorLookupRecovers(t *testing.T) {
	advance := blockedSuccessorClock(t)
	f := newBPDFake()
	blockedPRFixture(t, f, "acme/widget#84", "Depends-on: #85", "acme/widget#85")
	bpdSuperseded(f, "acme/widget#85", 86)
	f.item("acme/widget#85").commentsStatus = http.StatusInternalServerError
	f.put("acme/widget#86", bpdOpenPR("redo"))
	c := f.client(t)
	t.Cleanup(func() { forgetBlockedOnSeen("acme/widget", 84) })

	if res := sweepBPD(t, c); res.Errors != 1 || res.Unblocked+res.Marked != 0 || !blockedSuccessorLookupRecorded("acme/widget", 84, "acme/widget#85") {
		t.Fatalf("the failed lookup must keep the hold and go on record: %+v", res)
	}
	f.mu.Lock()
	f.items["acme/widget#85"].commentsStatus = 0
	f.mu.Unlock()
	advance(5 * time.Minute)
	res := sweepBPD(t, c)
	posted := f.postedOn("acme/widget#84")
	if res.Marked != 1 || res.Unblocked != 0 || res.Errors != 0 || len(posted) != 1 || !strings.Contains(posted[0], BlockedOnMarker("acme/widget", 86)) {
		t.Fatalf("once readable, the hold must move to the successor: %+v posted=%q", res, posted)
	}
	if blockedSuccessorLookupRecorded("acme/widget", 84, "acme/widget#85") {
		t.Fatal("a lookup that completed must clear the record")
	}
}
