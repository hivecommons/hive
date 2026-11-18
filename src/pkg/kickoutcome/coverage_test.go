package kickoutcome

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpenCorruptAndUnreadable(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o600)
	if s, err := Open(bad, nil); err == nil || s == nil || len(s.Agents()) != 0 {
		t.Fatalf("corrupt file: %v", err)
	}
	if s, err := Open(dir, nil); err == nil || s == nil {
		t.Fatalf("a directory is not a store: %v", err)
	}
	if s, err := Open(filepath.Join(dir, "missing.json"), nil); err != nil || s == nil {
		t.Fatalf("missing file is an empty store: %v", err)
	}
}

func TestOpenTrimsLoadedRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.json")
	s, _ := Open(p, nil)
	for i := 0; i < MaxRecordsPerAgent; i++ {
		s.Delivered("a", t0.Add(time.Duration(i)*time.Hour))
	}
	s2, _ := Open(p, nil)
	if len(s2.Records("a", 3)) != 3 || len(s2.Agents()) != 1 {
		t.Fatal("limit or agents")
	}
}

func TestAgentsAreBounded(t *testing.T) {
	s, _ := Open("", nil)
	for i := 0; i < MaxAgents+5; i++ {
		s.Delivered("agent"+strconv.Itoa(i), t0.Add(time.Duration(i)*time.Minute))
	}
	if n := len(s.Agents()); n != MaxAgents {
		t.Fatalf("%d agents", n)
	}
	if len(s.Records("agent0", 0)) != 0 {
		t.Fatal("the oldest agent was kept")
	}
}

func TestPushCommentAndDeclinedSignals(t *testing.T) {
	s, _ := Open("", nil)
	s.Delivered("a", t0)
	s.Pushed("a", "fix/x", "abc123", t0.Add(time.Minute))
	s.Pushed("a", "fix/y", "def456", t0.Add(2*time.Minute)) // first push wins
	s.Commented("a", t0.Add(time.Minute))
	s.Declined("a", "I'm declining to execute this", t0.Add(time.Minute))
	s.Declined("a", "again", t0.Add(time.Minute)) // already refused: ignored
	s.SetClock(func() time.Time { return t0.Add(5 * time.Minute) })
	d := s.Recent("a", 10)
	if len(d) != 1 || d[0].Kind != KindRefused || d[0].RefusalCode != "agent_declined" || !strings.Contains(d[0].Quote, "declining") {
		t.Fatalf("%+v", d)
	}
	r := s.Records("a", 1)[0]
	if r.PushBranch != "fix/x" || r.CommentAt == nil {
		t.Fatalf("%+v", r)
	}
	// A push alone derives pushed_commit and keeps branch and sha.
	s.Delivered("b", t0)
	s.Pushed("b", "fix/x", "abc123", t0.Add(time.Minute))
	b := s.Recent("b", 1)[0]
	if b.Kind != KindPushedCommit || b.Branch != "fix/x" || b.SHA != "abc123" || b.Summary != "pushed a commit to fix/x" {
		t.Fatalf("%+v", b)
	}
	s.Delivered("c", t0)
	s.Commented("c", t0.Add(time.Minute))
	if c := s.Recent("c", 1)[0]; c.Kind != KindCommented || c.Summary != "commented" {
		t.Fatalf("%+v", c)
	}
}

func TestSummaries(t *testing.T) {
	for kind, want := range map[string]string{
		KindStillRunning: "still running", KindEndedNoVisibleAction: "ended, no visible action",
		KindStoodDown: "stood down", KindOpenedPR: "opened a PR",
	} {
		if got := Summary(Record{}, Outcome{Kind: kind}); got != want {
			t.Errorf("%s: %q", kind, got)
		}
	}
	if got := Summary(Record{PushBranch: ""}, Outcome{Kind: KindPushedCommit}); got != "pushed a commit" {
		t.Error(got)
	}
	if got := Summary(Record{RefusalCode: "paused"}, Outcome{Kind: KindRefused}); got != "refused (paused)" {
		t.Error(got)
	}
}

func TestLatestAndLatestOf(t *testing.T) {
	s, _ := Open("", nil)
	s.SetClock(func() time.Time { return t0.Add(time.Minute) })
	s.Delivered("a", t0)
	if l := s.Latest(); len(l) != 1 || l["a"].Kind != KindStillRunning {
		t.Fatalf("%+v", l)
	}
	if _, ok := s.LatestOf("nobody"); ok {
		t.Fatal("unknown agent")
	}
	var nilStore *Store
	if len(nilStore.Latest()) != 0 || nilStore.Records("a", 0) != nil || nilStore.Agents() != nil {
		t.Fatal("nil store")
	}
	nilStore.Delivered("a", t0)
	nilStore.Refused("a", Trigger{}, "paused", "x", t0)
	nilStore.TurnEnded("a", "ended", "", t0, t0)
	nilStore.Intend("a", Trigger{})
	nilStore.SetClock(time.Now)
	nilStore.Declined("a", "x", t0)
	nilStore.PROpened("a", "u", 1, t0)
	nilStore.Pushed("a", "b", "s", t0)
	nilStore.Commented("a", t0)
	if v := nilStore.Recent("a", 3); len(v) != 0 {
		t.Fatal("nil recent")
	}
}

func TestRefusedNeedsACode(t *testing.T) {
	s, _ := Open("", nil)
	s.Refused("a", Trigger{Kind: TriggerGovernor}, "", "x", t0)
	s.Refused("", Trigger{Kind: TriggerGovernor}, "paused", "x", t0)
	if len(s.Agents()) != 0 {
		t.Fatal("a refusal with no code or agent was recorded")
	}
}

func TestSavingFailureIsLoggedNotFatal(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	s, _ := Open(filepath.Join(file, "sub", "k.json"), nil) // parent is a file
	s.Delivered("a", t0)
	if len(s.Records("a", 0)) != 1 {
		t.Fatal("the in-memory record was lost with the save")
	}
}

func TestCleanRefAndCap(t *testing.T) {
	if got := cleanRef("  a b\tc\x00d ", 10); got != "abcd" {
		t.Fatalf("%q", got)
	}
	if capRunes("abcdef", 0) != "" || capRunes("abc", 5) != "abc" || capRunes("abcdef", 3) != "ab…" {
		t.Fatal("capRunes")
	}
}
