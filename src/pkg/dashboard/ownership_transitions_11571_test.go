package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type transitionEntry11571 struct {
	at     time.Time
	repo   string
	number int
	detail string
}

// field reads one key from an auditDetail string.
func (e transitionEntry11571) field(key string) string {
	for _, part := range strings.Split(e.detail, ", ") {
		if k, v, ok := strings.Cut(part, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func ownershipTransitionsHarness11571(t *testing.T) (*[]transitionEntry11571, string) {
	t.Helper()
	oldPath := ownershipStatePath
	path := filepath.Join(t.TempDir(), "state.json")
	ownershipStatePath = path
	var got []transitionEntry11571
	setOwnershipAuditSink(func(at time.Time, repo string, number int, detail string) {
		got = append(got, transitionEntry11571{at: at, repo: repo, number: number, detail: detail})
	})
	t.Cleanup(func() { ownershipStatePath = oldPath; setOwnershipAuditSink(nil) })
	return &got, path
}

func repo11571(name string, o map[string]Ownership, mod func(*FrontendRepo)) FrontendRepo {
	r := FrontendRepo{Name: name, Full: "o/" + name, Ownership: o}
	if mod != nil {
		mod(&r)
	}
	return r
}

func TestRecordOwnershipTransitions11571(t *testing.T) {
	review := yourTurn(OwnershipReasonHivePRNeedsHuman, "needs review")
	closeIt := yourTurn(OwnershipReasonAdjudicatorRecommendedClose, "adjudicator recommends close")
	other := yourTurn(OwnershipReasonHivePRNeedsHuman, "different words")
	none := notYourTurn()
	paused := func(r *FrontendRepo) { r.Paused = true }
	incomplete := func(r *FrontendRepo) { r.CountsIncomplete = true }
	missing := func(r *FrontendRepo) { r.ownershipDataMissing = true }

	type entry struct {
		old, new          bool
		oldVerb, newVerb  string
		oldCode, newCode  string
		reason, url, repo string
		number            int
	}
	pr1 := func(o Ownership) map[string]Ownership { return map[string]Ownership{"pr#1": o} }
	const pr1URL = "https://github.com/o/r/pull/1"
	steps := []struct {
		name  string
		repos []FrontendRepo
		want  []entry
	}{
		{"first deploy only seeds", []FrontendRepo{repo11571("r", pr1(review), nil)}, nil},
		{"restart reloads the record, no entries", []FrontendRepo{repo11571("r", pr1(review), nil)}, nil},
		{"reason text only", []FrontendRepo{repo11571("r", pr1(other), nil)}, nil},
		{"verb and reason code change while staying your turn", []FrontendRepo{repo11571("r", pr1(closeIt), nil)},
			[]entry{{old: true, new: true, oldVerb: OwnershipVerbReview, newVerb: OwnershipVerbClose, oldCode: OwnershipReasonHivePRNeedsHuman, newCode: OwnershipReasonAdjudicatorRecommendedClose, reason: closeIt.Reason, url: pr1URL, repo: "o/r", number: 1}}},
		{"leaves your turn", []FrontendRepo{repo11571("r", pr1(none), nil)},
			[]entry{{old: true, oldVerb: OwnershipVerbClose, oldCode: OwnershipReasonAdjudicatorRecommendedClose, url: pr1URL, repo: "o/r", number: 1}}},
		{"re-enters", []FrontendRepo{repo11571("r", pr1(review), nil)},
			[]entry{{new: true, newVerb: OwnershipVerbReview, newCode: OwnershipReasonHivePRNeedsHuman, reason: review.Reason, url: pr1URL, repo: "o/r", number: 1}}},
		{"paused repo with no rows is not a departure", []FrontendRepo{repo11571("r", nil, paused)}, nil},
		{"incomplete repo with no rows is not a departure", []FrontendRepo{repo11571("r", nil, incomplete)}, nil},
		{"snapshot without enumeration data is not a departure", []FrontendRepo{repo11571("r", nil, missing)}, nil},
		{"resumed repo with the same item adds nothing", []FrontendRepo{repo11571("r", pr1(review), nil)}, nil},
		{"new item starting your turn", []FrontendRepo{repo11571("r", map[string]Ownership{"pr#1": review, "issue#7": review}, nil)},
			[]entry{{new: true, newVerb: OwnershipVerbReview, newCode: OwnershipReasonHivePRNeedsHuman, reason: review.Reason, url: "https://github.com/o/r/issues/7", repo: "o/r", number: 7}}},
		{"new item not your turn", []FrontendRepo{repo11571("r", map[string]Ownership{"pr#1": review, "issue#7": review, "issue#8": none}, nil)}, nil},
		{"closed or merged while your turn carries the URL", []FrontendRepo{repo11571("r", map[string]Ownership{"pr#1": review, "issue#8": none}, nil)},
			[]entry{{old: true, oldVerb: OwnershipVerbReview, oldCode: OwnershipReasonHivePRNeedsHuman, reason: "closed or merged", url: "https://github.com/o/r/issues/7", repo: "o/r", number: 7}}},
		{"closed while not your turn", []FrontendRepo{repo11571("r", pr1(review), nil)}, nil},
	}

	got, _ := ownershipTransitionsHarness11571(t)
	base := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for i, step := range steps {
		*got = nil
		now := base.Add(time.Duration(i) * time.Minute)
		recordOwnershipTransitions(step.repos, now)
		if len(*got) != len(step.want) {
			t.Fatalf("%s: %d entries %v, want %d", step.name, len(*got), *got, len(step.want))
		}
		for j, w := range step.want {
			e := (*got)[j]
			checks := []struct{ field, have, want string }{
				{"old_your_turn", e.field("old_your_turn"), boolString11571(w.old)},
				{"new_your_turn", e.field("new_your_turn"), boolString11571(w.new)},
				{"old_verb", e.field("old_verb"), w.oldVerb},
				{"new_verb", e.field("new_verb"), w.newVerb},
				{"old_reason_code", e.field("old_reason_code"), w.oldCode},
				{"new_reason_code", e.field("new_reason_code"), w.newCode},
				{"reason", e.field("reason"), w.reason},
				{"url", e.field("url"), w.url},
				{"at", e.field("at"), now.Format(time.RFC3339)},
				{"repo", e.repo, w.repo},
			}
			for _, c := range checks {
				if c.have != c.want {
					t.Errorf("%s: %s = %q, want %q (entry %v)", step.name, c.field, c.have, c.want, e.detail)
				}
			}
			if e.number != w.number || !e.at.Equal(now) {
				t.Errorf("%s: number/time = %d/%v, want %d/%v", step.name, e.number, e.at, w.number, now)
			}
		}
	}
}

func boolString11571(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestOwnershipTransitionsSeedPerRepository11571(t *testing.T) {
	got, path := ownershipTransitionsHarness11571(t)
	review := yourTurn(OwnershipReasonHivePRNeedsHuman, "needs review")
	rows := map[string]Ownership{"pr#1": review}
	now := time.Now()

	// "late" is incomplete at the first refresh; its rows are not stored.
	recordOwnershipTransitions([]FrontendRepo{
		repo11571("early", rows, nil),
		repo11571("late", rows, func(r *FrontendRepo) { r.CountsIncomplete = true }),
	}, now)
	// Its first complete refresh seeds it silently, with no burst.
	recordOwnershipTransitions([]FrontendRepo{
		repo11571("early", rows, nil),
		repo11571("late", rows, nil),
	}, now)
	if len(*got) != 0 {
		t.Fatalf("entries after seeding = %v, want none", *got)
	}
	// Once seeded it records changes.
	recordOwnershipTransitions([]FrontendRepo{
		repo11571("early", rows, nil),
		repo11571("late", map[string]Ownership{"pr#1": notYourTurn()}, nil),
	}, now)
	if len(*got) != 1 || (*got)[0].repo != "o/late" {
		t.Fatalf("entries = %v, want one for o/late", *got)
	}
	state := loadOwnershipState(path)
	if !state.SeededRepos["o/early"] || !state.SeededRepos["o/late"] {
		t.Fatalf("seeded repos = %v", state.SeededRepos)
	}
}

func TestOwnershipTransitionsSkipEntriesWhenSaveFails11571(t *testing.T) {
	got, _ := ownershipTransitionsHarness11571(t)
	review := yourTurn(OwnershipReasonHivePRNeedsHuman, "needs review")
	rows := func(o Ownership) []FrontendRepo {
		return []FrontendRepo{repo11571("r", map[string]Ownership{"pr#1": o}, nil)}
	}
	recordOwnershipTransitions(rows(review), time.Now())

	// Make the save fail: the state path's directory is gone.
	good := ownershipStatePath
	ownershipStatePath = filepath.Join(filepath.Dir(good), "missing", "state.json")
	recordOwnershipTransitions(rows(notYourTurn()), time.Now())
	recordOwnershipTransitions(rows(notYourTurn()), time.Now())
	if len(*got) != 0 {
		t.Fatalf("entries with a failing save = %v, want none", *got)
	}
	if err := saveOwnershipState(ownershipStatePath, ownershipState{}); err == nil {
		t.Fatal("saveOwnershipState to a missing directory returned nil")
	}

	// Once the save works the change is recorded exactly once.
	ownershipStatePath = good
	recordOwnershipTransitions(rows(notYourTurn()), time.Now())
	recordOwnershipTransitions(rows(notYourTurn()), time.Now())
	if len(*got) != 1 {
		t.Fatalf("entries after the save recovers = %v, want 1", *got)
	}
	if _, err := os.Stat(good); err != nil {
		t.Fatal(err)
	}
}

func TestApplyOwnershipDecisionsWritesTransitionToAuditLog11571(t *testing.T) {
	oldLedger := ownershipEscalationLedgerPath
	ownershipEscalationLedgerPath = filepath.Join(t.TempDir(), "missing-ledger.json")
	t.Cleanup(func() { ownershipEscalationLedgerPath = oldLedger })
	ownershipTransitionsHarness11571(t)
	s := newTestServer() // installs the audit sink on its AuditLog
	t.Cleanup(func() { setOwnershipAuditSink(nil) })

	level := 3
	cfg := &config.Config{ACMMLevel: &level}
	repos := func(labels ...string) []FrontendRepo {
		pr := github.PullRequest{Repo: "o/r", Number: 5, HiveAgent: "scanner", Labels: labels, URL: "https://github.com/o/r/pull/5",
			CreatedAt: ownershipNow11565.Add(-time.Hour), UpdatedAt: ownershipNow11565.Add(-time.Hour)}
		return []FrontendRepo{{Name: "r", Full: "o/r", OpenPrs: []any{FrontendPR{PullRequest: pr}}}}
	}
	now := ownershipNow11565
	first := repos("needs-human")
	applyOwnershipDecisions(cfg, first, nil, "", now)
	if d := first[0].Ownership["pr#5"]; !d.YourTurn {
		t.Fatalf("setup: PR not your turn: %+v", d)
	}
	second := repos()
	applyOwnershipDecisions(cfg, second, nil, "", now.Add(time.Minute))

	var found []AuditEntry
	for _, e := range s.audit.Recent(50) {
		if e.Action == ownershipTransitionAction {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("ownership_transition audit entries = %v, want 1", found)
	}
	e := found[0]
	if e.Repo != "o/r" || e.Target != 5 {
		t.Fatalf("entry repo/target = %q/%d, want o/r/5", e.Repo, e.Target)
	}
	for _, want := range []string{"old_your_turn=true", "new_your_turn=false", "old_verb=" + OwnershipVerbReview, "url=https://github.com/o/r/pull/5"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("detail %q missing %q", e.Detail, want)
		}
	}
	if e.Timestamp != now.Add(time.Minute).Format(time.RFC3339) {
		t.Errorf("timestamp = %q, want the refresh time", e.Timestamp)
	}
}
