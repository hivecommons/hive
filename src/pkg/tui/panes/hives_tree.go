package panes

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The Hives overlay's contribution tree screen (RFC #13153, TUI surface).
//
// The tree is the Hub → Hive → Repository view the Commons web page also
// shows, drawn from the same credential-free read model and edited through the
// same staged-edit session. This file owns only STATE and RENDERING: which rows
// are visible, where the cursor is, what the staging bar and the confirmation
// screen say. The app half (pkg/tui/hives_tree.go) owns the session, the
// discovery calls and the Apply, exactly as the list screen's mutations live in
// pkg/tui/hives.go. Nothing here can write a file or hold a token.

// HiveTreeKind is the level a tree row sits at.
type HiveTreeKind int

const (
	HiveTreeHub HiveTreeKind = iota
	HiveTreeHive
	HiveTreeRepo
	// HiveTreePlaceholder is the explanatory row under a hive with no
	// repositories to list: "membership unknown", "no repositories", or
	// "unrestricted — legacy". It is never selectable for an edit.
	HiveTreePlaceholder
)

// HiveTreeCheck is the tri-state checkbox a row draws.
type HiveTreeCheck int

const (
	HiveTreeCheckOff HiveTreeCheck = iota
	HiveTreeCheckPartial
	HiveTreeCheckAll
	// HiveTreeCheckDisabled is drawn for rows that cannot be toggled: legacy
	// profiles, hives with no known repositories, and incomplete branches
	// where a checked state would be vacuous.
	HiveTreeCheckDisabled
)

// HiveTreeRow is one line of the flattened tree in display order (every
// branch expanded). The overlay applies collapse, filter and search on top.
type HiveTreeRow struct {
	Kind HiveTreeKind
	// Key is unique within the tree and stable across reloads so the cursor
	// and the collapsed set survive a re-projection.
	Key string
	// ParentKey is the key of the enclosing row; empty for hubs.
	ParentKey string
	Level     int
	// ID addresses the edit: hub id, subscription name or repository id.
	ID string
	// Subscription is the owning subscription for repository rows.
	Subscription string
	Label        string
	Check        HiveTreeCheck
	Badges       []string
	// Changed marks a row the staged diff touches; NeedsReview marks a row the
	// "needs review" filter keeps (unreviewed, no longer offered, legacy,
	// discovery unavailable).
	Changed     bool
	NeedsReview bool
	// Capability flags the app reads to decide which keys mean something on
	// this row. They are projected from the read model so the renderer and
	// the key handler agree.
	CanSelect bool
	Following bool
	Disabled  bool
	Legacy    bool
	Excluded  bool
	Direct    bool
	// Detail is shown under the tree for the selected row.
	Detail []string
}

// HiveTreeSummary is the staging bar's content.
type HiveTreeSummary struct {
	Staged bool
	Lines  []string
	// Live and Durable are the runtime source lines: what the local relay's
	// status snapshot and the local activity store say, or "unavailable — "
	// with a reason when a source is missing. Labels, never claims.
	Live, Durable string
}

// HiveTreeFilter is the tree's row filter.
type HiveTreeFilter int

const (
	HiveTreeFilterAll HiveTreeFilter = iota
	HiveTreeFilterReview
	HiveTreeFilterChanged
)

func (f HiveTreeFilter) String() string {
	switch f {
	case HiveTreeFilterReview:
		return "needs review"
	case HiveTreeFilterChanged:
		return "changed"
	}
	return "all"
}

// hivesTreeVisibleRows bounds the tree window.
const hivesTreeVisibleRows = 12

// hiveTreeState is the tree screen's state, embedded in HivesOverlay.
type hiveTreeState struct {
	loading   bool
	rows      []HiveTreeRow
	collapsed map[string]bool
	cursor    int
	filter    HiveTreeFilter
	query     string
	searching bool
	help      bool
	summary   HiveTreeSummary
	// confirm holds the diff lines while the Apply confirmation is up.
	confirm []string
	// leaveWarned is set after the first esc with staged edits pending, so the
	// second esc is an informed discard.
	leaveWarned bool
	err         string
	note        string
}

// InTree reports whether the tree screen is up (including its search field
// and the Apply confirmation).
func (o HivesOverlay) InTree() bool {
	return o.mode == hivesModeTree || o.mode == hivesModeTreeConfirm
}

// TreeSearching reports whether the tree's search field has the keyboard.
func (o HivesOverlay) TreeSearching() bool { return o.mode == hivesModeTree && o.tree.searching }

// TreeConfirming reports whether the Apply confirmation is up.
func (o HivesOverlay) TreeConfirming() bool { return o.mode == hivesModeTreeConfirm }

// BeginTree opens the tree screen in its loading state. ok is false while the
// list itself is still loading, or a form or a write is in progress.
func (o HivesOverlay) BeginTree() (HivesOverlay, bool) {
	if o.pending || o.mode != hivesModeList || !o.loaded {
		return o, false
	}
	o.mode = hivesModeTree
	o.tree = hiveTreeState{loading: true, collapsed: map[string]bool{}}
	o.actionErr = ""
	return o, true
}

// SetTree records a (re)projection of the tree. The cursor is kept by key.
func (o HivesOverlay) SetTree(rows []HiveTreeRow, summary HiveTreeSummary) HivesOverlay {
	if !o.InTree() {
		return o
	}
	var under string
	if row, ok := o.TreeSelected(); ok {
		under = row.Key
	}
	o.tree.loading = false
	o.tree.err = ""
	o.tree.rows = append([]HiveTreeRow(nil), rows...)
	o.tree.summary = summary
	if o.tree.collapsed == nil {
		o.tree.collapsed = map[string]bool{}
	}
	o.tree.cursor = 0
	for i, row := range o.TreeVisible() {
		if row.Key == under {
			o.tree.cursor = i
			break
		}
	}
	return o
}

// SetTreeError records a failed open or a refused edit on the tree screen.
func (o HivesOverlay) SetTreeError(err error) HivesOverlay {
	if !o.InTree() || err == nil {
		return o
	}
	o.tree.loading = false
	o.tree.err = err.Error()
	o.tree.note = ""
	return o
}

// SetTreeNote records a receipt or a staging hint on the tree screen.
func (o HivesOverlay) SetTreeNote(note string) HivesOverlay {
	if !o.InTree() {
		return o
	}
	o.tree.err = ""
	o.tree.note = note
	return o
}

// TreeVisible returns the rows currently drawn, in order: collapsed branches
// hide their descendants, the filter keeps matching rows AND their ancestors,
// and the search keeps rows whose label (or any descendant's label) contains
// the query, again with ancestry retained so a match is never orphaned.
func (o HivesOverlay) TreeVisible() []HiveTreeRow {
	rows := o.tree.rows
	keep := make([]bool, len(rows))
	index := make(map[string]int, len(rows))
	for i, row := range rows {
		index[row.Key] = i
	}
	query := strings.ToLower(strings.TrimSpace(o.tree.query))
	for i, row := range rows {
		if !o.treeFilterKeeps(row) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(row.Label), query) && !strings.Contains(strings.ToLower(row.ID), query) {
			continue
		}
		keep[i] = true
		for parent := row.ParentKey; parent != ""; {
			j, ok := index[parent]
			if !ok {
				break
			}
			keep[j] = true
			parent = rows[j].ParentKey
		}
	}
	out := make([]HiveTreeRow, 0, len(rows))
	for i, row := range rows {
		if !keep[i] || o.treeHiddenByCollapse(row, index) {
			continue
		}
		out = append(out, row)
	}
	return out
}

func (o HivesOverlay) treeFilterKeeps(row HiveTreeRow) bool {
	switch o.tree.filter {
	case HiveTreeFilterReview:
		return row.NeedsReview
	case HiveTreeFilterChanged:
		return row.Changed
	}
	return true
}

func (o HivesOverlay) treeHiddenByCollapse(row HiveTreeRow, index map[string]int) bool {
	for parent := row.ParentKey; parent != ""; {
		if o.tree.collapsed[parent] {
			return true
		}
		j, ok := index[parent]
		if !ok {
			return false
		}
		parent = o.tree.rows[j].ParentKey
	}
	return false
}

// TreeSelected returns the row under the tree cursor.
func (o HivesOverlay) TreeSelected() (HiveTreeRow, bool) {
	if !o.InTree() || o.tree.loading {
		return HiveTreeRow{}, false
	}
	visible := o.TreeVisible()
	if o.tree.cursor < 0 || o.tree.cursor >= len(visible) {
		return HiveTreeRow{}, false
	}
	return visible[o.tree.cursor], true
}

// TreeMove moves the cursor by delta, clamped.
func (o HivesOverlay) TreeMove(delta int) HivesOverlay {
	n := len(o.TreeVisible())
	if n == 0 {
		o.tree.cursor = 0
		return o
	}
	o.tree.cursor = min(n-1, max(0, o.tree.cursor+delta))
	o.tree.leaveWarned = false
	return o
}

// TreeJump moves the cursor to the first or last visible row.
func (o HivesOverlay) TreeJump(end bool) HivesOverlay {
	n := len(o.TreeVisible())
	o.tree.cursor = 0
	if end && n > 0 {
		o.tree.cursor = n - 1
	}
	return o
}

// TreeExpand expands (open=true) or collapses the selected branch. Expanding
// an already-open branch moves to its first child; collapsing an already
// closed row — or a leaf — moves to its parent, matching the WAI-ARIA tree
// pattern the web page follows.
func (o HivesOverlay) TreeExpand(open bool) HivesOverlay {
	row, ok := o.TreeSelected()
	if !ok {
		return o
	}
	branch := row.Kind == HiveTreeHub || row.Kind == HiveTreeHive
	switch {
	case open && branch && o.tree.collapsed[row.Key]:
		delete(o.tree.collapsed, row.Key)
	case open && branch:
		return o.TreeMove(1)
	case !open && branch && !o.tree.collapsed[row.Key]:
		o.tree.collapsed[row.Key] = true
	case !open && row.ParentKey != "":
		for i, candidate := range o.TreeVisible() {
			if candidate.Key == row.ParentKey {
				o.tree.cursor = i
				break
			}
		}
	}
	return o
}

// TreeCycleFilter sets the filter; the cursor is clamped to the new list.
func (o HivesOverlay) TreeSetFilter(filter HiveTreeFilter) HivesOverlay {
	o.tree.filter = filter
	return o.TreeMove(0)
}

// TreeBeginSearch gives the search field the keyboard.
func (o HivesOverlay) TreeBeginSearch() HivesOverlay {
	if o.mode != hivesModeTree {
		return o
	}
	o.tree.searching = true
	return o
}

// TreeSearchType appends to the query; TreeSearchBackspace deletes one rune;
// TreeEndSearch returns the keyboard to the tree. clear drops the query.
func (o HivesOverlay) TreeSearchType(text string) HivesOverlay {
	o.tree.query += text
	return o.TreeMove(0)
}

func (o HivesOverlay) TreeSearchBackspace() HivesOverlay {
	if r := []rune(o.tree.query); len(r) > 0 {
		o.tree.query = string(r[:len(r)-1])
	}
	return o.TreeMove(0)
}

func (o HivesOverlay) TreeEndSearch(clear bool) HivesOverlay {
	o.tree.searching = false
	if clear {
		o.tree.query = ""
	}
	return o.TreeMove(0)
}

// TreeToggleHelp shows or hides the key reference.
func (o HivesOverlay) TreeToggleHelp() HivesOverlay {
	o.tree.help = !o.tree.help
	return o
}

// TreeError returns the tree screen's current error line.
func (o HivesOverlay) TreeError() string { return o.tree.err }

// BeginTreeApply marks the Apply as in flight; Pending() holds the keyboard
// until the receipt lands.
func (o HivesOverlay) BeginTreeApply() HivesOverlay {
	o.pending = true
	o.pendingWhat = "Applying staged changes"
	o.tree.err, o.tree.note = "", ""
	return o
}

// EndTreeApply clears the in-flight mark and returns from the confirmation to
// the tree without touching the list screen's state.
func (o HivesOverlay) EndTreeApply() HivesOverlay {
	o.pending = false
	o.pendingWhat = ""
	return o.TreeConfirmCancel()
}

// TreeStaged reports whether the session has uncommitted edits.
func (o HivesOverlay) TreeStaged() bool { return o.InTree() && o.tree.summary.Staged }

// BeginTreeConfirm raises the Apply confirmation listing the diff. ok is false
// when nothing is staged.
func (o HivesOverlay) BeginTreeConfirm() (HivesOverlay, bool) {
	if o.mode != hivesModeTree || !o.tree.summary.Staged {
		return o, false
	}
	o.mode = hivesModeTreeConfirm
	o.tree.confirm = append([]string(nil), o.tree.summary.Lines...)
	return o, true
}

// TreeConfirmCancel returns from the confirmation to the tree.
func (o HivesOverlay) TreeConfirmCancel() HivesOverlay {
	if o.mode == hivesModeTreeConfirm {
		o.mode = hivesModeTree
		o.tree.confirm = nil
	}
	return o
}

// TreeLeave returns to the list. With staged edits pending, the first call
// warns and stays; the second leaves and the app drops the session.
func (o HivesOverlay) TreeLeave() (HivesOverlay, bool) {
	if !o.InTree() {
		return o, false
	}
	if o.tree.summary.Staged && !o.tree.leaveWarned {
		o.tree.leaveWarned = true
		o.tree.err = "staged edits would be lost — press A to apply, U to discard, or esc again to leave without saving"
		return o, false
	}
	return o.TreeClose(), true
}

// TreeClose returns to the list unconditionally. The app uses it after an
// Apply has consumed the session, when there is nothing left to lose.
func (o HivesOverlay) TreeClose() HivesOverlay {
	if o.InTree() {
		o.mode = hivesModeList
		o.tree = hiveTreeState{}
	}
	return o
}

// TreeSummaryLines returns the staged diff as the review lines Apply shows.
func (o HivesOverlay) TreeSummaryLines() []string {
	return append([]string(nil), o.tree.summary.Lines...)
}

// treeBody renders the tree screen or the Apply confirmation.
func (o HivesOverlay) treeBody(contentWidth int) string {
	if o.mode == hivesModeTreeConfirm {
		return o.treeConfirmBody()
	}
	if o.tree.loading {
		return "Reading hubs, hives and repositories…"
	}
	if o.tree.help {
		return hiveTreeHelp
	}
	var out strings.Builder
	out.WriteString(hivesHeaderStyle.Render(o.treeHeaderLine()))
	out.WriteString("\n")
	for _, line := range o.treeRuntimeLines() {
		out.WriteString(hivesNoteStyle.Render(line))
		out.WriteString("\n")
	}
	out.WriteString(hivesRuleStyle.Render(strings.Repeat("─", contentWidth)))
	out.WriteString("\n")
	visible := o.TreeVisible()
	if len(visible) == 0 {
		switch {
		case len(o.tree.rows) == 0:
			out.WriteString(hivesNoteStyle.Render("no hubs or hives to show — add a hub with `hivectl hives hub add` or a hive with a"))
		default:
			out.WriteString("no rows match the " + o.tree.filter.String() + " filter" + o.treeQuerySuffix())
		}
	}
	start, end := treeWindow(o.tree.cursor, len(visible))
	for i := start; i < end; i++ {
		out.WriteString(o.treeRowLine(visible[i], i == o.tree.cursor, contentWidth))
		out.WriteString("\n")
	}
	if row, ok := o.TreeSelected(); ok && len(row.Detail) > 0 {
		out.WriteString(hivesNoteStyle.Render(truncateEnd(strings.Join(row.Detail, " · "), contentWidth)))
		out.WriteString("\n")
	}
	out.WriteString(o.treeStageLine(contentWidth))
	if o.tree.err != "" {
		out.WriteString("\n" + hivesErrorStyle.Render(o.tree.err))
	}
	if o.tree.note != "" {
		out.WriteString("\n" + hivesNoteStyle.Render(strings.ReplaceAll(o.tree.note, "; ", ";\n")))
	}
	return out.String()
}

func (o HivesOverlay) treeHeaderLine() string {
	line := "CONTRIBUTION TREE  filter: " + o.tree.filter.String()
	if o.tree.searching {
		line += "  search: " + o.tree.query + "▏"
	} else if o.tree.query != "" {
		line += "  search: " + o.tree.query
	}
	return line
}

// treeRuntimeLines gives each runtime source its own line so an
// unavailability reason is never truncated out of the header.
func (o HivesOverlay) treeRuntimeLines() []string {
	return []string{"live: " + hiveDash(o.tree.summary.Live), "durable: " + hiveDash(o.tree.summary.Durable)}
}

func (o HivesOverlay) treeQuerySuffix() string {
	if o.tree.query == "" {
		return ""
	}
	return fmt.Sprintf(" and search %q", o.tree.query)
}

func treeWindow(cursor, n int) (int, int) {
	if n <= hivesTreeVisibleRows {
		return 0, n
	}
	start := min(max(0, cursor-hivesTreeVisibleRows/2), n-hivesTreeVisibleRows)
	return start, start + hivesTreeVisibleRows
}

func treeCheckGlyph(check HiveTreeCheck) string {
	switch check {
	case HiveTreeCheckAll:
		return "[x]"
	case HiveTreeCheckPartial:
		return "[-]"
	case HiveTreeCheckDisabled:
		return "[·]"
	}
	return "[ ]"
}

func (o HivesOverlay) treeRowLine(row HiveTreeRow, selected bool, contentWidth int) string {
	cursor := "  "
	if selected {
		cursor = hivesCursorStyle.Render("▸ ")
	}
	indent := strings.Repeat("  ", max(0, row.Level-1))
	expander := "  "
	if row.Kind == HiveTreeHub || row.Kind == HiveTreeHive {
		expander = "▾ "
		if o.tree.collapsed[row.Key] {
			expander = "▸ "
		}
	}
	check := "   "
	if row.Kind != HiveTreePlaceholder {
		check = treeCheckGlyph(row.Check)
	}
	changed := " "
	if row.Changed {
		changed = hivesSuccessStyle.Render("*")
	}
	label := row.Label
	if row.Kind == HiveTreeHub || row.Kind == HiveTreeHive {
		label = lipgloss.NewStyle().Bold(true).Render(label)
	}
	if row.Kind == HiveTreePlaceholder {
		label = hivesNoteStyle.Render(label)
	}
	badges := ""
	if len(row.Badges) > 0 {
		badges = "  " + hivesNoteStyle.Render(strings.Join(row.Badges, " · "))
	}
	line := cursor + indent + expander + check + changed + " " + label + badges
	line = truncateEnd(line, contentWidth)
	if selected {
		return hivesSelectedStyle.Render(line)
	}
	return line
}

func (o HivesOverlay) treeStageLine(contentWidth int) string {
	if !o.tree.summary.Staged {
		return hivesNoteStyle.Render("No staged changes.")
	}
	return hivesWarningStyle.Render(truncateEnd("Staged: "+strings.Join(o.tree.summary.Lines, "; ")+"  — A apply  U discard", contentWidth))
}

func (o HivesOverlay) treeConfirmBody() string {
	var out strings.Builder
	out.WriteString(hivesTitleStyle.Render("Apply staged changes?"))
	out.WriteString("\n")
	for _, line := range o.tree.confirm {
		out.WriteString("  • " + line + "\n")
	}
	out.WriteString(hivesNoteStyle.Render("profiles.yml and contributor.env are written under the shared lock; a running relay is signaled when present and its acknowledgement is reported separately"))
	if o.tree.err != "" {
		out.WriteString("\n" + hivesErrorStyle.Render(o.tree.err))
	}
	return out.String()
}

const hiveTreeHelp = `Contribution tree keys
  ↑/↓ j/k   move          ←/→ h/l   collapse / expand (→ on open branch enters it)
  g/G       first / last  space     select / deselect current repositories at this row
  f         follow / stop following new repositories (hive)
  x         exclude / clear exclusion (repository)
  e         enable / disable (hive or hub)
  R         review and restrict a legacy unrestricted hive
  A         apply staged edits (shows the full diff first)    U  discard staged edits
  /         search (esc clears)   1/2/3  filter all / needs review / changed
  r         refresh discovery (refused while edits are staged)
  ?         close this help      esc    back to the hive list`

func (o HivesOverlay) treeFooter(closeHint string) string {
	switch {
	case o.mode == hivesModeTreeConfirm:
		return "enter apply  esc cancel"
	case o.tree.searching:
		return "type to search  backspace edit  enter keep  esc clear"
	case o.tree.help:
		return "? close help"
	}
	visible := len(o.TreeVisible())
	keys := "j/k move  h/l fold  space select  f follow  x exclude  e enable  R restrict  A apply  U discard  / 1-3 filter  ? help  esc list"
	if o.escQuits {
		keys = strings.TrimSuffix(keys, "esc list") + "esc list (" + closeHint + ")"
	}
	if visible > 0 {
		return fmt.Sprintf("%d of %d   %s", o.tree.cursor+1, visible, keys)
	}
	return keys
}
