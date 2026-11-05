package automerge

import (
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah#1091: the PR removes 25 resolved entries from
// baselines/triage.toml and adds 10 lines of docs. The adopted fixer's
// a75a567 put all 25 entries back, leaving a docs-only diff.
func utah1091Files(withTriage bool, extraFix string) []map[string]string {
	docs := map[string]string{"filename": "docs/skills/package-contract/references/factory-consumption.md", "status": "modified",
		"patch": "@@ -1,0 +1,10 @@\n" + strings.Repeat("+factory consumption note\n", 9) + "+see package-contract.md"}
	files := []map[string]string{docs}
	if withTriage {
		var b strings.Builder
		b.WriteString("@@ -1,25 +0,0 @@\n")
		for i := 0; i < 25; i++ {
			b.WriteString("-[[missing]] package = \"pkg-" + string(rune('a'+i)) + "\"\n")
		}
		files = append(files, map[string]string{"filename": "baselines/triage.toml", "status": "modified", "patch": strings.TrimSuffix(b.String(), "\n")})
	}
	if extraFix != "" {
		files = append(files, map[string]string{"filename": "tests/triage_test.py", "status": "modified", "patch": "@@ -3,1 +3,1 @@\n-" + extraFix + "_old\n+" + extraFix})
	}
	return files
}

func commitFiles(in []map[string]string) []*gh.CommitFile {
	out := make([]*gh.CommitFile, 0, len(in))
	for _, f := range in {
		out = append(out, &gh.CommitFile{Filename: gh.Ptr(f["filename"]), Status: gh.Ptr(f["status"]), Patch: gh.Ptr(f["patch"])})
	}
	return out
}

func TestJudgeAdoptionIntent(t *testing.T) {
	before := commitFiles(utah1091Files(true, ""))
	for _, tc := range []struct {
		name    string
		after   []*gh.CommitFile
		reverts bool
		kept    int
	}{
		{name: "utah#1091: the 25 removals put back, docs only", after: commitFiles(utah1091Files(false, "")), reverts: true, kept: 10},
		{name: "a normal fix keeps the PR's change and adds its own", after: commitFiles(utah1091Files(true, "assert_missing")), reverts: false, kept: 35},
		{name: "unchanged diff (a base merge)", after: before, reverts: false, kept: 35},
		{name: "everything reverted", after: nil, reverts: true, kept: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeAdoptionIntent(before, tc.after)
			if got.Reverts() != tc.reverts || got.Own != 35 || got.Kept != tc.kept {
				t.Fatalf("intent = %+v reverts=%v, want own 35 kept %d reverts %v", got, got.Reverts(), tc.kept, tc.reverts)
			}
		})
	}
	// Exactly half kept is not "most reverted".
	half := judgeAdoptionIntent(commitFiles([]map[string]string{{"filename": "a", "patch": "+x\n+y"}}), commitFiles([]map[string]string{{"filename": "a", "patch": "+x"}}))
	if half.Reverts() {
		t.Fatalf("half kept read as reverted: %+v", half)
	}
	// A file without a patch (binary) is one unit for its status.
	bin := judgeAdoptionIntent(commitFiles([]map[string]string{{"filename": "logo.png", "status": "added"}}), nil)
	if bin.Own != 1 || !bin.Reverts() {
		t.Fatalf("binary unit: %+v", bin)
	}
	// A diff that fills GitHub's compare page is not judged.
	many := make([]*gh.CommitFile, adoptionIntentMaxFiles)
	for i := range many {
		many[i] = &gh.CommitFile{Filename: gh.Ptr("f" + string(rune(i))), Patch: gh.Ptr("+x")}
	}
	if got := judgeAdoptionIntent(many, nil); !got.Unjudged || got.Reverts() {
		t.Fatalf("oversized diff judged: %+v", got)
	}
}

// adoptUtah1091InPlace adopts PR #7 in place at head aaa (the utah#1091 diff).
func adoptUtah1091InPlace(t *testing.T) (*adoptHarness, func(), []string) {
	t.Helper()
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	h.adopt.allowMaintainerEdits(7, true)
	h.adopt.comparePatches = map[string][]map[string]string{"aaa": utah1091Files(true, "")}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !rec.AdoptedInPlace() || rec.AdoptedHead != "aaa" {
		done()
		t.Fatalf("not adopted in place: %+v", rec)
	}
	return h, done, []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}
}

func fixerRevertsComments(api *stageAPI) []string {
	var out []string
	for _, c := range api.comments {
		if strings.Contains(c, "Fixing CI would undo this PR's change") {
			out = append(out, c)
		}
	}
	return out
}

// The fixer's push re-adds what the PR removed: refused, parked at 085 with
// the reason, one note, and the contributor lane will not merge that head.
func TestAdoptedFixerPushThatRevertsPRIsRefused(t *testing.T) {
	h, done, adopted := adoptUtah1091InPlace(t)
	defer done()
	h.adopt.comparePatches["a75a567"] = utah1091Files(false, "")
	h.adopt.setCommitAuthor("a75a567", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "a75a567", adopted...), "ci-pending")

	notes := fixerRevertsComments(h.api)
	if len(notes) != 1 || !strings.Contains(notes[0], "leaves 10 of the 35 lines") || !strings.Contains(notes[0], "will not merge `a75a567`") {
		t.Fatalf("notes = %v (all comments %v)", notes, h.api.comments)
	}
	if added := strings.Join(h.addedLabels(), ","); !strings.Contains(added, "stage/085-needs-decision") {
		t.Fatalf("added = %v, want the 085 park", h.api.added)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !rec.AdoptionEscalated("a75a567") || rec.AdoptEscalatedReason != stagelabels.LaneReasonAdoptionFixerReverts || rec.AdoptedHead != "a75a567" {
		t.Fatalf("record = %+v", rec)
	}
	if got := h.e.adoptionIntentGate("acme", "widget", 7, "a75a567"); got != stagelabels.LaneReasonAdoptionFixerReverts {
		t.Fatalf("merge gate = %q, want the refusal", got)
	}

	// Next sweep on the same head: the park holds, the note is not repeated.
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "a75a567", "stage/085-needs-decision", "role/contributor", "author/clanker"), "ci-pending")
	if len(fixerRevertsComments(h.api)) != 0 || strings.Contains(strings.Join(h.addedLabels(), ","), "stage/074") {
		t.Fatalf("second sweep: comments=%v added=%v", h.api.comments, h.api.added)
	}
}

// A fix that keeps the PR's change is the hive's head as before.
func TestAdoptedFixerPushThatKeepsPRIsAllowed(t *testing.T) {
	h, done, adopted := adoptUtah1091InPlace(t)
	defer done()
	h.adopt.comparePatches["bbb"] = utah1091Files(true, "assert_missing")
	h.adopt.setCommitAuthor("bbb", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", adopted...), "check-failing:unit")
	if h.api.writes() != 0 {
		t.Fatalf("writes after a good fix: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedHead != "bbb" || rec.AdoptionEscalated("bbb") {
		t.Fatalf("record = %+v", rec)
	}
	if got := h.e.adoptionIntentGate("acme", "widget", 7, "bbb"); got != "" {
		t.Fatalf("merge gate = %q, want none", got)
	}
}

// An unreadable diff leaves the hive's head unconfirmed: no merge until the
// check passes on a later sweep.
func TestAdoptedFixerPushIntentUnreadableHoldsMerge(t *testing.T) {
	h, done, adopted := adoptUtah1091InPlace(t)
	defer done()
	h.adopt.setCommitAuthor("bbb", testHiveAppBotLogin)
	h.adopt.failCompare = true
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", adopted...), "check-failing:unit")
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedHead != "aaa" || !rec.HivePushedHead("bbb") {
		t.Fatalf("record = %+v, want the head unconfirmed with push evidence kept", rec)
	}
	if got := h.e.adoptionIntentGate("acme", "widget", 7, "bbb"); got != contributorLaneReasonAdoptionIntentPending {
		t.Fatalf("merge gate = %q, want pending", got)
	}
	h.adopt.failCompare = false
	h.adopt.comparePatches["bbb"] = utah1091Files(true, "")
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", adopted...), "check-failing:unit")
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); rec.AdoptedHead != "bbb" {
		t.Fatalf("retry did not confirm the head: %+v", rec)
	}
	if got := h.e.adoptionIntentGate("acme", "widget", 7, "bbb"); got != "" {
		t.Fatalf("merge gate after retry = %q", got)
	}
}

// The push came through the hive's own push record (a fixer committing under
// a bare git identity): the same check applies, and a note already posted on
// the head is not posted again.
func TestAdoptedRecordedHivePushThatRevertsPRIsRefusedOnce(t *testing.T) {
	h, done, adopted := adoptUtah1091InPlace(t)
	defer done()
	key := stageKey("acme", "widget", 7)
	h.adopt.comparePatches["a75a567"] = utah1091Files(false, "")
	h.adopt.setCommitAuthor("a75a567", "git:scanner")
	h.e.stageStoreOrShared().MarkHivePushedHead(key, "a75a567")
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "a75a567", adopted...), "ci-pending")
	if len(fixerRevertsComments(h.api)) != 1 {
		t.Fatalf("comments = %v", h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.AdoptEscalatedReason != stagelabels.LaneReasonAdoptionFixerReverts || rec.AdoptedHead != "a75a567" {
		t.Fatalf("record = %+v", rec)
	}

	// The note posted but the head was not taken (a restart in between):
	// the re-check does not post it again.
	h.e.stageStoreOrShared().MarkAdoptedHead(key, "aaa")
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "a75a567", "stage/085-needs-decision", "role/contributor", "author/clanker"), "ci-pending")
	if n := len(fixerRevertsComments(h.api)); n != 0 {
		t.Fatalf("note re-posted: %v", h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.AdoptedHead != "a75a567" || rec.AdoptEscalatedReason != stagelabels.LaneReasonAdoptionFixerReverts {
		t.Fatalf("record after re-check = %+v", rec)
	}
}
