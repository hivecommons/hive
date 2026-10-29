package prstage

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestMergeTrainReasonRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   MergeTrain
		want string
	}{
		{name: "queued", in: MergeTrain{State: MergeTrainStateQueuedBehind, Front: 15074, Position: 3, Size: 4, OverlapFiles: []string{"src/pkg/dashboard/stage_tracker.go", "src/pkg/dashboard/static/index.html"}},
			want: "merge-train:queued-behind#15074:3/4:0/0:stage_tracker.go,index.html"},
		{name: "rebasing", in: MergeTrain{State: MergeTrainStateRebasing, Front: 7, Position: 1, Size: 2, Attempt: 1, MaxAttempts: 2, OverlapFiles: []string{"a.go"}},
			want: "merge-train:rebasing#7:1/2:1/2:a.go"},
		{name: "files capped", in: MergeTrain{State: MergeTrainStateQueuedBehind, Front: 1, Position: 2, Size: 2, OverlapFiles: []string{"a", "b", "c", "d", "e", "f"}},
			want: "merge-train:queued-behind#1:2/2:0/0:a,b,c,d,+2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Reason()
			if got != tc.want {
				t.Fatalf("Reason() = %q, want %q", got, tc.want)
			}
			back, ok := ParseMergeTrain(got + ":human-approved@pat")
			if !ok {
				t.Fatalf("ParseMergeTrain(%q) failed", got)
			}
			want := tc.in
			want.OverlapFiles = mergeTrainFileNames(tc.in.OverlapFiles)
			if !reflect.DeepEqual(back, want) {
				t.Fatalf("round trip = %+v, want %+v", back, want)
			}
		})
	}
	for _, bad := range []string{"", "conflicting", "merge-train:", "merge-train:queued-behind", "merge-train:queued-behind#x:1/2:0/0:", "merge-train:queued-behind#1:1:0/0:"} {
		if _, ok := ParseMergeTrain(bad); ok {
			t.Fatalf("ParseMergeTrain(%q) must fail", bad)
		}
	}
}

func TestMergeTrainSentence(t *testing.T) {
	m := MergeTrain{State: MergeTrainStateQueuedBehind, Front: 15074, Position: 3, Size: 4, OverlapFiles: []string{"stage_tracker.go", "index.html"}}
	if got, want := m.Sentence(), "behind #15074 in merge train (3rd of 4, overlapping files: stage_tracker.go, index.html)"; got != want {
		t.Fatalf("Sentence() = %q, want %q", got, want)
	}
	for n, want := range map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd", 103: "103rd", 111: "111th"} {
		if got := Ordinal(n); got != want {
			t.Fatalf("Ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestParseMergeTrainRejectsMalformed(t *testing.T) {
	for _, reason := range []string{
		"held",
		MergeTrainReasonPrefix + "queued-behind",
		MergeTrainReasonPrefix + "#7:2/4:0/2",
		MergeTrainReasonPrefix + "queued-behind#7",
		MergeTrainReasonPrefix + "queued-behind#7:2",
		MergeTrainReasonPrefix + "queued-behind#7:2:0/2",
		MergeTrainReasonPrefix + "queued-behind#7:2/4:0",
		MergeTrainReasonPrefix + "queued-behind#x:2/4:0/2",
		MergeTrainReasonPrefix + "queued-behind#7:x/4:0/2",
		MergeTrainReasonPrefix + "queued-behind#7:2/x:0/2",
		MergeTrainReasonPrefix + "queued-behind#7:2/4:x/2",
		MergeTrainReasonPrefix + "queued-behind#7:2/4:0/x",
	} {
		if m, ok := ParseMergeTrain(reason); ok {
			t.Errorf("ParseMergeTrain(%q) = %+v, want !ok", reason, m)
		}
		if reason != "held" && !IsMergeTrainReason(reason) {
			t.Errorf("IsMergeTrainReason(%q) must be true by prefix", reason)
		}
	}
	if IsMergeTrainReason("held") {
		t.Fatal("a plain reason is no train")
	}
	// The human-approved suffix is stripped, duplicate and empty file names dropped.
	m, ok := ParseMergeTrain(MergeTrain{State: MergeTrainStateQueuedBehind, Front: 7, Position: 2, Size: 4, OverlapFiles: []string{"src/a.go", "pkg/a.go", " ", "."}}.Reason() + ":human-approved@pat")
	if !ok || m.Front != 7 || len(m.OverlapFiles) != 1 || m.OverlapFiles[0] != "a.go" {
		t.Fatalf("parsed %+v ok=%v", m, ok)
	}
	many := make([]string, 0, MergeTrainReasonMaxFiles+3)
	for i := 0; i < MergeTrainReasonMaxFiles+3; i++ {
		many = append(many, "f"+strconv.Itoa(i)+".go")
	}
	m, _ = ParseMergeTrain(MergeTrain{State: MergeTrainStateQueuedBehind, Front: 7, Position: 2, Size: 9, OverlapFiles: many}.Reason())
	if len(m.OverlapFiles) != MergeTrainReasonMaxFiles+1 || m.OverlapFiles[MergeTrainReasonMaxFiles] != "+3" {
		t.Fatalf("files capped wrong: %v", m.OverlapFiles)
	}
	for state, want := range map[string]string{
		MergeTrainStateRebasing:        "1st in the merge queue, conflicting; the hive is rebasing it onto the base (attempt 1/2; 1st of 3)",
		MergeTrainStateRebaseExhausted: "1st in the merge queue, still conflicting after 2 rebase attempts; a person decides (1st of 3)",
		MergeTrainStateFront:           "1st in the merge queue; merges next (1st of 3)",
		MergeTrainStateQueuedBehind:    "behind #7 in merge train (1st of 3)",
	} {
		got := (MergeTrain{State: state, Front: 7, Position: 1, Size: 3, Attempt: 1, MaxAttempts: 2}).Sentence()
		if got != want {
			t.Errorf("Sentence(%s) = %q, want %q", state, got, want)
		}
		// The owner read "front" / "queued-behind" as noise: no sentence a
		// person sees carries either word (hivecommons/hive#15395).
		for _, bad := range []string{"front", "queued-behind"} {
			if strings.Contains(strings.ToLower(got), bad) {
				t.Errorf("Sentence(%s) = %q leaks %q", state, got, bad)
			}
		}
	}
}
