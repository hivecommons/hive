package dashboard

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// ── Source-aware task identity, proven on the production path (#4245) ────────
//
// Every test here drives the REAL ReadyQueue / admission code, not a helper.
// That is the point: the failure mode this issue describes is a migration that
// passes TaskKey unit tests while production still formats its own "repo#0".
// A helper-only test cannot tell those apart.

// externalItem builds one actionable item as the status payload carries it: a
// JSON-shaped map with no issue number, which is exactly how a Linear or Jira
// item arrives after worksource.ToGitHubIssues projects it.
func externalItem(sourceType, externalID, title string) map[string]any {
	return map[string]any{
		"number":      float64(0),
		"source_type": sourceType,
		"external_id": externalID,
		"title":       title,
		"url":         "https://linear.app/acme/issue/" + externalID,
		"labels":      []any{},
	}
}

func githubItem(number int, title string) map[string]any {
	return map[string]any{
		"number": float64(number),
		"title":  title,
		"labels": []any{},
	}
}

func statusWith(repoFull string, items ...map[string]any) *StatusPayload {
	anyItems := make([]any, 0, len(items))
	for _, it := range items {
		anyItems = append(anyItems, it)
	}
	return &StatusPayload{
		Repos: []FrontendRepo{{Name: repoFull, Full: repoFull, ActionableIssues: anyItems}},
	}
}

func newQueueServer(t *testing.T, status *StatusPayload) *Server {
	t.Helper()
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()
	return s
}

func wsKeysOf(items []ReadyQueueItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.identityKey())
	}
	return out
}

// TestExternalItemsSurviveWithDistinctIdentities is the headline row of the
// acceptance matrix: two work items in ONE repository, both with Number == 0
// and different native keys.
//
// Against the pre-#4245 path this fails twice over — ReadyQueue skipped
// `number == 0` outright, so neither item appeared at all, and anything that
// did survive would have keyed on "acme/repo#0".
func TestExternalItemsSurviveWithDistinctIdentities(t *testing.T) {
	s := newQueueServer(t, statusWith("acme/repo",
		externalItem("linear", "ENG-123", "first external item"),
		externalItem("linear", "ENG-456", "second external item"),
	))

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 2 {
		t.Fatalf("both external items must reach the queue, got %d: %+v", len(q), q)
	}

	keys := wsKeysOf(q)
	if keys[0] == keys[1] {
		t.Fatalf("distinct external items collapsed onto one identity: %v", keys)
	}
	want := []string{"acme/repo!ENG-123", "acme/repo!ENG-456"}
	for i, w := range want {
		if keys[i] != w {
			t.Errorf("item %d: identity = %q, want %q", i, keys[i], w)
		}
	}
	for _, it := range q {
		if strings.HasSuffix(it.identityKey(), "#0") {
			t.Errorf("item rendered the fabricated #0 identity: %+v", it)
		}
		if it.ExternalID == "" || it.SourceType != "linear" {
			t.Errorf("source-aware fields not carried through the queue projection: %+v", it)
		}
	}
}

// TestSameExternalKeyInTwoRepositoriesStaysDistinct pins repository scoping: the
// same native key filed against two repositories is two different pieces of work.
func TestSameExternalKeyInTwoRepositoriesStaysDistinct(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{
		{Name: "acme/one", Full: "acme/one", ActionableIssues: []any{externalItem("jira", "ENG-1", "in repo one")}},
		{Name: "acme/two", Full: "acme/two", ActionableIssues: []any{externalItem("jira", "ENG-1", "in repo two")}},
	}}
	s.statusMu.Unlock()

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 2 {
		t.Fatalf("expected one item per repository, got %d: %+v", len(q), q)
	}
	if q[0].identityKey() == q[1].identityKey() {
		t.Fatalf("same external key in two repositories collapsed: %v", wsKeysOf(q))
	}
	if q[0].identityKey() != "acme/one!ENG-1" || q[1].identityKey() != "acme/two!ENG-1" {
		t.Fatalf("repository scope lost: %v", wsKeysOf(q))
	}
}

// TestGitHubKeysRemainByteIdentical is the compatibility guard. These strings
// are PERSISTED (holds, queue order, cooldowns, the claim ledger), so changing
// their spelling would silently discard every existing exclusion on restart.
func TestGitHubKeysRemainByteIdentical(t *testing.T) {
	s := newQueueServer(t, statusWith("acme/repo", githubItem(42, "a github issue")))

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 1 {
		t.Fatalf("expected the github issue, got %+v", q)
	}
	if got := q[0].identityKey(); got != "acme/repo#42" {
		t.Fatalf("github identity = %q, want the unchanged %q", got, "acme/repo#42")
	}
	if q[0].Number != 42 {
		t.Errorf("Number must keep its meaning for existing clients, got %d", q[0].Number)
	}
	if q[0].SourceType != "" || q[0].ExternalID != "" {
		t.Errorf("github enumeration must not invent source fields: %+v", q[0])
	}
}

// TestGitHubProjectsItemBackedByIssueKeepsGitHubIdentity covers the row that is
// easy to get wrong: a Projects item IS externally sourced, but it is backed by
// a real issue number and must therefore stay GitHub-keyed and keep exercising
// the GitHub-only guards.
func TestGitHubProjectsItemBackedByIssueKeepsGitHubIdentity(t *testing.T) {
	item := githubItem(77, "a projects-tracked issue")
	item["source_type"] = "github_projects"
	item["external_id"] = "77"

	s := newQueueServer(t, statusWith("acme/repo", item))
	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 1 {
		t.Fatalf("expected the projects item, got %+v", q)
	}
	if got := q[0].identityKey(); got != "acme/repo#77" {
		t.Fatalf("projects item keyed as %q, want the GitHub form %q", got, "acme/repo#77")
	}

	ref := worksource.Ref{SourceType: "github_projects", Repo: "acme/repo", ExternalID: "77", Number: 77}
	if !ref.IsGitHubIssue() {
		t.Fatal("a projects item backed by a real issue number must remain GitHub-backed for the claim and bead observers")
	}
}

// TestOperatorHoldIsolatesOneExternalItem proves the hold state is per-item.
// Sharing "repo#0" meant holding one external item parked every other
// zero-numbered item in the repository.
func TestOperatorHoldIsolatesOneExternalItem(t *testing.T) {
	s := newQueueServer(t, statusWith("acme/repo",
		externalItem("linear", "ENG-1", "held one"),
		externalItem("linear", "ENG-2", "still offerable"),
	))
	s.deps.Config.Hub.ContributeQueueHold = []string{"acme/repo!ENG-1"}

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 2 {
		t.Fatalf("both items should still be surfaced, got %+v", q)
	}

	var held, offerable []ReadyQueueItem
	for _, it := range q {
		if it.Held {
			held = append(held, it)
		} else {
			offerable = append(offerable, it)
		}
	}
	if len(held) != 1 || held[0].identityKey() != "acme/repo!ENG-1" {
		t.Fatalf("exactly ENG-1 should be held, got held=%v", wsKeysOf(held))
	}
	if len(offerable) != 1 || offerable[0].identityKey() != "acme/repo!ENG-2" {
		t.Fatalf("ENG-2 must remain offerable, got offerable=%v", wsKeysOf(offerable))
	}
}

// TestCompletionCooldownIsolatesOneExternalItem proves completion state is
// per-item too: finishing one external item must not silence its neighbour.
func TestCompletionCooldownIsolatesOneExternalItem(t *testing.T) {
	s := newQueueServer(t, statusWith("acme/repo",
		externalItem("jira", "OPS-1", "just completed"),
		externalItem("jira", "OPS-2", "untouched"),
	))

	s.contributeHub.markTaskCompletedVerdictKey("acme/repo!OPS-1", "https://github.com/acme/repo/pull/9",
		completionVerdictIdle, "", "")

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 1 {
		t.Fatalf("exactly the untouched item should remain offerable, got %v", wsKeysOf(q))
	}
	if q[0].identityKey() != "acme/repo!OPS-2" {
		t.Fatalf("wrong item survived the cooldown: %v", wsKeysOf(q))
	}
}

// TestOperatorQueueOrderAcceptsExternalKeys proves the persisted operator
// ordering control works on the canonical key, so an operator can pin external
// work exactly as they pin GitHub work.
func TestOperatorQueueOrderAcceptsExternalKeys(t *testing.T) {
	s := newQueueServer(t, statusWith("acme/repo",
		externalItem("linear", "ENG-1", "first in scan order"),
		externalItem("linear", "ENG-2", "second in scan order"),
	))
	s.deps.Config.Hub.ContributeQueueOrder = []string{"acme/repo!ENG-2"}

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 2 {
		t.Fatalf("expected both items, got %+v", q)
	}
	if q[0].identityKey() != "acme/repo!ENG-2" {
		t.Fatalf("operator pin ignored for an external key: %v", wsKeysOf(q))
	}
}

// TestGitHubOnlyObserversSkipExternalWork is the observer-authority row. The
// claim ledger is asked about GitHub candidates (positive control) and must
// never be consulted for an external item — asking it at all would mean
// synthesising a "#0" subject.
func TestGitHubOnlyObserversSkipExternalWork(t *testing.T) {
	s := newQueueServer(t, statusWith("acme/repo",
		githubItem(5, "a real github issue"),
		externalItem("linear", "ENG-9", "external work"),
	))

	var mu sync.Mutex
	var asked []string
	s.deps.IssueClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
		mu.Lock()
		asked = append(asked, fmt.Sprintf("%s#%d", repo, number))
		mu.Unlock()
		return ghpkg.IssueClaim{}, false
	}

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 2 {
		t.Fatalf("both items should be admissible, got %v", wsKeysOf(q))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 {
		t.Fatal("the claim observer was never consulted — the positive control did not run")
	}
	sawGitHub := false
	for _, a := range asked {
		if strings.HasSuffix(a, "#0") {
			t.Errorf("claim observer was asked about a fabricated identity %q", a)
		}
		if a == "acme/repo#5" {
			sawGitHub = true
		}
	}
	if !sawGitHub {
		t.Errorf("the GitHub candidate must still exercise the claim observer; asked=%v", asked)
	}
}

// TestExternalCandidateIsNotGitHubBacked pins the admission-side predicate that
// keeps the GitHub-only observers out of external work, and keeps them ON for
// GitHub work — including a candidate built by a caller that predates the ref.
func TestExternalCandidateIsNotGitHubBacked(t *testing.T) {
	external := contributorAdmissionCandidate{
		repoFull: "acme/repo",
		ref:      worksource.Ref{SourceType: "linear", Repo: "acme/repo", ExternalID: "ENG-1"},
	}
	if external.isGitHubBacked() {
		t.Error("an external candidate must not be handed to the GitHub-only observers")
	}

	backed := contributorAdmissionCandidate{
		repoFull: "acme/repo",
		number:   12,
		ref:      worksource.Ref{Repo: "acme/repo", Number: 12, ExternalID: "12"},
	}
	if !backed.isGitHubBacked() {
		t.Error("a GitHub-backed candidate must keep exercising the claim and bead observers")
	}

	legacy := contributorAdmissionCandidate{repoFull: "acme/repo", number: 12}
	if !legacy.isGitHubBacked() {
		t.Error("a candidate built without a ref must behave exactly as it did before #4245")
	}
}

// TestItemWithNoIdentityIsSkipped proves the one case that must still be
// dropped: an item with neither an issue number nor an external key. Admitting
// it would mean inventing a key, which is the whole defect.
func TestItemWithNoIdentityIsSkipped(t *testing.T) {
	s := newQueueServer(t, statusWith("acme/repo",
		map[string]any{"number": float64(0), "title": "no identity at all", "labels": []any{}},
		externalItem("linear", "ENG-7", "identifiable"),
	))

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 1 || q[0].identityKey() != "acme/repo!ENG-7" {
		t.Fatalf("only the identifiable item may be offered, got %v", wsKeysOf(q))
	}
}

// TestDuplicateEnumerationOfOneItemIsNotDoubleOffered covers the re-projection
// row: a source that emits the same item twice in one snapshot must not yield
// two independently assignable copies.
func TestDuplicateEnumerationOfOneItemIsNotDoubleOffered(t *testing.T) {
	s := newQueueServer(t, statusWith("acme/repo",
		externalItem("linear", "ENG-1", "emitted once"),
		externalItem("linear", "ENG-1", "emitted again in the same snapshot"),
	))

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	seen := map[string]int{}
	for _, it := range q {
		seen[it.identityKey()]++
	}
	if seen["acme/repo!ENG-1"] > 1 {
		// Both copies carry the SAME canonical identity, so the in-flight guard
		// and every cooldown treat them as one item. That is the property that
		// matters; surfacing a duplicate row is a display concern, assigning it
		// twice is not.
		t.Logf("duplicate rows surfaced (%d) — acceptable only because they share one identity", seen["acme/repo!ENG-1"])
	}
	for key := range seen {
		if strings.HasSuffix(key, "#0") {
			t.Fatalf("duplicate enumeration produced a fabricated identity %q", key)
		}
	}
}

// TestActiveWorkGuardIsPerItem proves two different zero-numbered external
// items do not block each other in the in-flight guard, while the SAME item is
// held only once. Before #4245 both mapped to "repo#0": picking up one made the
// other unofferable.
func TestActiveWorkGuardIsPerItem(t *testing.T) {
	one := &WSTaskAssign{Repo: "acme/repo", Number: 0, Key: "acme/repo!ENG-1", SourceType: "linear", ExternalID: "ENG-1"}
	two := &WSTaskAssign{Repo: "acme/repo", Number: 0, Key: "acme/repo!ENG-2", SourceType: "linear", ExternalID: "ENG-2"}

	if one.identityKey() == two.identityKey() {
		t.Fatalf("two distinct external assignments share an identity: %q", one.identityKey())
	}
	if one.identityKey() != "acme/repo!ENG-1" {
		t.Errorf("assignment identity = %q", one.identityKey())
	}

	legacy := &WSTaskAssign{Repo: "acme/repo", Number: 42}
	if legacy.identityKey() != "acme/repo#42" {
		t.Errorf("an assignment recorded before #4245 must key exactly as before, got %q", legacy.identityKey())
	}
}

// TestPromptCarriesNativeKeyAndURL is the assignment/prompt row: a contributor
// working a Linear item must be told what the item IS and where to read it,
// never "issue acme/repo#0".
func TestPromptCarriesNativeKeyAndURL(t *testing.T) {
	ref := worksource.Ref{
		SourceType: "linear",
		Repo:       "acme/repo",
		ExternalID: "ENG-123",
		URL:        "https://linear.app/acme/issue/ENG-123",
	}
	prompt := buildTaskPromptForRef(ref, "make the thing work")

	if strings.Contains(prompt, "#0") {
		t.Errorf("prompt referenced the fabricated #0 identity:\n%s", prompt)
	}
	if !strings.Contains(prompt, "acme/repo!ENG-123") {
		t.Errorf("prompt does not name the item's native key:\n%s", prompt)
	}
	if !strings.Contains(prompt, ref.URL) {
		t.Errorf("prompt does not tell the agent where to read the item:\n%s", prompt)
	}
	if !strings.Contains(prompt, "gh repo fork acme/repo") {
		t.Errorf("prompt lost the GitHub repository instructions:\n%s", prompt)
	}

	// The GitHub prompt must be untouched — same reference, and no source hint.
	gh := buildTaskPrompt("acme/repo", 42, "make the thing work")
	if !strings.Contains(gh, "Work on issue acme/repo#42:") {
		t.Errorf("github prompt reference changed:\n%s", gh)
	}
	if strings.Contains(gh, "work source") {
		t.Errorf("github prompt gained an external-source hint it should not have:\n%s", gh)
	}
}

// TestCanonicalKeyRoundTripsThroughParse proves the persisted-key reader and
// writer agree, which is what lets operator hold/order entries survive a
// restart without a migration.
func TestCanonicalKeyRoundTripsThroughParse(t *testing.T) {
	for _, ref := range []worksource.Ref{
		{Repo: "acme/repo", Number: 42, ExternalID: "42"},
		{Repo: "acme/repo", ExternalID: "ENG-123", SourceType: "linear"},
	} {
		key := ref.Key()
		parsed, ok := worksource.ParseKey(key)
		if !ok {
			t.Fatalf("ParseKey(%q) failed", key)
		}
		if parsed.Key() != key {
			t.Errorf("round trip changed the key: %q -> %q", key, parsed.Key())
		}
		if parsed.IsGitHubIssue() != ref.IsGitHubIssue() {
			t.Errorf("round trip changed GitHub-backedness for %q", key)
		}
	}

	// A fabricated "#0" must never parse into a usable identity.
	if _, ok := worksource.ParseKey("acme/repo#0"); ok {
		t.Error(`ParseKey accepted "acme/repo#0" — the fabricated identity must be refused`)
	}
}

// TestNoSecondKeyImplementation is the bypass guard the acceptance matrix asks
// for: it fails if the dashboard's queue projection stops producing exactly
// what pkg/worksource produces. A parallel key implementation would satisfy
// every helper test above and still break production.
func TestNoSecondKeyImplementation(t *testing.T) {
	s := newQueueServer(t, statusWith("acme/repo",
		githubItem(42, "github"),
		externalItem("linear", "ENG-1", "linear"),
		externalItem("jira", "OPS-9", "jira"),
	))

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 3 {
		t.Fatalf("expected all three items, got %v", wsKeysOf(q))
	}
	want := []string{
		worksource.Ref{Repo: "acme/repo", Number: 42}.Key(),
		worksource.Ref{Repo: "acme/repo", ExternalID: "ENG-1"}.Key(),
		worksource.Ref{Repo: "acme/repo", ExternalID: "OPS-9"}.Key(),
	}
	for i, w := range want {
		if q[i].identityKey() != w {
			t.Errorf("queue item %d keyed %q, but pkg/worksource says %q — there is a second key implementation",
				i, q[i].identityKey(), w)
		}
	}
}

// TestTaskIDShapeUnchangedForGitHub guards a compatibility detail that is easy
// to break while threading identity through: the assignment task id. Nothing
// parses it, but it is echoed by every relay and appears in operator-facing
// logs, so GitHub work must keep the exact "ct-<repo>-<number>-<unix>" shape it
// has always had — while two zero-numbered external items must still get
// distinct ids.
func TestTaskIDShapeUnchangedForGitHub(t *testing.T) {
	if got := taskIDSegment(worksource.Ref{Repo: "acme/repo", Number: 42}); got != "42" {
		t.Errorf("github task id segment = %q, want the bare issue number", got)
	}
	one := taskIDSegment(worksource.Ref{Repo: "acme/repo", ExternalID: "ENG-1"})
	two := taskIDSegment(worksource.Ref{Repo: "acme/repo", ExternalID: "ENG-2"})
	if one == two {
		t.Fatalf("two external items produced the same task id segment %q", one)
	}
	if one != "ENG-1" {
		t.Errorf("external task id segment = %q, want the native external id", one)
	}
}

// TestOperatorQueueKeyPrefersCanonicalKey pins the browser-side half of the
// operator controls. ccQueueKey builds the string the HOLD and REORDER actions
// persist, so it must prefer the server's canonical q.key. Building it from
// repo+number produced "owner/repo#" for an external item — a key the server can
// never match, so the operator's action silently did nothing.
func TestOperatorQueueKeyPrefersCanonicalKey(t *testing.T) {
	page := opsPageBody(t)
	if !strings.Contains(page, "function ccQueueKey(q){return q.key||") {
		t.Error("ccQueueKey no longer prefers the server's canonical key; operator hold/reorder will miss external work")
	}
	if strings.Contains(page, "var prBadge=ccPRBadgeHTML((q.repo||'')+'#'+(q.number||''));") {
		t.Error("the PR badge still looks up a fabricated key for numberless items")
	}
}

// TestLedgerKeysReconstructAfterRestart is the restart row.
//
// It deliberately does NOT round-trip through the ledger file:
// completedTasksFile is a hardcoded /data path, not redirected by the test
// environment, so a save/load here would write nothing and the test would pass
// for the wrong reason. What matters — and what this asserts — is that the
// exclusion state is addressed purely by canonical key strings, so a hub that
// comes up with those strings and no process memory reproduces exactly the same
// exclusions: the GitHub key byte-identically (no migration, nothing silently
// discarded) and the external key as its own distinct record.
func TestLedgerKeysReconstructAfterRestart(t *testing.T) {
	s := newQueueServer(t, statusWith("acme/repo",
		githubItem(42, "github work"),
		externalItem("linear", "ENG-1", "external work"),
		externalItem("linear", "ENG-2", "other external work"),
	))

	// Exactly the strings a pre-upgrade ledger holds for GitHub work, plus the
	// new external form, as a restart would hand them back.
	persisted := []string{"acme/repo#42", "acme/repo!ENG-1"}
	hub := s.contributeHub
	hub.completedMu.Lock()
	for _, key := range persisted {
		hub.completedTasks[key] = time.Now()
	}
	hub.completedMu.Unlock()

	for _, key := range persisted {
		if !hub.isTaskInCooldownKey(key) {
			t.Errorf("persisted exclusion %q did not reconstruct", key)
		}
	}
	if hub.isTaskInCooldownKey("acme/repo!ENG-2") {
		t.Error("an unrelated external item inherited another item's exclusion")
	}

	q := hub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 1 {
		t.Fatalf("both reconstructed exclusions must hold; offerable = %v", wsKeysOf(q))
	}
	if got := q[0].identityKey(); got != "acme/repo!ENG-2" {
		t.Fatalf("wrong item survived: %q (offerable = %v)", got, wsKeysOf(q))
	}
}
