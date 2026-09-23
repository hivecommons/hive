package hub

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tests for kubestellar/hive#5691 phase 1: repo-ownership overlap detection.
//
// The RFC's finding is that nothing stops two spokes managing the same repo,
// which puts two fleets on one backlog opening competing PRs — the exact thing
// a subject-scoped split exists to prevent. The hub already receives every
// spoke's repo list on every heartbeat; the data was there, unused.
//
// The cases that matter are the ones where a real overlap is INVISIBLE to a
// naive compare, because two spokes are configured separately and so are
// exactly the pair most likely to spell the same repo differently.

func overlapHive(id, name, org, primary string, repos ...string) RegistryEntry {
	return RegistryEntry{ID: id, Name: name, Org: org, PrimaryRepo: primary, Repos: repos}
}

// The headline case: two spokes, one repo, found.
func TestRepoOverlap_TwoSpokesOneRepo(t *testing.T) {
	got := computeRepoOverlaps([]RegistryEntry{
		overlapHive("h-hive", "hive", "tunaos", "os", "os", "images", "docs"),
		overlapHive("h-reef", "reef", "tunaos", "apps", "apps", "docs"),
	})
	if len(got) != 1 {
		t.Fatalf("want exactly one overlap, got %d: %+v", len(got), got)
	}
	if got[0].Repo != "tunaos/docs" {
		t.Errorf("overlap repo = %q, want tunaos/docs", got[0].Repo)
	}
	if got[0].Host != publicGitHubHost {
		t.Errorf("host = %q, want %q", got[0].Host, publicGitHubHost)
	}
	if len(got[0].Hives) != 2 {
		t.Fatalf("want both hives named, got %+v", got[0].Hives)
	}
	// Ordered by id so a diff of two responses is meaningful.
	if got[0].Hives[0].ID != "h-hive" || got[0].Hives[1].ID != "h-reef" {
		t.Errorf("hives not ordered by id: %+v", got[0].Hives)
	}
	if got[0].Hives[0].Name != "hive" || got[0].Hives[1].Name != "reef" {
		t.Errorf("display names not carried: %+v", got[0].Hives)
	}
}

func TestRunFanoutOverlapsFromRegistryUsesHubFixture(t *testing.T) {
	got := RunFanoutOverlapsFromRegistry([]RegistryEntry{
		overlapHive("h-hive", "hive", "tunaos", "os", "os", "images", "docs"),
		overlapHive("h-reef", "reef", "tunaos", "apps", "apps", "docs"),
		overlapHive("h-clean", "clean", "tunaos", "cli", "cli"),
	}, []string{"tunaos/docs"})
	if len(got) != 1 {
		t.Fatalf("want one fan-out overlap, got %+v", got)
	}
	if got[0].Repo != "tunaos/docs" || len(got[0].Hives) != 2 {
		t.Fatalf("fan-out overlap = %+v", got[0])
	}
	if clean := RunFanoutOverlapsFromRegistry([]RegistryEntry{
		overlapHive("h-hive", "hive", "tunaos", "os", "os"),
		overlapHive("h-reef", "reef", "tunaos", "apps", "apps"),
	}, []string{"tunaos/docs"}); clean != nil {
		t.Fatalf("clean registry overlap = %+v", clean)
	}
	gheA := overlapHive("h-ghe-a", "ghe-a", "acme", "api", "ui")
	gheA.GitHubHost = "github.ibm.com"
	gheB := overlapHive("h-ghe-b", "ghe-b", "acme", "docs", "ui")
	gheB.GitHubHost = "github.ibm.com"
	if got := RunFanoutOverlapsFromRegistry([]RegistryEntry{gheA, gheB}, []string{"acme/ui"}); len(got) != 1 {
		t.Fatalf("unqualified run repo should match enterprise-host overlap, got %+v", got)
	}
}

// A clean fleet returns nil, not an empty slice, so the JSON field is omitted
// entirely rather than rendering an empty section.
func TestRepoOverlap_CleanFleetReportsNothing(t *testing.T) {
	got := computeRepoOverlaps([]RegistryEntry{
		overlapHive("h-a", "a", "tunaos", "os", "os", "images"),
		overlapHive("h-b", "b", "tunaos", "apps", "apps", "docs"),
	})
	if got != nil {
		t.Fatalf("a fleet with no shared repo must report nil, got %+v", got)
	}
}

// A hive is never in conflict with itself. Listing the primary repo in repos[]
// is the NORMAL shape, so getting this wrong would flag the entire fleet.
func TestRepoOverlap_SelfDuplicationIsNotAConflict(t *testing.T) {
	got := computeRepoOverlaps([]RegistryEntry{
		overlapHive("h-a", "a", "tunaos", "os", "os", "os", "tunaos/os", "https://github.com/tunaos/os"),
	})
	if got != nil {
		t.Fatalf("one hive spelling one repo four ways is one claim, got %+v", got)
	}
}

// The case a naive string compare misses, and the one most likely to occur:
// two separately-configured spokes spelling the same repo differently.
func TestRepoOverlap_MatchesAcrossSpellingsAndCase(t *testing.T) {
	got := computeRepoOverlaps([]RegistryEntry{
		overlapHive("h-a", "a", "tunaos", "os", "docs"),
		overlapHive("h-b", "b", "TunaOS", "apps", "TunaOS/Docs"),
		overlapHive("h-c", "c", "tunaos", "pkg", "https://github.com/tunaos/docs.git"),
	})
	if len(got) != 1 {
		t.Fatalf("bare, owner-qualified and URL spellings are one repo, got %d: %+v", len(got), got)
	}
	if len(got[0].Hives) != 3 {
		t.Errorf("all three spokes claim it: %+v", got[0].Hives)
	}
}

// Host is part of the identity: same owner/repo on different GitHub instances
// is different work, and flagging it would be a false alarm on any org running
// both a public and an enterprise fleet.
func TestRepoOverlap_DifferentHostsAreNotAConflict(t *testing.T) {
	pub := overlapHive("h-pub", "public", "acme", "ui", "ui")
	ghe := overlapHive("h-ghe", "enterprise", "acme", "ui", "ui")
	ghe.GitHubHost = "github.ibm.com"

	if got := computeRepoOverlaps([]RegistryEntry{pub, ghe}); got != nil {
		t.Fatalf("acme/ui on github.com and github.ibm.com are different repos, got %+v", got)
	}

	// ...but two spokes on the SAME enterprise host still conflict, so the host
	// check is not just suppressing everything.
	ghe2 := overlapHive("h-ghe2", "enterprise-2", "acme", "api", "ui")
	ghe2.GitHubHost = "github.ibm.com"
	got := computeRepoOverlaps([]RegistryEntry{ghe, ghe2})
	if len(got) != 1 || got[0].Host != "github.ibm.com" {
		t.Fatalf("same-host overlap must still be found, got %+v", got)
	}
}

// An unreported host means public GitHub — the GitHubHost field documents that
// resolution order, and sameGitHubHost treats "" and "github.com" as one host.
// If the key split them, a spoke too old to report its host would never be seen
// to overlap with a current one.
func TestRepoOverlap_UnreportedHostIsPublicGitHub(t *testing.T) {
	old := overlapHive("h-old", "old-spoke", "acme", "ui", "ui")
	current := overlapHive("h-new", "new-spoke", "acme", "api", "ui")
	current.GitHubHost = "github.com"

	got := computeRepoOverlaps([]RegistryEntry{old, current})
	if len(got) != 1 {
		t.Fatalf("an unreported host must resolve to public GitHub, got %+v", got)
	}
	if got[0].Host != publicGitHubHost {
		t.Errorf("host = %q, want %q", got[0].Host, publicGitHubHost)
	}
}

// The doubling-safe rule repoDisplayLine carries, exercised through this path:
// a primaryRepo that is ALREADY owner/repo must not be re-qualified with the
// org. The live evidence is a GHE hive recorded with a host in its org field.
func TestRepoOverlap_DoesNotDoubleTheOwner(t *testing.T) {
	weird := overlapHive("h-pages", "pages", "castrojo.github.io", "castrojo/endusers")
	plain := overlapHive("h-plain", "plain", "castrojo", "endusers", "endusers")

	got := computeRepoOverlaps([]RegistryEntry{weird, plain})
	if len(got) != 1 {
		t.Fatalf("an already-qualified primaryRepo must not gain a second owner, got %+v", got)
	}
	if strings.Count(got[0].Repo, "/") != 1 {
		t.Errorf("owner doubled: %q", got[0].Repo)
	}
}

// A claim with no owner half cannot be compared without inventing one, and
// inventing one would manufacture overlaps between unrelated repos that happen
// to share a name.
func TestRepoOverlap_OwnerlessClaimsAreIgnored(t *testing.T) {
	a := overlapHive("h-a", "a", "", "", "docs")
	b := overlapHive("h-b", "b", "", "", "docs")
	if got := computeRepoOverlaps([]RegistryEntry{a, b}); got != nil {
		t.Fatalf("two org-less \"docs\" claims are not evidence of the same repo, got %+v", got)
	}
}

// A hive with no id cannot be named in a conflict, so it cannot participate.
func TestRepoOverlap_SkipsHivesWithoutAnID(t *testing.T) {
	got := computeRepoOverlaps([]RegistryEntry{
		overlapHive("", "nameless", "acme", "ui", "ui"),
		overlapHive("h-real", "real", "acme", "ui", "ui"),
	})
	if got != nil {
		t.Fatalf("an id-less entry cannot make an overlap, got %+v", got)
	}
}

// Ordering is deterministic across map-iteration randomness — otherwise the
// alert text and the API response churn between identical evaluations.
func TestRepoOverlap_OrderingIsDeterministic(t *testing.T) {
	hives := []RegistryEntry{
		overlapHive("h-a", "a", "acme", "zeta", "zeta", "alpha", "middle"),
		overlapHive("h-b", "b", "acme", "alpha", "zeta", "alpha", "middle"),
	}
	first := computeRepoOverlaps(hives)
	if len(first) != 3 {
		t.Fatalf("want three overlaps, got %d", len(first))
	}
	want := []string{"acme/alpha", "acme/middle", "acme/zeta"}
	for round := 0; round < 20; round++ {
		got := computeRepoOverlaps(hives)
		for i := range want {
			if got[i].Repo != want[i] {
				t.Fatalf("round %d: order = %q at %d, want %q", round, got[i].Repo, i, want[i])
			}
		}
	}
	_ = first
}

// --- the per-hive view the alert rule reads ---------------------------------

func TestRepoOverlapsFor_SelectsOnlyThisHivesConflicts(t *testing.T) {
	overlaps := computeRepoOverlaps([]RegistryEntry{
		overlapHive("h-a", "a", "acme", "one", "shared-ab"),
		overlapHive("h-b", "b", "acme", "two", "shared-ab", "shared-bc"),
		overlapHive("h-c", "c", "acme", "three", "shared-bc"),
	})
	if len(overlaps) != 2 {
		t.Fatalf("want two overlaps, got %+v", overlaps)
	}
	if got := repoOverlapsFor("h-a", overlaps); len(got) != 1 || got[0].Repo != "acme/shared-ab" {
		t.Errorf("h-a should see only its own conflict, got %+v", got)
	}
	if got := repoOverlapsFor("h-b", overlaps); len(got) != 2 {
		t.Errorf("h-b is in both conflicts, got %+v", got)
	}
	if got := repoOverlapsFor("h-none", overlaps); got != nil {
		t.Errorf("an uninvolved hive sees nothing, got %+v", got)
	}
	if got := repoOverlapsFor("", overlaps); got != nil {
		t.Errorf("an empty id must not match, got %+v", got)
	}
}

// The alert line is what the operator actually reads, so it names the other
// hive rather than restating the reading hive's own name.
func TestRepoOverlapAlertReason(t *testing.T) {
	overlaps := computeRepoOverlaps([]RegistryEntry{
		overlapHive("h-hive", "hive", "tunaos", "os", "docs"),
		overlapHive("h-reef", "reef", "tunaos", "apps", "docs"),
	})
	reason := repoOverlapAlertReason("h-hive", repoOverlapsFor("h-hive", overlaps))
	if !strings.Contains(reason, "reef") {
		t.Errorf("reason must name the OTHER hive: %q", reason)
	}
	if strings.Contains(reason, "hive:") || strings.Contains(reason, "by hive") {
		t.Errorf("reason must not name the reading hive as the other party: %q", reason)
	}
	if !strings.Contains(reason, "tunaos/docs") {
		t.Errorf("reason must name the repo: %q", reason)
	}

	// A pair misconfigured onto a whole org overlaps on everything; the line
	// stays readable and the count carries the rest.
	var many []string
	for _, r := range []string{"a", "b", "c", "d", "e"} {
		many = append(many, r)
	}
	wide := computeRepoOverlaps([]RegistryEntry{
		overlapHive("h-1", "one", "acme", "a", many...),
		overlapHive("h-2", "two", "acme", "a", many...),
	})
	wideReason := repoOverlapAlertReason("h-1", repoOverlapsFor("h-1", wide))
	if !strings.Contains(wideReason, "and 2 more") {
		t.Errorf("a wide overlap must be summarised, got %q", wideReason)
	}
	if strings.Count(wideReason, "acme/") != repoOverlapAlertMaxRepos {
		t.Errorf("want %d repos named, got %q", repoOverlapAlertMaxRepos, wideReason)
	}

	if got := repoOverlapAlertReason("h-1", nil); got != "" {
		t.Errorf("no overlaps means no line, got %q", got)
	}
}

// --- the wire shape ---------------------------------------------------------

// The response embeds Registry, so every pre-existing field keeps its place and
// an old client sees no change; the new key appears only when there is one.
func TestRegistryResponse_ShapeIsAdditive(t *testing.T) {
	clean, err := json.Marshal(registryResponse{
		Registry: Registry{UpdatedAt: "2026-09-02T00:00:00Z", Hives: []RegistryEntry{
			overlapHive("h-a", "a", "acme", "one", "one"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clean), "repoOverlaps") {
		t.Errorf("a clean fleet must omit the key entirely: %s", clean)
	}
	for _, want := range []string{`"hives"`, `"updatedAt"`} {
		if !strings.Contains(string(clean), want) {
			t.Errorf("embedding must flatten %s into the top level: %s", want, clean)
		}
	}

	hives := []RegistryEntry{
		overlapHive("h-a", "a", "acme", "one", "shared"),
		overlapHive("h-b", "b", "acme", "two", "shared"),
	}
	dirty, err := json.Marshal(registryResponse{
		Registry:     Registry{Hives: hives},
		RepoOverlaps: computeRepoOverlaps(hives),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"repoOverlaps"`, `"acme/shared"`, `"h-a"`, `"h-b"`} {
		if !strings.Contains(string(dirty), want) {
			t.Errorf("overlap payload missing %s: %s", want, dirty)
		}
	}
}
