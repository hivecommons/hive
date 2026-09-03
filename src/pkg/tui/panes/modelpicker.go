package panes

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/theme"
)

// ModelPicker is the `m` overlay's whole state: which agent is being changed,
// what the catalogue call returned, where the cursor is, and whether a set
// request is in flight.
//
// It is a VALUE type with value receivers like every other pane, but unlike
// the four grid panes it is not a Pane: it is never a cell, it is an overlay
// the app raises over the grid. It therefore takes its keys from the app's
// modal branch (app.go's updateModelPicker) rather than from the pane routing
// seam, which is what makes "every key is consumed while open" a property of
// one switch statement instead of a rule four panes have to remember.
//
// The catalogue this renders is QUALIFIED data (see client.ModelList). The
// picker's job is to show the qualification, not to launder it: a fallback
// list is a set of unverified guesses, a partial list is a floor, and neither
// licenses the operator — or this code — to conclude that a model missing from
// the list is unsupported (#4438).
type ModelPicker struct {
	// agent is the canonical config-key name the set request is addressed to,
	// not the display name. It is what /api/model/{agent}/{model} expects.
	agent string
	// display is the label shown to the operator, which may be the display
	// name. Kept separate so the request can never accidentally use it.
	display string
	// backend is the agent's configured backend, the required path parameter
	// of GET /api/inference/models/{backend}.
	backend string
	// current is the agent's configured model at the moment the overlay
	// opened, used for preselection and for the "unchanged" hint.
	current string

	// loading is true between opening and the catalogue call returning. It is
	// distinct from "loaded an empty list": an operator must be able to tell a
	// request still in flight from a backend that reported nothing.
	loading bool
	// loaded is true once a catalogue call has returned successfully, whether
	// or not it carried any models.
	loaded bool
	// list is the last successful catalogue, including its qualification flags.
	list client.ModelList
	// listErr describes a catalogue call that failed in a way the operator has
	// to be told about. A 404 is NOT one of those: see SetCatalogueError.
	listErr string
	// unavailable is the 404 case — a backend with no configured discovery
	// endpoint. That is an ordinary state of a working hive, so it renders as
	// "no catalogue" rather than as an error with retry guidance.
	unavailable bool

	// selected indexes list.Models.
	selected int

	// pending is true from the moment enter is accepted until the set request
	// answers. It is the whole reason a second enter cannot restart the agent
	// a second time, so the applying branch checks it before doing anything.
	pending bool
	// pendingModel is the id the in-flight request is setting, so the overlay
	// can name it while working and so a late response for a superseded target
	// can be recognised.
	pendingModel string
	// applyErr is a failed set request rendered as UI. The overlay stays open
	// with retry/cancel guidance rather than the failure ending the program.
	applyErr string
}

// NewModelPicker returns the overlay in its loading state for one agent.
//
// It is constructed already loading because the app opens it and issues the
// catalogue call in the same Update: there is no frame in which the overlay is
// open but nothing has been asked for, so there is no state for one.
func NewModelPicker(agent, display, backend, current string) ModelPicker {
	if display == "" {
		display = agent
	}
	return ModelPicker{
		agent:   agent,
		display: display,
		backend: backend,
		current: current,
		loading: true,
	}
}

// Agent returns the canonical agent name the overlay is acting on.
func (p ModelPicker) Agent() string { return p.agent }

// Backend returns the backend whose catalogue the overlay requested.
func (p ModelPicker) Backend() string { return p.backend }

// Pending reports whether a set request is in flight. The app checks this
// before accepting enter, which is what bounds the overlay to exactly one
// session-restarting request at a time.
func (p ModelPicker) Pending() bool { return p.pending }

// Loading reports whether the catalogue call is still outstanding.
func (p ModelPicker) Loading() bool { return p.loading }

// Selected returns the model id under the cursor. ok is false whenever there
// is nothing selectable — still loading, a failed or unavailable catalogue, or
// a successful but empty one — so the app makes enter a no-op rather than
// sending an empty model id the client would reject anyway.
func (p ModelPicker) Selected() (string, bool) {
	if p.loading || !p.loaded {
		return "", false
	}
	if p.selected < 0 || p.selected >= len(p.list.Models) {
		return "", false
	}
	return string(p.list.Models[p.selected]), true
}

// SetCatalogue records a successful catalogue response and preselects the
// agent's current model when the list contains it.
//
// Preselection is a convenience, not an assertion: when the current model is
// absent the cursor starts at the top and the overlay says the current model
// is not listed, because on a fallback or partial list that absence is not
// evidence the backend lacks it.
func (p ModelPicker) SetCatalogue(list client.ModelList) ModelPicker {
	p.loading = false
	p.loaded = true
	p.unavailable = false
	p.listErr = ""
	p.list = list
	p.selected = 0
	for i, m := range list.Models {
		if string(m) == p.current && p.current != "" {
			p.selected = i
			break
		}
	}
	return p
}

// SetCatalogueError records a failed catalogue call.
//
// A 404 is deliberately not an error here. GET /api/inference/models/{backend}
// answers 404 for a backend with no configured discovery endpoint — watsonx or
// litellm before a gateway is set up — which is an ordinary configuration, not
// a broken hive. Rendering it with retry guidance would send an operator
// looking for a fault that does not exist, so it becomes the
// unavailable-catalogue state instead.
func (p ModelPicker) SetCatalogueError(err error) ModelPicker {
	p.loading = false
	p.loaded = false
	p.list = client.ModelList{}
	p.selected = 0
	if isNotFound(err) {
		p.unavailable = true
		p.listErr = ""
		return p
	}
	p.unavailable = false
	if client.IsForbidden(err) {
		p.listErr = "Catalogue unavailable: owner access required"
	} else {
		p.listErr = fmt.Sprintf("Catalogue unavailable: %v", err)
	}
	return p
}

// isNotFound reports whether err is an APIError carrying 404.
//
// Written here rather than added to the client package because T17 is a UI
// task and the issue puts client changes out of scope; client.IsForbidden is
// the precedent for the shape, and errors.As over the exported APIError is the
// documented way to ask.
func isNotFound(err error) bool {
	return apiStatus(err) == http.StatusNotFound
}

// apiStatus returns the HTTP status err carries as an APIError, or 0 when it is
// not one — a transport failure, a decode error, a cancelled context.
//
// T19 needed the same errors.As dance to recognise a 5xx (see acmm.go's
// isServerError), so the extraction happened then rather than a second copy
// being written. Zero is a safe "not an HTTP answer" for both callers: neither
// 404 nor >=500 matches it, so an unreachable dashboard is never mistaken for a
// server that replied.
func apiStatus(err error) int {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// Apply marks a set request as started for the model under the cursor.
//
// It refuses when one is already pending, which is the guard the acceptance
// criteria names: applying restarts the agent's session, so a second enter
// arriving before the first answers must not restart it twice. ok reports
// whether the caller should actually issue the request.
func (p ModelPicker) Apply() (ModelPicker, string, bool) {
	if p.pending {
		return p, "", false
	}
	model, ok := p.Selected()
	if !ok {
		return p, "", false
	}
	p.pending = true
	p.pendingModel = model
	p.applyErr = ""
	return p, model, true
}

// SetApplyError records a failed set request and clears the pending state so
// the operator can retry or cancel.
func (p ModelPicker) SetApplyError(err error) ModelPicker {
	p.pending = false
	if client.IsForbidden(err) {
		p.applyErr = "Model change failed: owner access required"
	} else {
		p.applyErr = fmt.Sprintf("Model change failed: %v", err)
	}
	return p
}

// Move steps the cursor by delta and clamps at the list's edges, so holding a
// direction key parks at an end rather than wrapping past it.
func (p ModelPicker) Move(delta int) ModelPicker {
	if len(p.list.Models) == 0 {
		p.selected = 0
		return p
	}
	next := p.selected + delta
	if next < 0 {
		next = 0
	}
	if next >= len(p.list.Models) {
		next = len(p.list.Models) - 1
	}
	p.selected = next
	return p
}

var (
	modelPickerBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.ThickBorder()).
				BorderForeground(theme.BorderFocus).
				Padding(0, 1)
	modelPickerTitleStyle  = lipgloss.NewStyle().Bold(true)
	modelPickerErrorStyle  = lipgloss.NewStyle().Bold(true)
	modelPickerNoteStyle   = lipgloss.NewStyle().Faint(true)
	modelPickerCursorStyle = lipgloss.NewStyle().Bold(true)
)

// modelPickerRestartWarning is the one sentence the overlay may not omit.
// POST /api/model/{agent}/{model} restarts the agent's session, which can
// interrupt work in progress; an operator choosing from a list must be told
// that before they press enter, not after.
const modelPickerRestartWarning = "Applying restarts the agent's session."

// The qualification labels. Each names what the flag actually means about the
// list rather than grading it, because "unverified" and "incomplete" are
// instructions to the reader about what a model's ABSENCE proves — namely
// nothing.
const (
	modelPickerFallbackNote = "Unverified: discovery found nothing, so these are common aliases. " +
		"A model missing here may still work."
	modelPickerPartialNote = "Incomplete: only some endpoints answered. " +
		"A model missing here may still be served by one that did not."
	modelPickerEntitledNote = "Narrowed to the models this key is entitled to use"
	modelPickerEmptyNote    = "no models reported"
	modelPickerNoCatalogue  = "no model catalogue for this backend"
)

// modelPickerVisibleRows bounds the list so a backend advertising hundreds of
// ids cannot grow the overlay past the terminal. The window scrolls with the
// cursor; the count line above it says how much is off screen.
const modelPickerVisibleRows = 8

// View renders the overlay's box, sized to its own content. Placing it over
// the frame is the app's job, matching the split pane.go draws between a
// pane's content and the chrome around it.
func (p ModelPicker) View(width int) string {
	var body strings.Builder
	body.WriteString(modelPickerTitleStyle.Render("Model for " + p.display))
	body.WriteString("\n")
	body.WriteString(modelPickerNoteStyle.Render(p.subtitle()))
	body.WriteString("\n\n")
	body.WriteString(p.bodyText())
	body.WriteString("\n\n")
	body.WriteString(modelPickerNoteStyle.Render(modelPickerRestartWarning))
	body.WriteString("\n")
	body.WriteString(modelPickerNoteStyle.Render(p.footer()))

	// Matches confirmView's sizing so the two modals sit in the same place at
	// the same widths; the extra columns give long model ids room to breathe.
	contentWidth := min(60, max(1, width-6))
	return modelPickerBoxStyle.Width(contentWidth).Render(body.String())
}

func (p ModelPicker) subtitle() string {
	backend := p.backend
	if backend == "" {
		backend = "—"
	}
	current := p.current
	if current == "" {
		current = "—"
	}
	return fmt.Sprintf("backend %s   current %s", backend, current)
}

func (p ModelPicker) bodyText() string {
	switch {
	case p.applyErr != "":
		// The failure is what the operator must read first, but the list stays
		// under it: retry means "press enter on the same row", so hiding the
		// row would make the guidance meaningless.
		return modelPickerErrorStyle.Render(p.applyErr) + "\n\n" + p.listBody()
	case p.pending:
		return fmt.Sprintf("Applying %s… restarting the agent's session.", p.pendingModel)
	default:
		return p.listBody()
	}
}

func (p ModelPicker) listBody() string {
	switch {
	case p.loading:
		return "Loading models…"
	case p.unavailable:
		// Not an error: the backend simply has no discovery endpoint
		// configured, so there is nothing to enumerate.
		return modelPickerNoCatalogue + "\n" +
			"This backend has no discovery endpoint configured."
	case p.listErr != "":
		return modelPickerErrorStyle.Render(p.listErr)
	case !p.loaded:
		return "Loading models…"
	case len(p.list.Models) == 0:
		// An empty SUCCESSFUL catalogue. Saying so explicitly is the
		// difference between "the backend told us it has nothing" and a box
		// that merely looks broken.
		return modelPickerEmptyNote
	}

	var out strings.Builder
	for _, note := range p.qualifications() {
		out.WriteString(modelPickerNoteStyle.Render(note) + "\n")
	}
	if len(p.qualifications()) > 0 {
		out.WriteString("\n")
	}

	start, end := p.window()
	for i := start; i < end; i++ {
		id := string(p.list.Models[i])
		cursor := "  "
		if i == p.selected {
			cursor = modelPickerCursorStyle.Render("▸ ")
		}
		mark := ""
		if id == p.current && p.current != "" {
			mark = "  (current)"
		}
		out.WriteString(cursor + id + mark + "\n")
	}
	out.WriteString(modelPickerNoteStyle.Render(fmt.Sprintf("%d of %d", p.selected+1, len(p.list.Models))))
	if p.current != "" && !p.currentListed() {
		// Absence is not evidence, so this states the fact and not a verdict.
		out.WriteString("\n" + modelPickerNoteStyle.Render(
			"Current model "+p.current+" is not in this list."))
	}
	return out.String()
}

// qualifications returns the flag notes that apply to the loaded catalogue, in
// the order they most change how the list should be read.
func (p ModelPicker) qualifications() []string {
	var notes []string
	if p.list.Fallback {
		notes = append(notes, modelPickerFallbackNote)
	}
	if p.list.Partial {
		notes = append(notes, modelPickerPartialNote)
	}
	if p.list.Entitled {
		note := modelPickerEntitledNote
		if p.list.EntitledSource != "" {
			note += " (" + p.list.EntitledSource + ")"
		}
		notes = append(notes, note)
	}
	return notes
}

func (p ModelPicker) currentListed() bool {
	for _, m := range p.list.Models {
		if string(m) == p.current {
			return true
		}
	}
	return false
}

// window returns the slice of model indices to draw, scrolled to keep the
// cursor visible.
func (p ModelPicker) window() (int, int) {
	n := len(p.list.Models)
	if n <= modelPickerVisibleRows {
		return 0, n
	}
	start := p.selected - modelPickerVisibleRows/2
	if start < 0 {
		start = 0
	}
	if start+modelPickerVisibleRows > n {
		start = n - modelPickerVisibleRows
	}
	return start, start + modelPickerVisibleRows
}

func (p ModelPicker) footer() string {
	switch {
	case p.pending:
		// No cancel offered while pending on purpose: the request is already
		// with the server and closing the overlay would not un-restart the
		// session.
		return "working…"
	case p.applyErr != "":
		return "enter retry  esc cancel"
	case p.loading:
		return "esc cancel"
	case p.loaded && len(p.list.Models) > 0:
		return "j/k move  enter apply  esc cancel"
	default:
		return "esc close"
	}
}
