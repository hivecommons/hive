package automerge

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah-packages#619: the author was a repository admin. The
// hive nudged the PR at 072, adopted it as contributor-offline 16 minutes
// later and then re-homed it, closing the admin's own PR. That rule became
// governor.labels.stage.adoption.maintainer_policy: never. The default is
// adopt since hivecommons/hive#14524 (a maintainer's own clanker PR sat at
// 072 for 26h with its hive offline): clanker PRs are adoptable on
// disconnect regardless of the owner's repo role. The tests below pin
// `never` where they exercise the hands-off rule; see
// automerge_adopt_maintainer_policy_test.go for adopt and ask.

// maintainerNeverPolicy is adoptPolicy() under maintainer_policy: never.
func maintainerNeverPolicy() StageLabelPolicy {
	p := adoptPolicy()
	p.AdoptionMaintainerPolicy = config.AdoptionMaintainerPolicyNever
	return p
}

func TestRepoMaintainerLevel(t *testing.T) {
	for _, tc := range []struct {
		permission, role string
		want             bool
	}{
		{"admin", "admin", true},
		{"write", "maintain", true},
		{"ADMIN", "", true},
		{"write", "write", false},
		{"write", "triage", false},
		{"read", "read", false},
		{"none", "", false},
	} {
		if got := repoMaintainerLevel(tc.permission, tc.role); got != tc.want {
			t.Errorf("repoMaintainerLevel(%q, %q) = %v, want %v", tc.permission, tc.role, got, tc.want)
		}
	}
}

func TestAdoptionSkipsRepositoryMaintainer(t *testing.T) {
	for _, role := range []string{"admin", "maintain"} {
		t.Run(role, func(t *testing.T) {
			p := maintainerNeverPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			h.adopt.permissions = map[string]string{"mallory": role}
			h.adopt.allowMaintainerEdits(7, true)
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
			// Past the grace, then two more sweeps inside one cache window.
			for _, step := range []time.Duration{2 * time.Hour, 3 * time.Minute, 3 * time.Minute} {
				h.clock.advance(step)
				h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
			}
			if n := adoptionComments(h.api); len(n) != 0 || len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
				t.Fatalf("maintainer's PR adopted: comments=%v pulls=%v refs=%v", n, h.adopt.pulls, h.adopt.refs)
			}
			rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
			if rec.Stage != stagelabels.ReturnedToContributor || rec.AdoptedHead != "" {
				t.Fatalf("record: %+v", rec)
			}
			if h.adopt.permissionReads != 1 {
				t.Fatalf("permission read %d times, want 1 (cached)", h.adopt.permissionReads)
			}
		})
	}
}

func TestAdoptionFailsClosedOnPermissionError(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, maintainerNeverPolicy())
	defer done()
	h.adopt.failPermission = true
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(2 * time.Hour)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(adoptionComments(h.api)) != 0 || len(h.adopt.pulls) != 0 || h.adopt.refAttempts != 0 {
		t.Fatalf("adopted although the permission read failed: %v", h.api.comments)
	}
	// The error is not cached: once the read works, a write-level author
	// is adopted as before.
	h.adopt.failPermission = false
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(adoptionComments(h.api)) != 1 {
		t.Fatalf("write-level author not adopted after the read recovered: %v", h.api.comments)
	}
}

// An in-place adoption taken before the author was known to maintain the
// repository is released on the next pass and never re-homed.
func TestInPlaceAdoptionOfMaintainerIsReleased(t *testing.T) {
	p := maintainerNeverPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(2 * time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() {
		t.Fatalf("setup: not adopted in place: %+v", rec)
	}

	// The author is (now) an admin; the cached write-level answer expires.
	h.adopt.permissions = map[string]string{"mallory": "admin"}
	h.clock.advance(maintainerPermTTL + time.Minute)
	h.api.reset()
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedHead != "" || rec.Stage == stagelabels.AdoptedByHive {
		t.Fatalf("maintainer's in-place adoption kept: %+v", rec)
	}
	if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
		t.Fatalf("maintainer's PR re-homed: pulls=%v refs=%v", h.adopt.pulls, h.adopt.refs)
	}
}

// The permission cache is keyed by repository and author, expires after
// maintainerPermTTL, and never caches a failed read.
func TestMaintainerPermissionCache(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, maintainerNeverPolicy())
	defer done()
	h.adopt.permissions = map[string]string{"mallory": "admin"}
	ctx := context.Background()
	obs := stageObservation{Owner: "acme", Repo: "widget", Author: "mallory", Number: 7}
	for i := 0; i < 2; i++ {
		if got, err := h.e.authorMaintainsRepo(ctx, obs, h.clock.at); !got || err != nil {
			t.Fatalf("maintainer=%v err=%v", got, err)
		}
		obs.Number++ // A second PR by the same author uses the cached read.
	}
	if h.adopt.permissionReads != 1 {
		t.Fatalf("reads=%d, want 1", h.adopt.permissionReads)
	}
	h.clock.advance(maintainerPermTTL)
	_, _ = h.e.authorMaintainsRepo(ctx, obs, h.clock.at)
	if h.adopt.permissionReads != 2 {
		t.Fatalf("expired cache reads=%d, want 2", h.adopt.permissionReads)
	}
	other := obs
	other.Author = "other"
	if got, _ := h.e.authorMaintainsRepo(ctx, other, h.clock.at); got {
		t.Fatal("a write-level author shares the maintainer's cache entry")
	}
	other = obs
	other.Repo = "gadget"
	_, _ = h.e.authorMaintainsRepo(ctx, other, h.clock.at)
	if h.adopt.permissionReads != 4 {
		t.Fatalf("cache failed to distinguish repo/author: reads=%d", h.adopt.permissionReads)
	}
	h.adopt.failPermission = true
	other.Author = "eve"
	for i := 0; i < 2; i++ {
		if got, err := h.e.authorMaintainsRepo(ctx, other, h.clock.at); got || err == nil {
			t.Fatalf("failed read: maintainer=%v err=%v", got, err)
		}
	}
	if h.adopt.permissionReads != 6 {
		t.Fatalf("failed read cached: reads=%d, want 6", h.adopt.permissionReads)
	}
}

// hivecommons/hive#14313: the cache mutex guards map access only; it is not
// held while GetPermissionLevel is in flight.
func TestMaintainerPermissionLockNotHeldAcrossAPICall(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, maintainerNeverPolicy())
	defer done()
	var held atomic.Bool
	h.adopt.onPermission = func() {
		if !h.e.maintainerPermMu.TryLock() {
			held.Store(true)
			return
		}
		h.e.maintainerPermMu.Unlock()
	}
	obs := stageObservation{Owner: "acme", Repo: "widget", Author: "mallory", Number: 7}
	if _, err := h.e.authorMaintainsRepo(context.Background(), obs, h.clock.at); err != nil {
		t.Fatal(err)
	}
	if h.adopt.permissionReads != 1 || held.Load() {
		t.Fatalf("reads=%d lock held across the API call=%v", h.adopt.permissionReads, held.Load())
	}
}

// An unreadable permission defers: a live adoption, in place or superseded,
// is neither released nor re-homed until the read succeeds.
func TestPermissionUnreadableKeepsLiveAdoption(t *testing.T) {
	t.Run("in-place", func(t *testing.T) {
		p := maintainerNeverPolicy()
		p.AdoptInPlace = true
		h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
		defer done()
		h.adopt.allowMaintainerEdits(7, true)
		h.observe(adoptPR(7, "aaa"), "check-failing:unit")
		h.clock.advance(2 * time.Hour)
		h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
		key := stageKey("acme", "widget", 7)
		before, _ := h.e.stageStoreOrShared().Get(key)
		if !before.AdoptedInPlace() {
			t.Fatalf("setup: not adopted in place: %+v", before)
		}
		h.adopt.failPermission = true
		h.clock.advance(maintainerPermTTL + time.Minute)
		h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
		rec, _ := h.e.stageStoreOrShared().Get(key)
		if !rec.AdoptedInPlace() || rec.AdoptedHead != before.AdoptedHead || rec.AdoptedBy != 0 {
			t.Fatalf("unreadable permission changed the adoption: %+v", rec)
		}
		if len(h.adopt.pulls) != 0 || h.adopt.refAttempts != 0 {
			t.Fatalf("unreadable permission re-homed: pulls=%v refs=%d", h.adopt.pulls, h.adopt.refAttempts)
		}
	})
	t.Run("supersede", func(t *testing.T) {
		h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, maintainerNeverPolicy())
		defer done()
		h.observe(adoptPR(7, "aaa"), "check-failing:unit")
		key := stageKey("acme", "widget", 7)
		h.e.stageStoreOrShared().MarkAdopted(key, "aaa", 90, h.clock.at)
		h.adopt.setSuperseder(90, "open", false)
		h.adopt.failPermission = true
		h.clock.advance(adoptCheckInterval + time.Minute)
		h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "author/clanker"), "check-failing:unit")
		rec, _ := h.e.stageStoreOrShared().Get(key)
		if rec.AdoptedHead != "aaa" || rec.AdoptedBy != 90 || rec.AdoptMode != stagelabels.AdoptModeSupersede {
			t.Fatalf("unreadable permission released a live supersede adoption: %+v", rec)
		}
		if len(h.adopt.prEdits) != 0 || len(h.adopt.pulls) != 0 || h.adopt.refAttempts != 0 {
			t.Fatalf("unreadable permission mutated the repository: edits=%v pulls=%v refs=%d", h.adopt.prEdits, h.adopt.pulls, h.adopt.refAttempts)
		}
	})
}

// hivecommons/hive#14311: releasing a maintainer's supersede adoption closes
// a still-open hive superseder with a note first; a merged or closed one is
// left alone. The maintainer's own PR is never edited.
func TestMaintainerAdoptionReleaseClosesOpenSuperseder(t *testing.T) {
	for _, tc := range []struct {
		name         string
		state        string
		merged       bool
		wantEdits    []string
		wantSupNotes int
	}{
		{name: "open", state: "open", wantEdits: []string{"90:closed"}, wantSupNotes: 1},
		{name: "closed", state: "closed"},
		{name: "merged", state: "closed", merged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, maintainerNeverPolicy())
			defer done()
			h.adopt.permissions = map[string]string{"mallory": "admin"}
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			key := stageKey("acme", "widget", 7)
			h.e.stageStoreOrShared().MarkAdopted(key, "aaa", 90, h.clock.at)
			h.adopt.setSuperseder(90, tc.state, tc.merged)
			labels := []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}
			for pass := 0; pass < 2; pass++ {
				h.api.reset()
				h.adopt.prEdits = nil
				h.clock.advance(time.Minute)
				h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
				supNotes := 0
				for _, c := range h.api.comments {
					if strings.HasPrefix(c, AdoptionMaintainerSupersederMarker) {
						supNotes++
					}
				}
				wantEdits, wantNotes := tc.wantEdits, tc.wantSupNotes
				if pass > 0 {
					wantEdits, wantNotes = nil, 0
				}
				if supNotes != wantNotes || !slices.Equal(h.adopt.prEdits, wantEdits) {
					t.Fatalf("pass %d: superseder notes=%d edits=%v comments=%v", pass, supNotes, h.adopt.prEdits, h.api.comments)
				}
				rec, _ := h.e.stageStoreOrShared().Get(key)
				if rec.AdoptedHead != "" || rec.AdoptedBy != 0 {
					t.Fatalf("pass %d: maintainer adoption kept: %+v", pass, rec)
				}
				if pass > 0 && rec.Stage == stagelabels.AdoptedByHive {
					t.Fatalf("maintainer's PR still at 074: %+v", rec)
				}
				if len(adoptionComments(h.api)) != 0 || len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
					t.Fatalf("maintainer takeover: comments=%v pulls=%v refs=%v", h.api.comments, h.adopt.pulls, h.adopt.refs)
				}
			}
		})
	}
}

func TestMaintainerSupersederCloseFailureKeepsAdoption(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, maintainerNeverPolicy())
	defer done()
	h.adopt.permissions = map[string]string{"mallory": "admin"}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	key := stageKey("acme", "widget", 7)
	h.e.stageStoreOrShared().MarkAdopted(key, "aaa", 90, h.clock.at)
	// No scripted superseder: GET pulls/90 is 404, so the release must wait.
	h.clock.advance(adoptCheckInterval + time.Minute)
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", adoptPR(7, "aaa", "stage/074-adopted-by-hive", "author/clanker"), "check-failing:unit", false, nil)
	if _, err := h.e.ApplyStageLabels(context.Background()); err == nil {
		t.Fatal("superseder lookup failure was not surfaced")
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.AdoptedHead != "aaa" || rec.AdoptedBy != 90 {
		t.Fatalf("release orphaned the superseder: %+v", rec)
	}
}

func TestSupersedeMaintainerRefusesBeforeMutation(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, maintainerNeverPolicy())
	defer done()
	h.adopt.permissions = map[string]string{"mallory": "admin"}
	obs := stageObservation{Owner: "acme", Repo: "widget", Author: "mallory", Number: 7, HeadSHA: "aaa", BaseRef: "v5"}
	if _, err := h.e.supersedeAdoptedPR(context.Background(), h.e.stageStoreOrShared(), stageKey("acme", "widget", 7), obs, stagelabels.Record{}, maintainerNeverPolicy(), AdoptReasonDCORehome, 0, false, true, h.clock.at); err != nil {
		t.Fatal(err)
	}
	if h.adopt.refAttempts != 0 || len(h.adopt.pulls) != 0 || len(h.adopt.prEdits) != 0 {
		t.Fatal("maintainer supersession mutated repository")
	}
}

func TestDelegatedSupersedeCannotTakeOverMaintainer(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, maintainerNeverPolicy())
	defer done()
	h.adopt.permissions = map[string]string{"mallory": "maintain"}
	labels := []string{"stage/085-needs-decision", delegatedecision.LabelAdoptSupersede}
	h.observe(adoptPR(7, "aaa", labels...), contributorLaneReasonNoReviewer)
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedHead != "" || h.adopt.refAttempts != 0 || len(h.adopt.pulls) != 0 {
		t.Fatalf("delegated request adopted maintainer: %+v", rec)
	}
}
