package tokens

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestParseLaunchRef(t *testing.T) {
	cases := []struct {
		in   string
		want LaunchRef
		ok   bool
	}{
		{"hivecommons/hive#12128", LaunchRef{Ref: "hivecommons/hive#12128", Kind: LaunchRefKindIssue, Repo: "hivecommons/hive", Number: 12128}, true},
		{"  org/repo#7 ", LaunchRef{Ref: "org/repo#7", Kind: LaunchRefKindIssue, Repo: "org/repo", Number: 7}, true},
		{"repo!EXT-1", LaunchRef{Ref: "repo!EXT-1", Kind: LaunchRefKindExternal}, true},
		{"org/repo#0", LaunchRef{Ref: "org/repo#0", Kind: LaunchRefKindExternal}, true},
		{"org/repo#", LaunchRef{Ref: "org/repo#", Kind: LaunchRefKindExternal}, true},
		{"", LaunchRef{}, false},
	}
	for _, c := range cases {
		got, ok := ParseLaunchRef(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseLaunchRef(%q) = %+v,%v want %+v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestPRLaunchRefCarriesHead(t *testing.T) {
	r := PRLaunchRef("org/repo", 42, " abc123 ")
	if r.Ref != "org/repo#42" || r.Kind != LaunchRefKindPR || r.Number != 42 || r.HeadSHA != "abc123" {
		t.Fatalf("unexpected ref %+v", r)
	}
}

func TestLaunchLogPersistsAndRestores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "launch.jsonl")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	l := NewLaunchLog(path, logger)
	l.Record(LaunchRecord{Agent: "scanner", Trigger: "governor-eval", LaunchedAtMs: 2000,
		Refs: []LaunchRef{{Ref: "o/r#1", Kind: LaunchRefKindIssue, Repo: "o/r", Number: 1}}})
	l.Record(LaunchRecord{Agent: "reviewer", Trigger: "review-review", LaunchedAtMs: 1000,
		Refs: []LaunchRef{PRLaunchRef("o/r", 2, "deadbeef")}})
	l.Record(LaunchRecord{Agent: "", LaunchedAtMs: 3000}) // dropped: no agent

	got := l.Records()
	if len(got) != 2 || got[0].Agent != "reviewer" || got[1].Agent != "scanner" {
		t.Fatalf("Records() = %+v, want reviewer then scanner (launch order)", got)
	}

	restored := NewLaunchLog(path, logger).Records()
	if len(restored) != 2 || restored[1].Refs[0].Ref != "o/r#1" || restored[0].Refs[0].HeadSHA != "deadbeef" {
		t.Fatalf("restored = %+v", restored)
	}
}

func TestLaunchLogRingBoundAndCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch.jsonl")
	l := NewLaunchLog(path, nil)
	for i := 1; i <= launchLogRingCap+5; i++ {
		l.Record(LaunchRecord{Agent: "a", LaunchedAtMs: int64(i)})
	}
	if n := len(l.Records()); n != launchLogRingCap {
		t.Fatalf("ring holds %d, want %d", n, launchLogRingCap)
	}
	restored := NewLaunchLog(path, nil).Records()
	if len(restored) != launchLogRingCap || restored[0].LaunchedAtMs != 6 {
		t.Fatalf("restored %d records starting at %d; want %d starting at 6", len(restored), restored[0].LaunchedAtMs, launchLogRingCap)
	}
}

func TestCollectorLaunchHooksNilSafe(t *testing.T) {
	c := NewCollector(t.TempDir(), nil)
	c.RecordLaunch(LaunchRecord{Agent: "x", LaunchedAtMs: 1}) // no log attached: no-op
	if got := c.Launches(); got != nil {
		t.Fatalf("Launches() without a log = %v, want nil", got)
	}
	c.SetLaunchLog(NewLaunchLog("", nil))
	c.RecordLaunch(LaunchRecord{Agent: "x", LaunchedAtMs: 1})
	if got := c.Launches(); len(got) != 1 {
		t.Fatalf("Launches() = %v, want one record", got)
	}
	var nilC *Collector
	nilC.RecordLaunch(LaunchRecord{Agent: "x"})
	if nilC.Launches() != nil {
		t.Fatal("nil collector must return nil launches")
	}
}
