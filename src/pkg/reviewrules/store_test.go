package reviewrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)

func openStore(t *testing.T, cfg Config) (*Store, string, string) {
	t.Helper()
	dir, vault := filepath.Join(t.TempDir(), "idx"), filepath.Join(t.TempDir(), "vault")
	s, err := Open(dir, vault, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir, vault
}

func cmt(id, assoc, body string) Comment {
	return Comment{Repo: "cncf/techdocs", PR: 424, CommentID: id, URL: "https://github.com/cncf/techdocs/pull/424#discussion_r" + strings.TrimPrefix(id, "inline:"),
		Reviewer: "nate-double-u", Association: assoc, Body: body, At: t0, Lane: "quality"}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCaptureTheMaintainersFileRule(t *testing.T) {
	s, _, vault := openStore(t, Config{})
	res := s.CaptureComment(cmt("inline:1001", "OWNER", maintainersComment), t0)
	if res.Outcome != OutcomeCaptured || res.Rule == nil || res.Err != nil {
		t.Fatalf("result = %+v, want a captured rule", res)
	}
	r := res.Rule
	if r.ID != "R-1" || r.Status != StatusActive || r.Scope != ScopeAgent || r.AppliesTo != "quality" || r.Lane != "quality" {
		t.Fatalf("rule = %+v", r)
	}
	if r.Text != ".project/maintainers.yaml is the source of truth for ownership and maintainership" {
		t.Fatalf("text = %q", r.Text)
	}
	if len(r.Provenance) != 1 {
		t.Fatalf("provenance = %+v", r.Provenance)
	}
	p := r.Provenance[0]
	if p.PRURL != "https://github.com/cncf/techdocs/pull/424" || p.CommentID != "inline:1001" || p.Reviewer != "nate-double-u" ||
		p.Association != "OWNER" || !p.At.Equal(t0) || p.Source != "sweep" || p.CommentURL == "" {
		t.Fatalf("provenance = %+v", p)
	}
	entry := readFile(t, filepath.Join(vault, "rule-r-1.md"))
	for _, want := range []string{"state: approved", "type: reviewer-rule", "tags: [review-rule, r-1, pin, reviewer:nate-double-u]",
		"source: https://github.com/cncf/techdocs/pull/424#discussion_r1001", "scope:\n  lanes: [quality]", "author: review-feedback",
		"confidence: 0.85", "reviewer `nate-double-u` (owner)", "hivectl knowledge state rule-r-1 deprecated"} {
		if !strings.Contains(entry, want) {
			t.Errorf("entry missing %q:\n%s", want, entry)
		}
	}
}

func TestCaptureTrustGate(t *testing.T) {
	tests := []struct {
		name   string
		cfg    Config
		assoc  string
		status Status
		out    Outcome
		state  string
	}{
		{"owner", Config{}, "OWNER", StatusActive, OutcomeCaptured, "approved"},
		{"member lower case", Config{}, "member", StatusActive, OutcomeCaptured, "approved"},
		{"collaborator", Config{}, "COLLABORATOR", StatusActive, OutcomeCaptured, "approved"},
		{"contributor is queued", Config{}, "CONTRIBUTOR", StatusPending, OutcomeQueued, "draft"},
		{"none is queued", Config{}, "NONE", StatusPending, OutcomeQueued, "draft"},
		{"unknown is queued", Config{}, "", StatusPending, OutcomeQueued, "draft"},
		{"configured contributor trusted", Config{Trusted: ParseAssociations(" contributor , owner ")}, "CONTRIBUTOR", StatusActive, OutcomeCaptured, "approved"},
		{"configured list excludes member", Config{Trusted: ParseAssociations("OWNER")}, "MEMBER", StatusPending, OutcomeQueued, "draft"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, vault := openStore(t, tc.cfg)
			res := s.CaptureComment(cmt("inline:1", tc.assoc, maintainersComment), t0)
			if res.Outcome != tc.out || res.Rule.Status != tc.status {
				t.Fatalf("outcome=%s status=%s, want %s/%s", res.Outcome, res.Rule.Status, tc.out, tc.status)
			}
			if !strings.Contains(readFile(t, filepath.Join(vault, "rule-r-1.md")), "state: "+tc.state+"\n") {
				t.Fatalf("entry state is not %s", tc.state)
			}
			if tc.status == StatusPending {
				if got := s.UnackedFor("cncf/techdocs", 424); len(got) != 0 {
					t.Fatalf("a queued rule owes the reviewer no acknowledgement: %+v", got)
				}
				if res.Rule.Confidence >= confidenceSweep {
					t.Fatalf("queued confidence = %v", res.Rule.Confidence)
				}
			}
		})
	}
}

func TestCaptureOnlyRules(t *testing.T) {
	s, _, vault := openStore(t, Config{})
	res := s.CaptureComment(cmt("inline:2", "OWNER", "The score is too high."), t0)
	if res.Outcome != OutcomeNotRule || res.Rule != nil || len(s.List()) != 0 {
		t.Fatalf("a correction must not become a rule: %+v", res)
	}
	if _, err := os.Stat(vault); err == nil {
		t.Fatal("a correction must not touch the vault")
	}
}

func TestCaptureIsIdempotentAcrossReload(t *testing.T) {
	s, dir, vault := openStore(t, Config{})
	c := cmt("inline:1001", "OWNER", maintainersComment)
	first := s.CaptureComment(c, t0)
	again := s.CaptureComment(c, t0.Add(time.Hour))
	if again.Outcome != OutcomeDuplicate || again.Rule.ID != first.Rule.ID || len(s.List()) != 1 {
		t.Fatalf("second capture of the same comment = %+v", again)
	}
	reopened, err := Open(dir, vault, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.CaptureComment(c, t0.Add(2*time.Hour)); got.Outcome != OutcomeDuplicate || got.Rule.ID != "R-1" {
		t.Fatalf("after reload = %+v, want a duplicate of R-1", got)
	}
	next := reopened.CaptureComment(cmt("inline:1002", "OWNER", "Going forward, cite the scorecard with a commit-pinned link."), t0)
	if next.Outcome != OutcomeCaptured || next.Rule.ID != "R-2" {
		t.Fatalf("numbering must continue after reload: %+v", next)
	}
	if len(reopened.List()) != 2 {
		t.Fatalf("rules = %d, want 2", len(reopened.List()))
	}
}

func TestNearDuplicatesMerge(t *testing.T) {
	tests := []struct {
		name      string
		second    Comment
		wantMerge bool
	}{
		{"same text, new comment", cmt("inline:2", "MEMBER", "The .project/maintainers.yaml is the source of truth for ownership and maintainership."), true},
		{"reworded, same tokens", cmt("inline:3", "OWNER", "maintainership and ownership: .project/maintainers.yaml is the source of truth for"), true},
		{"different rule", cmt("inline:4", "OWNER", "Going forward, cite the scorecard with a commit-pinned link."), false},
		{"same text, other agent lane", func() Comment {
			c := cmt("inline:5", "OWNER", maintainersComment)
			c.Lane = "reviewer"
			return c
		}(), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, vault := openStore(t, Config{})
			s.CaptureComment(cmt("inline:1", "OWNER", maintainersComment), t0)
			res := s.CaptureComment(tc.second, t0.Add(time.Minute))
			if tc.wantMerge {
				if res.Outcome != OutcomeMerged || res.Rule.ID != "R-1" || len(res.Rule.Provenance) != 2 || len(s.List()) != 1 {
					t.Fatalf("merge result = %+v", res)
				}
				if !res.Rule.Provenance[1].Merged {
					t.Fatal("the merged comment's provenance must say it restated an existing rule")
				}
				if _, err := os.Stat(filepath.Join(vault, "rule-r-2.md")); err == nil {
					t.Fatal("a merge must not create a second entry")
				}
				if acks := s.UnackedFor("cncf/techdocs", 424); len(acks) != 2 || !acks[1].Merged && !acks[0].Merged {
					t.Fatalf("acks = %+v, want the original and the merge", acks)
				}
				if again := s.CaptureComment(tc.second, t0); again.Outcome != OutcomeDuplicate || again.Rule.ID != "R-1" {
					t.Fatalf("a merged comment is idempotent too: %+v", again)
				}
			} else if res.Outcome != OutcomeCaptured || res.Rule.ID != "R-2" {
				t.Fatalf("distinct rule must stay separate: %+v", res)
			}
		})
	}
}

func TestTrustedCorroborationPromotesAQueuedRule(t *testing.T) {
	s, _, vault := openStore(t, Config{})
	s.CaptureComment(cmt("inline:1", "CONTRIBUTOR", maintainersComment), t0)
	res := s.CaptureComment(cmt("inline:2", "OWNER", maintainersComment), t0)
	if res.Outcome != OutcomeMerged || res.Rule.Status != StatusActive {
		t.Fatalf("a trusted restatement must activate the queued rule: %+v", res)
	}
	if !strings.Contains(readFile(t, filepath.Join(vault, "rule-r-1.md")), "state: approved\n") {
		t.Fatal("entry not approved after corroboration")
	}
	// An untrusted restatement of an active rule changes nothing visible.
	res = s.CaptureComment(cmt("inline:3", "NONE", maintainersComment), t0)
	if res.Outcome != OutcomeMerged || res.Rule.Status != StatusActive || !res.Rule.Provenance[2].Acked {
		t.Fatalf("untrusted merge = %+v", res)
	}
}

func TestRuleSetIsBounded(t *testing.T) {
	s, _, _ := openStore(t, Config{MaxRules: 1})
	s.CaptureComment(cmt("inline:1", "OWNER", maintainersComment), t0)
	res := s.CaptureComment(cmt("inline:2", "OWNER", "Going forward, cite the scorecard with a commit-pinned link."), t0)
	if res.Outcome != OutcomeRefused || res.Err != ErrFull || len(s.List()) != 1 {
		t.Fatalf("result = %+v, want refused ErrFull", res)
	}
	if err := s.Retract("R-1", "operator", t0); err != nil {
		t.Fatal(err)
	}
	if res := s.CaptureComment(cmt("inline:2", "OWNER", "Going forward, cite the scorecard with a commit-pinned link."), t0); res.Outcome != OutcomeCaptured {
		t.Fatalf("retracting must free room: %+v", res)
	}
}

func TestABareCaptureRequestIsStillTheRule(t *testing.T) {
	s, _, _ := openStore(t, Config{})
	res := s.CaptureComment(cmt("inline:1", "OWNER", "capture this"), t0)
	if res.Outcome != OutcomeCaptured || res.Rule.Text != "capture this" {
		t.Fatalf("result = %+v", res)
	}
}

func TestRetractAndGet(t *testing.T) {
	s, _, vault := openStore(t, Config{})
	s.CaptureComment(cmt("inline:1", "OWNER", maintainersComment), t0)
	if err := s.Retract(" r-1 ", "an operator", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r := s.Get("r-1")
	if r.Status != StatusRetracted || r.RetractedBy != "an operator" || !r.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("rule = %+v", r)
	}
	if !strings.Contains(readFile(t, filepath.Join(vault, "rule-r-1.md")), "state: deprecated\n") {
		t.Fatal("a retracted rule's entry must be deprecated, not deleted")
	}
	if err := s.Retract("R-1", "again", t0); err != nil {
		t.Fatalf("retracting twice is a no-op: %v", err)
	}
	if err := s.Retract("R-9", "x", t0); err == nil {
		t.Fatal("retracting an unknown rule must fail")
	}
	if s.Get("R-9") != nil {
		t.Fatal("unknown rule must be nil")
	}
	// A retracted rule is not a merge target: restating it makes a new rule.
	if res := s.CaptureComment(cmt("inline:2", "OWNER", maintainersComment), t0); res.Outcome != OutcomeCaptured || res.Rule.ID != "R-2" {
		t.Fatalf("restating a retracted rule = %+v", res)
	}
}

func setEntryState(t *testing.T, vault, slug, state string) {
	t.Helper()
	path := filepath.Join(vault, slug+".md")
	b := readFile(t, path)
	var out []string
	for _, line := range strings.Split(b, "\n") {
		if strings.HasPrefix(line, "state:") {
			line = "state: " + state
		}
		out = append(out, line)
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileFollowsOperatorChanges(t *testing.T) {
	tests := []struct {
		name   string
		cap    string
		act    func(t *testing.T, vault string)
		status Status
		change string
	}{
		{"entry deleted", "OWNER", func(t *testing.T, v string) { _ = os.Remove(filepath.Join(v, "rule-r-1.md")) }, StatusRetracted, "removed"},
		{"entry deprecated", "OWNER", func(t *testing.T, v string) { setEntryState(t, v, "rule-r-1", "deprecated") }, StatusRetracted, "deprecated"},
		{"entry superseded", "OWNER", func(t *testing.T, v string) { setEntryState(t, v, "rule-r-1", "superseded") }, StatusRetracted, "superseded"},
		{"active entry moved to draft", "OWNER", func(t *testing.T, v string) { setEntryState(t, v, "rule-r-1", "draft") }, StatusPending, "review queue"},
		{"queued entry approved", "NONE", func(t *testing.T, v string) { setEntryState(t, v, "rule-r-1", "approved") }, StatusActive, "confirmed"},
		{"untouched active", "OWNER", func(t *testing.T, v string) {}, StatusActive, ""},
		{"untouched queued", "NONE", func(t *testing.T, v string) {}, StatusPending, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, dir, vault := openStore(t, Config{})
			s.CaptureComment(cmt("inline:1", tc.cap, maintainersComment), t0)
			tc.act(t, vault)
			changes := s.Reconcile(t0.Add(time.Hour))
			if got := s.Get("R-1").Status; got != tc.status {
				t.Fatalf("status = %s, want %s (changes %v)", got, tc.status, changes)
			}
			if tc.change == "" && len(changes) != 0 || tc.change != "" && (len(changes) != 1 || !strings.Contains(changes[0], tc.change)) {
				t.Fatalf("changes = %v, want one mentioning %q", changes, tc.change)
			}
			if tc.status == StatusActive && tc.cap == "NONE" && len(s.UnackedFor("cncf/techdocs", 424)) != 1 {
				t.Fatal("a rule a person confirmed owes the reviewer its acknowledgement")
			}
			reopened, err := Open(dir, vault, Config{})
			if err != nil || reopened.Get("R-1").Status != tc.status {
				t.Fatalf("reconciled status must persist: err=%v", err)
			}
		})
	}
}

func TestEntryStateWithoutFrontmatter(t *testing.T) {
	s, _, vault := openStore(t, Config{})
	r := &Rule{ID: "R-1"}
	if _, found := s.entryState(r); found {
		t.Fatal("missing entry reported as found")
	}
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"plain": "no frontmatter", "unterminated": "---\ntitle: x\n", "no state": "---\ntitle: x\n---\nbody"} {
		if err := os.WriteFile(s.entryPath(r), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if st, found := s.entryState(r); !found || st != "approved" {
			t.Fatalf("%s: state=%q found=%v, want approved", name, st, found)
		}
	}
}

func TestForeignHomeProposalsAreOncePerRule(t *testing.T) {
	s, dir, vault := openStore(t, Config{})
	c := cmt("inline:1", "OWNER", "Going forward, the criteria belong in your docs: the maintainers file is the source of truth.")
	s.CaptureComment(c, t0)
	props := s.HomeProposals()
	if len(props) != 1 || props[0].ForeignHome != "cncf/techdocs" {
		t.Fatalf("proposals = %+v", props)
	}
	if err := s.MarkHomeNoted("R-1"); err != nil {
		t.Fatal(err)
	}
	if len(s.HomeProposals()) != 0 {
		t.Fatal("a rule is proposed once")
	}
	reopened, _ := Open(dir, vault, Config{})
	if len(reopened.HomeProposals()) != 0 {
		t.Fatal("the flag must persist")
	}
	if err := s.MarkHomeNoted("R-9"); err == nil {
		t.Fatal("unknown rule must fail")
	}
	// A managed repo's rule is the hive's own to keep; a queued one waits.
	m := cmt("inline:2", "OWNER", "Going forward, document this in your repo: sign every commit.")
	m.ManagedRepo = true
	s.CaptureComment(m, t0)
	q := cmt("inline:3", "NONE", "Going forward, add it to your docs: always run the linter first.")
	s.CaptureComment(q, t0)
	if got := s.HomeProposals(); len(got) != 0 {
		t.Fatalf("proposals = %+v, want none", got)
	}
}

func TestAcknowledgementsAreTrackedPerComment(t *testing.T) {
	s, dir, vault := openStore(t, Config{})
	s.CaptureComment(cmt("inline:1", "OWNER", maintainersComment), t0)
	s.CaptureComment(cmt("inline:2", "OWNER", "Going forward, cite the scorecard with a commit-pinned link."), t0)
	acks := s.UnackedFor("cncf/techdocs", 424)
	if len(acks) != 2 || len(s.UnackedFor("cncf/techdocs", 1)) != 0 || len(s.UnackedFor("other/repo", 424)) != 0 {
		t.Fatalf("acks = %+v", acks)
	}
	if err := s.MarkAcked("cncf/techdocs", 424, []string{"inline:1"}); err != nil {
		t.Fatal(err)
	}
	reopened, _ := Open(dir, vault, Config{})
	left := reopened.UnackedFor("cncf/techdocs", 424)
	if len(left) != 1 || left[0].CommentID != "inline:2" {
		t.Fatalf("left = %+v", left)
	}
}

func TestPersistenceFailureRollsBack(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(blocker, "idx"), filepath.Join(t.TempDir(), "vault"), Config{})
	if err == nil {
		t.Fatal("an index path under a file must report an error")
	}
	res := s.CaptureComment(cmt("inline:1", "OWNER", maintainersComment), t0)
	if res.Outcome != OutcomeRefused || res.Err == nil || len(s.List()) != 0 {
		t.Fatalf("result = %+v, want a refusal that leaves no rule in memory", res)
	}
	if err := s.Remember(cmt("inline:1", "OWNER", "x"), t0); err == nil {
		t.Fatal("Remember must report a failed write")
	}
	if len(s.st.Seen) != 0 {
		t.Fatal("a failed write must not leave memory ahead of disk")
	}
	if err := s.Retract("R-1", "x", t0); err == nil {
		t.Fatal("nothing to retract")
	}
}

func TestOpenReadsOrRejectsTheIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, indexFile), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, t.TempDir(), Config{})
	if err == nil || s == nil || s.st.Comments == nil {
		t.Fatalf("a corrupt index must error but stay usable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, indexFile), []byte(`{"rules":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, t.TempDir(), Config{})
	if err != nil || s.st.Next != 1 || s.st.Comments == nil || s.st.Seen == nil || s.st.Replies == nil {
		t.Fatalf("an index missing its maps must be filled: %v %+v", err, s.st)
	}
}

func TestRememberIsBounded(t *testing.T) {
	s, _, _ := openStore(t, Config{})
	if err := s.Remember(Comment{Repo: "o/r", PR: 1, CommentID: "a"}, t0); err != nil || len(s.st.Seen) != 0 {
		t.Fatalf("a comment without a URL is not remembered: %v", err)
	}
	for i := 0; i < maxSeenComments+5; i++ {
		c := cmt("inline:"+strings.Repeat("1", 1), "OWNER", "x")
		c.URL = "https://github.com/o/r/pull/1#discussion_r" + string(rune('a'+i%26)) + strings.Repeat("z", i/26)
		if err := s.Remember(c, t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.st.Seen) != maxSeenComments {
		t.Fatalf("seen = %d, want %d", len(s.st.Seen), maxSeenComments)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(TrustedAssociationsEnvVar, "owner, contributor")
	t.Setenv(MaxRulesEnvVar, "7")
	t.Setenv(MergeThresholdEnvVar, "0.5")
	cfg := ConfigFromEnv()
	if !cfg.Trusted["OWNER"] || !cfg.Trusted["CONTRIBUTOR"] || cfg.Trusted["MEMBER"] || cfg.MaxRules != 7 || cfg.MergeThreshold != 0.5 {
		t.Fatalf("cfg = %+v", cfg)
	}
	t.Setenv(TrustedAssociationsEnvVar, "")
	t.Setenv(MaxRulesEnvVar, "nope")
	t.Setenv(MergeThresholdEnvVar, "7")
	cfg = ConfigFromEnv()
	if !cfg.Trusted["MEMBER"] || cfg.Trusted["CONTRIBUTOR"] || cfg.MaxRules != DefaultMaxRules || cfg.MergeThreshold != DefaultMergeThreshold {
		t.Fatalf("defaults = %+v", cfg)
	}
	n := Config{MaxRules: -1, MergeThreshold: 2}.Normalized()
	if n.MaxRules != DefaultMaxRules || n.MergeThreshold != DefaultMergeThreshold || !n.Trusted["COLLABORATOR"] {
		t.Fatalf("normalized = %+v", n)
	}
}

func TestEnabledDirAndVault(t *testing.T) {
	for _, v := range []string{"0", "false", "OFF", " no "} {
		t.Setenv(DisableEnvVar, v)
		if Enabled() {
			t.Errorf("%q must disable capture", v)
		}
	}
	t.Setenv(DisableEnvVar, "")
	if !Enabled() {
		t.Error("capture is on by default")
	}
	t.Setenv(DirEnvVar, "")
	t.Setenv(VaultEnvVar, "")
	if Dir() != DefaultDir || VaultDir() != DefaultVaultDir {
		t.Fatalf("defaults = %s %s", Dir(), VaultDir())
	}
	t.Setenv(DirEnvVar, " /x/y ")
	t.Setenv(VaultEnvVar, " /v/w ")
	if Dir() != "/x/y" || VaultDir() != "/v/w" {
		t.Fatalf("overrides = %s %s", Dir(), VaultDir())
	}
}

func TestSimilarity(t *testing.T) {
	tests := []struct {
		a, b string
		want float64
	}{
		{"", "x", 0},
		{"a b c", "a b c", 1},
		{"a b", "a c", 1.0 / 3.0},
		{"...", "x", 0},
	}
	for _, tc := range tests {
		if got := similarity(tc.a, tc.b); got < tc.want-0.001 || got > tc.want+0.001 {
			t.Errorf("similarity(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
