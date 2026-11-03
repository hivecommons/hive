package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/writeback"
)

// The three live shapes (2026-10-25), reduced to the facts the re-check
// reads: projectbluefin/server#385 (a "Refs #N" PR merged after the park),
// projectbluefin/utah#13 (the PR the park names closed unmerged, then a PR
// listing the issue in a "Refs" clause merged), projectbluefin/website#926
// (the sister issue the park names closed when the precedent PR merged).

var parkT0 = time.Date(2026, 10, 3, 23, 22, 0, 0, time.UTC)

func prTS(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func prLabelEvent(event, name string, at time.Time) map[string]any {
	return map[string]any{"event": event, "label": map[string]any{"name": name}, "created_at": prTS(at), "actor": map[string]any{"login": "hive[bot]"}}
}

// crossRef is a timeline cross-reference from repo#n with its current state.
func prCrossRef(repo string, n int, isPR bool, state string, closedAt, mergedAt time.Time, body string) map[string]any {
	src := map[string]any{"number": n, "state": state, "body": body, "repository": map[string]any{"full_name": repo}}
	if !closedAt.IsZero() {
		src["closed_at"] = prTS(closedAt)
	}
	if isPR {
		pr := map[string]any{"url": fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", repo, n)}
		if !mergedAt.IsZero() {
			pr["merged_at"] = prTS(mergedAt)
		}
		src["pull_request"] = pr
	}
	return map[string]any{"event": "cross-referenced", "created_at": prTS(parkT0.Add(-48 * time.Hour)), "source": map[string]any{"type": "issue", "issue": src}}
}

type parkFake struct {
	mu       sync.Mutex
	issue    map[string]any
	timeline []map[string]any
	comments []map[string]any
	lookups  map[int]map[string]any // GET issues/{n} for named refs
	// timelines and others serve the timeline and comments of items other
	// than the parked issue; reads, gets and creads record what was asked for.
	timelines map[int][]map[string]any
	others    map[int][]map[string]any
	reads     []int
	gets      []int
	creads    []int
	posted    []string
	edited    []string
	labelOps  int
	nextID    int64
}

func newParkFake(t *testing.T, f *parkFake) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/repos/org/repo/")
		n := f.issue["number"].(int)
		self := "issues/" + strconv.Itoa(n)
		switch {
		case r.Method == http.MethodGet && path == "issues":
			if !strings.Contains(r.URL.Query().Get("labels"), "stage/045-needs-decision") {
				t.Errorf("listing filtered on %q", r.URL.Query().Get("labels"))
			}
			_ = json.NewEncoder(w).Encode([]any{f.issue})
		case r.Method == http.MethodGet && path == self+"/timeline":
			_ = json.NewEncoder(w).Encode(f.timeline)
		case r.Method == http.MethodGet && path == self+"/comments":
			_ = json.NewEncoder(w).Encode(f.comments)
		case r.Method == http.MethodPost && path == self+"/comments":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.nextID++
			f.posted = append(f.posted, body["body"])
			f.comments = append(f.comments, map[string]any{"id": 9000 + f.nextID, "body": body["body"], "user": map[string]any{"login": "hive[bot]"}, "created_at": prTS(time.Now())})
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9000 + f.nextID})
		case r.Method == http.MethodPatch && strings.HasPrefix(path, "issues/comments/"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := strconv.ParseInt(strings.TrimPrefix(path, "issues/comments/"), 10, 64)
			for _, cm := range f.comments {
				if cm["id"] == id {
					cm["body"] = body["body"]
				}
			}
			f.edited = append(f.edited, body["body"])
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "issues/") && strings.HasSuffix(path, "/timeline"):
			num, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "issues/"), "/timeline"))
			f.reads = append(f.reads, num)
			if got, ok := f.timelines[num]; ok {
				_ = json.NewEncoder(w).Encode(got)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "issues/") && strings.HasSuffix(path, "/comments"):
			num, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "issues/"), "/comments"))
			f.creads = append(f.creads, num)
			if got, ok := f.others[num]; ok {
				_ = json.NewEncoder(w).Encode(got)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "issues/"):
			num, _ := strconv.Atoi(strings.TrimPrefix(path, "issues/"))
			f.gets = append(f.gets, num)
			if got, ok := f.lookups[num]; ok {
				_ = json.NewEncoder(w).Encode(got)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(path, "/labels"):
			f.labelOps++
			w.WriteHeader(http.StatusTeapot)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func parkClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := newTestClient(t, srv, "org", []string{"repo"})
	c.SetAppBotLogin("hive[bot]")
	return c
}

// parkedIssue is an open issue parked at parkT0 by a relay comment (posted
// under a contributor account just before the label) carrying parkText.
func prParkedIssue(number int, parkText string) *parkFake {
	return &parkFake{
		issue: map[string]any{"number": number, "state": "open", "title": "t", "body": "reporter body, no refs",
			"user": map[string]any{"login": "reporter"}, "created_at": prTS(parkT0.Add(-30 * 24 * time.Hour)),
			"labels": []any{map[string]any{"name": "stage/045-needs-decision"}}},
		comments: []map[string]any{{"id": int64(1), "body": parkText, "user": map[string]any{"login": "contributor"}, "created_at": prTS(parkT0.Add(-20 * time.Second))}},
		timeline: []map[string]any{
			// needs-decision → stage/085 → stage/045, swapped a second apart:
			// one continuous park since parkT0.
			prLabelEvent("labeled", "needs-decision", parkT0),
			prLabelEvent("labeled", "stage/085-needs-decision", parkT0.Add(14*24*time.Hour)),
			prLabelEvent("unlabeled", "needs-decision", parkT0.Add(14*24*time.Hour+time.Second)),
			prLabelEvent("unlabeled", "stage/085-needs-decision", parkT0.Add(15*24*time.Hour)),
			prLabelEvent("labeled", "stage/045-needs-decision", parkT0.Add(15*24*time.Hour+time.Second)),
		},
		lookups: map[int]map[string]any{},
	}
}

func recheck(t *testing.T, f *parkFake) *ParkRecheckResult {
	t.Helper()
	c := parkClient(t, newParkFake(t, f))
	res, err := c.RecheckParkedIssues(context.Background(), ParkRecheckConfig{Repos: []string{"org/repo"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func wantNote(t *testing.T, note string, want ...string) {
	t.Helper()
	if !strings.HasPrefix(note, parkRecheckMarker) {
		t.Fatalf("note lacks marker:\n%s", note)
	}
	for _, w := range want {
		if !strings.Contains(note, w) {
			t.Fatalf("note missing %q:\n%s", w, note)
		}
	}
}

// server#385: the park asked whether to adopt ruff and type checking; a PR
// whose body says "Refs #385" merged three days later and delivered both.
func TestParkRecheckReferencedPRMerged(t *testing.T) {
	f := prParkedIssue(385, "Maintainer decision needed: whether to adopt ruff and a type checker.")
	f.timeline = append(f.timeline, prCrossRef("org/repo", 409, true, "closed", parkT0.Add(50*time.Hour), parkT0.Add(50*time.Hour), "Refs #385\n\nAdds ruff and mypy."))
	res := recheck(t, f)
	if res.Posted != 1 || len(f.posted) != 1 || f.labelOps != 0 {
		t.Fatalf("result %+v posted=%d labelOps=%d", res, len(f.posted), f.labelOps)
	}
	wantNote(t, f.posted[0], "parked for a maintainer (`stage/045-needs-decision`) since 2026-10-03",
		"- #409 merged on 2026-10-06 (it refers to this issue).", "does not un-park")
}

func TestParkRecheckWritebackRefusalDoesNotPostOrEdit(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			f := prParkedIssue(385, "Maintainer decision needed.")
			f.timeline = append(f.timeline, prCrossRef("org/repo", 409, true, "closed", parkT0.Add(50*time.Hour), parkT0.Add(50*time.Hour), "Refs #385"))
			if existing {
				f.comments = append(f.comments, map[string]any{"id": int64(99), "body": parkRecheckMarker + "\nstale", "user": map[string]any{"login": "hive[bot]"}})
			}
			client := parkClient(t, newParkFake(t, f))
			gate := writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 1 }}, "")
			client.SetWritebackGate(gate)
			res, err := client.RecheckParkedIssues(context.Background(), ParkRecheckConfig{Repos: []string{"org/repo"}})
			if err != nil || res.Posted != 0 || res.Updated != 0 || len(f.posted)+len(f.edited) != 0 {
				t.Fatalf("result=%+v err=%v posted=%q edited=%q", res, err, f.posted, f.edited)
			}
			if suppressed := gate.Recent(); len(suppressed) != 1 || suppressed[0].Kind != writeback.KindNeedsDecisionComment || suppressed[0].Number != 385 {
				t.Fatalf("suppressed=%+v", suppressed)
			}
		})
	}
}

// utah#13: the park said #722 was in progress and agents should leave the
// issue alone; #722 closed unmerged, and #885 merged listing the issue at
// the end of a "Refs" clause.
func TestParkRecheckNamedPRClosedUnmerged(t *testing.T) {
	f := prParkedIssue(13, "Most of the gate is being finished in open PR #722. Agents are asked not to open a competing implementation while #722 is in progress.")
	f.timeline = append(f.timeline,
		prCrossRef("org/repo", 722, true, "closed", parkT0.Add(21*24*time.Hour), time.Time{}, "ci: gate testing promotion"),
		prCrossRef("org/repo", 885, true, "closed", parkT0.Add(22*24*time.Hour), parkT0.Add(22*24*time.Hour), "Refs #722 (gate testing promotion), #791 (stale promote PR), #13. Nothing closed."))
	recheck(t, f)
	if len(f.posted) != 1 {
		t.Fatalf("posted %q", f.posted)
	}
	wantNote(t, f.posted[0],
		"- #722 was closed without merging on 2026-10-24 (the park reason names it).",
		"- #885 merged on 2026-10-25 (it refers to this issue).")
	if strings.Index(f.posted[0], "#722") > strings.Index(f.posted[0], "#885") {
		t.Fatalf("changes not in time order:\n%s", f.posted[0])
	}
}

// website#926: the park said the precedent had not been set and named the
// sister issue blocked on the same decision; the precedent PR merged that
// day and closed the sister issue. The PR itself never mentions #926.
func TestParkRecheckNamedSisterIssueClosed(t *testing.T) {
	f := prParkedIssue(926, "PR #925 was merged; the precedent for the nine remaining entries has not been set. Sister backlog issue #928 is blocked on the same decision.")
	f.timeline = append(f.timeline, prCrossRef("org/repo", 928, false, "closed", parkT0.Add(16*time.Hour), time.Time{}, "cs and eo aliases"))
	// #925 merged before the park: no change. It is not a cross-reference,
	// so it is read directly.
	f.lookups[925] = map[string]any{"number": 925, "state": "closed", "closed_at": prTS(parkT0.Add(-3 * time.Hour)),
		"pull_request": map[string]any{"merged_at": prTS(parkT0.Add(-3 * time.Hour))}, "body": ""}
	recheck(t, f)
	if len(f.posted) != 1 {
		t.Fatalf("posted %q", f.posted)
	}
	wantNote(t, f.posted[0], "- #928 was closed on 2026-10-04 (the park reason names it).")
	if strings.Contains(f.posted[0], "#925") {
		t.Fatalf("a PR merged before the park is listed:\n%s", f.posted[0])
	}
}

func TestParkRecheckNoChangeNoNote(t *testing.T) {
	cases := map[string]map[string]any{
		"merged before the park":     prCrossRef("org/repo", 409, true, "closed", parkT0.Add(-time.Hour), parkT0.Add(-time.Hour), "Refs #385"),
		"still open":                 prCrossRef("org/repo", 409, true, "open", time.Time{}, time.Time{}, "Refs #385"),
		"passing mention":            prCrossRef("org/repo", 409, true, "closed", parkT0.Add(time.Hour), parkT0.Add(time.Hour), "Unrelated cleanup; see the discussion in #385 for background."),
		"other repo's #385":          prCrossRef("org/repo", 409, true, "closed", parkT0.Add(time.Hour), parkT0.Add(time.Hour), "Refs other/repo#385"),
		"intake table":               prCrossRef("org/repo", 409, true, "closed", parkT0.Add(time.Hour), parkT0.Add(time.Hour), "Refs #1, #2, #3, #4, #5, #6, #7, #8, #9, #10, #11, #385"),
		"closed unmerged, not named": prCrossRef("org/repo", 409, true, "closed", parkT0.Add(time.Hour), time.Time{}, "Refs #385"),
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			f := prParkedIssue(385, "Maintainer decision needed.")
			f.timeline = append(f.timeline, ev)
			res := recheck(t, f)
			if len(f.posted) != 0 || res.Posted != 0 {
				t.Fatalf("note posted: %q", f.posted)
			}
		})
	}
}

// The note is one comment: an unchanged pass edits nothing, a new change
// edits it in place.
func TestParkRecheckNoteEditedInPlace(t *testing.T) {
	f := prParkedIssue(13, "Waiting on #722.")
	f.timeline = append(f.timeline, prCrossRef("org/repo", 722, true, "closed", parkT0.Add(21*24*time.Hour), time.Time{}, ""))
	c := parkClient(t, newParkFake(t, f))
	run := func() *ParkRecheckResult {
		res, err := c.RecheckParkedIssues(context.Background(), ParkRecheckConfig{Repos: []string{"org/repo"}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run(); res.Posted != 1 {
		t.Fatalf("first pass %+v", res)
	}
	if res := run(); res.Posted != 0 || res.Updated != 0 || len(f.edited) != 0 {
		t.Fatalf("unchanged pass wrote: %+v edited=%q", res, f.edited)
	}
	f.timeline = append(f.timeline, prCrossRef("org/repo", 885, true, "closed", parkT0.Add(22*24*time.Hour), parkT0.Add(22*24*time.Hour), "Refs #13"))
	if res := run(); res.Updated != 1 || len(f.posted) != 1 || len(f.edited) != 1 {
		t.Fatalf("new change: %+v posted=%d edited=%d", res, len(f.posted), len(f.edited))
	}
	wantNote(t, f.edited[0], "#722 was closed without merging", "#885 merged")
}

// A park lifted and re-applied after a real gap starts over: work that moved
// during the earlier park is not news to the new one.
func TestParkStartFromNewParkAfterGap(t *testing.T) {
	f := prParkedIssue(385, "Maintainer decision needed.")
	reparked := parkT0.Add(20 * 24 * time.Hour)
	f.timeline = []map[string]any{
		prLabelEvent("labeled", "needs-decision", parkT0),
		prLabelEvent("unlabeled", "needs-decision", parkT0.Add(time.Hour)),
		prLabelEvent("labeled", "stage/045-needs-decision", reparked),
		prCrossRef("org/repo", 409, true, "closed", parkT0.Add(74*time.Hour), parkT0.Add(74*time.Hour), "Refs #385"),
	}
	f.comments[0]["created_at"] = prTS(reparked.Add(-time.Minute))
	recheck(t, f)
	if len(f.posted) != 0 {
		t.Fatalf("a PR merged between two parks is listed: %q", f.posted)
	}
}

func TestParkNamedRefs(t *testing.T) {
	got := parkNamedRefs("See #722, org/other#5 and #13 (self); &#39; is an entity, a#9 is not a ref, (#722) again.", "org/repo", 13)
	want := []string{"org/repo#722", "org/other#5"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parkNamedRefs = %v, want %v", got, want)
	}
}

// prCrossRefBy is prCrossRef with the source's author.
func prCrossRefBy(login, repo string, n int, isPR bool, state string, closedAt, mergedAt time.Time, body string) map[string]any {
	ev := prCrossRef(repo, n, isPR, state, closedAt, mergedAt, body)
	ev["source"].(map[string]any)["issue"].(map[string]any)["user"] = map[string]any{"login": login}
	return ev
}

func prComment(id int64, login, body string, at time.Time) map[string]any {
	return map[string]any{"id": id, "body": body, "user": map[string]any{"login": login}, "created_at": prTS(at),
		"html_url": fmt.Sprintf("https://github.com/org/repo/pull/873#issuecomment-%d", id)}
}

// The utah#13 shape (2026-10-27): the park names #722; the hive adopted it
// as #873 (body as automerge.AdoptionPRBody writes it) and #722 closed; a
// maintainer then closed #873 unmerged saying other PRs landed the intent.
// #873 never mentions #13, so only #722's own timeline leads to it.
var (
	parkCopyClosed = time.Date(2026, 10, 27, 2, 17, 25, 0, time.UTC)
	parkCopyBody   = "<!-- hive:carried-fence reason=contributor-lane:workflow-path from=#722 -->\nSupersedes #722 (adopted after CI went red and the contributor was unavailable; original commits by @hanthor preserved).\n\nThis branch is #722's head."
	parkCopyNote   = "Closing as superseded: #885 (monotonic ratchet, per-digest E2E evidence) and #905 (weekly :stable) landed the intent, and this PR's `run_e2e` switch would reintroduce the tip-equality dependency #885 removed (cc @castrojo).\n\nStill worth a fresh PR from main if wanted."
)

func prAdoptedCopyIssue(closedBy string) *parkFake {
	f := prParkedIssue(13, "Most of the gate is being finished in open PR #722. Agents are asked not to open a competing implementation while #722 is in progress.")
	f.timeline = append(f.timeline, prCrossRef("org/repo", 722, true, "closed", parkT0.Add(21*24*time.Hour), time.Time{}, "ci: gate testing promotion"))
	f.timelines = map[int][]map[string]any{722: {
		prCrossRefBy("reporter", "org/repo", 13, false, "open", time.Time{}, time.Time{}, "waiting on #722"),
		// An earlier adoption the hive closed; the later copy is current.
		prCrossRefBy("hive[bot]", "org/repo", 800, true, "closed", parkT0.Add(20*24*time.Hour), time.Time{}, strings.Replace(parkCopyBody, "This branch", "First attempt.", 1)),
		prCrossRefBy("hive[bot]", "org/repo", 873, true, "closed", parkCopyClosed, time.Time{}, parkCopyBody),
	}}
	got := map[string]any{"number": 873, "state": "closed", "closed_at": prTS(parkCopyClosed), "pull_request": map[string]any{}}
	if closedBy != "" {
		got["closed_by"] = map[string]any{"login": closedBy}
	}
	f.lookups[873] = got
	f.others = map[int][]map[string]any{873: {
		prComment(1, "Danathar", "@hanthor quick check before anyone resolves the conflicts here.", parkCopyClosed.Add(-34*time.Hour)),
		prComment(2, "hive[bot]", "<!-- hive:stage-needs-decision -->\nNeeds a manual merge decision.", parkCopyClosed.Add(-30*time.Minute)),
		prComment(3, "hanthor", parkCopyNote, parkCopyClosed.Add(-time.Second)),
		prComment(4, "castrojo", "Agreed.", parkCopyClosed.Add(10*time.Minute)),
	}}
	return f
}

const parkCopyHint = "If the closing note quoted above settles the question, a maintainer can reply `/fixed` to close this issue as completed, or `/hive decision <your instructions>` to un-park it."

func TestParkRecheckFollowsAdoptedCopyClosingNote(t *testing.T) {
	f := prAdoptedCopyIssue("hanthor")
	recheck(t, f)
	if len(f.posted) != 1 {
		t.Fatalf("posted %q", f.posted)
	}
	note := f.posted[0]
	wantNote(t, note,
		"- #722 was closed without merging on 2026-10-24 (the park reason names it).\n",
		"- #873 (the hive's copy of #722) was closed without merging on 2026-10-27 by hanthor: “Closing as superseded: #885 (monotonic ratchet, per-digest E2E evidence) and #905 (weekly :stable) landed the intent, and this PR's `run_e2e` switch would reintroduce the tip-equality dependency #885 removed (cc castrojo).” ([comment](https://github.com/org/repo/pull/873#issuecomment-3)).\n",
		"Hive updates this note if more of the referenced work moves.\n\n"+parkCopyHint)
	if !strings.HasSuffix(note, parkCopyHint) || strings.Contains(note, "Still worth") || strings.Contains(note, "@") {
		t.Fatalf("hint not last, quote not the first paragraph, or a mention left:\n%s", note)
	}
	if strings.Join(intsToStrings(f.reads), ",") != "722" || strings.Join(intsToStrings(f.gets), ",") != "873" {
		t.Fatalf("reads %v gets %v, want one timeline read of 722 and one get of 873", f.reads, f.gets)
	}
}

func intsToStrings(ns []int) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, strconv.Itoa(n))
	}
	return out
}

// No person's note at the copy's close: the closer's comments are a day and
// a half older or more than an hour later, or the hive closed the copy
// itself (a person commenting then is not explaining the hive's close). The
// copy line still says the adopted work closed too, since the park names
// #722, but nothing is quoted and no reply is suggested. When the closer
// cannot be read, any person's comment at the close counts.
func TestParkRecheckAdoptedCopyClosedWithoutNote(t *testing.T) {
	plain := "- #873 (the hive's copy of #722) was closed without merging on 2026-10-27.\n"
	t.Run("closer silent at close", func(t *testing.T) {
		f := prAdoptedCopyIssue("hanthor")
		f.others[873] = []map[string]any{
			prComment(1, "hanthor", "Earlier review discussion.", parkCopyClosed.Add(-34*time.Hour)),
			prComment(2, "hanthor", "Follow-up much later.", parkCopyClosed.Add(parkCopyNoteWindow+time.Minute)),
		}
		recheck(t, f)
		wantNote(t, f.posted[0], plain)
		if strings.Contains(f.posted[0], parkCopyHint) {
			t.Fatalf("hint without a note:\n%s", f.posted[0])
		}
	})
	t.Run("hive closed it", func(t *testing.T) {
		f := prAdoptedCopyIssue("hive[bot]")
		recheck(t, f)
		wantNote(t, f.posted[0], plain)
		if strings.Contains(f.posted[0], "“") || len(f.creads) != 0 {
			t.Fatalf("read or quoted comments on a copy the hive closed (reads %v):\n%s", f.creads, f.posted[0])
		}
	})
	t.Run("closer unknown", func(t *testing.T) {
		f := prAdoptedCopyIssue("")
		f.others[873] = append(f.others[873], prComment(5, "hive[bot]", "<!-- hive:duplicate-sweep -->\nPossible duplicate.", parkCopyClosed.Add(20*time.Minute)))
		recheck(t, f)
		wantNote(t, f.posted[0], "by castrojo: “Agreed.”", parkCopyHint)
	})
	// A PR that only refers to the issue is not in the note when it closes
	// unmerged, so its copy closing without a note is no change either.
	t.Run("original only refers", func(t *testing.T) {
		f := prAdoptedCopyIssue("hanthor")
		f.comments[0]["body"] = "Maintainer decision needed."
		f.timeline[len(f.timeline)-1] = prCrossRef("org/repo", 722, true, "closed", parkT0.Add(21*24*time.Hour), time.Time{}, "Refs #13")
		f.others[873] = f.others[873][:2]
		res := recheck(t, f)
		if len(f.posted) != 0 || res.Posted != 0 {
			t.Fatalf("note posted: %q", f.posted)
		}
	})
}

// A PR that refers to the issue is followed too: it is not in the note when
// it closes unmerged, but its copy closing with a person's note is.
func TestParkRecheckFollowsReferringPRCopy(t *testing.T) {
	f := prAdoptedCopyIssue("hanthor")
	f.comments[0]["body"] = "Maintainer decision needed."
	f.timeline[len(f.timeline)-1] = prCrossRef("org/repo", 722, true, "closed", parkT0.Add(21*24*time.Hour), time.Time{}, "Refs #13")
	recheck(t, f)
	if len(f.posted) != 1 {
		t.Fatalf("posted %q", f.posted)
	}
	wantNote(t, f.posted[0], "- #873 (the hive's copy of #722) was closed without merging on 2026-10-27 by hanthor: “Closing as superseded:", parkCopyHint)
	if strings.Contains(f.posted[0], "- #722") {
		t.Fatalf("an unnamed PR closed unmerged is listed:\n%s", f.posted[0])
	}
}

// A closed copy that itself refers to the issue is not followed to a copy of
// its own: that would spend a copy-lookup slot and a timeline read on a PR
// that has none.
func TestParkRecheckSkipsAdoptionCopyWhenFollowing(t *testing.T) {
	f := prAdoptedCopyIssue("hanthor")
	f.comments[0]["body"] = "Maintainer decision needed."
	f.timeline[len(f.timeline)-1] = prCrossRef("org/repo", 722, true, "closed", parkT0.Add(21*24*time.Hour), time.Time{}, "Refs #13")
	f.timeline = append(f.timeline, prCrossRefBy("hive[bot]", "org/repo", 873, true, "closed", parkCopyClosed, time.Time{}, parkCopyBody+"\nRefs #13"))
	recheck(t, f)
	if got := strings.Join(intsToStrings(f.reads), ","); got != "722" {
		t.Fatalf("timeline reads %s, want only 722 (the copy has no copy of its own)", got)
	}
}

// A merged copy: the hive closes the original once its copy merges, so the
// original reads "closed without merging" and the merge would go unseen
// unless the copy itself refers to the issue.
func TestParkRecheckAdoptedCopyMerged(t *testing.T) {
	merged := parkCopyClosed
	t.Run("copy refers to the issue", func(t *testing.T) {
		f := prAdoptedCopyIssue("hanthor")
		body := parkCopyBody + "\nRefs #13"
		f.timeline = append(f.timeline, prCrossRefBy("hive[bot]", "org/repo", 873, true, "closed", merged, merged, body))
		f.timelines[722][2] = prCrossRefBy("hive[bot]", "org/repo", 873, true, "closed", merged, merged, body)
		recheck(t, f)
		wantNote(t, f.posted[0], "- #873 merged on 2026-10-27 (it refers to this issue).")
		if strings.Count(f.posted[0], "#873") != 1 || len(f.gets) != 0 {
			t.Fatalf("copy listed twice or its close read (gets %v):\n%s", f.gets, f.posted[0])
		}
	})
	t.Run("copy does not refer to the issue", func(t *testing.T) {
		f := prAdoptedCopyIssue("hanthor")
		f.timelines[722][2] = prCrossRefBy("hive[bot]", "org/repo", 873, true, "closed", merged, merged, parkCopyBody)
		recheck(t, f)
		wantNote(t, f.posted[0], "- #873 (the hive's copy of #722) merged on 2026-10-27.\n")
		if strings.Contains(f.posted[0], parkCopyHint) || len(f.gets) != 0 {
			t.Fatalf("merged copy read as closed (gets %v):\n%s", f.gets, f.posted[0])
		}
	})
}

// Only the hive's adoption copy is followed: a person's PR saying
// "Supersedes #722", a hive PR adopting another PR, a hive PR re-authoring a
// bot's PR (automerge_bot_takeover.go, not an adoption), a hive issue, and
// a hive PR in another repository are not.
func TestParkRecheckIgnoresNonAdoptionCrossRefs(t *testing.T) {
	cases := map[string]map[string]any{
		"person's PR":     prCrossRefBy("hanthor", "org/repo", 873, true, "closed", parkCopyClosed, time.Time{}, parkCopyBody),
		"adopts another":  prCrossRefBy("hive[bot]", "org/repo", 873, true, "closed", parkCopyClosed, time.Time{}, strings.Replace(parkCopyBody, "Supersedes #722", "Supersedes #721", 1)),
		"bot takeover":    prCrossRefBy("hive[bot]", "org/repo", 873, true, "closed", parkCopyClosed, time.Time{}, "Supersedes #722. `github-actions[bot]` opened it with the workflow `GITHUB_TOKEN`."),
		"hive issue":      prCrossRefBy("hive[bot]", "org/repo", 873, false, "closed", parkCopyClosed, time.Time{}, parkCopyBody),
		"other repo's PR": prCrossRefBy("hive[bot]", "org/other", 873, true, "closed", parkCopyClosed, time.Time{}, parkCopyBody),
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			f := prAdoptedCopyIssue("hanthor")
			f.timelines[722] = []map[string]any{f.timelines[722][0], ev}
			recheck(t, f)
			wantNote(t, f.posted[0], "- #722 was closed without merging")
			if strings.Contains(f.posted[0], "copy") || len(f.gets) != 0 {
				t.Fatalf("followed a non-adoption cross-reference (gets %v):\n%s", f.gets, f.posted[0])
			}
		})
	}
}

// At most parkRecheckMaxCopyLookups closed PRs are followed per pass, the
// earliest closed first, so the same ones are read every pass.
func TestParkRecheckCopyLookupCap(t *testing.T) {
	f := prParkedIssue(13, "Waiting on #705, #701, #704, #702 and #703.")
	for n := 701; n <= 705; n++ {
		f.timeline = append(f.timeline, prCrossRef("org/repo", n, true, "closed", parkT0.Add(time.Duration(n-700)*time.Hour), time.Time{}, ""))
	}
	recheck(t, f)
	if got := strings.Join(intsToStrings(f.reads), ","); got != "701,702,703" {
		t.Fatalf("timeline reads %s, want the three earliest-closed", got)
	}
}

func TestParkCopyQuote(t *testing.T) {
	long := strings.Repeat("word ", 100)
	got := parkCopyQuote("  Closing:\r\n@hanthor and @org/team <!-- hive:park-recheck --> see #885.  " + long + "\r\n\r\nSecond paragraph.")
	if strings.ContainsAny(got, "@\n\r") || strings.Contains(got, "<!--") || strings.Contains(got, "Second") {
		t.Fatalf("quote not sanitized: %q", got)
	}
	if !strings.HasPrefix(got, "Closing: hanthor and org/team see #885. word") || !strings.HasSuffix(got, " …") {
		t.Fatalf("quote = %q", got)
	}
	if n := len([]rune(got)); n > parkCopyQuoteMax {
		t.Fatalf("quote is %d runes, over %d", n, parkCopyQuoteMax)
	}
	if got := parkCopyQuote("Short."); got != "Short." {
		t.Fatalf("short quote = %q", got)
	}
}
