package hub

import (
	"testing"
	"time"
)

// fixedReapNow is the clock every case in this file is evaluated against, so
// ages are exact rather than relative to a running wall clock.
var fixedReapNow = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

// reapAgo builds a deletionTimestamp d before fixedReapNow.
func reapAgo(d time.Duration) time.Time { return fixedReapNow.Add(-d) }

// TestPodIsOrphanedTerminating pins the exact predicate that cleared 32 pods by
// hand with zero collateral damage (#5328):
//
//	deletionTimestamp != null && finalizers == [] && phase != "Running" &&
//	  age(deletionTimestamp) > threshold
//
// The NEGATIVE cases are the point of this test. Each of them is a pod that a
// looser predicate would delete and that must survive: loosening any one clause
// converts a cleanup tool into an outage, and this table is what makes that
// regression fail CI instead of production.
func TestPodIsOrphanedTerminating(t *testing.T) {
	cases := []struct {
		name string
		pod  orphanedPodCandidate
		want bool
		why  string
	}{
		// --- The signature that must be reaped. ---
		{
			name: "orphan: deleted, no finalizers, Failed, three weeks old",
			pod: orphanedPodCandidate{
				Namespace:         "hive-hosted-abc",
				Name:              "hive-1",
				DeletionTimestamp: reapAgo(21 * 24 * time.Hour),
				Finalizers:        nil,
				Phase:             "Failed",
			},
			want: true,
			why:  "the exact measured signature — the oldest of the 27 orphans",
		},
		{
			name: "orphan: Succeeded phase, empty (non-nil) finalizer slice",
			pod: orphanedPodCandidate{
				DeletionTimestamp: reapAgo(2 * time.Hour),
				Finalizers:        []string{},
				Phase:             "Succeeded",
			},
			want: true,
			why:  "finalizers: [] is the observed shape; empty slice == nil here",
		},
		{
			name: "orphan: Pending, just past the threshold",
			pod: orphanedPodCandidate{
				DeletionTimestamp: reapAgo(orphanedPodMinAge + time.Minute),
				Phase:             "Pending",
			},
			want: true,
			why:  "strictly greater than the threshold is enough",
		},
		{
			name: "orphan: Unknown phase — the classic lost-kubelet phase",
			pod: orphanedPodCandidate{
				DeletionTimestamp: reapAgo(6 * time.Hour),
				Phase:             "Unknown",
			},
			want: true,
			why:  "Unknown is precisely what a vanished node leaves behind",
		},

		// --- NEGATIVE: has finalizers. Never delete. ---
		{
			name: "skipped: has a finalizer, otherwise a perfect match",
			pod: orphanedPodCandidate{
				DeletionTimestamp: reapAgo(21 * 24 * time.Hour),
				Finalizers:        []string{"kubernetes.io/pv-protection"},
				Phase:             "Failed",
			},
			want: false,
			why:  "a finalizer means a controller has unfinished work; forcing past it strands the resource silently",
		},
		{
			name: "skipped: multiple finalizers",
			pod: orphanedPodCandidate{
				DeletionTimestamp: reapAgo(30 * 24 * time.Hour),
				Finalizers:        []string{"foo.io/cleanup", "bar.io/dereg"},
				Phase:             "Unknown",
			},
			want: false,
			why:  "age never overrides a finalizer, no matter how old",
		},
		{
			name: "skipped: finalizer on an otherwise ancient Succeeded pod",
			pod: orphanedPodCandidate{
				DeletionTimestamp: reapAgo(365 * 24 * time.Hour),
				Finalizers:        []string{"example.com/f"},
				Phase:             "Succeeded",
			},
			want: false,
			why:  "the finalizer clause is absolute",
		},

		// --- NEGATIVE: Running. Never delete. ---
		{
			name: "skipped: Running with a deletionTimestamp (normal shutdown)",
			pod: orphanedPodCandidate{
				DeletionTimestamp: reapAgo(2 * time.Hour),
				Phase:             podPhaseRunning,
			},
			want: false,
			why:  "a Running pod mid-shutdown is still serving; reaping it is an outage caused by the cleanup tool",
		},
		{
			name: "skipped: Running and very old — this is the live spoke",
			pod: orphanedPodCandidate{
				Namespace:         "hive-hosted-abc",
				Name:              "hive-live",
				DeletionTimestamp: reapAgo(21 * 24 * time.Hour),
				Phase:             podPhaseRunning,
			},
			want: false,
			why:  "every measured namespace kept exactly 1 Running pod; this clause is what guarantees it is untouchable",
		},
		{
			name: "skipped: Running with surrounding whitespace",
			pod: orphanedPodCandidate{
				DeletionTimestamp: reapAgo(5 * time.Hour),
				Phase:             "  Running  ",
			},
			want: false,
			why:  "phase is trimmed before comparison so whitespace cannot smuggle a live pod past the guard",
		},

		// --- NEGATIVE: too young. Never delete yet. ---
		{
			name: "skipped: deleted seconds ago — normal termination in progress",
			pod: orphanedPodCandidate{
				DeletionTimestamp: reapAgo(5 * time.Second),
				Phase:             "Failed",
			},
			want: false,
			why:  "normal termination completes in seconds; this pod is shutting down correctly",
		},
		{
			name: "skipped: deleted 59 minutes ago — inside the window",
			pod: orphanedPodCandidate{
				DeletionTimestamp: reapAgo(59 * time.Minute),
				Phase:             "Unknown",
			},
			want: false,
			why:  "under the threshold, so presumed healthy",
		},
		{
			name: "skipped: exactly at the threshold (boundary is exclusive)",
			pod: orphanedPodCandidate{
				DeletionTimestamp: reapAgo(orphanedPodMinAge),
				Phase:             "Failed",
			},
			want: false,
			why:  "the rule is strictly greater-than; being one sweep late is cheaper than being early",
		},

		// --- NEGATIVE: never deleted at all. ---
		{
			name: "skipped: no deletionTimestamp — a live pod",
			pod: orphanedPodCandidate{
				Phase: "Failed",
			},
			want: false,
			why:  "without a deletionTimestamp this would be a NEW delete, not the completion of an issued one",
		},
		{
			name: "skipped: no deletionTimestamp and not Running",
			pod: orphanedPodCandidate{
				Phase: "Pending",
			},
			want: false,
			why:  "a Pending pod nobody asked to delete is just scheduling",
		},

		// --- Guard against a future clock skew / negative age. ---
		{
			name: "skipped: deletionTimestamp in the future (clock skew)",
			pod: orphanedPodCandidate{
				DeletionTimestamp: fixedReapNow.Add(time.Hour),
				Phase:             "Failed",
			},
			want: false,
			why:  "a negative age can never exceed the threshold",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := podIsOrphanedTerminating(tc.pod, fixedReapNow, orphanedPodMinAge)
			if got != tc.want {
				t.Fatalf("podIsOrphanedTerminating = %v, want %v\nreason: %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestOrphanedPodMinAgeIsGenerous pins the threshold's intent. A normal
// termination is seconds; if someone ever shrinks this to minutes the predicate
// starts racing legitimate shutdowns.
func TestOrphanedPodMinAgeIsGenerous(t *testing.T) {
	if orphanedPodMinAge < time.Hour {
		t.Fatalf("orphanedPodMinAge = %v, want >= 1h — a shorter window races normal termination "+
			"(default grace period is 30s, and slow shutdowns are legitimate)", orphanedPodMinAge)
	}
}

// TestParseOrphanCandidates covers the kubectl-JSON decode, including the
// timestamp cases that must fail SAFE.
func TestParseOrphanCandidates(t *testing.T) {
	raw := []byte(`{"items":[
      {"metadata":{"name":"orphan","namespace":"hive-hosted-a","deletionTimestamp":"2026-08-10T00:00:00Z","finalizers":[]},
       "status":{"phase":"Failed"}},
      {"metadata":{"name":"live","namespace":"hive-hosted-a"},
       "status":{"phase":"Running"}},
      {"metadata":{"name":"held","namespace":"hive-hosted-a","deletionTimestamp":"2026-08-10T00:00:00Z","finalizers":["x.io/f"]},
       "status":{"phase":"Failed"}},
      {"metadata":{"name":"badstamp","namespace":"hive-hosted-a","deletionTimestamp":"not-a-timestamp"},
       "status":{"phase":"Failed"}}
    ]}`)

	got, err := parseOrphanCandidates(raw)
	if err != nil {
		t.Fatalf("parseOrphanCandidates returned error: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("parsed %d candidates, want 4", len(got))
	}

	byName := map[string]orphanedPodCandidate{}
	for _, c := range got {
		byName[c.Name] = c
	}

	if byName["orphan"].DeletionTimestamp.IsZero() {
		t.Error("orphan: deletionTimestamp should have parsed")
	}
	if byName["orphan"].Phase != "Failed" || byName["orphan"].Namespace != "hive-hosted-a" {
		t.Errorf("orphan: unexpected fields %+v", byName["orphan"])
	}
	if !byName["live"].DeletionTimestamp.IsZero() {
		t.Error("live: absent deletionTimestamp must stay zero")
	}
	if len(byName["held"].Finalizers) != 1 {
		t.Errorf("held: finalizers not carried through: %+v", byName["held"].Finalizers)
	}
	// The safety-critical case: an unparseable timestamp must yield the zero
	// value, which the predicate rejects. Never guess an age.
	if !byName["badstamp"].DeletionTimestamp.IsZero() {
		t.Error("badstamp: unparseable deletionTimestamp must leave the zero value so the pod is SKIPPED")
	}
	if podIsOrphanedTerminating(byName["badstamp"], fixedReapNow, orphanedPodMinAge) {
		t.Error("badstamp: a pod with an unreadable timestamp must never be reaped")
	}
}

func TestParseOrphanCandidatesRejectsGarbage(t *testing.T) {
	if _, err := parseOrphanCandidates([]byte("not json")); err == nil {
		t.Fatal("expected an error on non-JSON input")
	}
}

func TestParseOrphanCandidatesEmptyList(t *testing.T) {
	got, err := parseOrphanCandidates([]byte(`{"items":[]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d candidates from an empty list, want 0", len(got))
	}
}

// TestSelectOrphanedPods reproduces the measured incident shape end to end over
// the pure path: one namespace holding the live Running spoke plus several
// orphans, with a finalizer-held pod and a young pod mixed in. Only the true
// orphans may be selected.
func TestSelectOrphanedPods(t *testing.T) {
	candidates := []orphanedPodCandidate{
		{Name: "live", Phase: podPhaseRunning, DeletionTimestamp: reapAgo(21 * 24 * time.Hour)},
		{Name: "orphan-old", Phase: "Failed", DeletionTimestamp: reapAgo(21 * 24 * time.Hour)},
		{Name: "orphan-mid", Phase: "Unknown", DeletionTimestamp: reapAgo(3 * time.Hour)},
		{Name: "held", Phase: "Failed", DeletionTimestamp: reapAgo(9 * time.Hour), Finalizers: []string{"x.io/f"}},
		{Name: "young", Phase: "Failed", DeletionTimestamp: reapAgo(time.Minute)},
		{Name: "not-deleted", Phase: "Failed"},
	}

	got := selectOrphanedPods(candidates, fixedReapNow, orphanedPodMinAge)
	if len(got) != 2 {
		t.Fatalf("selected %d pods, want 2: %+v", len(got), got)
	}
	selected := map[string]bool{}
	for _, c := range got {
		selected[c.Name] = true
	}
	for _, want := range []string{"orphan-old", "orphan-mid"} {
		if !selected[want] {
			t.Errorf("expected %q to be selected", want)
		}
	}
	// The live spoke surviving is the single most important assertion here.
	for _, mustSurvive := range []string{"live", "held", "young", "not-deleted"} {
		if selected[mustSurvive] {
			t.Errorf("%q must NOT be selected — the predicate has been loosened", mustSurvive)
		}
	}
}

// TestSelectOrphanedPodsNoneWhenHealthy pins the common steady state: a
// namespace with only a live pod yields nothing, so a healthy fleet is a no-op.
func TestSelectOrphanedPodsNoneWhenHealthy(t *testing.T) {
	got := selectOrphanedPods([]orphanedPodCandidate{
		{Name: "live", Phase: podPhaseRunning},
	}, fixedReapNow, orphanedPodMinAge)
	if len(got) != 0 {
		t.Fatalf("selected %d pods from a healthy namespace, want 0", len(got))
	}
}
