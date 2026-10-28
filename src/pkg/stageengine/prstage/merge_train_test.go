package prstage

import (
	"reflect"
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
