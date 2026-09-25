package panes

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui/theme"
)

// HivesOverlay is the `H` overlay: the hives this contributor lends a CLI to,
// with the active one marked, and the four mutations `hivectl hives` offers
// bound to keys (#8128, phase 4 of #8097).
//
// It is a VALUE type with value receivers like the other overlays, and like
// them it is NOT a Pane: it is never a grid cell, so it takes its keys from
// app.go's modal branch (updateHives) rather than the pane routing seam. That
// is what makes "every key is consumed while open" a property of one switch
// statement — and it matters more here than anywhere else in the TUI, because
// two of this overlay's states are TEXT FIELDS. A key leaking out of the add
// form would both fail to type and fire an action the operator did not mean.
//
// THIS OVERLAY IS THE ONE PART OF THE TUI THAT IS NOT A DASHBOARD CLIENT.
// Everything else in pkg/tui reads the dashboard API of ONE hive; the hives
// list is the set of hives this machine can lend a CLI to at all, which lives
// in the contributor's own files (~/.config/hive/profiles.yml and the
// contributor.env generated from it). The overlay never parses or writes those
// files itself: it renders rows the app loaded through pkg/hivectl and emits a
// HivesAction the app performs through the same package's functions, which are
// the ones `hivectl hives` calls. There is no second implementation of the file
// format here, and there must not be — the file holds live registration tokens
// the hub will never reprint.
type HivesOverlay struct {
	// loading is true between opening and the profile read returning. It is
	// distinct from "loaded an empty set": an operator must be able to tell a
	// read still in flight from a machine with no hives configured.
	loading bool
	// loaded is true once a read has returned successfully, whether or not it
	// carried any hives.
	loaded bool
	// rows is the last successful read, in projection order (active first),
	// which is the order the relay walks.
	rows     []HiveRow
	strategy string
	// listErr describes a failed read the operator has to be told about — no
	// profiles configured yet, a corrupt file, a legacy file too misaligned to
	// migrate.
	listErr string
	// path and envPath name the two files, so the overlay can say WHERE the
	// thing it just changed lives rather than leaving the operator to guess.
	path    string
	envPath string

	// selected indexes rows.
	selected int

	// mode is which of the four screens is up: the list, or one of the three
	// forms. See hivesMode.
	mode hivesMode
	// name and hub are the add form's two fields; field says which one has the
	// cursor.
	name  string
	hub   string
	field int
	// target is the hive the remove/rename form acts on, captured when the form
	// opened so moving the cursor cannot retarget a form in progress.
	target HiveRow
	// typed is the text entered into the remove confirmation or the rename
	// field. Nothing is normalized before it is compared.
	typed string

	// pending is true from the moment an action is accepted until it answers.
	// It is the whole reason a second enter cannot run the same mutation twice.
	pending bool
	// pendingWhat describes the in-flight action for the working line.
	pendingWhat string
	// actionErr is a failed mutation rendered as UI rather than ending the
	// program.
	actionErr string
	// note is the last successful mutation's receipt, held on the list screen
	// so the operator reads what happened without it flashing past.
	note string

	// escQuits is set by the hives-only entry, where the overlay is the whole
	// program rather than a modal above the operator grid.
	escQuits bool
}

// HiveRow is one hive as the overlay draws it.
//
// It is deliberately NOT hivectl.Profile: that struct carries the registration
// token, and a type that reaches the render layer holding a live credential is
// one refactor away from printing it. The app projects the fields this frame
// shows and leaves the secret in the store.
type HiveRow struct {
	Name          string
	Hub           string
	ContributorID string
	Session       string
	Active        bool

	// Reachable is the probe's answer, or nil while it is still outstanding.
	// A pointer rather than two bools because "not asked yet" and "asked and
	// the hub did not answer" are different facts and render differently.
	Reachable *bool
}

// hivesMode is which screen the overlay is showing.
type hivesMode int

const (
	// hivesModeList is the list, and the only mode ordinary letters are
	// bindings in.
	hivesModeList hivesMode = iota
	// hivesModeAdd is the two-field add form.
	hivesModeAdd
	// hivesModeRemove is the typed removal confirmation.
	hivesModeRemove
	// hivesModeRename is the single-field rename form.
	hivesModeRename
)

// HivesActionKind names the mutation the app should perform.
type HivesActionKind int

const (
	// HivesActionUse switches the active hive — the acceptance criterion of
	// #8128: enter on a row must have the same effect as `hivectl hives use`.
	HivesActionUse HivesActionKind = iota
	// HivesActionAdd registers with a hub and appends a profile.
	HivesActionAdd
	// HivesActionRemove forgets a profile and its registration token.
	HivesActionRemove
	// HivesActionRename renames a profile.
	HivesActionRename
	HivesActionMoveUp
	HivesActionMoveDown
	HivesActionStrategy
)

// HivesAction is the mutation Submit accepted, addressed by NAME rather than by
// row index: the app reloads the profile set before applying it (the file is
// shared with the CLI and may have moved under us), and an index into a stale
// snapshot would then name a different hive.
type HivesAction struct {
	Kind     HivesActionKind
	Name     string
	Hub      string
	NewName  string
	Strategy string
}

// NewHivesOverlay returns the overlay in its loading state.
//
// It is constructed already loading because the app opens it and issues the
// profile read in the same Update: there is no frame in which the overlay is
// open but nothing has been asked for, so there is no state for one.
func NewHivesOverlay() HivesOverlay {
	return HivesOverlay{loading: true}
}

// WithEscQuit changes list-screen hints for the hives-only entry, where Esc
// exits the program instead of closing back to an operator grid.
func (o HivesOverlay) WithEscQuit() HivesOverlay {
	o.escQuits = true
	return o
}

// Loading reports whether the profile read is still outstanding.
func (o HivesOverlay) Loading() bool { return o.loading }

// Pending reports whether a mutation is in flight. The app checks it before
// accepting enter and before accepting esc, which bounds the overlay to one
// credential-file write at a time.
func (o HivesOverlay) Pending() bool { return o.pending }

// Typing reports whether the overlay is composing text.
//
// The app routes rune presses to Type only while this is true, which is what
// makes `a`, `d` and `r` bindings on the list and literal characters inside a
// hive name.
func (o HivesOverlay) Typing() bool { return o.mode != hivesModeList }

// Rows returns the hives currently on screen, in the order they are drawn.
func (o HivesOverlay) Rows() []HiveRow { return o.rows }

// Selected returns the row under the cursor. ok is false whenever there is
// nothing selectable — still loading, a failed read, or a successful but empty
// one — so the app makes enter a no-op rather than switching to a hive nobody
// chose.
func (o HivesOverlay) Selected() (HiveRow, bool) {
	if o.loading || !o.loaded {
		return HiveRow{}, false
	}
	if o.selected < 0 || o.selected >= len(o.rows) {
		return HiveRow{}, false
	}
	return o.rows[o.selected], true
}

// SetHives records a successful profile read.
//
// The cursor is preserved BY NAME across a reload rather than by index. Every
// mutation reloads, and `use` reorders the list (the active hive is projected
// first), so an index-preserving reload would leave the cursor on whichever
// hive happened to slide into that position — usually not the one the operator
// was looking at.
func (o HivesOverlay) SetHives(rows []HiveRow, path, envPath, strategy string) HivesOverlay {
	var under string
	if current, ok := o.Selected(); ok {
		under = current.Name
	}
	o.loading = false
	o.loaded = true
	o.listErr = ""
	o.rows = append([]HiveRow(nil), rows...)
	o.strategy = strategy
	o.path = path
	o.envPath = envPath
	o.selected = 0
	for i, row := range o.rows {
		if strings.EqualFold(row.Name, under) {
			o.selected = i
			break
		}
	}
	return o
}

// MoveSelectedRank starts a rank-order move for the selected hive.
func (o HivesOverlay) MoveSelectedRank(delta int) (HivesOverlay, HivesAction, bool) {
	if o.pending || o.mode != hivesModeList {
		return o, HivesAction{}, false
	}
	row, ok := o.Selected()
	if !ok {
		return o, HivesAction{}, false
	}
	if delta < 0 {
		return o.start("Moving " + row.Name + " up"), HivesAction{Kind: HivesActionMoveUp, Name: row.Name}, true
	}
	return o.start("Moving " + row.Name + " down"), HivesAction{Kind: HivesActionMoveDown, Name: row.Name}, true
}

// CycleStrategy advances The Commons routing preset.
func (o HivesOverlay) CycleStrategy() (HivesOverlay, HivesAction, bool) {
	if o.pending || o.mode != hivesModeList {
		return o, HivesAction{}, false
	}
	next := "ranked"
	switch strings.ToLower(strings.TrimSpace(o.strategy)) {
	case "", "ranked":
		next = "spread"
	case "spread":
		next = "neediest"
	}
	return o.start("Setting The Commons strategy to " + next), HivesAction{Kind: HivesActionStrategy, Strategy: next}, true
}

// SetListError records a failed profile read.
func (o HivesOverlay) SetListError(err error) HivesOverlay {
	o.loading = false
	o.loaded = false
	o.rows = nil
	o.selected = 0
	o.listErr = fmt.Sprintf("%v", err)
	return o
}

// SetReachable records one hub's probe answer.
//
// It is keyed by HUB rather than by name or index because the probes are issued
// per hub and answer in whatever order the network allows, and because a
// mutation may have reloaded the list in between. Several profiles can name the
// same hub (two named sessions against one hive), and both rows get the answer
// — it is the hub that was probed, not the profile.
//
// An answer for a hub no longer in the list is dropped, which is the case that
// makes this safe to call at any time: a probe outstanding when the operator
// removes a hive cannot resurrect a row or panic on a stale index.
func (o HivesOverlay) SetReachable(hub string, reachable bool) HivesOverlay {
	rows := append([]HiveRow(nil), o.rows...)
	found := false
	for i := range rows {
		if rows[i].Hub == hub {
			value := reachable
			rows[i].Reachable = &value
			found = true
		}
	}
	if !found {
		return o
	}
	o.rows = rows
	return o
}

// Move steps the cursor by delta and clamps at the list's edges.
//
// It refuses to move while a form is open or a mutation is pending, for the
// same reason the ACMM overlay's does: the cursor is what a form refers to, so
// letting j/k slide it under a half-typed removal confirmation would be exactly
// the class of mistake the confirmation exists to prevent.
func (o HivesOverlay) Move(delta int) HivesOverlay {
	if o.mode != hivesModeList || o.pending {
		return o
	}
	if len(o.rows) == 0 {
		o.selected = 0
		return o
	}
	next := o.selected + delta
	if next < 0 {
		next = 0
	}
	if next >= len(o.rows) {
		next = len(o.rows) - 1
	}
	o.selected = next
	return o
}

// BeginAdd opens the add form.
//
// ok is false while a mutation is pending. Unlike the other two forms it does
// NOT need a selected row: adding the first hive to an empty machine is exactly
// when there is nothing to select.
func (o HivesOverlay) BeginAdd() (HivesOverlay, bool) {
	if o.pending || o.mode != hivesModeList {
		return o, false
	}
	o.mode = hivesModeAdd
	o.name = ""
	o.hub = ""
	o.field = 0
	o.actionErr = ""
	o.note = ""
	return o, true
}

// BeginRemove opens the typed removal confirmation for the selected hive.
//
// WHY TYPED, when pausing an agent gets a y/n. Removing a profile discards a
// live registration token that the hub CANNOT reprint — register is
// unauthenticated, so it must never hand an existing contributor's credential
// back to whoever asks (#4408). Re-adding the hive means registering again or
// moving the identity with `just contribute-move`. The CLI asks for the hive's
// name for that reason, and an overlay that asked for less would be a softer
// gate on the same irreversible act.
func (o HivesOverlay) BeginRemove() (HivesOverlay, bool) {
	if o.pending || o.mode != hivesModeList {
		return o, false
	}
	row, ok := o.Selected()
	if !ok {
		return o, false
	}
	o.mode = hivesModeRemove
	o.target = row
	o.typed = ""
	o.actionErr = ""
	o.note = ""
	return o, true
}

// BeginRename opens the rename form for the selected hive, prefilled with its
// current name so a small correction is a couple of keys rather than a retype.
func (o HivesOverlay) BeginRename() (HivesOverlay, bool) {
	if o.pending || o.mode != hivesModeList {
		return o, false
	}
	row, ok := o.Selected()
	if !ok {
		return o, false
	}
	o.mode = hivesModeRename
	o.target = row
	o.typed = row.Name
	o.actionErr = ""
	o.note = ""
	return o, true
}

// Cancel closes an open form and returns to the list, discarding what was
// typed. It refuses while a mutation is pending: the write is already with the
// filesystem and hiding its result would not undo it.
func (o HivesOverlay) Cancel() HivesOverlay {
	if o.pending {
		return o
	}
	o.mode = hivesModeList
	o.name = ""
	o.hub = ""
	o.typed = ""
	o.field = 0
	o.target = HiveRow{}
	o.actionErr = ""
	return o
}

// NextField moves between the add form's two fields. It is a no-op in the
// single-field forms, so `tab` cannot type a tab character into a hive name.
func (o HivesOverlay) NextField(delta int) HivesOverlay {
	if o.mode != hivesModeAdd || o.pending {
		return o
	}
	o.field = (o.field + delta + 2) % 2
	return o
}

// Type appends the runes of one key press to the focused field.
func (o HivesOverlay) Type(runes string) HivesOverlay {
	if o.mode == hivesModeList || o.pending {
		return o
	}
	switch {
	case o.mode == hivesModeAdd && o.field == 0:
		o.name += runes
	case o.mode == hivesModeAdd:
		o.hub += runes
	default:
		o.typed += runes
	}
	return o
}

// Backspace removes the last RUNE of the focused field.
//
// A rune rather than a byte: hub URLs and hive names are ASCII in practice, but
// a pasted multi-byte character would otherwise be left as an invalid fragment
// the operator can neither see nor clear.
func (o HivesOverlay) Backspace() HivesOverlay {
	if o.mode == hivesModeList || o.pending {
		return o
	}
	trim := func(s string) string {
		if s == "" {
			return s
		}
		runes := []rune(s)
		return string(runes[:len(runes)-1])
	}
	switch {
	case o.mode == hivesModeAdd && o.field == 0:
		o.name = trim(o.name)
	case o.mode == hivesModeAdd:
		o.hub = trim(o.hub)
	default:
		o.typed = trim(o.typed)
	}
	return o
}

// Submit accepts the current screen and returns the action the app should
// perform.
//
// ok is false — and NOTHING is emitted — for every case that must not become a
// write. In particular, on the removal screen a typed name that does not match
// the hive is a NO-OP: the overlay simply stays where it is, which is the
// acceptance criterion for `d`. The comparison is case-insensitive to match
// `hivectl hives remove`, whose profile names are unique case-insensitively, so
// the two surfaces accept exactly the same confirmations.
func (o HivesOverlay) Submit() (HivesOverlay, HivesAction, bool) {
	if o.pending {
		return o, HivesAction{}, false
	}
	switch o.mode {
	case hivesModeList:
		row, ok := o.Selected()
		if !ok {
			return o, HivesAction{}, false
		}
		return o.start("Switching to " + row.Name), HivesAction{Kind: HivesActionUse, Name: row.Name}, true
	case hivesModeAdd:
		name := strings.TrimSpace(o.name)
		hub := strings.TrimSpace(o.hub)
		if name == "" || hub == "" {
			// Both fields are required and neither has a defensible default: a
			// hub cannot be guessed, and an unnamed profile is the positional
			// file this whole feature replaced. Saying so beats a validation
			// error from three layers down.
			o.actionErr = "a name and a hub URL are both required"
			return o, HivesAction{}, false
		}
		return o.start("Registering with " + hub), HivesAction{Kind: HivesActionAdd, Name: name, Hub: hub}, true
	case hivesModeRemove:
		if !strings.EqualFold(strings.TrimSpace(o.typed), o.target.Name) {
			return o, HivesAction{}, false
		}
		return o.start("Removing " + o.target.Name), HivesAction{Kind: HivesActionRemove, Name: o.target.Name}, true
	case hivesModeRename:
		newName := strings.TrimSpace(o.typed)
		if newName == "" || newName == o.target.Name {
			return o, HivesAction{}, false
		}
		return o.start("Renaming " + o.target.Name), HivesAction{Kind: HivesActionRename, Name: o.target.Name, NewName: newName}, true
	}
	return o, HivesAction{}, false
}

// start marks a mutation as in flight.
func (o HivesOverlay) start(what string) HivesOverlay {
	o.pending = true
	o.pendingWhat = what
	o.actionErr = ""
	o.note = ""
	return o
}

// SetActionResult records a successful mutation: back to the list, with the
// receipt held under it.
//
// The list itself is NOT updated from here. The app reloads it from the store
// instead, so what the overlay shows after a write is what the file now says
// rather than what this frame believed it asked for — which is the only version
// that stays right when the CLI and the pane are both open.
func (o HivesOverlay) SetActionResult(note string) HivesOverlay {
	o.pending = false
	o.pendingWhat = ""
	o.actionErr = ""
	o.note = note
	o.mode = hivesModeList
	o.name = ""
	o.hub = ""
	o.typed = ""
	o.field = 0
	o.target = HiveRow{}
	return o
}

// SetActionError records a failed mutation.
//
// The overlay stays on the form it was submitted from so the operator can fix a
// typo and retry, rather than being bounced to the list having lost what they
// entered. A failed write means the profiles are exactly as they were: every
// mutation goes through ProfileStore.Commit, which validates before it writes
// and writes profiles.yml before the projection.
func (o HivesOverlay) SetActionError(err error) HivesOverlay {
	o.pending = false
	o.pendingWhat = ""
	o.actionErr = fmt.Sprintf("%v", err)
	return o
}

var (
	hivesBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder()).
			BorderForeground(theme.BorderFocus).
			Foreground(theme.Text).
			Padding(0, 1)
	hivesTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(theme.Accent)
	hivesHeaderStyle   = lipgloss.NewStyle().Bold(true).Foreground(theme.Muted)
	hivesRuleStyle     = lipgloss.NewStyle().Foreground(theme.Border)
	hivesFooterStyle   = lipgloss.NewStyle().Foreground(theme.Muted)
	hivesErrorStyle    = lipgloss.NewStyle().Bold(true).Foreground(theme.Danger)
	hivesNoteStyle     = lipgloss.NewStyle().Foreground(theme.Muted)
	hivesCursorStyle   = lipgloss.NewStyle().Bold(true).Foreground(theme.Accent)
	hivesActiveStyle   = lipgloss.NewStyle().Bold(true).Foreground(theme.Accent)
	hivesSuccessStyle  = lipgloss.NewStyle().Foreground(theme.Success)
	hivesWarningStyle  = lipgloss.NewStyle().Foreground(theme.Warning)
	hivesDangerStyle   = lipgloss.NewStyle().Foreground(theme.Danger)
	hivesSelectedStyle = lipgloss.NewStyle().Background(theme.Selected)
)

// hivesVisibleRows bounds the list so a contributor with many hives cannot grow
// the overlay past the terminal.
const hivesVisibleRows = 8

// hivesNone is the empty-but-successful read. Saying so explicitly is the
// difference between "this machine has no hives configured" and a box that
// looks broken.
const hivesNone = "no hives configured — press a to add one"

// hivesActiveLegend explains the marker in the terms the relay actually works
// in, matching `hivectl hives list`'s own footer. "Active" on its own would
// suggest a hive that is running rather than the hub that gets solicited first.
const hivesActiveLegend = "* = active: the hub the relay solicits from first."

// hivesSwitchNote describes the live-switch half the app appends after the
// profile set is committed and the relay signal has been attempted.
const hivesSwitchNote = "the running relay is signaled when present"

// View renders the overlay's box, sized to its own content. Placing it over the
// frame is the app's job, matching every other overlay.
func (o HivesOverlay) View(width int) string {
	boxWidth := max(1, width-4)
	contentWidth := max(1, boxWidth-2)

	var body strings.Builder
	body.WriteString(hivesTitleStyle.Render("Hives"))
	body.WriteString("\n")
	body.WriteString(hivesNoteStyle.Render(o.subtitle()))
	body.WriteString("\n\n")
	body.WriteString(o.bodyText(contentWidth))
	body.WriteString("\n\n")
	body.WriteString(hivesFooterStyle.Render(o.footer()))

	// Wider than the ACMM overlay when the terminal is wide enough.
	// A row carries a name, a FULL hub URL, a contributor id and two short
	// columns, and the hub is the field an operator most needs to read whole:
	// two profiles for the same hive differ only in their session, and two
	// hives under one domain differ only in the part a truncated URL cuts off.
	return hivesBoxStyle.Width(boxWidth).Render(body.String())
}

func (o HivesOverlay) subtitle() string {
	if !o.loaded {
		return "contributor profiles"
	}
	strategy := strings.TrimSpace(o.strategy)
	if strategy == "" {
		strategy = "ranked"
	}
	for _, row := range o.rows {
		if row.Active {
			return "The Commons: " + strategy + " · active: " + row.Name
		}
	}
	return "The Commons: " + strategy + " · no active hive"
}

func (o HivesOverlay) bodyText(contentWidth int) string {
	switch {
	case o.pending:
		return o.pendingWhat + "…"
	case o.mode == hivesModeAdd:
		return o.addBody()
	case o.mode == hivesModeRemove:
		return o.removeBody()
	case o.mode == hivesModeRename:
		return o.renameBody()
	default:
		return o.listBody(contentWidth)
	}
}

func (o HivesOverlay) listBody(contentWidth int) string {
	switch {
	case o.loading:
		return "Reading hive profiles…"
	case o.listErr != "":
		// A read failure is not always a fault: "no hives configured yet" comes
		// through here on a machine that has never run contribute-setup. The
		// add hint below is what makes that case actionable rather than a dead
		// end, so it is printed under every read error.
		return hivesErrorStyle.Render(o.listErr) + "\n\n" + hivesNoteStyle.Render("press a to add a hive")
	case !o.loaded:
		return "Reading hive profiles…"
	case len(o.rows) == 0:
		return hivesNone
	}

	cols := o.hiveColumns(contentWidth)
	var out strings.Builder
	out.WriteString(hivesHeaderStyle.Render(hivesHeader(cols)))
	out.WriteString("\n")
	out.WriteString(hivesRuleStyle.Render(strings.Repeat("─", contentWidth)))
	out.WriteString("\n")
	start, end := o.window()
	for i := start; i < end; i++ {
		out.WriteString(o.hiveRow(i, cols, contentWidth))
		out.WriteString("\n")
	}
	if o.actionErr != "" {
		out.WriteString("\n" + hivesErrorStyle.Render(o.actionErr))
	}
	if o.note != "" {
		out.WriteString("\n" + hivesNoteStyle.Render(strings.ReplaceAll(o.note, "; ", ";\n")))
	}
	return out.String()
}

const (
	hiveMinNameWidth = 7
	hiveContribWidth = 12
	hiveSessionWidth = 7
	// Wide enough for "checking…", the longest of the three probe states.
	hiveReachableWidth = 9
	hiveRowChromeWidth = 7 // cursor + active marker + spaces between columns
)

type hiveColumns struct {
	name int
	hub  int
}

func (o HivesOverlay) hiveColumns(contentWidth int) hiveColumns {
	nameNeed := lipgloss.Width("NAME")
	hubNeed := lipgloss.Width("HUB")
	for _, row := range o.rows {
		nameNeed = max(nameNeed, lipgloss.Width(row.Name))
		hubNeed = max(hubNeed, lipgloss.Width(row.Hub))
	}
	remaining := max(0, contentWidth-hiveRowChromeWidth-hiveContribWidth-hiveSessionWidth-hiveReachableWidth)
	nameReserve := min(nameNeed, min(hiveMinNameWidth, remaining))
	hubWidth := min(hubNeed, max(0, remaining-nameReserve))
	nameWidth := min(nameNeed, max(0, remaining-hubWidth))
	if nameWidth < lipgloss.Width("NAME") && remaining >= lipgloss.Width("NAME") {
		nameWidth = lipgloss.Width("NAME")
	}
	if hubWidth < lipgloss.Width("HUB") && remaining-nameWidth >= lipgloss.Width("HUB") {
		hubWidth = lipgloss.Width("HUB")
	}
	return hiveColumns{name: nameWidth, hub: hubWidth}
}

func hivesHeader(cols hiveColumns) string {
	return fmt.Sprintf("   %s %s %s %s %s",
		hiveColumn("NAME", cols.name, false),
		hiveColumn("HUB", cols.hub, false),
		hiveColumn("CONTRIB ID", hiveContribWidth, false),
		hiveColumn("SESSION", hiveSessionWidth, false),
		hiveColumn("REACHABLE", hiveReachableWidth, false),
	)
}

func (o HivesOverlay) hiveRow(i int, cols hiveColumns, contentWidth int) string {
	row := o.rows[i]
	selected := i == o.selected
	cursor := "  "
	cursorStyle := hivesCellStyle(lipgloss.NewStyle(), selected)
	if selected {
		cursorStyle = hivesCellStyle(hivesCursorStyle, selected)
		cursor = "▸ "
	}
	cursor = cursorStyle.Render(cursor)
	marker := " "
	if row.Active {
		marker = "*"
	}
	marker = hivesCellStyle(hivesActiveStyle, selected).Render(marker)
	space := hivesCellStyle(lipgloss.NewStyle(), selected).Render(" ")
	line := fmt.Sprintf("%s%s%s%s%s%s%s%s%s%s%s",
		cursor, marker,
		hiveColumn(row.Name, cols.name, selected), space,
		hiveHubColumn(row.Hub, cols.hub, selected), space,
		hiveColumn(hiveDash(row.ContributorID), hiveContribWidth, selected), space,
		hiveColumn(hiveDash(row.Session), hiveSessionWidth, selected), space,
		hiveReachableColumn(row.Reachable, hiveReachableWidth, selected),
	)
	return padCells(line, contentWidth, selected)
}

func padCells(s string, width int, selected bool) string {
	if extra := width - lipgloss.Width(s); extra > 0 {
		return s + hivesCellStyle(lipgloss.NewStyle(), selected).Render(strings.Repeat(" ", extra))
	}
	return lipgloss.NewStyle().Inline(true).MaxWidth(width).Render(s)
}

func hiveColumn(value string, width int, selected bool) string {
	return hiveStyledColumn(truncateEnd(value, width), width, lipgloss.NewStyle(), selected)
}

func hiveHubColumn(value string, width int, selected bool) string {
	return hiveStyledColumn(truncateMiddle(value, width), width, lipgloss.NewStyle(), selected)
}

func hiveStyledColumn(value string, width int, style lipgloss.Style, selected bool) string {
	return hivesCellStyle(style, selected).Inline(true).Width(width).MaxWidth(width).Render(value)
}

func hiveReachableColumn(reachable *bool, width int, selected bool) string {
	label := reachableLabel(reachable)
	style := hivesWarningStyle
	if reachable != nil && *reachable {
		style = hivesSuccessStyle
	} else if reachable != nil {
		style = hivesDangerStyle
	}
	return hiveStyledColumn(label, width, style, selected)
}

func hivesCellStyle(style lipgloss.Style, selected bool) lipgloss.Style {
	if selected {
		return style.Inherit(hivesSelectedStyle)
	}
	return style
}

func truncateEnd(s string, width int) string {
	r := []rune(s)
	if width <= 0 {
		return ""
	}
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

func truncateMiddle(s string, width int) string {
	r := []rune(s)
	if width <= 0 {
		return ""
	}
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	prefix := (width - 1) / 2
	if scheme := strings.Index(s, "://"); scheme >= 0 {
		schemeWidth := len([]rune(s[:scheme+3]))
		if schemeWidth < width-1 {
			prefix = max(prefix, schemeWidth)
		}
	}
	suffix := width - prefix - 1
	return string(r[:prefix]) + "…" + string(r[len(r)-suffix:])
}

func hiveDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

// reachableLabel renders the probe column's three states.
//
// "checking…" is not cosmetic. The probes are issued per hub and answer
// independently, so the list is on screen before any of them return; without a
// distinct label for "no answer yet", an unreachable hive and one that simply
// has not been asked would look the same for as long as the timeout lasts —
// and the operator would read the slower one as broken.
func reachableLabel(reachable *bool) string {
	switch {
	case reachable == nil:
		return "checking…"
	case *reachable:
		return "yes"
	default:
		return "no"
	}
}

// window returns the slice of row indices to draw, scrolled to keep the cursor
// visible.
func (o HivesOverlay) window() (int, int) {
	n := len(o.rows)
	if n <= hivesVisibleRows {
		return 0, n
	}
	start := o.selected - hivesVisibleRows/2
	if start < 0 {
		start = 0
	}
	if start+hivesVisibleRows > n {
		start = n - hivesVisibleRows
	}
	return start, start + hivesVisibleRows
}

func (o HivesOverlay) addBody() string {
	var out strings.Builder
	out.WriteString(hivesTitleStyle.Render("Add a hive"))
	out.WriteString("\n\n")
	out.WriteString(o.inputLine("name", o.name, o.field == 0))
	out.WriteString("\n")
	out.WriteString(o.inputLine("hub ", o.hub, o.field == 1))
	out.WriteString("\n\n")
	out.WriteString(hivesNoteStyle.Render(
		"Registers with the hub and appends the profile — the registration half of\n" +
			"'just contribute-setup', with no gh login and no backend preflight. The hub\n" +
			"URL is the contributor endpoint, e.g. wss://<hive>/contribute."))
	if o.actionErr != "" {
		out.WriteString("\n\n" + hivesErrorStyle.Render(o.actionErr))
	}
	return out.String()
}

func (o HivesOverlay) removeBody() string {
	var out strings.Builder
	out.WriteString(hivesErrorStyle.Render(fmt.Sprintf("Remove hive %q (%s)?", o.target.Name, o.target.Hub)))
	out.WriteString("\n\n")
	out.WriteString("This discards its registration token, which the hub cannot reprint:\n" +
		"re-adding this hive means registering again, or moving the identity with\n" +
		"'just contribute-move'. The previous contributor.env is kept at .bak.")
	out.WriteString("\n\n")
	out.WriteString(fmt.Sprintf("Type the hive name (%s) to confirm:", o.target.Name))
	out.WriteString("\n")
	// Echoed inside brackets so trailing spaces — invisible otherwise, and a
	// real reason a confirmation fails to match — are visible.
	out.WriteString("  [" + o.typed + "]")
	if o.actionErr != "" {
		out.WriteString("\n\n" + hivesErrorStyle.Render(o.actionErr))
	}
	return out.String()
}

func (o HivesOverlay) renameBody() string {
	var out strings.Builder
	out.WriteString(hivesTitleStyle.Render(fmt.Sprintf("Rename hive %q", o.target.Name)))
	out.WriteString("\n\n")
	out.WriteString(o.inputLine("new name", o.typed, true))
	out.WriteString("\n\n")
	out.WriteString(hivesNoteStyle.Render(
		"Names may use letters, digits, '-', '_' and '.', and are unique\n" +
			"case-insensitively. The hub, credential and contributor id are untouched."))
	if o.actionErr != "" {
		out.WriteString("\n\n" + hivesErrorStyle.Render(o.actionErr))
	}
	return out.String()
}

// inputLine renders one text field, marking the one with the cursor.
func (o HivesOverlay) inputLine(label, value string, focused bool) string {
	cursor := "  "
	if focused {
		cursor = hivesCursorStyle.Render("▸ ")
	}
	return fmt.Sprintf("%s%s  [%s]", cursor, label, value)
}

func (o HivesOverlay) footer() string {
	closeHint := "esc close"
	if o.escQuits {
		closeHint = "esc/q quit"
	}
	keys := ""
	switch {
	case o.pending:
		// No cancel offered while pending: the write is already under way and
		// closing the overlay would neither undo it nor show its result.
		keys = "working…"
	case o.mode == hivesModeAdd:
		keys = "type to edit  tab next field  enter add  esc cancel"
	case o.mode == hivesModeRemove:
		keys = "type the name  backspace edit  enter remove  esc cancel"
	case o.mode == hivesModeRename:
		keys = "type to edit  backspace edit  enter rename  esc cancel"
	case o.loading:
		keys = closeHint
	case o.loaded && len(o.rows) > 0:
		keys = "j/k cursor  [/ ] rank  s strategy  enter use  a add  d remove  r rename  " + closeHint
	default:
		keys = "a add  " + closeHint
	}
	if o.loaded && len(o.rows) > 0 && o.mode == hivesModeList {
		return fmt.Sprintf("%d of %d   %s\n%s", o.selected+1, len(o.rows), hivesActiveLegend, keys)
	}
	return keys
}

// HivesUseNote is the receipt `use` leaves on the list, and the note the
// operator most needs after switching: where the projection was written. The
// app appends whether a running relay was signaled after the commit succeeds.
//
// Exported so the app builds it and the tests assert it without either of them
// re-deriving the wording.
func HivesUseNote(name, hub, envPath string) string {
	return fmt.Sprintf("✓ active hive is now %q (%s) — %s regenerated; %s", name, hub, envPath, hivesSwitchNote)
}
