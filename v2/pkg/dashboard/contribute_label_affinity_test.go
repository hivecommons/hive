package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// ── Label-affinity (#2637) tests ───────────────────────────────────────────────
//
// These cover the matching/sort logic (pure, no HTTP), the personalised queue
// endpoint, and the contributor-owned interests endpoint. The invariant under
// test throughout is the SOFT-SIGNAL contract: matches are highlighted and floated
// to the front, but nothing is ever removed — a no-interests viewer and a
// no-labels issue are never starved.

// qItem is a tiny helper to build a ReadyQueueItem with labels for the pure tests.
func qItem(repo string, number int, labels ...string) ReadyQueueItem {
	return ReadyQueueItem{Repo: repo, Number: number, Title: "issue", Labels: labels}
}

// keysOf renders the queue as "repo#number" keys so order assertions read clearly.
func keysOf(items []ReadyQueueItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = itKey(it)
	}
	return out
}

func itKey(it ReadyQueueItem) string {
	return it.Repo + "#" + strconv.Itoa(it.Number)
}

// TestIssueMatchesInterests_ExactCaseInsensitive proves the matching rule: exact
// label-name match, case-insensitive and whitespace-trimmed — NOT substring.
func TestIssueMatchesInterests_ExactCaseInsensitive(t *testing.T) {
	set := labelInterestSet([]string{"nvidia", "  Good First Issue "})

	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"exact match", []string{"nvidia"}, true},
		{"case-insensitive match", []string{"NVIDIA"}, true},
		{"whitespace-trimmed interest matches", []string{"good first issue"}, true},
		{"no match", []string{"amd"}, false},
		{"substring must NOT match", []string{"nvidia-docs"}, false},
		{"issue with no labels never matches", nil, false},
		{"one of several labels matches", []string{"amd", "nvidia"}, true},
	}
	for _, c := range cases {
		if got := issueMatchesInterests(c.labels, set); got != c.want {
			t.Errorf("%s: issueMatchesInterests(%v) = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
}

// TestIssueMatchesInterests_EmptyInterestsNeverMatches proves an empty interest set
// matches nothing (the seed of the anti-starvation guarantee).
func TestIssueMatchesInterests_EmptyInterestsNeverMatches(t *testing.T) {
	empty := labelInterestSet(nil)
	if issueMatchesInterests([]string{"nvidia"}, empty) {
		t.Fatal("empty interest set must not match any label")
	}
	if issueMatchesInterests(nil, empty) {
		t.Fatal("empty interest set + no labels must not match")
	}
}

// TestPersonalizeQueue_FloatsMatchesStably proves matching issues are tagged and
// STABLY floated to the front, with both the matching and non-matching groups
// keeping their prior relative order (a pure view reorder).
func TestPersonalizeQueue_FloatsMatchesStably(t *testing.T) {
	q := []ReadyQueueItem{
		qItem("r", 1, "amd"),
		qItem("r", 2, "nvidia"),
		qItem("r", 3), // no labels
		qItem("r", 4, "docs", "nvidia"),
		qItem("r", 5, "amd"),
	}
	personalizeQueueByInterests(q, []string{"nvidia"})

	got := keysOf(q)
	want := []string{"r#2", "r#4", "r#1", "r#3", "r#5"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	// The two matching items carry the flag; the rest do not.
	if !q[0].MatchesInterest || !q[1].MatchesInterest {
		t.Error("floated items must be tagged MatchesInterest")
	}
	for _, it := range q[2:] {
		if it.MatchesInterest {
			t.Errorf("non-matching item %s must not be tagged", itKey(it))
		}
	}
}

// TestPersonalizeQueue_EmptyInterestsNoStarvation proves the anti-starvation
// contract on the sort path: a viewer with no interests gets the queue UNCHANGED
// (same items, same order) — nothing is dropped or reordered.
func TestPersonalizeQueue_EmptyInterestsNoStarvation(t *testing.T) {
	orig := []ReadyQueueItem{
		qItem("r", 1, "amd"),
		qItem("r", 2, "nvidia"),
		qItem("r", 3),
	}
	q := append([]ReadyQueueItem(nil), orig...)
	personalizeQueueByInterests(q, nil)

	if len(q) != len(orig) {
		t.Fatalf("no-interests must not change queue length: got %d want %d", len(q), len(orig))
	}
	for i := range orig {
		if itKey(q[i]) != itKey(orig[i]) {
			t.Fatalf("no-interests must preserve order: got %v want %v", keysOf(q), keysOf(orig))
		}
		if q[i].MatchesInterest {
			t.Errorf("no-interests must not tag any item (%s tagged)", itKey(q[i]))
		}
	}
}

// TestPersonalizeQueue_NoLabelIssueStaysEligible proves a label-less issue is never
// removed by personalisation — it just sorts behind matches, still present.
func TestPersonalizeQueue_NoLabelIssueStaysEligible(t *testing.T) {
	q := []ReadyQueueItem{
		qItem("r", 1), // no labels
		qItem("r", 2, "nvidia"),
	}
	personalizeQueueByInterests(q, []string{"nvidia"})
	if len(q) != 2 {
		t.Fatalf("no item may be dropped: got %d want 2", len(q))
	}
	found := false
	for _, it := range q {
		if it.Number == 1 {
			found = true
			if it.MatchesInterest {
				t.Error("label-less issue must not be tagged as a match")
			}
		}
	}
	if !found {
		t.Fatal("label-less issue was starved (removed) — anti-starvation violated")
	}
}

// TestSanitizeLabelInterests normalises, dedupes, caps, and drops bad entries.
func TestSanitizeLabelInterests(t *testing.T) {
	in := []string{"NVIDIA", "  nvidia ", "", "   ", "amd", "AMD"}
	got := sanitizeLabelInterests(in)
	want := []string{"nvidia", "amd"}
	if len(got) != len(want) {
		t.Fatalf("sanitize = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sanitize = %v, want %v", got, want)
		}
	}

	// Cap: more than maxLabelInterests entries are truncated.
	big := make([]string, maxLabelInterests+10)
	for i := range big {
		big[i] = "label-" + strconv.Itoa(i)
	}
	if n := len(sanitizeLabelInterests(big)); n != maxLabelInterests {
		t.Fatalf("cap not enforced: got %d want %d", n, maxLabelInterests)
	}

	// Over-length entries are dropped.
	long := make([]byte, maxLabelInterestLen+1)
	for i := range long {
		long[i] = 'x'
	}
	if n := len(sanitizeLabelInterests([]string{string(long)})); n != 0 {
		t.Fatalf("over-length label not dropped: got %d", n)
	}
}

// ── HTTP: personalised queue + interests endpoints ─────────────────────────────

// seedLabeledActionable builds a status where each issue carries the given label
// set, so the personalised queue endpoint has real labels to match against.
func seedLabeledActionable(repo string, labelsByNumber map[int][]string) *StatusPayload {
	issues := make([]any, 0, len(labelsByNumber))
	// Deterministic scan order: numbers ascending.
	for n := 1; n <= len(labelsByNumber); n++ {
		labs, ok := labelsByNumber[n]
		if !ok {
			continue
		}
		anyLabs := make([]any, len(labs))
		for i, l := range labs {
			anyLabs[i] = l
		}
		issues = append(issues, map[string]any{
			"number": float64(n),
			"title":  "issue " + strconv.Itoa(n),
			"labels": anyLabs,
		})
	}
	return &StatusPayload{Repos: []FrontendRepo{{Name: repo, Full: repo, ActionableIssues: issues}}}
}

func seedInterestProfile(t *testing.T, username string, interests []string) {
	t.Helper()
	p := &ContributorProfile{
		GitHubUsername: username,
		ContributorID:  "c-" + username,
		TrustTier:      "newcomer",
		RegisteredAt:   time.Now().UTC().Format(time.RFC3339),
		LabelInterests: interests,
	}
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("save profile: %v", err)
	}
}

// TestQueueEndpoint_PersonalisedForViewer proves the queue endpoint floats a
// signed-in contributor's interested issues to the front and echoes their
// interests, while an ANONYMOUS caller gets the plain shared order (no starvation,
// no personalisation leak).
func TestQueueEndpoint_PersonalisedForViewer(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	s.statusMu.Lock()
	s.status = seedLabeledActionable("projectbluefin/common", map[int][]string{
		1: {"amd"},
		2: {"nvidia"},
		3: {}, // no labels
	})
	s.statusMu.Unlock()

	seedInterestProfile(t, "gpuperson", []string{"nvidia"})

	// Signed-in viewer: nvidia issue floats first, interests echoed.
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/queue", nil)
	req.Header.Set("X-Hive-User", "gpuperson")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Queue     []ReadyQueueItem `json:"queue"`
		Interests []string         `json:"interests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Queue) != 3 {
		t.Fatalf("no item may be dropped by personalisation: got %d want 3", len(resp.Queue))
	}
	if resp.Queue[0].Number != 2 || !resp.Queue[0].MatchesInterest {
		t.Fatalf("nvidia issue must float first and be tagged; got %+v", resp.Queue[0])
	}
	if len(resp.Interests) != 1 || resp.Interests[0] != "nvidia" {
		t.Fatalf("interests must be echoed: got %v", resp.Interests)
	}

	// Anonymous viewer: plain shared scan order (1,2,3), no matches_interest.
	anon := httptest.NewRequest(http.MethodGet, "/api/contribute/queue", nil)
	aw := httptest.NewRecorder()
	s.mux.ServeHTTP(aw, anon)
	var ar struct {
		Queue     []ReadyQueueItem `json:"queue"`
		Interests []string         `json:"interests"`
	}
	if err := json.Unmarshal(aw.Body.Bytes(), &ar); err != nil {
		t.Fatalf("invalid anon JSON: %v", err)
	}
	if len(ar.Queue) != 3 {
		t.Fatalf("anon queue must have all 3 items, got %d", len(ar.Queue))
	}
	if ar.Queue[0].Number != 1 {
		t.Fatalf("anon queue must be plain scan order (1 first), got %d", ar.Queue[0].Number)
	}
	for _, it := range ar.Queue {
		if it.MatchesInterest {
			t.Error("anon queue must never carry matches_interest")
		}
	}
	if len(ar.Interests) != 0 {
		t.Errorf("anon response must not echo interests, got %v", ar.Interests)
	}
}

// seedInterestProfileAtFile writes a contributor profile to an EXPLICIT filename
// that need not equal the GitHubUsername. This is what lets a test force a
// fast-path (exact-filename) miss in findContributor on ANY filesystem —
// including case-insensitive ones like macOS APFS where a plain case-mismatched
// filename would still be found by the O(1) file read — so the case-insensitive
// slow-path match is genuinely exercised.
func seedInterestProfileAtFile(t *testing.T, fileBase, githubUsername string, interests []string) {
	t.Helper()
	p := &ContributorProfile{
		GitHubUsername: githubUsername,
		ContributorID:  "c-" + fileBase,
		TrustTier:      "newcomer",
		RegisteredAt:   time.Now().UTC().Format(time.RFC3339),
		LabelInterests: interests,
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	ensureDir(getContributorsDir())
	if err := os.WriteFile(filepath.Join(getContributorsDir(), fileBase+".json"), data, 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}

// TestQueueEndpoint_InterestsAttachedCaseInsensitiveViewer proves the queue
// endpoint attaches the "interests" array (which un-hides the "My label
// interests" editor client-side) for a signed-in contributor even when the
// viewer's resolved GitHub login differs in CASE from the stored profile's
// GitHubUsername. The Leaderboard's "YOU" badge already matches the viewer
// case-insensitively, so a leaderboard-present contributor whose OAuth login
// case differs must still get their interests echoed here — otherwise the
// editor stays invisible for exactly the users who have a profile. It also
// re-asserts the anonymous viewer gets NO interests key (editor stays hidden).
//
// The profile is written to a filename ("c-clubanderson.json") that does NOT
// case-fold-match the resolved viewer ("clubanderson"), guaranteeing the
// findContributor fast path (exact-filename file read) MISSES on every
// filesystem — so only the case-insensitive GitHubUsername slow-path match can
// resolve the profile. Without that match, interests are never attached.
func TestQueueEndpoint_InterestsAttachedCaseInsensitiveViewer(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	s.statusMu.Lock()
	s.status = seedLabeledActionable("projectbluefin/common", map[int][]string{
		1: {"amd"},
		2: {"nvidia"},
		3: {},
	})
	s.statusMu.Unlock()

	// Profile GitHubUsername is "ClubAnderson" (the case that first registered),
	// stored at file "c-clubanderson.json" so the fast-path exact-filename lookup
	// for the resolved viewer "clubanderson" misses. The viewer is resolved as
	// "clubanderson" — only a case-insensitive username match can find them.
	seedInterestProfileAtFile(t, "c-clubanderson", "ClubAnderson", []string{"nvidia"})

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/queue", nil)
	req.Header.Set("X-Hive-User", "clubanderson")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Queue     []ReadyQueueItem `json:"queue"`
		Interests *[]string        `json:"interests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// The interests KEY must be present (a real array) — that is what un-hides
	// the editor client-side (ccApplyInterestsFromResponse returns early unless
	// interests is an array).
	if resp.Interests == nil {
		t.Fatalf("interests must be attached for a case-mismatched signed-in contributor; got body %s", w.Body.String())
	}
	if len(*resp.Interests) != 1 || (*resp.Interests)[0] != "nvidia" {
		t.Fatalf("interests must echo the stored set: got %v", *resp.Interests)
	}
	// Personalisation still applies: the nvidia issue floats first and is tagged.
	if len(resp.Queue) != 3 {
		t.Fatalf("no item may be dropped by personalisation: got %d want 3", len(resp.Queue))
	}
	if resp.Queue[0].Number != 2 || !resp.Queue[0].MatchesInterest {
		t.Fatalf("nvidia issue must float first and be tagged; got %+v", resp.Queue[0])
	}

	// Anonymous viewer: NO interests key at all, so the editor stays hidden.
	anon := httptest.NewRequest(http.MethodGet, "/api/contribute/queue", nil)
	aw := httptest.NewRecorder()
	s.mux.ServeHTTP(aw, anon)
	var ar struct {
		Interests *[]string `json:"interests"`
	}
	if err := json.Unmarshal(aw.Body.Bytes(), &ar); err != nil {
		t.Fatalf("invalid anon JSON: %v", err)
	}
	if ar.Interests != nil {
		t.Errorf("anonymous viewer must not receive an interests key (editor must stay hidden), got %v", *ar.Interests)
	}
}

// TestQueueEndpoint_NoInterestsNoStarvation proves a signed-in contributor who set
// NO interests still gets the full queue in the plain shared order.
func TestQueueEndpoint_NoInterestsNoStarvation(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	s.statusMu.Lock()
	s.status = seedLabeledActionable("projectbluefin/common", map[int][]string{
		1: {"amd"}, 2: {"nvidia"}, 3: {},
	})
	s.statusMu.Unlock()
	seedInterestProfile(t, "noprefs", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/queue", nil)
	req.Header.Set("X-Hive-User", "noprefs")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var resp struct {
		Queue []ReadyQueueItem `json:"queue"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Queue) != 3 {
		t.Fatalf("no-interests contributor must see all 3 items, got %d", len(resp.Queue))
	}
	if resp.Queue[0].Number != 1 {
		t.Fatalf("no-interests contributor must see plain scan order, got first=%d", resp.Queue[0].Number)
	}
}

// TestInterestsEndpoint_GetPutRoundTrip proves a contributor can read and replace
// their OWN interests, and that the store persists a sanitised set.
func TestInterestsEndpoint_GetPutRoundTrip(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	seedInterestProfile(t, "editor", nil)

	// PUT a messy set; server sanitises (lower-case, dedupe).
	put := httptest.NewRequest(http.MethodPut, "/api/contribute/interests",
		bytes.NewBufferString(`{"interests":["NVIDIA"," nvidia ","amd"]}`))
	put.Header.Set("Content-Type", "application/json")
	put.Header.Set("X-Hive-User", "editor")
	pw := httptest.NewRecorder()
	s.mux.ServeHTTP(pw, put)
	if pw.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", pw.Code, pw.Body.String())
	}
	var pr struct {
		Interests []string `json:"interests"`
	}
	_ = json.Unmarshal(pw.Body.Bytes(), &pr)
	if len(pr.Interests) != 2 || pr.Interests[0] != "nvidia" || pr.Interests[1] != "amd" {
		t.Fatalf("PUT must return sanitised set, got %v", pr.Interests)
	}

	// It persisted to the profile store.
	prof := findContributor("editor")
	if prof == nil || len(prof.LabelInterests) != 2 {
		t.Fatalf("interests not persisted to profile: %+v", prof)
	}

	// GET reads them back.
	get := httptest.NewRequest(http.MethodGet, "/api/contribute/interests", nil)
	get.Header.Set("X-Hive-User", "editor")
	gw := httptest.NewRecorder()
	s.mux.ServeHTTP(gw, get)
	if gw.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", gw.Code)
	}
	var gr struct {
		Interests []string `json:"interests"`
	}
	_ = json.Unmarshal(gw.Body.Bytes(), &gr)
	if len(gr.Interests) != 2 {
		t.Fatalf("GET must return the 2 stored interests, got %v", gr.Interests)
	}
}

// TestInterestsEndpoint_AnonymousRejected proves an anonymous caller cannot read or
// write interests (identity is server-side, never a body param).
func TestInterestsEndpoint_AnonymousRejected(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodPut, "/api/contribute/interests",
		bytes.NewBufferString(`{"interests":["nvidia"]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous PUT must be 401, got %d", w.Code)
	}
}

// TestInterestsEndpoint_NoProfileForbidden proves a signed-in caller WITHOUT a
// contributor profile is told to register first (403), not silently no-op'd.
func TestInterestsEndpoint_NoProfileForbidden(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodPut, "/api/contribute/interests",
		bytes.NewBufferString(`{"interests":["nvidia"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "stranger")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("no-profile PUT must be 403, got %d: %s", w.Code, w.Body.String())
	}
}
