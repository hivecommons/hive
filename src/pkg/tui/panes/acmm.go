package panes

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/theme"
)

// ACMMOverlay is the `A` overlay's whole state: the pack list the server
// returned, where the cursor is, whether a typed confirmation is being
// composed, and what an apply came back with.
//
// Like ModelPicker it is a VALUE type with value receivers and it is NOT a
// Pane: it is never a grid cell, it is an overlay the app raises over the
// frame, so it takes its keys from app.go's modal branch (updateACMM) rather
// than from the pane routing seam. That is what makes "every key is consumed
// while open" a property of one switch statement.
//
// WHY THE TYPED CONFIRMATION EXISTS. PUT /api/packs/level is the widest write
// in the dashboard API: the handler clears every per-agent mode override,
// force-applies the pack to reconcile the roster, rewrites the governor's
// cadences and thresholds, and pauses or resumes agents to match the new
// roster (see client.ACMMLevelResult). A y/n modal is calibrated for pausing
// ONE agent; this operation is fleet-wide and is not obviously reversible from
// the same screen, which is why the design doc (§4) specifies typed
// confirmation here and a plain confirm for pause/resume.
type ACMMOverlay struct {
	// loading is true between opening and the packs call returning. It is
	// distinct from "loaded an empty list": an operator must be able to tell a
	// request still in flight from a hive that defines no packs.
	loading bool
	// loaded is true once a packs call has returned successfully, whether or
	// not it carried any packs.
	loaded bool
	// status is the last successful pack list plus the level in force.
	status client.ACMMStatus
	// listErr describes a failed packs call the operator has to be told about.
	listErr string

	// selected indexes status.Packs. The list's ORDER is the server's; this is
	// a position in it, never a level number, because nothing in the contract
	// promises packs arrive sorted by level or that levels are contiguous.
	selected int

	// confirming is true once enter has been pressed on a non-current level and
	// the overlay is collecting the typed phrase.
	confirming bool
	// confirmLevel is the level the typed phrase must name. Held separately
	// from the cursor so moving the cursor cannot silently retarget a
	// confirmation already in progress — except that navigation is refused
	// while confirming, which makes this belt and braces.
	confirmLevel int
	// typed is what the operator has entered so far. It is compared against the
	// exact required phrase; nothing is normalized, because a confirmation that
	// accepts a near miss is not a confirmation.
	typed string

	// pending is true from the moment a valid confirmation is accepted until
	// the apply answers. It is the whole reason a second enter cannot issue a
	// second fleet-wide reconciliation.
	pending bool
	// pendingLevel is the level the in-flight apply is setting.
	pendingLevel int

	// applyErr is a failed apply rendered as UI rather than ending the program.
	applyErr string
	// partial marks the 500 case: the level may already have been persisted
	// before the roster could be reconciled to it, so the overlay must not
	// claim nothing changed. See SetApplyError.
	partial bool

	// result is a successful apply's receipt, held so the operator reads the
	// full reconciliation before the overlay is dismissed.
	result *client.ACMMLevelResult

	// noop carries the explanation for selecting the level already in force.
	noop string
}

// NewACMMOverlay returns the overlay in its loading state.
//
// It is constructed already loading because the app opens it and issues the
// packs call in the same Update: there is no frame in which the overlay is open
// but nothing has been asked for, so there is no state for one.
func NewACMMOverlay() ACMMOverlay {
	return ACMMOverlay{loading: true}
}

// Loading reports whether the packs call is still outstanding.
func (o ACMMOverlay) Loading() bool { return o.loading }

// Pending reports whether an apply is in flight. The app checks this before
// accepting enter and before accepting esc, which is what bounds the overlay to
// exactly one fleet-wide write at a time.
func (o ACMMOverlay) Pending() bool { return o.pending }

// Confirming reports whether the typed-confirmation state is active.
func (o ACMMOverlay) Confirming() bool { return o.confirming }

// Typed returns the phrase entered so far.
func (o ACMMOverlay) Typed() string { return o.typed }

// Done reports whether a successful apply's receipt is on screen. The overlay
// stays up in this state so the reconciliation is read rather than flashing
// past; only esc/enter dismisses it.
func (o ACMMOverlay) Done() bool { return o.result != nil }

// SelectedPack returns the pack under the cursor. ok is false whenever there is
// nothing selectable — still loading, a failed list, or a successful but empty
// one — so the app makes enter a no-op rather than confirming a level nobody
// chose.
func (o ACMMOverlay) SelectedPack() (client.Pack, bool) {
	if o.loading || !o.loaded {
		return client.Pack{}, false
	}
	if o.selected < 0 || o.selected >= len(o.status.Packs) {
		return client.Pack{}, false
	}
	return o.status.Packs[o.selected], true
}

// SetStatus records a successful packs response and preselects the level in
// force so the cursor opens where the hive actually is.
func (o ACMMOverlay) SetStatus(status client.ACMMStatus) ACMMOverlay {
	o.loading = false
	o.loaded = true
	o.listErr = ""
	o.status = status
	o.selected = 0
	for i, p := range status.Packs {
		if p.Current {
			o.selected = i
			break
		}
	}
	return o
}

// SetStatusError records a failed packs call.
//
// Unlike the model picker's catalogue there is no benign 404 here: /api/packs
// is always routed, so a 404 from it is a real fault rather than an ordinary
// configuration, and it stays an error.
func (o ACMMOverlay) SetStatusError(err error) ACMMOverlay {
	o.loading = false
	o.loaded = false
	o.status = client.ACMMStatus{}
	o.selected = 0
	if client.IsForbidden(err) {
		o.listErr = "ACMM packs unavailable: owner access required"
	} else {
		o.listErr = fmt.Sprintf("ACMM packs unavailable: %v", err)
	}
	return o
}

// Move steps the cursor by delta and clamps at the list's edges.
//
// It refuses to move at all while confirming, pending or showing a receipt: the
// cursor is what the confirmation phrase refers to, so letting j/k slide it
// under a half-typed `APPLY L4` would be the exact class of mistake typing the
// phrase exists to prevent.
func (o ACMMOverlay) Move(delta int) ACMMOverlay {
	if o.confirming || o.pending || o.result != nil {
		return o
	}
	o.noop = ""
	if len(o.status.Packs) == 0 {
		o.selected = 0
		return o
	}
	next := o.selected + delta
	if next < 0 {
		next = 0
	}
	if next >= len(o.status.Packs) {
		next = len(o.status.Packs) - 1
	}
	o.selected = next
	return o
}

// BeginConfirm moves the overlay into the typed-confirmation state for the
// selected level.
//
// ok is false — and NOTHING is applied — for every case that must not become a
// write: nothing selected, an apply already pending, a confirmation already
// open, and, most importantly, the level already in force. That last one is an
// explicit no-op with a message rather than a silently ignored keypress,
// because an operator who pressed enter is owed an answer either way.
func (o ACMMOverlay) BeginConfirm() (ACMMOverlay, bool) {
	if o.pending || o.confirming || o.result != nil {
		return o, false
	}
	pack, ok := o.SelectedPack()
	if !ok {
		return o, false
	}
	if pack.Current {
		o.noop = fmt.Sprintf("L%d is already the level in force — nothing to apply.", pack.Level)
		return o, false
	}
	o.confirming = true
	o.confirmLevel = pack.Level
	o.typed = ""
	o.noop = ""
	o.applyErr = ""
	o.partial = false
	return o, true
}

// CancelConfirm leaves the typed-confirmation state without applying anything.
func (o ACMMOverlay) CancelConfirm() ACMMOverlay {
	o.confirming = false
	o.confirmLevel = 0
	o.typed = ""
	return o
}

// Type appends the runes of one key press to the confirmation phrase.
func (o ACMMOverlay) Type(runes string) ACMMOverlay {
	if !o.confirming || o.pending {
		return o
	}
	o.typed += runes
	return o
}

// Backspace removes the last rune of the confirmation phrase.
//
// It deletes a RUNE rather than a byte: the phrase is ASCII today, but a
// mistyped multi-byte character pasted into the field would otherwise be left
// as an invalid fragment the operator cannot see or clear.
func (o ACMMOverlay) Backspace() ACMMOverlay {
	if !o.confirming || o.pending || o.typed == "" {
		return o
	}
	runes := []rune(o.typed)
	o.typed = string(runes[:len(runes)-1])
	return o
}

// ConfirmPhrase is the exact text that must be typed to apply level.
//
// Exported so the app's tests and the overlay itself name the same string, and
// so nothing has to reconstruct the format from prose.
func ConfirmPhrase(level int) string {
	return fmt.Sprintf("APPLY L%d", level)
}

// Apply accepts the typed confirmation and marks an apply as started.
//
// It refuses unless the phrase matches EXACTLY — no trimming, no case folding —
// and refuses while one is already pending, which together are the acceptance
// criteria: wrong or incomplete text cannot apply, and the exact phrase sends
// one PUT however many times enter is pressed before it answers. The returned
// level is what the caller should send.
func (o ACMMOverlay) Apply() (ACMMOverlay, int, bool) {
	if !o.confirming || o.pending {
		return o, 0, false
	}
	if o.typed != ConfirmPhrase(o.confirmLevel) {
		return o, 0, false
	}
	o.pending = true
	o.pendingLevel = o.confirmLevel
	o.applyErr = ""
	o.partial = false
	return o, o.confirmLevel, true
}

// SetResult records a successful apply's receipt and leaves the overlay open on
// it. Clearing the confirmation state here is what stops a second enter, landing
// on the receipt, from re-applying the level it just read out.
func (o ACMMOverlay) SetResult(result client.ACMMLevelResult) ACMMOverlay {
	o.pending = false
	o.confirming = false
	o.typed = ""
	o.applyErr = ""
	o.partial = false
	o.result = &result
	return o
}

// SetApplyError records a failed apply and returns the overlay to the
// confirmation state so it can be retried or cancelled.
//
// THE 500 IS NOT AN ORDINARY FAILURE. The handler answers 500 when the level
// was persisted but the roster could not be reconciled to it, which it reports
// precisely so the drift is visible (see client.ApplyACMM). Presenting that as
// "the change did not happen" would tell the operator the opposite of the
// truth, so it gets its own wording and its own flag — and the app refreshes on
// it, because the hive may well have moved.
func (o ACMMOverlay) SetApplyError(err error) ACMMOverlay {
	o.pending = false
	level := o.pendingLevel
	switch {
	case isServerError(err):
		o.partial = true
		o.applyErr = fmt.Sprintf(
			"Apply L%d failed during reconciliation: %v. "+
				"The level may already be set while the roster is NOT reconciled to it — "+
				"the hive may be PARTIALLY RECONCILED. Re-check the level and the roster before retrying.",
			level, err)
	case client.IsForbidden(err):
		o.partial = false
		o.applyErr = fmt.Sprintf("Apply L%d failed: owner access required", level)
	default:
		o.partial = false
		o.applyErr = fmt.Sprintf("Apply L%d failed: %v", level, err)
	}
	return o
}

// PartiallyReconciled reports whether the last failure was the potentially
// partial one. The app reads it to decide whether a FAILED apply should still
// trigger a refresh.
func (o ACMMOverlay) PartiallyReconciled() bool { return o.partial }

// isServerError reports whether err is an APIError carrying any 5xx.
//
// The whole 5xx class rather than 500 alone: a 502 or 503 from a proxy in front
// of the dashboard is equally "the request reached something and we do not know
// what it did", and erring towards the warning is the safe direction when the
// alternative is telling an operator nothing changed when it might have.
//
// Written here rather than added to the client package for the same reason
// modelpicker.go's isNotFound was: this is a UI task and the issue puts client
// changes out of scope. It sits beside that one so the pair stay together.
func isServerError(err error) bool {
	return apiStatus(err) >= 500
}

var (
	acmmBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder()).
			BorderForeground(theme.BorderFocus).
			Padding(0, 1)
	acmmTitleStyle  = lipgloss.NewStyle().Bold(true)
	acmmErrorStyle  = lipgloss.NewStyle().Bold(true)
	acmmNoteStyle   = lipgloss.NewStyle().Faint(true)
	acmmCursorStyle = lipgloss.NewStyle().Bold(true)
)

// acmmBlastRadius is the sentence the confirmation may not omit. Every clause
// names something the handler actually does (client.ACMMLevelResult), because a
// warning that generalizes is a warning an operator learns to skip.
const acmmBlastRadius = "This applies FLEET-WIDE: it can replace the agent roster, " +
	"pause and resume agents, reset every per-agent mode override, " +
	"and change governor cadences, thresholds and the evaluation interval."

// acmmNoPacks is the empty-but-successful list. Saying so explicitly is the
// difference between "the hive defines no packs" and a box that looks broken.
const acmmNoPacks = "no ACMM packs defined"

// acmmNoCurrent is what an unconfigured hive looks like: packs exist, none is
// flagged current. That is an ordinary state (client.ACMMStatus.Level is 0),
// not an error, so it is stated rather than dressed as one.
const acmmNoCurrent = "no level configured"

// acmmEmptyList is what a receipt's empty category says. The acceptance
// criteria require it: a category that silently disappeared would read as "not
// reported" rather than "nothing in it".
const acmmEmptyList = "none"

// acmmVisibleRows bounds the pack list so a hive defining many levels cannot
// grow the overlay past the terminal.
const acmmVisibleRows = 6

// View renders the overlay's box, sized to its own content. Placing it over the
// frame is the app's job, matching confirmView and the model picker.
func (o ACMMOverlay) View(width int) string {
	var body strings.Builder
	body.WriteString(acmmTitleStyle.Render("ACMM level"))
	body.WriteString("\n")
	body.WriteString(acmmNoteStyle.Render(o.subtitle()))
	body.WriteString("\n\n")
	body.WriteString(o.bodyText())
	body.WriteString("\n\n")
	body.WriteString(acmmNoteStyle.Render(o.footer()))

	// Wider than the pause modal and the picker: a pack row carries a name, a
	// description, an agent count and the recommended governor settings, and
	// the receipt carries several agent lists.
	contentWidth := min(72, max(1, width-6))
	return acmmBoxStyle.Width(contentWidth).Render(body.String())
}

func (o ACMMOverlay) subtitle() string {
	// After a successful apply the level in force is the one the RESULT
	// reports, not the one the pack list was fetched with. Keeping the stale
	// figure here would put "current L4" directly above "Level is now L5",
	// which is a frame that contradicts itself about the single most important
	// fact on it.
	if o.result != nil {
		return fmt.Sprintf("current L%d", o.result.Level)
	}
	if !o.loaded {
		return "current —"
	}
	if o.status.Level == 0 {
		return "current " + acmmNoCurrent
	}
	return fmt.Sprintf("current L%d", o.status.Level)
}

func (o ACMMOverlay) bodyText() string {
	switch {
	case o.result != nil:
		return o.receiptBody()
	case o.pending:
		return fmt.Sprintf("Applying L%d… reconciling the roster, agent run states and governor configuration.",
			o.pendingLevel)
	case o.applyErr != "":
		// The failure is what must be read first, but the confirmation stays
		// under it: retry means "the phrase is still there", so hiding the
		// field would make the guidance meaningless.
		return acmmErrorStyle.Render(o.applyErr) + "\n\n" + o.confirmBody()
	case o.confirming:
		return o.confirmBody()
	default:
		return o.listBody()
	}
}

// confirmBody is the typed-confirmation field: what is about to happen, the
// blast radius, and the phrase with what has been entered so far echoed back.
func (o ACMMOverlay) confirmBody() string {
	pack, name := o.packByLevel(o.confirmLevel)
	var out strings.Builder
	out.WriteString(acmmErrorStyle.Render(fmt.Sprintf("Apply L%d %s?", pack, name)))
	out.WriteString("\n\n")
	out.WriteString(acmmBlastRadius)
	out.WriteString("\n\n")
	out.WriteString(fmt.Sprintf("Type %s to confirm:", ConfirmPhrase(o.confirmLevel)))
	out.WriteString("\n")
	// The typed text is echoed with a visible field so an operator can see
	// exactly what they have entered — including trailing spaces, which are
	// otherwise invisible and are a real reason an exact match fails.
	out.WriteString("  [" + o.typed + "]")
	return out.String()
}

// packByLevel returns a level and the pack name for it, for the confirmation
// heading. The name is looked up rather than carried so it cannot go stale.
func (o ACMMOverlay) packByLevel(level int) (int, string) {
	for _, p := range o.status.Packs {
		if p.Level == level {
			return level, p.Name
		}
	}
	return level, ""
}

func (o ACMMOverlay) listBody() string {
	switch {
	case o.loading:
		return "Loading ACMM packs…"
	case o.listErr != "":
		return acmmErrorStyle.Render(o.listErr)
	case !o.loaded:
		return "Loading ACMM packs…"
	case len(o.status.Packs) == 0:
		return acmmNoPacks
	}

	var out strings.Builder
	start, end := o.window()
	for i := start; i < end; i++ {
		out.WriteString(o.packRow(i))
	}
	out.WriteString(acmmNoteStyle.Render(fmt.Sprintf("%d of %d", o.selected+1, len(o.status.Packs))))
	if o.status.Level == 0 {
		out.WriteString("\n" + acmmNoteStyle.Render("No level is currently configured on this hive."))
	}
	if o.noop != "" {
		out.WriteString("\n" + acmmNoteStyle.Render(o.noop))
	}
	return out.String()
}

// packRow renders one pack: the level and name on the first line with the
// current marker, then the description, the agent count and the governor
// settings the level recommends.
//
// NOTHING here is derived from a hardcoded L1-L6 table. Levels, names, order
// and count all come from the server's response, so a hive that gains a level
// renders it without this file changing.
func (o ACMMOverlay) packRow(i int) string {
	p := o.status.Packs[i]
	cursor := "  "
	if i == o.selected {
		cursor = acmmCursorStyle.Render("▸ ")
	}
	mark := ""
	if p.Current {
		mark = "  (current)"
	}
	head := fmt.Sprintf("%sL%d %s%s", cursor, p.Level, p.Name, mark)

	modes := p.Governor.Modes
	if modes == "" {
		modes = "—"
	}
	policy := p.Governor.MergePolicy
	if policy == "" {
		policy = "—"
	}
	detail := fmt.Sprintf("      %d agents   governor %s   merge %s", p.AgentCount, modes, policy)

	row := head + "\n"
	if p.Description != "" {
		row += acmmNoteStyle.Render("      "+p.Description) + "\n"
	}
	row += acmmNoteStyle.Render(detail) + "\n"
	return row
}

// window returns the slice of pack indices to draw, scrolled to keep the cursor
// visible.
func (o ACMMOverlay) window() (int, int) {
	n := len(o.status.Packs)
	if n <= acmmVisibleRows {
		return 0, n
	}
	start := o.selected - acmmVisibleRows/2
	if start < 0 {
		start = 0
	}
	if start+acmmVisibleRows > n {
		start = n - acmmVisibleRows
	}
	return start, start + acmmVisibleRows
}

// receiptBody renders the FULL reconciliation result.
//
// Every category is printed whether or not it has anything in it, because an
// omitted category is indistinguishable from one the server did not report —
// and "which agents did this pause?" is exactly the question an operator asks
// after a fleet-wide change. Empty ones say "none".
func (o ACMMOverlay) receiptBody() string {
	r := *o.result
	var out strings.Builder
	out.WriteString(acmmTitleStyle.Render(fmt.Sprintf("Applied. Level is now L%d.", r.Level)))
	out.WriteString("\n\n")
	out.WriteString("Roster (pack agents):  " + joinOrNone(r.PackAgents) + "\n")
	out.WriteString("Updated by the pack:   " + joinOrNone(r.PackUpdated) + "\n")
	out.WriteString("Paused:                " + joinOrNone(r.Paused) + "\n")
	out.WriteString("Resumed:               " + joinOrNone(r.Resumed) + "\n")
	out.WriteString("Governor changes:\n")
	out.WriteString(o.governorReceipt(r.GovernorChanges))
	// The governor section is line-oriented and ends in a newline; View adds
	// its own blank line before the footer, so returning it as-is would put two
	// blank rows between the receipt and the keys.
	return strings.TrimRight(out.String(), "\n")
}

// governorReceipt renders the governor half of the receipt. A nil
// GovernorChanges means the level moved no governor setting, which is a real
// answer and is said as one.
func (o ACMMOverlay) governorReceipt(g *client.GovernorChanges) string {
	if g == nil || (g.EvalIntervalS == nil && len(g.Cadences) == 0) {
		return "  " + acmmEmptyList + "\n"
	}
	var out strings.Builder
	if g.EvalIntervalS != nil {
		out.WriteString(fmt.Sprintf("  evaluation interval %ds → %ds\n",
			g.EvalIntervalS.From, g.EvalIntervalS.To))
	}
	if len(g.Cadences) == 0 {
		out.WriteString("  cadences: " + acmmEmptyList + "\n")
		return out.String()
	}
	for _, c := range g.Cadences {
		out.WriteString(fmt.Sprintf("  cadence %s/%s %s → %s\n", c.Mode, c.Agent, c.From, c.To))
	}
	return out.String()
}

// joinOrNone renders a list of agent names, or the explicit empty marker.
func joinOrNone(names []string) string {
	if len(names) == 0 {
		return acmmEmptyList
	}
	return strings.Join(names, ", ")
}

func (o ACMMOverlay) footer() string {
	switch {
	case o.result != nil:
		return "enter/esc close"
	case o.pending:
		// No cancel offered while pending on purpose: the request is already
		// with the server and closing the overlay would neither un-apply it nor
		// show the result.
		return "working…"
	case o.applyErr != "":
		return "type to edit  enter apply  esc cancel"
	case o.confirming:
		return "type to confirm  backspace edit  enter apply  esc cancel"
	case o.loading:
		return "esc cancel"
	case o.loaded && len(o.status.Packs) > 0:
		return "j/k move  enter select  esc cancel"
	default:
		return "esc close"
	}
}
