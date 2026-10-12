package dashboard

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordOwnershipTransitions11571(t *testing.T) {
	oldPath := ownershipStatePath
	ownershipStatePath = filepath.Join(t.TempDir(), "state.json")
	t.Cleanup(func() { ownershipStatePath = oldPath; setOwnershipAuditSink(nil) })
	var got []string
	setOwnershipAuditSink(func(_ time.Time, _ string, _ int, detail string) { got = append(got, detail) })

	snap := func(o map[string]Ownership) []FrontendRepo {
		return []FrontendRepo{{Name: "r", Full: "o/r", Ownership: o, ownershipURLs: map[string]string{"pr#1": "https://x/pr/1"}}}
	}
	now := time.Now()
	review := yourTurn(OwnershipReasonParkedNeedsHuman, "needs review")
	for _, c := range []struct {
		name string
		o    map[string]Ownership
		want int
	}{
		{"first refresh only seeds", map[string]Ownership{"pr#1": review}, 0},
		{"unchanged", map[string]Ownership{"pr#1": review}, 0},
		{"reason text only", map[string]Ownership{"pr#1": {YourTurn: true, Verb: review.Verb, ReasonCode: review.ReasonCode, Reason: "other", Action: "x"}}, 0},
		{"leaves your turn", map[string]Ownership{"pr#1": notYourTurn()}, 1},
		{"re-enters", map[string]Ownership{"pr#1": review}, 1},
		{"closed while your turn", map[string]Ownership{}, 1},
	} {
		got = nil
		recordOwnershipTransitions(snap(c.o), now)
		if len(got) != c.want {
			t.Fatalf("%s: %d entries %v, want %d", c.name, len(got), got, c.want)
		}
	}
	got = nil
	recordOwnershipTransitions(snap(map[string]Ownership{"pr#1": review}), now)
	if len(got) != 1 || !strings.Contains(got[0], "url=https://x/pr/1") {
		t.Fatalf("entry = %v", got)
	}
}
