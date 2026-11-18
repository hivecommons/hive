package automerge

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func coveredComments(h *adoptHarness) []string {
	var out []string
	for _, c := range h.api.comments {
		if strings.HasPrefix(c, AdoptionCoveredMarker) {
			out = append(out, c)
		}
	}
	return out
}

func openPR(number int, login, association, body string, labels ...string) map[string]any {
	ls := []map[string]any{}
	for _, l := range labels {
		ls = append(ls, map[string]any{"name": l})
	}
	return map[string]any{
		"number": number, "state": "open", "title": "fix", "body": body,
		"user":               map[string]any{"login": login},
		"author_association": association,
		"labels":             ls,
		"base":               map[string]any{"ref": "v5"},
		"head":               map[string]any{"sha": "other", "ref": "x"},
	}
}

// With an owner's PR open for the issue, a contributor PR that is the
// adoption candidate is closed as a duplicate of it with a thank-you note,
// not adopted (hivecommons/hive#18900: #18870 was adopted as #18879 while the
// owner's #18873 was open). Once the owner's PR is gone the same PR is
// adopted as usual.
func TestAdoptionClosesCoveredContributorPRInsteadOfAdopting(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "queued"}, adoptPolicy())
	defer done()
	start := h.clock.at
	h.sink.presence = &ContributorPresence{Registered: true, Online: false}
	h.observe(adoptPR(7, "aaa"), "conflicting")
	if len(nudgeComments(h.api)) != 1 {
		t.Fatalf("no nudge: %v", h.api.comments)
	}
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	key := stageKey("acme", "widget", 7)

	// The owner's PR for the same issue (adoptPR closes #12 and #13).
	h.adopt.openPRs = []map[string]any{openPR(50, "andy", "OWNER", "Fixes #12")}
	h.api.reset()
	h.clock.at = start.Add(31 * time.Minute)
	res := h.observe(adoptPR(7, "aaa", labels...), "conflicting")
	if len(h.adopt.pulls) != 0 || len(adoptionComments(h.api)) != 0 {
		t.Fatalf("adopted a PR another open PR covers: pulls=%v comments=%v", h.adopt.pulls, h.api.comments)
	}
	notes := coveredComments(h)
	if len(notes) != 1 || res.Comments != 1 {
		t.Fatalf("want one covered note, got %v (res %+v)", h.api.comments, res)
	}
	for _, want := range []string{"covered_by=50", "A person's PR #50 already covers #12", "Thank you for this PR", "credited"} {
		if !strings.Contains(notes[0], want) {
			t.Fatalf("covered note missing %q:\n%s", want, notes[0])
		}
	}
	if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("contributor PR not closed as a duplicate: %v", h.adopt.prEdits)
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.AdoptionBlocked("aaa") != stagelabels.AdoptBlockedCoveredByOpenPR {
		t.Fatalf("block not recorded: %+v", rec)
	}

	// Later passes do not repeat the note or the close.
	h.api.reset()
	h.clock.at = start.Add(46 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "conflicting")
	if got := coveredComments(h); len(got) != 0 {
		t.Fatalf("covered note repeated: %v", got)
	}
}

// Equal trust is a tie, not cover: another contributor's PR for the issue does
// not stop the adoption, and neither does the hive's own copy of this PR.
func TestAdoptionNotBlockedByAnotherContributorsOrItsOwnCopyPR(t *testing.T) {
	for name, other := range map[string]map[string]any{
		"another contributor's clanker PR": openPR(51, "someone", "NONE", "Fixes #12", "author/clanker"),
		"a maintainer's clanker PR":        openPR(52, "mallory2", "MEMBER", "Fixes #12", "author/clanker"),
		"the hive's copy of this PR":       openPR(53, "hive-app[bot]", "NONE", "Fixes #12\n\nSupersedes #7"),
		"an unrelated person's PR":         openPR(54, "andy", "OWNER", "Fixes #99"),
		"a person's PR on another branch":  withBase(openPR(55, "andy", "OWNER", "Fixes #12"), "v4"),
	} {
		t.Run(name, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "queued"}, adoptPolicy())
			defer done()
			start := h.clock.at
			h.sink.presence = &ContributorPresence{Registered: true, Online: false}
			h.observe(adoptPR(7, "aaa"), "conflicting")
			labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
			h.adopt.openPRs = []map[string]any{other}
			h.api.reset()
			h.clock.at = start.Add(31 * time.Minute)
			h.observe(adoptPR(7, "aaa", labels...), "conflicting")
			if len(h.adopt.pulls) != 1 {
				t.Fatalf("adoption blocked by %s: pulls=%v comments=%v", name, h.adopt.pulls, h.api.comments)
			}
			if got := coveredComments(h); len(got) != 0 {
				t.Fatalf("covered note posted for %s: %v", name, got)
			}
		})
	}
}

func withBase(pr map[string]any, ref string) map[string]any {
	pr["base"] = map[string]any{"ref": ref}
	return pr
}
