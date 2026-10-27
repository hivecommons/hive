package panes

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The contribution tree is a mode of the Hives overlay (#13291), not a screen
// of its own. Like the list, it renders rows the app projected from pkg/hivectl
// — here the shared Hub → Hive → Repository tree and staged diff — and emits
// HivesTreeActions the app stages through hivectl.StagedEdits. Nothing in this
// file decides consent or writes a file; it never sees a credential either,
// because the projection carries only canonical identities and labels.

// HiveTreeKind is a tree node's level.
type HiveTreeKind int

const (
	HiveTreeHub HiveTreeKind = iota
	HiveTreeHive
	HiveTreeRepo
)

// HiveTreeNode is one projected tree row. Key is unique for this appearance
// (a shared subscription appears once per hub) and stable across refreshes, so
// focus survives a reload. Hub, Subscription and Repository are the canonical
// targets an action names; display text never addresses an edit.
type HiveTreeNode struct {
	Key, Parent string
	Kind        HiveTreeKind
	Depth       int

	Short    string
	Identity string

	Hub          string
	Subscription string
	Repository   string

	// Selection is "x" (all current), "-" (partial) or " " (off).
	Selection string
	Badges    []string
	Follow    string
	Readiness string
	Activity  string

	Provenance      string
	Explanation     string
	ExclusionSource string

	Enabled  bool
	Excluded bool
	Legacy   bool
	Direct   bool

	// Subscriptions and Repositories are the counts a bulk action on this node
	// affects, shown before it is staged.
	Subscriptions int
	Repositories  int
}

// HiveTreeView is everything the app projects for one render of the tree.
type HiveTreeView struct {
	Nodes []HiveTreeNode
	// Scope is the effective-selection summary, always kept visible.
	Scope string
	// Diff is the shared staged diff rendered as text, one change per line.
	Diff []string
	// Staged counts the changes Apply would save; zero means nothing staged.
	Staged int
	// Stale is set once the profiles changed under this review.
	Stale bool
}

// HivesTreeActionKind names the staged edit the app should perform.
type HivesTreeActionKind int

const (
	HivesTreeSelect HivesTreeActionKind = iota
	HivesTreeSelectBranch
	HivesTreeExclude
	HivesTreeFollow
	HivesTreeEnable
	HivesTreeHubEnable
	HivesTreeRestrict
)

// HivesTreeAction is one staged edit, addressed by canonical tree identity.
// Branch actions name the hub or subscription; the shared layer resolves the
// full branch, never the filtered rows on screen.
type HivesTreeAction struct {
	Kind       HivesTreeActionKind
	Target     string
	Repository string
	Hub        bool
	Value      bool
}

type hivesTreeScreen int

const (
	hivesTreeBrowse hivesTreeScreen = iota
	hivesTreeSearch
	hivesTreeConfirm
	hivesTreeReview
	hivesTreeReceipt
	hivesTreeDiscard
)

// HivesTreeLeave is what accepting the discard confirmation leads to.
type HivesTreeLeave int

const (
	// HivesTreeStay discards staged edits and stays on the tree (D).
	HivesTreeStay HivesTreeLeave = iota
	// HivesTreeClose returns to the hives list.
	HivesTreeClose
	// HivesTreeQuit exits the hives-only program.
	HivesTreeQuit
)

type hivesTreeState struct {
	loading      bool
	view         HiveTreeView
	expanded     map[string]bool
	cursor       string
	filter       string
	screen       hivesTreeScreen
	confirm      HivesTreeAction
	prompt       string
	leave        HivesTreeLeave
	reviewOffset int
	receipt      []string
	err          string
	note         string
}

// OpenTree switches the overlay to the contribution tree, loading. Focus and
// expansion from an earlier visit survive.
func (o HivesOverlay) OpenTree() (HivesOverlay, bool) {
	if o.pending || o.mode != hivesModeList {
		return o, false
	}
	o.mode = hivesModeTree
	o.tree.loading = true
	o.tree.screen = hivesTreeBrowse
	o.tree.err, o.tree.note = "", ""
	o.actionErr, o.note = "", ""
	return o, true
}

// BeginTreeRefresh marks a re-read outstanding so edits cannot be staged until
// it lands; staged edits already held by the app are kept.
func (o HivesOverlay) BeginTreeRefresh() HivesOverlay {
	o.tree.loading = true
	return o
}

// InTree reports whether the contribution tree is showing.
func (o HivesOverlay) InTree() bool { return o.mode == hivesModeTree }

// CloseTree returns to the hives list. The app has already resolved staged
// edits (applied, discarded, or none).
func (o HivesOverlay) CloseTree() HivesOverlay {
	if o.pending {
		return o
	}
	o.mode = hivesModeList
	o.tree.screen = hivesTreeBrowse
	o.tree.filter = ""
	return o
}

// SetTree records a projection. The cursor stays on the same node key, or its
// nearest surviving ancestor, so refresh and every edit keep focus.
func (o HivesOverlay) SetTree(view HiveTreeView) HivesOverlay {
	o.tree.loading = false
	o.tree.view = view
	if o.tree.expanded == nil {
		o.tree.expanded = map[string]bool{}
		for _, n := range view.Nodes {
			if n.Kind == HiveTreeHub {
				o.tree.expanded[n.Key] = true
			}
		}
	}
	o.tree.cursor = o.treeFocus(o.tree.cursor)
	return o
}

// SetTreeError renders a refusal or failure without leaving the tree.
func (o HivesOverlay) SetTreeError(err string) HivesOverlay {
	o.pending, o.pendingWhat = false, ""
	o.tree.loading = false
	o.tree.err, o.tree.note = err, ""
	return o
}

// SetTreeNote renders an informational line.
func (o HivesOverlay) SetTreeNote(note string) HivesOverlay {
	o.tree.err, o.tree.note = "", note
	return o
}

// TreeStaged is how many staged changes Apply would save.
func (o HivesOverlay) TreeStaged() int { return o.tree.view.Staged }

// TreeStale reports a review whose profiles changed underneath it.
func (o HivesOverlay) TreeStale() bool { return o.tree.view.Stale }

// TreeLoading reports whether a tree read is outstanding.
func (o HivesOverlay) TreeLoading() bool { return o.tree.loading }

func (o HivesOverlay) TreeSearching() bool  { return o.tree.screen == hivesTreeSearch }
func (o HivesOverlay) TreeConfirming() bool { return o.tree.screen == hivesTreeConfirm }
func (o HivesOverlay) TreeReviewing() bool  { return o.tree.screen == hivesTreeReview }
func (o HivesOverlay) TreeReceipt() bool    { return o.tree.screen == hivesTreeReceipt }

// TreeDiscarding reports the open discard confirmation and where accepting it
// leads.
func (o HivesOverlay) TreeDiscarding() (HivesTreeLeave, bool) {
	return o.tree.leave, o.tree.screen == hivesTreeDiscard
}

// TreeCurrent returns the focused node.
func (o HivesOverlay) TreeCurrent() (HiveTreeNode, bool) {
	if o.tree.loading {
		return HiveTreeNode{}, false
	}
	for _, n := range o.tree.view.Nodes {
		if n.Key == o.tree.cursor {
			return n, true
		}
	}
	return HiveTreeNode{}, false
}

func (o HivesOverlay) treeNode(key string) (HiveTreeNode, bool) {
	for _, n := range o.tree.view.Nodes {
		if n.Key == key {
			return n, true
		}
	}
	return HiveTreeNode{}, false
}

func (o HivesOverlay) hasChildren(key string) bool {
	for _, n := range o.tree.view.Nodes {
		if n.Parent == key {
			return true
		}
	}
	return false
}

// visible lists the drawn nodes. A search shows matches with their ancestry
// regardless of expansion; otherwise every ancestor must be expanded.
func (o HivesOverlay) visible() []HiveTreeNode {
	nodes := o.tree.view.Nodes
	var out []HiveTreeNode
	if q := strings.ToLower(strings.TrimSpace(o.tree.filter)); q != "" {
		keep := map[string]bool{}
		for _, n := range nodes {
			if strings.Contains(strings.ToLower(n.Short), q) || strings.Contains(strings.ToLower(n.Identity), q) {
				for key := n.Key; key != ""; {
					keep[key] = true
					parent, _ := o.treeNode(key)
					key = parent.Parent
				}
			}
		}
		for _, n := range nodes {
			if keep[n.Key] {
				out = append(out, n)
			}
		}
		return out
	}
	for _, n := range nodes {
		shown := true
		for parent := n.Parent; parent != ""; {
			if !o.tree.expanded[parent] {
				shown = false
				break
			}
			p, _ := o.treeNode(parent)
			parent = p.Parent
		}
		if shown {
			out = append(out, n)
		}
	}
	return out
}

// treeFocus resolves key to a visible node: itself, its nearest visible
// ancestor, or the first visible row.
func (o HivesOverlay) treeFocus(key string) string {
	visible := o.visible()
	shown := map[string]bool{}
	for _, n := range visible {
		shown[n.Key] = true
	}
	for k := key; k != ""; {
		if shown[k] {
			return k
		}
		n, ok := o.treeNode(k)
		if !ok {
			break
		}
		k = n.Parent
	}
	if len(visible) > 0 {
		return visible[0].Key
	}
	return ""
}

func (o HivesOverlay) treeBrowsing() bool {
	return o.mode == hivesModeTree && !o.pending && !o.tree.loading && o.tree.screen == hivesTreeBrowse
}

// TreeMove steps the focus through the visible rows.
func (o HivesOverlay) TreeMove(delta int) HivesOverlay {
	if !o.treeBrowsing() {
		return o
	}
	visible := o.visible()
	if len(visible) == 0 {
		return o
	}
	i := 0
	for j, n := range visible {
		if n.Key == o.tree.cursor {
			i = j
		}
	}
	i = min(max(i+delta, 0), len(visible)-1)
	o.tree.cursor = visible[i].Key
	return o
}

// TreeExpand expands (true) or collapses (false) the focused branch. Collapsing
// a leaf moves focus to its parent.
func (o HivesOverlay) TreeExpand(expand bool) HivesOverlay {
	if !o.treeBrowsing() {
		return o
	}
	n, ok := o.TreeCurrent()
	if !ok {
		return o
	}
	if !expand && (!o.hasChildren(n.Key) || !o.tree.expanded[n.Key]) {
		if n.Parent != "" {
			o.tree.cursor = n.Parent
		}
		return o
	}
	if !o.hasChildren(n.Key) {
		return o
	}
	expanded := make(map[string]bool, len(o.tree.expanded)+1)
	for k, v := range o.tree.expanded {
		expanded[k] = v
	}
	expanded[n.Key] = expand
	o.tree.expanded = expanded
	return o
}

// TreeToggleExpand flips the focused branch.
func (o HivesOverlay) TreeToggleExpand() HivesOverlay {
	n, ok := o.TreeCurrent()
	if !ok {
		return o
	}
	return o.TreeExpand(!o.tree.expanded[n.Key])
}

func (o HivesOverlay) subscriptionOf(n HiveTreeNode) (HiveTreeNode, bool) {
	if n.Kind == HiveTreeHive {
		return n, true
	}
	if n.Kind == HiveTreeRepo {
		return o.treeNode(n.Parent)
	}
	return HiveTreeNode{}, false
}

// confirmBulk opens the affected-counts prompt; nothing is staged until the
// operator accepts it.
func (o HivesOverlay) confirmBulk(action HivesTreeAction, what string, n HiveTreeNode) (HivesOverlay, HivesTreeAction, bool) {
	o.tree.screen = hivesTreeConfirm
	o.tree.confirm = action
	o.tree.prompt = fmt.Sprintf("%s %s?\nAffects %d %s and %d %s.",
		what, n.Short, n.Subscriptions, plural(n.Subscriptions, "subscription", "subscriptions"),
		n.Repositories, plural(n.Repositories, "repository", "repositories"))
	o.tree.err, o.tree.note = "", ""
	return o, HivesTreeAction{}, false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// TreeToggleSelect is space: a repository toggles directly; a hive or hub is a
// bulk branch selection and asks first.
func (o HivesOverlay) TreeToggleSelect() (HivesOverlay, HivesTreeAction, bool) {
	n, ok := o.TreeCurrent()
	if !o.treeBrowsing() || !ok {
		return o, HivesTreeAction{}, false
	}
	o.tree.err, o.tree.note = "", ""
	if n.Kind == HiveTreeRepo {
		return o, HivesTreeAction{Kind: HivesTreeSelect, Target: n.Subscription, Repository: n.Repository, Value: n.Selection != "x"}, true
	}
	value := n.Selection != "x"
	verb := "Select all current repositories in"
	if !value {
		verb = "Deselect all current repositories in"
	}
	if n.Kind == HiveTreeHub {
		return o.confirmBulk(HivesTreeAction{Kind: HivesTreeSelectBranch, Target: n.Hub, Hub: true, Value: value}, verb, n)
	}
	return o.confirmBulk(HivesTreeAction{Kind: HivesTreeSelectBranch, Target: n.Subscription, Value: value}, verb, n)
}

// TreeToggleExclude is x: exclude a repository everywhere, or clear this
// subscription's exclusion.
func (o HivesOverlay) TreeToggleExclude() (HivesOverlay, HivesTreeAction, bool) {
	n, ok := o.TreeCurrent()
	if !o.treeBrowsing() || !ok {
		return o, HivesTreeAction{}, false
	}
	if n.Kind != HiveTreeRepo {
		return o.SetTreeNote("x excludes a repository; focus one first"), HivesTreeAction{}, false
	}
	return o, HivesTreeAction{Kind: HivesTreeExclude, Target: n.Subscription, Repository: n.Repository, Value: !n.Excluded}, true
}

// TreeToggleFollow is f, for the focused (or enclosing) subscription.
func (o HivesOverlay) TreeToggleFollow() (HivesOverlay, HivesTreeAction, bool) {
	n, ok := o.TreeCurrent()
	if !o.treeBrowsing() || !ok {
		return o, HivesTreeAction{}, false
	}
	sub, ok := o.subscriptionOf(n)
	if !ok {
		return o.SetTreeNote("f follows new repositories for a Hive subscription; focus one first"), HivesTreeAction{}, false
	}
	return o, HivesTreeAction{Kind: HivesTreeFollow, Target: sub.Subscription, Value: sub.Follow != "following"}, true
}

// TreeToggleEnabled is e: a subscription toggles directly; a hub fans out to
// every subscription it lists and asks first.
func (o HivesOverlay) TreeToggleEnabled() (HivesOverlay, HivesTreeAction, bool) {
	n, ok := o.TreeCurrent()
	if !o.treeBrowsing() || !ok {
		return o, HivesTreeAction{}, false
	}
	o.tree.err, o.tree.note = "", ""
	if n.Kind == HiveTreeHub {
		verb := "Disable every subscription under"
		if !n.Enabled {
			verb = "Enable every subscription under"
		}
		return o.confirmBulk(HivesTreeAction{Kind: HivesTreeHubEnable, Target: n.Hub, Hub: true, Value: !n.Enabled}, verb, n)
	}
	sub, _ := o.subscriptionOf(n)
	return o, HivesTreeAction{Kind: HivesTreeEnable, Target: sub.Subscription, Value: !sub.Enabled}, true
}

// TreeRestrict is R: review-and-restrict for an Unrestricted — legacy
// subscription.
func (o HivesOverlay) TreeRestrict() (HivesOverlay, HivesTreeAction, bool) {
	n, ok := o.TreeCurrent()
	if !o.treeBrowsing() || !ok {
		return o, HivesTreeAction{}, false
	}
	sub, ok := o.subscriptionOf(n)
	if !ok || !sub.Legacy {
		return o.SetTreeNote("R reviews and restricts an Unrestricted — legacy subscription; focus one first"), HivesTreeAction{}, false
	}
	return o.confirmBulk(HivesTreeAction{Kind: HivesTreeRestrict, Target: sub.Subscription}, "Review and restrict", sub)
}

// TreeConfirm accepts the bulk prompt and returns its action.
func (o HivesOverlay) TreeConfirm() (HivesOverlay, HivesTreeAction, bool) {
	if o.mode != hivesModeTree || o.pending || o.tree.screen != hivesTreeConfirm {
		return o, HivesTreeAction{}, false
	}
	action := o.tree.confirm
	o.tree.screen = hivesTreeBrowse
	o.tree.prompt = ""
	return o, action, true
}

// TreeCancel backs out of a prompt, review, receipt or search to browsing.
func (o HivesOverlay) TreeCancel() HivesOverlay {
	if o.pending || o.mode != hivesModeTree {
		return o
	}
	if o.tree.screen == hivesTreeSearch {
		o.tree.filter = ""
	}
	o.tree.screen = hivesTreeBrowse
	o.tree.prompt = ""
	o.tree.cursor = o.treeFocus(o.tree.cursor)
	return o
}

// TreeBeginSearch opens the / filter.
func (o HivesOverlay) TreeBeginSearch() HivesOverlay {
	if !o.treeBrowsing() {
		return o
	}
	o.tree.screen = hivesTreeSearch
	return o
}

// TreeEndSearch keeps the filter and returns to browsing it.
func (o HivesOverlay) TreeEndSearch() HivesOverlay {
	if o.tree.screen != hivesTreeSearch {
		return o
	}
	o.tree.screen = hivesTreeBrowse
	o.tree.cursor = o.treeFocus(o.tree.cursor)
	return o
}

// TreeType appends to the search filter.
func (o HivesOverlay) TreeType(text string) HivesOverlay {
	if o.tree.screen != hivesTreeSearch {
		return o
	}
	o.tree.filter += text
	o.tree.cursor = o.treeFocus(o.tree.cursor)
	return o
}

// TreeBackspace removes the filter's last rune.
func (o HivesOverlay) TreeBackspace() HivesOverlay {
	if o.tree.screen != hivesTreeSearch || o.tree.filter == "" {
		return o
	}
	r := []rune(o.tree.filter)
	o.tree.filter = string(r[:len(r)-1])
	o.tree.cursor = o.treeFocus(o.tree.cursor)
	return o
}

// TreeBeginApply opens the staged diff for review. A stale review cannot be
// applied: it must be discarded and reviewed again.
func (o HivesOverlay) TreeBeginApply() HivesOverlay {
	if !o.treeBrowsing() {
		return o
	}
	if o.tree.view.Stale {
		return o.SetTreeError("profiles changed in another hivectl, TUI or web session; this review is stale — D discards it and reloads for a renewed review")
	}
	if o.tree.view.Staged == 0 {
		return o.SetTreeNote("nothing staged; nothing to apply")
	}
	o.tree.screen = hivesTreeReview
	o.tree.reviewOffset = 0
	o.tree.err, o.tree.note = "", ""
	return o
}

// TreeStartApply marks Apply in flight once the reviewed diff is accepted.
func (o HivesOverlay) TreeStartApply() (HivesOverlay, bool) {
	if o.mode != hivesModeTree || o.pending || o.tree.screen != hivesTreeReview {
		return o, false
	}
	return o.start("Applying staged edits"), true
}

// SetTreeReceipt shows the three-part Apply receipt.
func (o HivesOverlay) SetTreeReceipt(lines []string, err string) HivesOverlay {
	o.pending, o.pendingWhat = false, ""
	o.tree.screen = hivesTreeReceipt
	o.tree.receipt = append([]string(nil), lines...)
	o.tree.err, o.tree.note = err, ""
	return o
}

// TreeBeginLeave asks before discarding unapplied edits. leaveNow is true when
// nothing is staged and the caller may leave at once.
func (o HivesOverlay) TreeBeginLeave(leave HivesTreeLeave) (HivesOverlay, bool) {
	if o.pending || o.mode != hivesModeTree {
		return o, false
	}
	if o.tree.view.Staged == 0 && !o.tree.view.Stale && leave != HivesTreeStay {
		return o, true
	}
	o.tree.screen = hivesTreeDiscard
	o.tree.leave = leave
	o.tree.err, o.tree.note = "", ""
	return o, false
}

func (o HivesOverlay) treeBody(width int) string {
	if o.tree.loading {
		return "Reading the contribution tree…"
	}
	t := o.tree
	var out strings.Builder
	out.WriteString(truncateEnd("Effective scope: "+t.view.Scope, width) + "\n")
	staged := "Staged: none"
	if t.view.Staged > 0 {
		staged = fmt.Sprintf("Staged: %d unapplied — A apply · D discard", t.view.Staged)
	}
	out.WriteString(truncateEnd(staged, width))
	if t.view.Stale {
		out.WriteString("\n" + hivesErrorStyle.Render(truncateEnd("STALE: profiles changed elsewhere — D discards and reviews again", width)))
	}
	out.WriteString("\n")
	switch t.screen {
	case hivesTreeReceipt:
		out.WriteString("\n" + hivesTitleStyle.Render("Apply receipt") + "\n")
		for _, line := range t.receipt {
			out.WriteString(truncateEnd(line, width) + "\n")
		}
		if t.err != "" {
			out.WriteString(hivesErrorStyle.Render(t.err) + "\n")
		}
		return strings.TrimRight(out.String(), "\n")
	case hivesTreeDiscard:
		what := fmt.Sprintf("Discard %d unapplied %s?", t.view.Staged, plural(t.view.Staged, "edit", "edits"))
		if t.view.Stale {
			what = "Discard this stale review and reload?"
		}
		out.WriteString("\n" + hivesErrorStyle.Render(what) + "\n")
		out.WriteString("Nothing has been written. y discard · n/esc keep reviewing")
		return out.String()
	}
	out.WriteString(o.treeRows(width))
	if t.screen == hivesTreeConfirm {
		out.WriteString("\n" + hivesWarningStyle.Render(t.prompt) + "\nNothing is written until Apply. enter/y stage · n/esc cancel")
		return out.String()
	}
	if n, ok := o.TreeCurrent(); ok {
		out.WriteString(hivesRuleStyle.Render(strings.Repeat("─", width)) + "\n")
		out.WriteString(truncateEnd("Identity: "+n.Identity, width) + "\n")
		for _, line := range []string{
			"Provenance: " + n.Provenance,
			"Selection: " + n.Explanation,
			"Exclusion source: " + n.ExclusionSource,
			"Follow: " + n.Follow + " · Readiness: " + n.Readiness + " · Activity: " + n.Activity,
		} {
			out.WriteString(truncateEnd(line, width) + "\n")
		}
		if len(n.Badges) > 0 {
			out.WriteString(truncateEnd("Badges: "+strings.Join(n.Badges, ", "), width) + "\n")
		}
	}
	if t.screen == hivesTreeSearch || t.filter != "" {
		out.WriteString("Search: [" + t.filter + "]\n")
	}
	if t.err != "" {
		out.WriteString(hivesErrorStyle.Render(t.err) + "\n")
	}
	if t.note != "" {
		out.WriteString(hivesNoteStyle.Render(t.note) + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// hivesTreeVisibleRows bounds the tree so the detail pane, scope and Apply hint
// stay on screen however many repositories a Hive offers.
const hivesTreeVisibleRows = 10

// hivesTreeWide is the content width from which the follow, readiness and
// activity columns are drawn. Narrower terminals keep the name, selection and
// badges; the detail pane still names identity and effective selection.
const hivesTreeWide = 84

func (o HivesOverlay) treeRows(width int) string {
	visible := o.visible()
	if len(visible) == 0 {
		if o.tree.filter != "" {
			return "no matches\n"
		}
		return "no subscriptions — add a hive from the list first\n"
	}
	wide := width >= hivesTreeWide
	nameWidth := 24
	if !wide {
		nameWidth = max(8, width/2)
	}
	const followW, readyW, activityW = 13, 12, 11
	badgeWidth := width - 2 - 4 - nameWidth - 1
	if wide {
		badgeWidth -= followW + readyW + activityW + 3
	}
	badgeWidth = max(0, badgeWidth)
	var out strings.Builder
	header := "  SEL " + fmt.Sprintf("%-*s %-*s", nameWidth, "NAME", badgeWidth, "BADGES")
	if wide {
		header += fmt.Sprintf(" %-*s %-*s %-*s", followW, "FOLLOW", readyW, "READINESS", activityW, "ACTIVITY")
	}
	out.WriteString(hivesHeaderStyle.Render(truncateEnd(header, width)) + "\n")
	start := 0
	for i, n := range visible {
		if n.Key == o.tree.cursor {
			start = i - hivesTreeVisibleRows/2
		}
	}
	start = max(0, min(start, len(visible)-hivesTreeVisibleRows))
	end := min(len(visible), start+hivesTreeVisibleRows)
	for _, n := range visible[start:end] {
		cursor := "  "
		if n.Key == o.tree.cursor {
			cursor = "▸ "
		}
		expander := "  "
		if o.hasChildren(n.Key) {
			expander = "+ "
			if o.tree.expanded[n.Key] || o.tree.filter != "" {
				expander = "- "
			}
		}
		name := strings.Repeat("  ", n.Depth) + expander + n.Short
		line := cursor + "[" + n.Selection + "] " +
			fmt.Sprintf("%-*s %-*s", nameWidth, truncateEnd(name, nameWidth), badgeWidth, truncateEnd(strings.Join(n.Badges, ", "), badgeWidth))
		if wide {
			line += fmt.Sprintf(" %-*s %-*s %-*s",
				followW, truncateEnd(n.Follow, followW),
				readyW, truncateEnd(n.Readiness, readyW),
				activityW, truncateEnd(n.Activity, activityW))
		}
		line = truncateEnd(line, width)
		if pad := width - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		if n.Key == o.tree.cursor {
			line = hivesSelectedStyle.Render(line)
		}
		out.WriteString(line + "\n")
	}
	if len(visible) > end-start {
		out.WriteString(hivesNoteStyle.Render(fmt.Sprintf("%d–%d of %d rows", start+1, end, len(visible))) + "\n")
	}
	return out.String()
}

func (o HivesOverlay) treeFooter() string {
	switch {
	case o.pending:
		return "working…"
	case o.tree.loading:
		return "esc back"
	}
	switch o.tree.screen {
	case hivesTreeSearch:
		return "type to filter  enter keep filter  esc clear"
	case hivesTreeConfirm:
		return "enter/y stage  n/esc cancel"
	case hivesTreeReview:
		return "enter apply  esc back"
	case hivesTreeReceipt:
		return "enter/esc continue"
	case hivesTreeDiscard:
		return "y discard  n/esc cancel"
	}
	back := "esc back"
	if o.escQuits {
		back = "esc back  q quit"
	}
	apply := "A apply"
	if o.tree.view.Staged > 0 {
		apply = fmt.Sprintf("A apply (%d staged)", o.tree.view.Staged)
	}
	return apply + "  D discard  space select  x exclude  f follow  e enable  R restrict\n" +
		"j/k move  enter/l/h expand  / search  ctrl+r refresh  " + back
}

// Six fixed context/action rows (title, notice, scope, position, two hints) and
// two margin rows surround the viewport.
const hivesTreeReviewReservedRows = 8

// The review needs room for its pinned context and enough diff rows to read:
// at 12 rows the viewport is 4 rows (at 10 it was 2, i.e. 60 pages for 61 changes).
const (
	hivesTreeReviewMinWidth  = 40
	hivesTreeReviewMinHeight = 12
)

// TreeReviewFits requires room for confirmation context and a readable diff
// window. Enter must not apply when a resize has hidden the review.
func (o HivesOverlay) TreeReviewFits(width, height int) bool {
	return width >= hivesTreeReviewMinWidth && height >= hivesTreeReviewMinHeight
}

// treeReviewLines wraps only the diff; the scope is pinned above the viewport.
func (o HivesOverlay) treeReviewLines(width int) []string {
	// Hard wrapping retains every character of long canonical identities,
	// including wide/combining Unicode, without inserting ellipses.
	return strings.Split(ansi.Hardwrap(strings.Join(o.tree.view.Diff, "\n"), max(1, width-4), true), "\n")
}

// TreeReviewScroll moves in physical wrapped rows using the current terminal
// dimensions, so page size and end bounds follow resize without mutating edits.
func (o HivesOverlay) TreeReviewScroll(key string, width, height int) HivesOverlay {
	if !o.TreeReviewing() || o.pending || !o.TreeReviewFits(width, height) {
		return o
	}
	page := height - hivesTreeReviewReservedRows
	last := max(0, len(o.treeReviewLines(width))-page)
	o.tree.reviewOffset = min(o.tree.reviewOffset, last)
	switch key {
	case "down", "j":
		o.tree.reviewOffset++
	case "up", "k":
		o.tree.reviewOffset--
	case "pgdown":
		o.tree.reviewOffset += page
	case "pgup":
		o.tree.reviewOffset -= page
	case "home":
		o.tree.reviewOffset = 0
	case "end":
		o.tree.reviewOffset = last
	}
	o.tree.reviewOffset = max(0, min(o.tree.reviewOffset, last))
	return o
}

func (o HivesOverlay) treeReviewView(width, height int) string {
	if !o.TreeReviewFits(width, height) {
		// No enabled Apply hint on a screen too small to show its context. No
		// trailing punctuation: it would wrap onto a line of its own.
		lines := strings.Split(ansi.Hardwrap(fmt.Sprintf("Enter disabled; Esc back\nReview paused: resize to %dx%d\nStaged edits kept", hivesTreeReviewMinWidth, hivesTreeReviewMinHeight), max(1, width), true), "\n")
		return strings.Join(lines[:min(len(lines), max(0, height))], "\n")
	}
	lines := o.treeReviewLines(width)
	page := height - hivesTreeReviewReservedRows
	start := max(0, min(o.tree.reviewOffset, len(lines)-page))
	end := min(len(lines), start+page)
	body := append([]string(nil), lines[start:end]...)
	for len(body) < page {
		body = append(body, "")
	}
	// The scope is a summary, not an identity, so it is truncated rather than wrapped.
	scope := ansi.Truncate("Effective scope: "+o.tree.view.Scope, width, "…")
	// "Lines" are wrapped screen lines; the title counts changes.
	return fmt.Sprintf("%s\nNothing has been written yet.\n%s\n%s\nLines %d-%d of %d\n↑/↓ j/k scroll · PgUp/PgDn · Home/End\nEnter apply · Esc back (keep edits)",
		hivesTitleStyle.Render(fmt.Sprintf("Review staged changes (%d)", o.tree.view.Staged)), scope, strings.Join(body, "\n"), start+1, end, len(lines))
}
