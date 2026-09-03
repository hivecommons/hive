package panes

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/tui/client"
)

const pickerWidth = 70

// flat collapses the rendered box to a single space-separated line.
//
// The overlay WRAPS long prose to its width, so a phrase an assertion cares
// about ("connection refused", "entitled to use") legitimately spans two
// rows with the border glyphs and padding in between. Asserting on the raw
// frame would make these tests fail whenever the box width changed, which
// says nothing about whether the operator was told the right thing. The
// golden file is what pins the exact layout; these assert the CONTENT.
func flat(view string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("┃", " ", "┏", " ", "┓", " ", "┗", " ", "┛", " ", "━", " ").Replace(view)), " ")
}

func newPicker() ModelPicker {
	return NewModelPicker("scanner", "Scanner", "claude", "claude-opus-4-5")
}

func catalogue(models ...string) client.ModelList {
	list := client.ModelList{Backend: "claude"}
	for _, m := range models {
		list.Models = append(list.Models, client.ModelOption(m))
	}
	return list
}

// TestModelPickerLoadingState: the overlay opens already asking, and says so.
// A blank box while a request is in flight is indistinguishable from a backend
// that answered with nothing, which is exactly the confusion the empty-state
// wording exists to prevent.
func TestModelPickerLoadingState(t *testing.T) {
	p := newPicker()
	if !p.Loading() {
		t.Fatal("a freshly opened picker is not loading; the catalogue call is issued with it")
	}
	if _, ok := p.Selected(); ok {
		t.Error("a loading picker offers a selection, so enter would apply a model nobody chose")
	}
	view := p.View(pickerWidth)
	if !strings.Contains(view, "Loading models") {
		t.Errorf("loading view does not say it is loading:\n%s", view)
	}
	if !strings.Contains(view, modelPickerRestartWarning) {
		t.Errorf("loading view omits the restart warning:\n%s", view)
	}
}

// TestModelPickerEmptySuccessfulCatalogue is the state the acceptance criteria
// singles out: a call that SUCCEEDED and returned no models must say "no
// models reported" rather than render an empty box that reads as broken.
func TestModelPickerEmptySuccessfulCatalogue(t *testing.T) {
	p := newPicker().SetCatalogue(client.ModelList{Backend: "claude"})
	if p.Loading() {
		t.Error("picker still reports loading after a successful response")
	}
	view := p.View(pickerWidth)
	if !strings.Contains(view, modelPickerEmptyNote) {
		t.Errorf("empty successful catalogue does not say so:\n%s", view)
	}
	if strings.Contains(view, "Loading models") {
		t.Errorf("empty successful catalogue still claims to be loading:\n%s", view)
	}
	if _, ok := p.Selected(); ok {
		t.Error("an empty catalogue offers a selection")
	}
}

// TestModelPickerFallbackIsLabelledUnverified: client.ModelList.Fallback means
// discovery found nothing and the server substituted static aliases. Rendering
// those as a discovered catalogue would offer models the gateway may not serve
// — and would imply that a model's absence proves something.
func TestModelPickerFallbackIsLabelledUnverified(t *testing.T) {
	list := catalogue("claude-opus-4-5", "claude-sonnet-4-5")
	list.Fallback = true
	view := newPicker().SetCatalogue(list).View(pickerWidth)

	if !strings.Contains(view, "Unverified") {
		t.Errorf("fallback catalogue is not labelled unverified:\n%s", view)
	}
	if !strings.Contains(view, "may still work") {
		t.Errorf("fallback catalogue does not say absence is not proof:\n%s", view)
	}
	if list.Authoritative() {
		t.Fatal("client says a fallback list is authoritative; this test's premise is wrong")
	}
}

// TestModelPickerPartialIsLabelledIncomplete: Partial means only some
// endpoints answered, so every id present was really discovered but a model's
// ABSENCE proves nothing (#4438).
func TestModelPickerPartialIsLabelledIncomplete(t *testing.T) {
	list := catalogue("gpt-5", "gpt-5-mini")
	list.Partial = true
	view := newPicker().SetCatalogue(list).View(pickerWidth)

	if !strings.Contains(view, "Incomplete") {
		t.Errorf("partial catalogue is not labelled incomplete:\n%s", view)
	}
	if !strings.Contains(view, "may still be served") {
		t.Errorf("partial catalogue does not say absence is not proof:\n%s", view)
	}
}

// TestModelPickerEntitledIsLabelled: an entitlement-narrowed list is neither
// wrong nor complete — it is what THIS key may use, and the source of that
// knowledge is worth naming.
func TestModelPickerEntitledIsLabelled(t *testing.T) {
	list := catalogue("gpt-5")
	list.Entitled = true
	list.EntitledSource = "key-info"
	view := newPicker().SetCatalogue(list).View(pickerWidth)

	if !strings.Contains(flat(view), "entitled to use") {
		t.Errorf("entitlement-narrowed catalogue is not labelled:\n%s", view)
	}
	// The source is asserted with the wrap collapsed AND the hyphen break
	// healed: the box is a fixed 60 columns, so "key-info" lands across a line
	// boundary. What matters is that the source is named at all — where it
	// wraps is the golden file's business.
	if !strings.Contains(strings.ReplaceAll(flat(view), "- ", "-"), "key-info") {
		t.Errorf("entitlement source is not named:\n%s", view)
	}
}

// TestModelPickerSuccessfulCatalogueCarriesNoFalseQualification: the flags are
// only shown when set. A clean discovery labelled "unverified" would be as
// wrong as a fallback list presented as discovered.
func TestModelPickerSuccessfulCatalogueCarriesNoFalseQualification(t *testing.T) {
	view := newPicker().SetCatalogue(catalogue("claude-opus-4-5", "claude-sonnet-4-5")).View(pickerWidth)
	for _, unwanted := range []string{"Unverified", "Incomplete", "entitled", modelPickerEmptyNote} {
		if strings.Contains(view, unwanted) {
			t.Errorf("a full discovery is labelled %q:\n%s", unwanted, view)
		}
	}
}

// TestModelPickerPreselectsCurrentModel. Preselection is the difference
// between "change this" and "pick from scratch"; getting it wrong is how an
// operator restarts an agent onto a neighbouring model by pressing enter.
func TestModelPickerPreselectsCurrentModel(t *testing.T) {
	p := NewModelPicker("scanner", "Scanner", "claude", "claude-sonnet-4-5").
		SetCatalogue(catalogue("claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5"))
	got, ok := p.Selected()
	if !ok || got != "claude-sonnet-4-5" {
		t.Fatalf("Selected() = (%q, %v), want the current model preselected", got, ok)
	}
	if !strings.Contains(p.View(pickerWidth), "(current)") {
		t.Error("the preselected row is not marked as the current model")
	}
}

// TestModelPickerAbsentCurrentModelIsStatedNotJudged: when the current model
// is missing from the list, the overlay reports the fact and starts at the top
// — it does not conclude the model is unsupported, because on a fallback or
// partial list that conclusion is unfounded.
func TestModelPickerAbsentCurrentModelIsStatedNotJudged(t *testing.T) {
	p := NewModelPicker("scanner", "Scanner", "claude", "claude-opus-4-6").
		SetCatalogue(catalogue("claude-opus-4-5", "claude-sonnet-4-5"))
	got, ok := p.Selected()
	if !ok || got != "claude-opus-4-5" {
		t.Fatalf("Selected() = (%q, %v), want the first row when the current model is absent", got, ok)
	}
	view := p.View(pickerWidth)
	if !strings.Contains(view, "not in this list") {
		t.Errorf("absent current model is not reported:\n%s", view)
	}
	for _, verdict := range []string{"unsupported", "unavailable model", "invalid"} {
		if strings.Contains(strings.ToLower(view), verdict) {
			t.Errorf("absence is rendered as a verdict (%q):\n%s", verdict, view)
		}
	}
}

// TestModelPickerNavigationClamps. Holding a direction key must park at an
// end, not wrap: wrapping past the bottom onto the top is how a fast operator
// applies the wrong model.
func TestModelPickerNavigationClamps(t *testing.T) {
	p := NewModelPicker("scanner", "Scanner", "claude", "").
		SetCatalogue(catalogue("a", "b", "c"))

	if got, _ := p.Selected(); got != "a" {
		t.Fatalf("initial selection = %q, want the first row", got)
	}
	p = p.Move(1)
	if got, _ := p.Selected(); got != "b" {
		t.Errorf("after Move(1) selection = %q, want b", got)
	}
	p = p.Move(1).Move(1).Move(1)
	if got, _ := p.Selected(); got != "c" {
		t.Errorf("moving past the end selection = %q, want it clamped at c", got)
	}
	p = p.Move(-1).Move(-1).Move(-1).Move(-1)
	if got, _ := p.Selected(); got != "a" {
		t.Errorf("moving past the start selection = %q, want it clamped at a", got)
	}
}

// TestModelPickerNavigationOnEmptyCatalogueIsSafe: an empty list has nothing
// to move within, and moving must not produce an index that Selected() then
// reads out of range.
func TestModelPickerNavigationOnEmptyCatalogueIsSafe(t *testing.T) {
	p := newPicker().SetCatalogue(client.ModelList{Backend: "claude"}).Move(1).Move(-1).Move(5)
	if _, ok := p.Selected(); ok {
		t.Error("moving within an empty catalogue produced a selection")
	}
}

// TestModelPickerScrollsWithoutGrowing: a backend advertising many ids must
// not grow the overlay past a window, and the cursor must stay inside it.
func TestModelPickerScrollsWithoutGrowing(t *testing.T) {
	var ids []string
	for i := 0; i < modelPickerVisibleRows*3; i++ {
		ids = append(ids, fmt.Sprintf("model-%02d", i))
	}
	p := NewModelPicker("scanner", "Scanner", "claude", "").SetCatalogue(catalogue(ids...))
	for i := 0; i < len(ids)-1; i++ {
		p = p.Move(1)
	}
	view := p.View(pickerWidth)
	if !strings.Contains(view, ids[len(ids)-1]) {
		t.Errorf("the selected row scrolled out of the visible window:\n%s", view)
	}
	if strings.Contains(view, ids[0]) {
		t.Errorf("the list is not clipped; the first row is still drawn:\n%s", view)
	}
	if !strings.Contains(view, fmt.Sprintf("%d of %d", len(ids), len(ids))) {
		t.Errorf("the clipped list does not report the full count:\n%s", view)
	}
}

// TestModelPickerNotFoundIsNotAnError. GET /api/inference/models/{backend}
// answers 404 for a backend with no configured discovery endpoint. That is an
// ordinary configuration, so rendering it with retry guidance would send an
// operator hunting for a fault that does not exist.
func TestModelPickerNotFoundIsNotAnError(t *testing.T) {
	err := &client.APIError{StatusCode: http.StatusNotFound, Method: http.MethodGet, Path: "/api/inference/models/watsonx"}
	view := newPicker().SetCatalogueError(err).View(pickerWidth)

	if !strings.Contains(view, modelPickerNoCatalogue) {
		t.Errorf("a 404 does not render as an unavailable catalogue:\n%s", view)
	}
	for _, wrong := range []string{"failed", "Not Found", "retry"} {
		if strings.Contains(view, wrong) {
			t.Errorf("a 404 is rendered as an error (%q):\n%s", wrong, view)
		}
	}
}

// TestModelPickerForbiddenCatalogue: 403 is the one failure worth wording
// differently — the request worked, the role does not permit it, and retrying
// will never help.
func TestModelPickerForbiddenCatalogue(t *testing.T) {
	err := &client.APIError{StatusCode: http.StatusForbidden, Method: http.MethodGet, Path: "/api/inference/models/claude"}
	view := newPicker().SetCatalogueError(err).View(pickerWidth)
	if !strings.Contains(view, "owner access required") {
		t.Errorf("a 403 catalogue failure does not name the cause:\n%s", view)
	}
}

// TestModelPickerTransportErrorIsShown: a dial failure has no status to
// classify, so it is reported verbatim rather than silently leaving a loading
// spinner up forever.
func TestModelPickerTransportErrorIsShown(t *testing.T) {
	p := newPicker().SetCatalogueError(errors.New("dial tcp 127.0.0.1:8080: connection refused"))
	if p.Loading() {
		t.Error("a failed catalogue call leaves the overlay loading forever")
	}
	view := p.View(pickerWidth)
	if !strings.Contains(flat(view), "Catalogue unavailable") || !strings.Contains(flat(view), "connection refused") {
		t.Errorf("transport error is not surfaced:\n%s", view)
	}
	if _, ok := p.Selected(); ok {
		t.Error("a failed catalogue offers a selection")
	}
}

// TestModelPickerApplyIsSingleShotWhilePending is the acceptance criterion
// that matters most: applying RESTARTS the agent's session, so a second enter
// arriving before the first response must not restart it again.
func TestModelPickerApplyIsSingleShotWhilePending(t *testing.T) {
	p := newPicker().SetCatalogue(catalogue("claude-opus-4-5", "claude-sonnet-4-5"))

	p, chosen, ok := p.Apply()
	if !ok || chosen != "claude-opus-4-5" {
		t.Fatalf("first Apply() = (%q, %v), want the selected model", chosen, ok)
	}
	if !p.Pending() {
		t.Fatal("Apply() did not enter the pending state, so nothing blocks a second request")
	}
	for i := 0; i < 5; i++ {
		var again bool
		p, _, again = p.Apply()
		if again {
			t.Fatalf("Apply() was accepted again while pending (attempt %d) — the agent would restart twice", i+2)
		}
	}
}

// TestModelPickerPendingViewSaysTheSessionRestarts.
func TestModelPickerPendingViewSaysTheSessionRestarts(t *testing.T) {
	p, _, _ := newPicker().SetCatalogue(catalogue("claude-opus-4-5")).Apply()
	view := p.View(pickerWidth)
	if !strings.Contains(view, "claude-opus-4-5") {
		t.Errorf("the pending view does not name the model being applied:\n%s", view)
	}
	if !strings.Contains(strings.ToLower(view), "restart") {
		t.Errorf("the pending view does not say the session restarts:\n%s", view)
	}
}

// TestModelPickerRestartWarningIsAlwaysShown: it is guidance BEFORE the key
// press, so it must be present in every state an operator can press enter
// from, not only once the request is under way.
func TestModelPickerRestartWarningIsAlwaysShown(t *testing.T) {
	states := map[string]ModelPicker{
		"loading":     newPicker(),
		"empty":       newPicker().SetCatalogue(client.ModelList{}),
		"loaded":      newPicker().SetCatalogue(catalogue("claude-opus-4-5")),
		"unavailable": newPicker().SetCatalogueError(&client.APIError{StatusCode: http.StatusNotFound}),
		"failed":      newPicker().SetCatalogueError(errors.New("boom")),
	}
	for name, p := range states {
		if !strings.Contains(p.View(pickerWidth), modelPickerRestartWarning) {
			t.Errorf("%s state omits the restart warning", name)
		}
	}
}

// TestModelPickerApplyFailureKeepsTheListAndOffersRetry. A validation 400 is
// the backend's authoritative refusal of a model id — including an id that was
// in a fallback list. The overlay reports it and stays open on the same row so
// "retry" means something.
func TestModelPickerApplyFailureKeepsTheListAndOffersRetry(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "400 validation",
			err:  &client.APIError{StatusCode: http.StatusBadRequest, Method: http.MethodPost, Path: "/api/model/scanner/bogus", Body: "unsupported model for backend claude"},
			want: "unsupported model for backend claude",
		},
		{
			name: "403 forbidden",
			err:  &client.APIError{StatusCode: http.StatusForbidden, Method: http.MethodPost, Path: "/api/model/scanner/claude-opus-4-5"},
			want: "owner access required",
		},
		{
			name: "transport error",
			err:  errors.New("dial tcp 127.0.0.1:8080: connection refused"),
			want: "connection refused",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _ := newPicker().SetCatalogue(catalogue("claude-opus-4-5", "claude-sonnet-4-5")).Apply()
			p = p.SetApplyError(tc.err)

			if p.Pending() {
				t.Fatal("a failed apply left the overlay pending, so retry is impossible")
			}
			view := p.View(pickerWidth)
			if !strings.Contains(flat(view), tc.want) {
				t.Errorf("failure view does not carry %q:\n%s", tc.want, view)
			}
			if !strings.Contains(view, "retry") || !strings.Contains(view, "cancel") {
				t.Errorf("failure view does not offer retry/cancel:\n%s", view)
			}
			// The list stays under the error: retry means "press enter on the
			// same row", which is meaningless if the row is gone.
			if !strings.Contains(view, "claude-opus-4-5") {
				t.Errorf("failure view dropped the model list:\n%s", view)
			}
			if _, ok := p.Selected(); !ok {
				t.Error("failure cleared the selection, so retry has no target")
			}
		})
	}
}

// TestModelPickerRetryAfterFailureIsAcceptedOnce: the single-shot guard must
// RELEASE on failure, otherwise "retry" is advertised and does nothing.
func TestModelPickerRetryAfterFailureIsAcceptedOnce(t *testing.T) {
	p, _, _ := newPicker().SetCatalogue(catalogue("claude-opus-4-5")).Apply()
	p = p.SetApplyError(errors.New("boom"))

	p, chosen, ok := p.Apply()
	if !ok || chosen != "claude-opus-4-5" {
		t.Fatalf("retry Apply() = (%q, %v), want the same model accepted once", chosen, ok)
	}
	if _, _, again := p.Apply(); again {
		t.Error("a retried apply is not single-shot")
	}
}

// TestModelPickerAddressesTheConfigKeyNotTheDisplayName: /api/model/{agent}
// takes the config key. A display name leaking into the request would target a
// path that does not exist.
func TestModelPickerAddressesTheConfigKeyNotTheDisplayName(t *testing.T) {
	p := NewModelPicker("scanner", "Fleet Scanner", "claude", "")
	if p.Agent() != "scanner" {
		t.Errorf("Agent() = %q, want the config key", p.Agent())
	}
	if !strings.Contains(p.View(pickerWidth), "Fleet Scanner") {
		t.Error("the overlay does not show the display name to the operator")
	}
}

// TestModelPickerFallsBackToNameWhenDisplayNameIsEmpty. client.Agent marshals
// displayName with omitempty, so an agent can arrive without one.
func TestModelPickerFallsBackToNameWhenDisplayNameIsEmpty(t *testing.T) {
	if !strings.Contains(NewModelPicker("scanner", "", "claude", "").View(pickerWidth), "scanner") {
		t.Error("an agent with no display name is not labelled at all")
	}
}
