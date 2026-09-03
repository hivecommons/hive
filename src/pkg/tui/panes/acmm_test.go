package panes

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/tui/client"
)

const acmmWidth = 80

// packs returns a small, DELIBERATELY non-canonical pack list: three levels,
// not six, and not numbered from one. Every test here uses it, which is what
// keeps the overlay honest about the server owning the list — code that assumed
// L1-L6, or that the index into the slice was the level, fails on this fixture
// rather than passing by coincidence against a realistic one.
func packs(currentLevel int) client.ACMMStatus {
	defs := []struct {
		level int
		name  string
		desc  string
		count int
		modes string
		merge string
	}{
		{2, "Assisted", "Agents propose, humans merge.", 4, "advisory", "human"},
		{4, "Supervised", "Agents merge green PRs under review.", 9, "supervised", "lgtm"},
		{5, "Autonomous", "Agents own the merge path.", 12, "autonomous", "auto"},
	}
	status := client.ACMMStatus{}
	for _, d := range defs {
		status.Packs = append(status.Packs, client.Pack{
			Level:       d.level,
			Name:        d.name,
			Description: d.desc,
			AgentCount:  d.count,
			Current:     d.level == currentLevel,
			Governor:    client.PackGovernor{Modes: d.modes, MergePolicy: d.merge},
		})
	}
	status.Level = 0
	if _, ok := status.Current(); ok {
		status.Level = currentLevel
	}
	return status
}

// loadedOverlay is the overlay in the state an operator navigates: packs
// fetched, cursor on the level in force.
func loadedOverlay(currentLevel int) ACMMOverlay {
	return NewACMMOverlay().SetStatus(packs(currentLevel))
}

// confirmingOverlay is the overlay with the typed confirmation open for a level
// other than the one in force.
func confirmingOverlay(t *testing.T, currentLevel, target int) ACMMOverlay {
	t.Helper()
	o := loadedOverlay(currentLevel)
	for {
		pack, ok := o.SelectedPack()
		if !ok {
			t.Fatalf("level %d is not in the fixture", target)
		}
		if pack.Level == target {
			break
		}
		next := o.Move(1)
		if next.selected == o.selected {
			t.Fatalf("level %d is not in the fixture", target)
		}
		o = next
	}
	o, ok := o.BeginConfirm()
	if !ok {
		t.Fatalf("BeginConfirm() for L%d refused", target)
	}
	return o
}

// apiError builds the client error a non-2xx response produces, so the tests
// exercise the same errors.As path the overlay uses rather than a stand-in.
func apiError(code int) error {
	return &client.APIError{StatusCode: code, Method: http.MethodPut, Path: "/api/packs/level"}
}

// TestACMMOverlayLoadingState: the overlay opens already asking, and says so. A
// blank box while a request is in flight is indistinguishable from a hive that
// defines no packs, which is the confusion the two separate strings prevent.
func TestACMMOverlayLoadingState(t *testing.T) {
	o := NewACMMOverlay()
	if !o.Loading() {
		t.Fatal("a freshly opened overlay is not loading; the packs call is issued with it")
	}
	if _, ok := o.SelectedPack(); ok {
		t.Error("a loading overlay offers a selection, so enter would confirm a level nobody chose")
	}
	if _, ok := o.BeginConfirm(); ok {
		t.Error("enter began a confirmation before any pack existed to apply")
	}
	view := flat(o.View(acmmWidth))
	if !strings.Contains(view, "Loading ACMM packs") {
		t.Errorf("loading view does not say it is loading:\n%s", view)
	}
}

// TestACMMOverlayRendersEveryPackField is the acceptance criterion that the
// list is informative enough to choose from: level, name, description, agent
// count and the recommended governor mode and merge policy.
func TestACMMOverlayRendersEveryPackField(t *testing.T) {
	view := flat(loadedOverlay(4).View(acmmWidth))
	for _, p := range packs(4).Packs {
		for _, want := range []string{
			fmt.Sprintf("L%d", p.Level),
			p.Name,
			p.Description,
			fmt.Sprintf("%d agents", p.AgentCount),
			p.Governor.Modes,
			p.Governor.MergePolicy,
		} {
			if !strings.Contains(view, want) {
				t.Errorf("pack list omits %q for L%d:\n%s", want, p.Level, view)
			}
		}
	}
}

// TestACMMOverlayMarksAndPreselectsTheCurrentLevel. The marker is what an
// operator reads to know where the hive IS before choosing where it goes, and
// opening the cursor there means the first thing under it is never a change.
func TestACMMOverlayMarksAndPreselectsTheCurrentLevel(t *testing.T) {
	o := loadedOverlay(4)
	pack, ok := o.SelectedPack()
	if !ok {
		t.Fatal("nothing selected after a successful pack list")
	}
	if pack.Level != 4 {
		t.Errorf("cursor opened on L%d, want the level in force (L4)", pack.Level)
	}
	view := flat(o.View(acmmWidth))
	if !strings.Contains(view, "current L4") {
		t.Errorf("overlay does not report the level in force:\n%s", view)
	}
	if strings.Count(view, "(current)") != 1 {
		t.Errorf("want exactly one (current) marker in:\n%s", view)
	}
}

// TestACMMOverlayUnconfiguredHiveIsNotAnError: packs exist, none is flagged
// current. client.ACMMStatus.Level is 0 for that, and it is an ordinary state
// of a hive nobody has set a level on — so it must read as a fact, not a fault.
func TestACMMOverlayUnconfiguredHiveIsNotAnError(t *testing.T) {
	o := NewACMMOverlay().SetStatus(packs(0))
	view := flat(o.View(acmmWidth))
	if !strings.Contains(view, acmmNoCurrent) {
		t.Errorf("overlay does not say no level is configured:\n%s", view)
	}
	if strings.Contains(view, "(current)") {
		t.Errorf("overlay marked a current level on a hive that has none:\n%s", view)
	}
	// Every level is applicable on such a hive, including the first one.
	if _, ok := o.BeginConfirm(); !ok {
		t.Error("no level can be applied on an unconfigured hive")
	}
}

// TestACMMOverlayEmptyPackListIsAnExplicitSafeState. An acceptance criterion:
// a successful call that returned nothing must say so, and must not offer
// anything to apply.
func TestACMMOverlayEmptyPackListIsAnExplicitSafeState(t *testing.T) {
	o := NewACMMOverlay().SetStatus(client.ACMMStatus{})
	if _, ok := o.SelectedPack(); ok {
		t.Error("an empty pack list offers a selection")
	}
	if _, ok := o.BeginConfirm(); ok {
		t.Error("enter began a confirmation with no packs to apply")
	}
	view := flat(o.View(acmmWidth))
	if !strings.Contains(view, acmmNoPacks) {
		t.Errorf("empty list does not say so:\n%s", view)
	}
}

// TestACMMOverlayListErrors: a 403 names the access required, anything else is
// preserved verbatim so an operator can act on what the server actually said.
func TestACMMOverlayListErrors(t *testing.T) {
	t.Run("forbidden", func(t *testing.T) {
		o := NewACMMOverlay().SetStatusError(apiError(http.StatusForbidden))
		view := flat(o.View(acmmWidth))
		if !strings.Contains(view, "owner access required") {
			t.Errorf("403 does not name the access required:\n%s", view)
		}
	})
	t.Run("other", func(t *testing.T) {
		o := NewACMMOverlay().SetStatusError(errors.New("connection refused"))
		view := flat(o.View(acmmWidth))
		if !strings.Contains(view, "connection refused") {
			t.Errorf("the server's error was not preserved:\n%s", view)
		}
	})
	t.Run("nothing is applicable after a failed list", func(t *testing.T) {
		o := NewACMMOverlay().SetStatusError(errors.New("boom"))
		if _, ok := o.BeginConfirm(); ok {
			t.Error("enter began a confirmation against a list that failed to load")
		}
	})
}

// TestACMMOverlayMoveClampsAtTheEdges: holding a direction parks at an end
// rather than wrapping past it onto a level the operator did not aim at.
func TestACMMOverlayMoveClampsAtTheEdges(t *testing.T) {
	o := loadedOverlay(4)
	for i := 0; i < 5; i++ {
		o = o.Move(-1)
	}
	if pack, _ := o.SelectedPack(); pack.Level != 2 {
		t.Errorf("cursor parked on L%d after moving up repeatedly, want the first pack (L2)", pack.Level)
	}
	for i := 0; i < 5; i++ {
		o = o.Move(1)
	}
	if pack, _ := o.SelectedPack(); pack.Level != 5 {
		t.Errorf("cursor parked on L%d after moving down repeatedly, want the last pack (L5)", pack.Level)
	}
}

// TestACMMOverlayCurrentLevelIsAnExplicitNoOp. The acceptance criteria name
// this: pressing enter on the level already in force must neither apply nor be
// silently swallowed. The operator pressed a key and is owed an answer.
func TestACMMOverlayCurrentLevelIsAnExplicitNoOp(t *testing.T) {
	o := loadedOverlay(4)
	next, ok := o.BeginConfirm()
	if ok {
		t.Fatal("enter on the level already in force began a confirmation")
	}
	if next.Confirming() {
		t.Error("the overlay entered the confirmation state for the current level")
	}
	view := flat(next.View(acmmWidth))
	if !strings.Contains(view, "already the level in force") {
		t.Errorf("the no-op is silent; the view does not explain it:\n%s", view)
	}
}

// TestACMMOverlayEnterDoesNotApply is the safety property the whole design
// rests on: the first enter moves into a confirmation and NOTHING is sent.
func TestACMMOverlayEnterDoesNotApply(t *testing.T) {
	o := confirmingOverlay(t, 4, 5)
	if !o.Confirming() {
		t.Fatal("enter on a non-current level did not open the confirmation")
	}
	if o.Pending() {
		t.Fatal("enter on a non-current level marked an apply as started")
	}
	if _, _, ok := o.Apply(); ok {
		t.Fatal("Apply() succeeded with nothing typed — enter would have applied immediately")
	}
	view := flat(o.View(acmmWidth))
	if !strings.Contains(view, ConfirmPhrase(5)) {
		t.Errorf("the confirmation does not name the phrase to type:\n%s", view)
	}
	if !strings.Contains(view, "Apply L5") {
		t.Errorf("the confirmation does not name the level being applied:\n%s", view)
	}
	// The blast radius is the reason this state exists; check the clauses
	// individually rather than the whole sentence, which the box wraps.
	for _, clause := range []string{"FLEET-WIDE", "roster", "pause", "resume", "mode override", "governor"} {
		if !strings.Contains(view, clause) {
			t.Errorf("the confirmation does not warn about %q:\n%s", clause, view)
		}
	}
}

// TestACMMOverlayOnlyTheExactPhraseApplies enumerates the near misses. Each one
// is a plausible thing an operator actually types, and every one of them must
// refuse: a confirmation that accepts an approximation is not a confirmation.
func TestACMMOverlayOnlyTheExactPhraseApplies(t *testing.T) {
	refuse := []string{
		"",
		"A",
		"APPLY",
		"APPLY ",
		"APPLY L",
		"APPLY L5 ",   // trailing space
		" APPLY L5",   // leading space
		"apply l5",    // wrong case
		"Apply L5",    // wrong case
		"APPLY  L5",   // doubled space
		"APPLY L4",    // the level the hive is already on, not the target
		"APPLY L55",   // right prefix, wrong level
		"APPLY L5x",   // trailing junk
		"YES",         //
		"APPLYL5",     // missing space
		"APPLY LEVEL", //
	}
	for _, typed := range refuse {
		t.Run(fmt.Sprintf("%q", typed), func(t *testing.T) {
			o := confirmingOverlay(t, 4, 5)
			for _, r := range typed {
				o = o.Type(string(r))
			}
			next, level, ok := o.Apply()
			if ok {
				t.Fatalf("typing %q applied L%d", typed, level)
			}
			if next.Pending() {
				t.Fatalf("typing %q left the overlay pending", typed)
			}
		})
	}

	t.Run("the exact phrase", func(t *testing.T) {
		o := confirmingOverlay(t, 4, 5)
		for _, r := range ConfirmPhrase(5) {
			o = o.Type(string(r))
		}
		next, level, ok := o.Apply()
		if !ok {
			t.Fatalf("the exact phrase %q did not apply", ConfirmPhrase(5))
		}
		if level != 5 {
			t.Errorf("Apply() = L%d, want L5", level)
		}
		if !next.Pending() {
			t.Error("an accepted apply did not mark the overlay pending")
		}
	})
}

// TestACMMOverlayApplyIsRefusedWhilePending is the duplicate-write guard at the
// pane level; the app-level test drives it through a blocking server.
func TestACMMOverlayApplyIsRefusedWhilePending(t *testing.T) {
	o := confirmingOverlay(t, 4, 5)
	o = o.Type(ConfirmPhrase(5))
	o, _, ok := o.Apply()
	if !ok {
		t.Fatal("the first apply was refused")
	}
	for i := 0; i < 3; i++ {
		next, _, ok := o.Apply()
		if ok {
			t.Fatalf("apply %d was accepted while one was already pending", i+2)
		}
		o = next
	}
}

// TestACMMOverlayEditingTheConfirmation covers backspace and cancellation, the
// two edits an operator needs when they mistype a phrase this exacting.
func TestACMMOverlayEditingTheConfirmation(t *testing.T) {
	t.Run("backspace", func(t *testing.T) {
		o := confirmingOverlay(t, 4, 5)
		o = o.Type("APPLY L55")
		o = o.Backspace()
		if got := o.Typed(); got != "APPLY L5" {
			t.Fatalf("Typed() = %q after backspace, want %q", got, "APPLY L5")
		}
		if _, _, ok := o.Apply(); !ok {
			t.Error("the corrected phrase did not apply")
		}
	})

	t.Run("backspace on an empty field", func(t *testing.T) {
		o := confirmingOverlay(t, 4, 5)
		if got := o.Backspace().Typed(); got != "" {
			t.Errorf("Typed() = %q, want empty", got)
		}
	})

	t.Run("backspace removes a rune, not a byte", func(t *testing.T) {
		o := confirmingOverlay(t, 4, 5)
		o = o.Type("APPLY L5é")
		if got := o.Backspace().Typed(); got != "APPLY L5" {
			t.Errorf("Typed() = %q, want the multi-byte character removed whole", got)
		}
	})

	t.Run("cancel clears the phrase and the target", func(t *testing.T) {
		o := confirmingOverlay(t, 4, 5)
		o = o.Type("APPLY L5").CancelConfirm()
		if o.Confirming() {
			t.Error("cancel left the confirmation open")
		}
		if o.Typed() != "" {
			t.Errorf("cancel left %q in the field; reopening would inherit it", o.Typed())
		}
		if _, _, ok := o.Apply(); ok {
			t.Error("Apply() succeeded after the confirmation was cancelled")
		}
	})

	t.Run("reopening starts from an empty field", func(t *testing.T) {
		o := confirmingOverlay(t, 4, 5).Type("APPLY L5").CancelConfirm()
		o, ok := o.BeginConfirm()
		if !ok {
			t.Fatal("the confirmation did not reopen")
		}
		if o.Typed() != "" {
			t.Errorf("the reopened field carries %q from the cancelled attempt", o.Typed())
		}
	})
}

// TestACMMOverlayCursorIsFrozenWhileConfirming. The phrase names the level the
// cursor was on; letting j/k slide the cursor underneath a half-typed
// confirmation is the exact substitution the typed confirmation exists to make
// impossible.
func TestACMMOverlayCursorIsFrozenWhileConfirming(t *testing.T) {
	o := confirmingOverlay(t, 4, 5)
	before, _ := o.SelectedPack()
	o = o.Move(-1).Move(-1)
	after, _ := o.SelectedPack()
	if before.Level != after.Level {
		t.Errorf("the cursor moved from L%d to L%d during a confirmation", before.Level, after.Level)
	}
	o = o.Type(ConfirmPhrase(5))
	if _, level, ok := o.Apply(); !ok || level != 5 {
		t.Errorf("Apply() = (L%d, %v), want (L5, true) — the target was retargeted", level, ok)
	}
}

// TestACMMOverlayReceiptRendersEveryCategory is the acceptance criterion that
// the reconciliation is not summarized away. Every list the server reported is
// on screen, named.
func TestACMMOverlayReceiptRendersEveryCategory(t *testing.T) {
	o := confirmingOverlay(t, 4, 5).Type(ConfirmPhrase(5))
	o, _, _ = o.Apply()
	o = o.SetResult(client.ACMMLevelResult{
		OK:          true,
		Level:       5,
		PackAgents:  []string{"scanner", "reviewer", "feature"},
		PackUpdated: []string{"reviewer"},
		Paused:      []string{"outreach"},
		Resumed:     []string{"feature"},
		GovernorChanges: &client.GovernorChanges{
			EvalIntervalS: &client.GovernorIntervalChange{From: 300, To: 120},
			Cadences: []client.GovernorCadenceChange{
				{Mode: "autonomous", Agent: "scanner", From: "6h", To: "1h"},
			},
		},
	})
	if !o.Done() {
		t.Fatal("a successful apply did not produce a receipt")
	}
	if o.Pending() {
		t.Error("a successful apply left the overlay pending")
	}
	view := flat(o.View(acmmWidth))
	for _, want := range []string{
		"Level is now L5",
		"scanner", "reviewer", "feature", // roster
		"outreach",   // paused
		"300", "120", // evaluation interval
		"autonomous", "6h", "1h", // cadence
	} {
		if !strings.Contains(view, want) {
			t.Errorf("receipt omits %q:\n%s", want, view)
		}
	}
	// A second enter landing on a receipt must not re-apply what it describes.
	if _, _, ok := o.Apply(); ok {
		t.Error("Apply() succeeded on a receipt; enter would re-apply the level")
	}
}

// TestACMMOverlayEmptyReceiptCategoriesSayNone. The criterion is explicit:
// empty lists must not disappear, because a missing category reads as "not
// reported" rather than "nothing in it" — and "which agents did this pause?" is
// exactly what an operator asks after a fleet-wide change.
func TestACMMOverlayEmptyReceiptCategoriesSayNone(t *testing.T) {
	o := confirmingOverlay(t, 4, 5).Type(ConfirmPhrase(5))
	o, _, _ = o.Apply()
	// Every list empty and no governor changes at all: the minimal successful
	// receipt, which is a real response (client.ACMMLevelResult documents
	// GovernorChanges as absent when nothing moved).
	o = o.SetResult(client.ACMMLevelResult{OK: true, Level: 5})

	view := flat(o.View(acmmWidth))
	for _, label := range []string{"Roster", "Updated by the pack", "Paused", "Resumed", "Governor changes"} {
		if !strings.Contains(view, label) {
			t.Errorf("receipt dropped the %q category entirely:\n%s", label, view)
		}
	}
	// Five categories, five explicit empties.
	if got := strings.Count(view, acmmEmptyList); got != 5 {
		t.Errorf("receipt says %q %d times, want 5 — one per empty category:\n%s", acmmEmptyList, got, view)
	}
}

// TestACMMOverlayApplyErrors is the three-way distinction the acceptance
// criteria demand. The 500 is the one that matters most and is asserted
// separately below; this pins that the other two do NOT carry its warning.
func TestACMMOverlayApplyErrors(t *testing.T) {
	t.Run("forbidden", func(t *testing.T) {
		o := confirmingOverlay(t, 4, 5).Type(ConfirmPhrase(5))
		o, _, _ = o.Apply()
		o = o.SetApplyError(apiError(http.StatusForbidden))
		view := flat(o.View(acmmWidth))
		if !strings.Contains(view, "owner access required") {
			t.Errorf("403 does not name the access required:\n%s", view)
		}
		if o.PartiallyReconciled() {
			t.Error("a 403 was flagged as potentially partial; it is a clean refusal")
		}
		if strings.Contains(view, "PARTIALLY RECONCILED") {
			t.Errorf("a 403 warned about partial reconciliation:\n%s", view)
		}
	})

	t.Run("ordinary failure preserves the server's error", func(t *testing.T) {
		o := confirmingOverlay(t, 4, 5).Type(ConfirmPhrase(5))
		o, _, _ = o.Apply()
		o = o.SetApplyError(errors.New("connection refused"))
		view := flat(o.View(acmmWidth))
		if !strings.Contains(view, "connection refused") {
			t.Errorf("the server's error was not preserved:\n%s", view)
		}
		if o.PartiallyReconciled() {
			t.Error("a transport failure was flagged as potentially partial")
		}
	})

	t.Run("a 400 is a clean refusal", func(t *testing.T) {
		// The server validates the level range and answers 400. Nothing was
		// written, so this must not carry the partial warning either.
		o := confirmingOverlay(t, 4, 5).Type(ConfirmPhrase(5))
		o, _, _ = o.Apply()
		o = o.SetApplyError(apiError(http.StatusBadRequest))
		if o.PartiallyReconciled() {
			t.Error("a 400 was flagged as potentially partial")
		}
	})

	t.Run("a failure clears pending so it can be retried", func(t *testing.T) {
		o := confirmingOverlay(t, 4, 5).Type(ConfirmPhrase(5))
		o, _, _ = o.Apply()
		o = o.SetApplyError(errors.New("boom"))
		if o.Pending() {
			t.Fatal("a failed apply left the overlay pending; it could never be retried")
		}
		if !o.Confirming() {
			t.Fatal("a failed apply left the confirmation state, so the phrase is gone")
		}
		if _, level, ok := o.Apply(); !ok || level != 5 {
			t.Errorf("retry = (L%d, %v), want (L5, true) — the typed phrase did not survive", level, ok)
		}
	})
}

// TestACMMOverlay500WarnsThatTheHiveMayBePartiallyReconciled is the subtlest
// requirement in the task, and it is asserted on its own because getting it
// wrong is invisible.
//
// PUT /api/packs/level answers 500 when the level was PERSISTED but the roster
// could not be reconciled to it (client.ApplyACMM documents this precisely).
// Every other failure means "nothing happened"; this one means "something
// happened and we do not know how much of it". Rendering it with the ordinary
// failure wording would tell the operator the exact opposite of the truth and
// send them away from a hive that is now in drift.
func TestACMMOverlay500WarnsThatTheHiveMayBePartiallyReconciled(t *testing.T) {
	for _, code := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			o := confirmingOverlay(t, 4, 5).Type(ConfirmPhrase(5))
			o, _, _ = o.Apply()
			o = o.SetApplyError(apiError(code))

			if !o.PartiallyReconciled() {
				t.Fatalf("a %d was not flagged as potentially partial", code)
			}
			view := flat(o.View(acmmWidth))
			if !strings.Contains(view, "PARTIALLY RECONCILED") {
				t.Errorf("the %d does not warn that the hive may be partially reconciled:\n%s", code, view)
			}
			if !strings.Contains(view, "may already be set") {
				t.Errorf("the %d does not say the level may already have been persisted:\n%s", code, view)
			}
			// The failure must not read as a no-op. These are the phrasings a
			// well-meaning refactor would reach for.
			for _, forbidden := range []string{"nothing changed", "no changes were made", "was not applied"} {
				if strings.Contains(strings.ToLower(view), forbidden) {
					t.Errorf("the %d claims %q, which may be false:\n%s", code, forbidden, view)
				}
			}
		})
	}
}

// TestACMMOverlayFooterMatchesTheState. The footer is the only place the
// available keys are named, so a state whose footer lies is a state an operator
// cannot get out of.
func TestACMMOverlayFooterMatchesTheState(t *testing.T) {
	cases := []struct {
		name    string
		overlay func() ACMMOverlay
		want    []string
	}{
		{"loading", NewACMMOverlay, []string{"esc"}},
		{"list", func() ACMMOverlay { return loadedOverlay(4) }, []string{"j/k", "enter", "esc"}},
		{"confirming", func() ACMMOverlay { return confirmingOverlay(t, 4, 5) }, []string{"backspace", "enter", "esc"}},
		{"pending", func() ACMMOverlay {
			o := confirmingOverlay(t, 4, 5).Type(ConfirmPhrase(5))
			o, _, _ = o.Apply()
			return o
		}, []string{"working"}},
		{"done", func() ACMMOverlay {
			o := confirmingOverlay(t, 4, 5).Type(ConfirmPhrase(5))
			o, _, _ = o.Apply()
			return o.SetResult(client.ACMMLevelResult{OK: true, Level: 5})
		}, []string{"enter", "esc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := flat(tc.overlay().View(acmmWidth))
			for _, want := range tc.want {
				if !strings.Contains(view, want) {
					t.Errorf("the %s footer does not offer %q:\n%s", tc.name, want, view)
				}
			}
		})
	}

	t.Run("pending offers no cancel", func(t *testing.T) {
		// Deliberate: the request is already with the server and closing the
		// overlay would neither un-apply it nor show the result.
		o := confirmingOverlay(t, 4, 5).Type(ConfirmPhrase(5))
		o, _, _ = o.Apply()
		if strings.Contains(flat(o.View(acmmWidth)), "esc") {
			t.Error("the pending footer offers esc, which does not cancel the write")
		}
	})
}

// TestACMMOverlayDoesNotHardcodeLevels: the server owns the pack list and its
// order. A hive that gained an L7, or returned its packs in a different order,
// must render without this file changing.
func TestACMMOverlayDoesNotHardcodeLevels(t *testing.T) {
	status := client.ACMMStatus{Level: 7, Packs: []client.Pack{
		{Level: 7, Name: "Experimental", AgentCount: 20, Current: true},
		{Level: 1, Name: "Manual", AgentCount: 1},
	}}
	o := NewACMMOverlay().SetStatus(status)
	pack, ok := o.SelectedPack()
	if !ok || pack.Level != 7 {
		t.Fatalf("cursor opened on L%d (ok=%v), want the current L7 wherever it sits in the list", pack.Level, ok)
	}
	view := flat(o.View(acmmWidth))
	for _, want := range []string{"L7", "Experimental", "L1", "Manual"} {
		if !strings.Contains(view, want) {
			t.Errorf("view omits %q from a non-canonical pack list:\n%s", want, view)
		}
	}
	// The second row is L1, and confirming it must ask for APPLY L1 — the
	// LEVEL, not the index.
	o = o.Move(1)
	o, ok = o.BeginConfirm()
	if !ok {
		t.Fatal("BeginConfirm() refused the non-current L1")
	}
	if got := flat(o.View(acmmWidth)); !strings.Contains(got, ConfirmPhrase(1)) {
		t.Errorf("the confirmation asks for the wrong phrase; want %q in:\n%s", ConfirmPhrase(1), got)
	}
}

// TestACMMOverlayTypeIsInertOutsideTheConfirmation. Rune input is text ONLY
// while confirming; a stray letter pressed over the list must not accumulate
// into a phrase that later applies something.
func TestACMMOverlayTypeIsInertOutsideTheConfirmation(t *testing.T) {
	// Cursor on L5 — the next level down from the L4 in force — with a full
	// phrase typed at the LIST, where letters are navigation and not text.
	o := loadedOverlay(4).Move(1).Type("APPLY L5")
	if o.Typed() != "" {
		t.Errorf("Typed() = %q outside a confirmation", o.Typed())
	}
	o, ok := o.BeginConfirm()
	if !ok {
		t.Fatal("BeginConfirm() refused")
	}
	if _, _, applied := o.Apply(); applied {
		t.Fatal("text typed before the confirmation opened was carried into it and applied")
	}
}
