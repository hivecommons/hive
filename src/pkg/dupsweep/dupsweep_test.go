package dupsweep

import (
	"strings"
	"testing"
	"time"
)

func at(day int) time.Time {
	return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day)
}

// utahPackages reproduces the measured projectbluefin cluster named in #7469:
// utah-packages #97 / #114 / #121, three PRs binding the same workflow
// dispatch inputs through `env:`. #114 and #121 are byte-identical; #97 is
// strictly larger (job-scope rather than step-scope) and was opened first.
func utahPackages() []PR {
	const wf = ".github/workflows/import-rawhide-package.yml"
	return []PR{
		{Repo: "ublue-os/utah-packages", Number: 97, Title: "bind dispatch inputs at job scope", Author: "alice", CreatedAt: at(1), Files: []string{wf}, DiffHash: "jobscope"},
		{Repo: "ublue-os/utah-packages", Number: 114, Title: "bind dispatch inputs through env", Author: "bob", CreatedAt: at(5), Files: []string{wf}, DiffHash: "stepscope"},
		{Repo: "ublue-os/utah-packages", Number: 121, Title: "fix unbound workflow inputs", Author: "carol", CreatedAt: at(7), Files: []string{wf}, DiffHash: "stepscope"},
	}
}

func TestFindClustersIdenticalFileSets(t *testing.T) {
	got := Find(utahPackages(), Options{})
	if len(got) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(got))
	}
	c := got[0]
	if c.Survivor.Number != 97 {
		t.Errorf("survivor = #%d, want #97 (earliest submission survives)", c.Survivor.Number)
	}
	if len(c.Superseded) != 2 {
		t.Fatalf("want 2 superseded, got %d", len(c.Superseded))
	}
	if c.Superseded[0].Number != 114 || c.Superseded[1].Number != 121 {
		t.Errorf("superseded = %d/%d, want 114/121", c.Superseded[0].Number, c.Superseded[1].Number)
	}
}

// The three-PR cluster has two byte-identical members and one that differs, so
// the CLUSTER grade must be the weaker tier even though a pair inside it
// matches exactly. Claiming identical-diff here would overstate the evidence
// for #97, the PR a human is being asked to keep.
func TestClusterGradeDowngradesOnAnyDiffMismatch(t *testing.T) {
	c := Find(utahPackages(), Options{})[0]
	if c.Confidence != ConfidenceSameFiles {
		t.Errorf("confidence = %q, want %q — one member's diff differs", c.Confidence, ConfidenceSameFiles)
	}
	ident := c.IdenticalTo(c.Superseded[1]) // #121
	if len(ident) != 1 || ident[0] != 114 {
		t.Errorf("IdenticalTo(#121) = %v, want [114]", ident)
	}
	if got := c.IdenticalTo(c.Survivor); len(got) != 0 {
		t.Errorf("IdenticalTo(#97) = %v, want none", got)
	}
}

// An unknown diff hash must never render as a match. "We could not check" and
// "we checked and they agree" are different claims and only one of them
// licenses a human to close a PR without reading it.
func TestUnknownDiffHashNeverGradesIdentical(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "a", CreatedAt: at(1), Files: []string{"f.go"}, DiffHash: ""},
		{Repo: "o/r", Number: 2, Author: "b", CreatedAt: at(2), Files: []string{"f.go"}, DiffHash: ""},
	}
	if c := Find(prs, Options{})[0]; c.Confidence != ConfidenceUnknown {
		t.Errorf("confidence = %q, want %q for unknown hashes", c.Confidence, ConfidenceUnknown)
	}
}

func TestIdenticalDiffGrade(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "a", CreatedAt: at(1), Files: []string{"renovate.json"}, DiffHash: "same"},
		{Repo: "o/r", Number: 2, Author: "b", CreatedAt: at(2), Files: []string{"renovate.json"}, DiffHash: "same"},
	}
	if c := Find(prs, Options{})[0]; c.Confidence != ConfidenceIdenticalDiff {
		t.Errorf("confidence = %q, want %q", c.Confidence, ConfidenceIdenticalDiff)
	}
}

// NEGATIVE CONTROL. Everything above asserts that duplicates ARE found; this
// asserts the clustering can say no. PRs whose file sets merely OVERLAP are
// not a cluster — if they were, every PR touching a busy file would be
// reported as a duplicate of every other, and the sweep would be pure noise.
func TestOverlappingButUnequalFileSetsDoNotCluster(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "a", CreatedAt: at(1), Files: []string{"a.go", "b.go"}},
		{Repo: "o/r", Number: 2, Author: "b", CreatedAt: at(2), Files: []string{"a.go"}},
		{Repo: "o/r", Number: 3, Author: "c", CreatedAt: at(3), Files: []string{"a.go", "b.go", "c.go"}},
	}
	if got := Find(prs, Options{}); len(got) != 0 {
		t.Fatalf("want no clusters from overlapping-but-unequal file sets, got %d: %+v", len(got), got)
	}
}

// Second negative control: the same path in two different repos is a
// coincidence, not duplicated work. Nearly every repo has a
// .github/workflows/ci.yml.
func TestSamePathInDifferentReposDoesNotCluster(t *testing.T) {
	prs := []PR{
		{Repo: "o/one", Number: 1, Author: "a", CreatedAt: at(1), Files: []string{".github/workflows/ci.yml"}, DiffHash: "x"},
		{Repo: "o/two", Number: 2, Author: "b", CreatedAt: at(2), Files: []string{".github/workflows/ci.yml"}, DiffHash: "x"},
	}
	if got := Find(prs, Options{}); len(got) != 0 {
		t.Fatalf("want no cross-repo cluster, got %d", len(got))
	}
}

func TestDraftsAreExcluded(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "a", CreatedAt: at(1), Files: []string{"f.go"}},
		{Repo: "o/r", Number: 2, Author: "b", CreatedAt: at(2), Files: []string{"f.go"}, Draft: true},
	}
	if got := Find(prs, Options{}); len(got) != 0 {
		t.Fatalf("a draft must not form a cluster, got %d", len(got))
	}
}

func TestFileOrderAndDuplicatesDoNotAffectClustering(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "a", CreatedAt: at(1), Files: []string{"b.go", "a.go"}},
		{Repo: "o/r", Number: 2, Author: "b", CreatedAt: at(2), Files: []string{"a.go", "a.go", " b.go "}},
	}
	got := Find(prs, Options{})
	if len(got) != 1 {
		t.Fatalf("want 1 cluster despite ordering/whitespace/dupes, got %d", len(got))
	}
	if want := []string{"a.go", "b.go"}; strings.Join(got[0].Files, ",") != strings.Join(want, ",") {
		t.Errorf("files = %v, want %v", got[0].Files, want)
	}
}

// The documentation-repo churn case from #7469: a daily bot regenerating one
// JSON file. Every PR but the NEWEST is superseded by construction, which is
// the opposite of the earliest-survives rule used for humans — keeping the
// oldest here would propose merging stale generated data.
func TestBotSeriesNewestSurvives(t *testing.T) {
	const churn = "static/data/update-churn.json"
	prs := []PR{
		{Repo: "ublue-os/documentation", Number: 1264, Title: "chore: update churn", Author: "renovate[bot]", CreatedAt: at(1), Files: []string{churn}, DiffHash: "d1"},
		{Repo: "ublue-os/documentation", Number: 1272, Title: "chore: update churn", Author: "renovate[bot]", CreatedAt: at(2), Files: []string{churn}, DiffHash: "d2"},
		{Repo: "ublue-os/documentation", Number: 1275, Title: "chore: update churn", Author: "renovate[bot]", CreatedAt: at(3), Files: []string{churn}, DiffHash: "d3"},
	}
	c := Find(prs, Options{})[0]
	if !c.BotSeries {
		t.Fatal("same-bot-authored cluster not recognised as a regeneration series")
	}
	if c.Survivor.Number != 1275 {
		t.Errorf("survivor = #%d, want #1275 (newest regeneration survives)", c.Survivor.Number)
	}
	targets := c.Targets()
	if len(targets) != 1 || targets[0].Number != 1275 {
		t.Errorf("bot series targets = %+v, want a single summary on #1275", targets)
	}
}

// A human resubmitting their own work is NOT superseded by construction, so
// the bot rule must not fire on a non-bot login however consistent the author.
func TestSameHumanAuthorIsNotABotSeries(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Title: "fix", Author: "dave", CreatedAt: at(1), Files: []string{"f.go"}},
		{Repo: "o/r", Number: 2, Title: "fix again", Author: "dave", CreatedAt: at(2), Files: []string{"f.go"}},
	}
	c := Find(prs, Options{})[0]
	if c.BotSeries {
		t.Fatal("a human author was treated as a regeneration series")
	}
	if c.Survivor.Number != 1 {
		t.Errorf("survivor = #%d, want #1", c.Survivor.Number)
	}
}

// Two DIFFERENT bots are not one series, so newest-supersedes must not apply.
func TestDifferentBotsAreNotOneSeries(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "renovate[bot]", CreatedAt: at(1), Files: []string{"f.json"}},
		{Repo: "o/r", Number: 2, Author: "dependabot[bot]", CreatedAt: at(2), Files: []string{"f.json"}},
	}
	c := Find(prs, Options{})[0]
	if c.BotSeries {
		t.Fatal("two different bots were treated as one regeneration series")
	}
}

func TestBotAuthorsOptionExtendsDetection(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "churn-updater", CreatedAt: at(1), Files: []string{"f.json"}},
		{Repo: "o/r", Number: 2, Author: "churn-updater", CreatedAt: at(2), Files: []string{"f.json"}},
	}
	if c := Find(prs, Options{})[0]; c.BotSeries {
		t.Fatal("unconfigured login must not be treated as a bot")
	}
	if c := Find(prs, Options{BotAuthors: []string{"churn-updater"}})[0]; !c.BotSeries {
		t.Fatal("configured bot login was not recognised")
	}
}

// projectbluefin/website#1023: one dependency bot (renovate running as an
// App) opens one PR per dependency, and every one edits package.json and its
// lockfile. Those are separate updates, not a regeneration series and not
// duplicates of each other, so the sweep must say nothing — not "superseded
// by construction", and not "Possible duplicate" either.
func TestDependencyBotBumpsSharingAManifestAreNotClustered(t *testing.T) {
	manifest := []string{"package.json", "package-lock.json"}
	prs := []PR{
		{Repo: "o/website", Number: 1009, Title: "chore(deps): update tailwindcss monorepo to v5 (major)", Author: "mergeraptor[bot]", CreatedAt: at(1), Files: manifest, DiffHash: "tw"},
		{Repo: "o/website", Number: 1019, Title: "chore(deps): update dependency vue-tsc to v3.3.14", Author: "mergeraptor[bot]", CreatedAt: at(2), Files: manifest, DiffHash: "vt"},
		{Repo: "o/website", Number: 1023, Title: "chore(deps): update dependency @antfu/eslint-config to v10", Author: "mergeraptor[bot]", CreatedAt: at(3), Files: manifest, DiffHash: "ae"},
		{Repo: "o/website", Number: 1037, Title: "chore(deps): update dependency js-yaml to v5", Author: "mergeraptor[bot]", CreatedAt: at(4), Files: manifest, DiffHash: "jy"},
	}
	if got := Find(prs, Options{}); len(got) != 0 {
		t.Fatalf("independent dependency bumps clustered: %+v", got)
	}
	// Unknown diff hashes do not change that: the titles already show two jobs.
	for i := range prs {
		prs[i].DiffHash = ""
	}
	if got := Find(prs, Options{}); len(got) != 0 {
		t.Fatalf("independent bumps with unknown diffs clustered: %+v", got)
	}
}

// A regeneration bot dates or numbers each run; that is still the same job,
// so #7469's newest-survives series behaviour is unchanged.
func TestRegenerationTitlesDifferingOnlyByDateOrVersionAreASeries(t *testing.T) {
	const out = "static/data/update-churn.json"
	prs := []PR{
		{Repo: "o/docs", Number: 1264, Title: "chore: regenerate churn (2026-11-15)", Author: "churn[bot]", CreatedAt: at(1), Files: []string{out}, DiffHash: "d1"},
		{Repo: "o/docs", Number: 1272, Title: "chore: regenerate churn (2026-11-16)", Author: "churn[bot]", CreatedAt: at(2), Files: []string{out}, DiffHash: "d2"},
		{Repo: "o/docs", Number: 1275, Title: "Chore: regenerate  churn (2026-11-17)", Author: "churn[bot]", CreatedAt: at(3), Files: []string{out}, DiffHash: "d3"},
	}
	got := Find(prs, Options{})
	if len(got) != 1 || !got[0].BotSeries {
		t.Fatalf("dated regenerations are not one series: %+v", got)
	}
	if got[0].Survivor.Number != 1275 {
		t.Errorf("survivor = #%d, want the newest #1275", got[0].Survivor.Number)
	}
}

// A byte-identical patch is the same job whatever the titles say, so one bot's
// identical PRs still cluster as a series; a shared finding identity is
// evidence beyond the shared file too, so it still clusters (as an ordinary
// cluster, since the titles name different jobs).
func TestSameBotWithStrongerEvidenceStillClusters(t *testing.T) {
	identical := []PR{
		{Repo: "o/r", Number: 1, Title: "chore: sync labels", Author: "sync[bot]", CreatedAt: at(1), Files: []string{"labels.yml"}, DiffHash: "same"},
		{Repo: "o/r", Number: 2, Title: "chore: apply label set", Author: "sync[bot]", CreatedAt: at(2), Files: []string{"labels.yml"}, DiffHash: "same"},
	}
	got := Find(identical, Options{})
	if len(got) != 1 || got[0].Confidence != ConfidenceIdenticalDiff || !got[0].BotSeries {
		t.Fatalf("identical bot patches: %+v, want one identical-diff series", got)
	}

	finding := []PR{
		{Repo: "o/r", Number: 3, Title: "fix: pin action in ci.yml", Author: "scanner[bot]", CreatedAt: at(1), Files: []string{".github/workflows/ci.yml"}, FindingKey: "k1"},
		{Repo: "o/r", Number: 4, Title: "fix: unpinned uses: in workflow", Author: "scanner[bot]", CreatedAt: at(2), Files: []string{".github/workflows/ci.yml"}, FindingKey: "k1"},
	}
	got = Find(finding, Options{FindingKey: func(p PR) string { return p.FindingKey }})
	if len(got) != 1 || got[0].Confidence != ConfidenceFindingIdentity || got[0].BotSeries {
		t.Fatalf("same-finding bot PRs: %+v, want one ordinary finding-identity cluster", got)
	}
}

// Only a cluster made entirely of one bot's different jobs is dropped. A
// person's PR against the same file as a bot's is still a candidate.
func TestMixedAuthorClusterIsUnchanged(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Title: "chore(deps): update dependency vite to v8", Author: "renovate[bot]", CreatedAt: at(1), Files: []string{"package.json"}, DiffHash: "a"},
		{Repo: "o/r", Number: 2, Title: "bump vite", Author: "dave", CreatedAt: at(2), Files: []string{"package.json"}, DiffHash: "b"},
	}
	got := Find(prs, Options{})
	if len(got) != 1 || got[0].BotSeries || got[0].Survivor.Number != 1 {
		t.Fatalf("mixed-author cluster changed: %+v", got)
	}
}

func TestJobTitleSetsAsideVersionsDatesAndHashes(t *testing.T) {
	same := [][2]string{
		{"chore: regenerate X (2026-11-16)", "chore: regenerate X (2026-11-17)"},
		{"update churn data v1.2.3", "Update churn data v1.2.4"},
		{"chore: update docker digest to 1a2b3c4", "chore: update docker digest to 9f8e7d6c5b"},
	}
	for _, p := range same {
		if jobTitle(p[0]) != jobTitle(p[1]) {
			t.Errorf("%q and %q should name one job: %q vs %q", p[0], p[1], jobTitle(p[0]), jobTitle(p[1]))
		}
	}
	different := [][2]string{
		{"update dependency vue-tsc to v3.3.14", "update dependency js-yaml to v5"},
		{"update ghcr.io/o/foo docker digest to 1a2b3c4", "update ghcr.io/o/bar docker digest to 1a2b3c4"},
		{"update dependency web3 to v4", "update dependency web2 to v4"},
		{"Bump actions/checkout from 4.1.0 to 4.2.0", "Bump actions/setup-node from 4.0.0 to 4.1.0"},
	}
	for _, p := range different {
		if jobTitle(p[0]) == jobTitle(p[1]) {
			t.Errorf("%q and %q name different jobs but both normalise to %q", p[0], p[1], jobTitle(p[0]))
		}
	}
}

// Ordinary clusters must notify the LATER authors, not the first proposer.
func TestTargetsAreTheLaterPRs(t *testing.T) {
	c := Find(utahPackages(), Options{})[0]
	targets := c.Targets()
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(targets))
	}
	for _, tgt := range targets {
		if tgt.Number == c.Survivor.Number {
			t.Errorf("survivor #%d must not be a comment target", tgt.Number)
		}
	}
}

func TestMaxFilesExcludesTreeWideChanges(t *testing.T) {
	files := make([]string, 300)
	for i := range files {
		files[i] = string(rune('a'+i%26)) + string(rune('0'+i/26)) + ".go"
	}
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "a", CreatedAt: at(1), Files: files},
		{Repo: "o/r", Number: 2, Author: "b", CreatedAt: at(2), Files: files},
	}
	if got := Find(prs, Options{}); len(got) != 0 {
		t.Fatalf("want no cluster above MaxFiles, got %d", len(got))
	}
	if got := Find(prs, Options{MaxFiles: 400}); len(got) != 1 {
		t.Fatalf("want a cluster when MaxFiles permits it, got %d", len(got))
	}
}

func TestFindIsDeterministic(t *testing.T) {
	prs := append(utahPackages(), PR{Repo: "aaa/other", Number: 5, Author: "z", CreatedAt: at(1), Files: []string{"x"}},
		PR{Repo: "aaa/other", Number: 6, Author: "y", CreatedAt: at(2), Files: []string{"x"}})
	first := Find(prs, Options{})
	for i := 0; i < 5; i++ {
		again := Find(prs, Options{})
		if len(again) != len(first) {
			t.Fatalf("cluster count varies between runs")
		}
		for j := range first {
			if again[j].Repo != first[j].Repo || again[j].Survivor.Number != first[j].Survivor.Number {
				t.Fatalf("cluster ordering varies between runs")
			}
		}
	}
	if first[0].Repo != "aaa/other" {
		t.Errorf("clusters are not ordered by repo: got %q first", first[0].Repo)
	}
}

func TestDiffFingerprintDistinguishesPathAndPatch(t *testing.T) {
	a := DiffFingerprint(map[string]string{"a.go": "+x"})
	b := DiffFingerprint(map[string]string{"b.go": "+x"})
	if a == b {
		t.Error("same patch on different paths must not share a fingerprint")
	}
	c := DiffFingerprint(map[string]string{"a.go": "+y"})
	if a == c {
		t.Error("different patches on the same path must not share a fingerprint")
	}
	if DiffFingerprint(nil) != "" {
		t.Error("empty patch map must yield an empty fingerprint, not a hash of nothing")
	}
	// Order independence: the map has no order, but the hash must be stable.
	for i := 0; i < 20; i++ {
		if DiffFingerprint(map[string]string{"a.go": "+x", "b.go": "+y"}) != DiffFingerprint(map[string]string{"b.go": "+y", "a.go": "+x"}) {
			t.Fatal("fingerprint is not order-independent")
		}
	}
}
