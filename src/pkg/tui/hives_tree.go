package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// The contribution tree's app-side half (#13291): the Hub → Hive → Repository
// tree of #13180 inside the Hives overlay, edited through the staging layer of
// #13289. Like `hivectl hives web`, it delegates the tree, diff and receipt to
// hivectl.StagedEdits rather than deriving its own, so both surfaces show the
// same diff and receipt for the same edits. Staged edits live only in memory:
// the single write is StagedEdits.Apply, after the operator reviewed the diff.

// hivesTreeDiscoveryTimeout bounds one refresh of hub and repository discovery.
const hivesTreeDiscoveryTimeout = 10 * time.Second

// hivesTreeDiscoverer supplies discovery snapshots for a loaded profile set.
type hivesTreeDiscoverer func(ctx context.Context, set *hivectl.ProfileSet) ([]hivectl.TreeHubSnapshot, map[string]hivectl.TreeRepositorySnapshot)

// defaultHivesTreeDiscovery is the same public, credential-free discovery the
// Web tree uses.
func defaultHivesTreeDiscovery(ctx context.Context, set *hivectl.ProfileSet) ([]hivectl.TreeHubSnapshot, map[string]hivectl.TreeRepositorySnapshot) {
	return hivectl.DiscoverContributionTree(ctx, set, hivesTreeDiscoveryTimeout)
}

// hivesTreeSession is one staged review. set is the profile set the review was
// opened on, read only for provenance (presets carry no credentials and are
// never projected beyond their ID and reviewed revision).
type hivesTreeSession struct {
	edits             *hivectl.StagedEdits
	set               *hivectl.ProfileSet
	revision          string
	stale             bool
	projectionPending bool
}

func (s *hivesTreeSession) staged() bool {
	return s != nil && !s.edits.Diff().Empty()
}

type (
	// hivesTreeLoadedMsg is a tree read. A refresh with staged edits returns
	// the same session (stale set when the profiles changed underneath it).
	hivesTreeLoadedMsg struct {
		overlayID uint64
		session   *hivesTreeSession
		stale     bool
		note      string
		err       error
	}

	// hivesTreeAppliedMsg is a completed Apply.
	hivesTreeAppliedMsg struct {
		overlayID uint64
		receipt   hivectl.ApplyReceipt
		err       error
	}
)

var errHivesTreeStore = errors.New("the contribution tree needs the shared hivectl profile store")

func hivesProfileStore(env hivesEnv) (*hivectl.ProfileStore, error) {
	if env.store == nil {
		return nil, errNoHivesStore
	}
	store, ok := env.store.(*hivectl.ProfileStore)
	if !ok {
		return nil, errHivesTreeStore
	}
	return store, nil
}

// hivesReviewSet reads without migrating contributor.env or rewriting consent,
// exactly as the Web tree does: opening a review is not a first use.
func hivesReviewSet(store *hivectl.ProfileStore) (*hivectl.ProfileSet, error) {
	set, err := store.LoadForReview()
	if errors.Is(err, hivectl.ErrNoProfiles) || err == nil && set == nil {
		return &hivectl.ProfileSet{Version: hivectl.ProfilesVersion}, nil
	}
	if err != nil {
		return nil, errors.New("cannot read contribution profiles")
	}
	return set, nil
}

// loadHivesTree opens a review, or refreshes keep. A refresh never drops
// staged edits: with edits staged it only checks whether the profiles changed
// underneath them, and new discovery waits for Apply or Discard.
func (m model) loadHivesTree(overlayID uint64, keep *hivesTreeSession) tea.Cmd {
	env := m.hivesEnv
	keepEdits := keep != nil && (keep.staged() || keep.projectionPending || keep.stale)
	return func() tea.Msg {
		store, err := hivesProfileStore(env)
		if err != nil {
			return hivesTreeLoadedMsg{overlayID: overlayID, err: err}
		}
		set, err := hivesReviewSet(store)
		if err != nil {
			return hivesTreeLoadedMsg{overlayID: overlayID, err: err}
		}
		if keepEdits {
			stale := keep.stale || !keep.projectionPending && set.Revision() != keep.revision
			return hivesTreeLoadedMsg{overlayID: overlayID, session: keep, stale: stale,
				note: "Staged edits preserved. Apply or Discard before discovering new membership."}
		}
		hubs, repos := hivectl.SavedTreeHubs(set), map[string]hivectl.TreeRepositorySnapshot(nil)
		if env.discoverTree != nil {
			hubs, repos = env.discoverTree(context.Background(), set)
		}
		edits, err := hivectl.NewStagedEdits(set, hubs, repos)
		if err != nil {
			return hivesTreeLoadedMsg{overlayID: overlayID, err: errors.New("cannot construct contribution review")}
		}
		return hivesTreeLoadedMsg{overlayID: overlayID, session: &hivesTreeSession{edits: edits, set: set, revision: set.Revision()}}
	}
}

func (m model) handleHivesTreeLoaded(msg hivesTreeLoadedMsg) (tea.Model, tea.Cmd) {
	if m.hives == nil || m.hivesID != msg.overlayID || !m.hives.InTree() {
		return m, nil
	}
	if msg.err != nil {
		next := m.hives.SetTreeError(msg.err.Error())
		m.hives = &next
		return m, nil
	}
	m.hivesTree = msg.session
	if msg.stale {
		m.hivesTree.stale = true
	}
	next := m.hives.SetTree(hivesTreeView(m.hivesTree))
	if msg.note != "" {
		next = next.SetTreeNote(msg.note)
	}
	m.hives = &next
	return m, nil
}

// runHivesTreeApply re-checks the revision (as the Web tree does) and applies
// the reviewed diff through the shared layer, which owns the guarded write,
// the relay signal and the receipt.
func (m model) runHivesTreeApply(overlayID uint64, session *hivesTreeSession) tea.Cmd {
	env := m.hivesEnv
	return func() tea.Msg {
		store, err := hivesProfileStore(env)
		if err != nil {
			return hivesTreeAppliedMsg{overlayID: overlayID, receipt: hivectl.ApplyReceipt{Saved: hivectl.StageNotSaved, RelayReload: hivectl.StageReloadNotRequested, Acknowledged: "unknown"}, err: err}
		}
		if !session.projectionPending {
			set, err := hivesReviewSet(store)
			if err == nil && set.Revision() != session.revision {
				err = hivectl.ErrStaleRevision
			}
			if err != nil {
				return hivesTreeAppliedMsg{overlayID: overlayID, receipt: hivectl.ApplyReceipt{Saved: hivectl.StageNotSaved, RelayReload: hivectl.StageReloadNotRequested, Acknowledged: "unknown"}, err: err}
			}
		}
		receipt, err := session.edits.Apply(context.Background(), store)
		return hivesTreeAppliedMsg{overlayID: overlayID, receipt: receipt, err: err}
	}
}

func (m model) handleHivesTreeApplied(msg hivesTreeAppliedMsg) (tea.Model, tea.Cmd) {
	if m.hives == nil || m.hivesID != msg.overlayID || !m.hives.InTree() || m.hivesTree == nil {
		return m, nil
	}
	session := m.hivesTree
	session.projectionPending = msg.receipt.Saved == hivectl.StageProfilesSaved
	if errors.Is(msg.err, hivectl.ErrStaleRevision) {
		session.stale = true
	}
	errText := ""
	if msg.err != nil {
		errText = hivesTreeErrorText(msg.err)
	}
	next := m.hives.SetTree(hivesTreeView(session))
	next = next.SetTreeReceipt(hivesTreeReceiptLines(msg.receipt), errText)
	m.hives = &next
	if msg.receipt.Saved == hivectl.StageSaved || msg.receipt.Saved == hivectl.StageUnchanged {
		// The applied review is spent; the next one starts from the saved file.
		next = next.BeginTreeRefresh()
		m.hives = &next
		return m, m.loadHivesTree(msg.overlayID, nil)
	}
	return m, nil
}

// hivesTreeErrorText names only the shared layer's safe errors; anything else
// may embed a path or credential and is replaced, as on the Web tree.
func hivesTreeErrorText(err error) string {
	if errors.Is(err, hivectl.ErrStaleRevision) {
		return "profiles changed; discard this stale review and review again before Apply"
	}
	if errors.Is(err, errNoHivesStore) || errors.Is(err, errHivesTreeStore) {
		return err.Error()
	}
	for _, safe := range []error{hivectl.ErrStageIncomplete, hivectl.ErrStageLegacy, hivectl.ErrStageSave, hivectl.ErrStageProjection, hivectl.ErrStageReload} {
		if errors.Is(err, safe) {
			return safe.Error()
		}
	}
	return "cannot stage contribution edit; review the target and try again"
}

// hivesTreeReceiptLines is the three-part receipt, worded as on the Web tree.
func hivesTreeReceiptLines(r hivectl.ApplyReceipt) []string {
	return []string{
		"Saved: " + r.Saved,
		"Relay reload: " + r.RelayReload,
		"Acknowledged: " + r.Acknowledged,
		"Change saved does not prove relay adoption. In-flight tasks finish under their original lease.",
	}
}

// stageHivesTree performs one staged edit in memory. Nothing is written.
func (m model) stageHivesTree(action panes.HivesTreeAction) (tea.Model, tea.Cmd) {
	session := m.hivesTree
	if session == nil {
		return m, nil
	}
	edits := session.edits
	var err error
	switch {
	case session.stale:
		err = hivectl.ErrStaleRevision
	case action.Kind == panes.HivesTreeSelect:
		err = edits.SelectRepository(action.Target, action.Repository, action.Value)
	case action.Kind == panes.HivesTreeSelectBranch:
		err = edits.SelectCurrent(action.Target, action.Hub, action.Value)
	case action.Kind == panes.HivesTreeExclude:
		err = edits.SetExcluded(action.Target, action.Repository, action.Value)
	case action.Kind == panes.HivesTreeFollow:
		err = edits.SetFollow(action.Target, action.Value)
	case action.Kind == panes.HivesTreeEnable:
		err = edits.SetHiveEnabled(action.Target, action.Value)
	case action.Kind == panes.HivesTreeHubEnable:
		err = edits.SetHubEnabled(action.Target, action.Value)
	case action.Kind == panes.HivesTreeRestrict:
		err = edits.ReviewAndRestrict(action.Target)
	default:
		err = errors.New("invalid contribution operation")
	}
	next := m.hives.SetTree(hivesTreeView(session))
	if err != nil {
		next = next.SetTreeError(hivesTreeErrorText(err))
	} else {
		next = next.SetTreeNote("Staged. Nothing is written until A apply.")
	}
	m.hives = &next
	return m, nil
}

// leaveHivesTree finishes leaving once staged edits are resolved.
func (m model) leaveHivesTree(leave panes.HivesTreeLeave) (tea.Model, tea.Cmd) {
	switch leave {
	case panes.HivesTreeQuit:
		return m.stopSSE(), tea.Quit
	case panes.HivesTreeClose:
		m.hivesTree = nil
		next := m.hives.CloseTree()
		m.hives = &next
		return m, m.loadHives(m.hivesID)
	}
	// Discard and stay: a fresh review, so a stale one is renewed rather than
	// silently rebased.
	next := m.hives.TreeCancel()
	if m.hivesTree != nil {
		next = next.SetTree(hivesTreeView(m.hivesTree))
	}
	next = next.SetTreeNote("Discarded; nothing was written.")
	m.hives = &next
	m.hivesTree = nil
	return m, m.loadHivesTree(m.hivesID, nil)
}

// updateHivesTree consumes every key while the tree is showing; see
// updateHives for why no key may fall through.
func (m model) updateHivesTree(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	o := *m.hives
	k := msg.String()

	if o.TreeSearching() {
		switch {
		case k == "esc":
			o = o.TreeCancel()
		case k == "enter":
			o = o.TreeEndSearch()
		case k == "backspace":
			o = o.TreeBackspace()
		case msg.Type == tea.KeySpace:
			o = o.TreeType(" ")
		case msg.Type == tea.KeyRunes && !msg.Alt:
			o = o.TreeType(string(msg.Runes))
		}
		m.hives = &o
		return m, nil
	}
	if o.TreeConfirming() {
		switch k {
		case "enter", "y":
			next, action, ok := o.TreeConfirm()
			m.hives = &next
			if ok {
				return m.stageHivesTree(action)
			}
		case "n", "esc":
			o = o.TreeCancel()
			m.hives = &o
		}
		return m, nil
	}
	if o.TreeReviewing() {
		switch k {
		case "enter":
			if !o.TreeReviewFits(m.width, m.height) {
				return m, nil
			}
			next, ok := o.TreeStartApply()
			m.hives = &next
			if ok && m.hivesTree != nil {
				return m, m.runHivesTreeApply(m.hivesID, m.hivesTree)
			}
		case "esc":
			o = o.TreeCancel()
			m.hives = &o
		default:
			o = o.TreeReviewScroll(k, m.width, m.height)
			m.hives = &o
		}
		return m, nil
	}
	if o.TreeReceipt() {
		if k == "enter" || k == "esc" {
			o = o.TreeCancel()
			m.hives = &o
		}
		return m, nil
	}
	if leave, ok := o.TreeDiscarding(); ok {
		switch k {
		case "y":
			if m.hivesTree != nil {
				if err := m.hivesTree.edits.Discard(); err != nil {
					o = o.TreeCancel().SetTreeError(hivesTreeErrorText(err))
					m.hives = &o
					return m, nil
				}
			}
			return m.leaveHivesTree(leave)
		case "n", "esc":
			o = o.TreeCancel()
			m.hives = &o
		}
		return m, nil
	}

	var (
		action panes.HivesTreeAction
		staged bool
	)
	switch k {
	case "esc", "q":
		leave := panes.HivesTreeClose
		if k == "q" {
			if !m.hivesOnly {
				return m, nil
			}
			leave = panes.HivesTreeQuit
		}
		next, now := o.TreeBeginLeave(leave)
		m.hives = &next
		if now {
			return m.leaveHivesTree(leave)
		}
		return m, nil
	case "j", "down":
		o = o.TreeMove(1)
	case "k", "up":
		o = o.TreeMove(-1)
	case "enter":
		o = o.TreeToggleExpand()
	case "l", "right":
		o = o.TreeExpand(true)
	case "h", "left":
		o = o.TreeExpand(false)
	case " ", "space":
		o, action, staged = o.TreeToggleSelect()
	case "x":
		o, action, staged = o.TreeToggleExclude()
	case "f":
		o, action, staged = o.TreeToggleFollow()
	case "e":
		o, action, staged = o.TreeToggleEnabled()
	case "R":
		o, action, staged = o.TreeRestrict()
	case "/":
		o = o.TreeBeginSearch()
	case "A":
		o = o.TreeBeginApply()
	case "D":
		if o.TreeStaged() == 0 && !o.TreeStale() {
			o = o.SetTreeNote("nothing staged; nothing to discard")
			break
		}
		o, _ = o.TreeBeginLeave(panes.HivesTreeStay)
	case "ctrl+r":
		if !o.TreeLoading() {
			o = o.BeginTreeRefresh()
			m.hives = &o
			return m, m.loadHivesTree(m.hivesID, m.hivesTree)
		}
	}
	m.hives = &o
	if staged {
		return m.stageHivesTree(action)
	}
	return m, nil
}

// hivesTreeView projects the staged tree and diff into the rows the overlay
// draws. Only canonical identities, names and labels cross; no credential or
// hub endpoint query does.
func hivesTreeView(s *hivesTreeSession) panes.HiveTreeView {
	view := panes.HiveTreeView{Stale: s.stale}
	tree, err := s.edits.Tree()
	if err != nil {
		view.Scope = "unavailable"
		return view
	}
	diff := s.edits.Diff()
	diff.Stale = diff.Stale || s.stale
	view.Stale = diff.Stale
	view.Staged = len(diff.Membership) + len(diff.Enabled) + len(diff.HubFollow)
	view.Diff = hivesTreeDiffLines(diff)
	view.Scope = hivesTreeGroupExplain(tree.Selection, tree.Subscriptions)

	subs := make(map[string]hivectl.TreeSubscription, len(tree.Subscriptions))
	for _, sub := range tree.Subscriptions {
		subs[sub.ID] = sub
	}
	for _, h := range tree.Hubs {
		hubKey := "hub:" + h.ID
		enabled := false
		for _, name := range h.Subscriptions {
			enabled = enabled || !subs[name].Disabled
		}
		members := make([]hivectl.TreeSubscription, 0, len(h.Subscriptions))
		for _, name := range h.Subscriptions {
			members = append(members, subs[name])
		}
		hub := panes.HiveTreeNode{
			Key: hubKey, Kind: panes.HiveTreeHub,
			Short: hivesTreeHubName(h), Identity: h.ID, Hub: h.ID,
			Selection: hivesTreeMark(h.Selection), Badges: hivesTreeBadges(h.Selection, 0, false),
			Follow: h.Follow, Readiness: "unavailable", Activity: "unavailable",
			Provenance:      "tracked hub " + hivesTreeHubName(h),
			Explanation:     hivesTreeGroupExplain(h.Selection, members),
			ExclusionSource: hivesTreeExclusions(h.Selection),
			Enabled:         enabled, Legacy: h.Selection.Legacy, Direct: h.Direct,
			Subscriptions: len(h.Subscriptions), Repositories: h.Selection.Repositories,
		}
		if h.Direct {
			hub.Identity = "direct — saved subscriptions no tracked hub lists"
			hub.Follow = "n/a"
			hub.Provenance = "not enrolled through a hub"
		}
		view.Nodes = append(view.Nodes, hub)
		for _, name := range h.Subscriptions {
			sub := subs[name]
			subKey := hubKey + "/sub:" + name
			follow := "not following"
			if sub.Follow {
				follow = "following"
			}
			if sub.Selection.Legacy {
				follow = "n/a — legacy"
			}
			badges := hivesTreeBadges(sub.Selection, sub.SharedAcrossHubs, sub.Disabled)
			provenance := hivesTreeProvenance(s.set, sub.Sources)
			explanation := hivesTreeExplain(sub.Selection, 1)
			if sub.Disabled {
				explanation = "Disabled; saved scope retained. " + explanation
			}
			view.Nodes = append(view.Nodes, panes.HiveTreeNode{
				Key: subKey, Parent: hubKey, Kind: panes.HiveTreeHive, Depth: 1,
				Short: name, Identity: sub.HiveID + " (subscription " + name + ")",
				Hub: h.ID, Subscription: name,
				Selection: hivesTreeMark(sub.Selection), Badges: badges,
				Follow: follow, Readiness: "unavailable", Activity: "unavailable",
				Provenance: provenance, Explanation: explanation,
				ExclusionSource: hivesTreeExclusions(sub.Selection),
				Enabled:         !sub.Disabled, Legacy: sub.Selection.Legacy,
				Subscriptions: 1, Repositories: sub.Selection.Repositories,
			})
			for _, repo := range sub.Repositories {
				node := panes.HiveTreeNode{
					Key: subKey + "/repo:" + repo.ID, Parent: subKey, Kind: panes.HiveTreeRepo, Depth: 2,
					Short: hivesTreeRepoName(repo.ID), Identity: repo.ID,
					Hub: h.ID, Subscription: name, Repository: repo.ID,
					Selection: " ", Follow: "—", Readiness: "unavailable", Activity: "unavailable",
					Provenance: provenance, ExclusionSource: "none",
					Enabled: !sub.Disabled, Legacy: sub.Selection.Legacy,
				}
				if repo.Selected {
					node.Selection = "x"
				}
				if len(repo.ExcludedBy) > 0 {
					node.Badges = append(node.Badges, "Excluded")
					node.ExclusionSource = "excluded by " + strings.Join(repo.ExcludedBy, ", ")
					for _, by := range repo.ExcludedBy {
						node.Excluded = node.Excluded || by == name
					}
				}
				if repo.Unreviewed {
					node.Badges = append(node.Badges, "Unreviewed")
				}
				if repo.Auto {
					node.Badges = append(node.Badges, "Auto")
				}
				if repo.Offered == "no" {
					node.Badges = append(node.Badges, "Not offered now")
				}
				switch {
				case sub.Selection.Legacy:
					node.Selection = "-"
					node.Explanation = "Unrestricted — legacy: receives every repository the Hive offers; R reviews and restricts"
				case len(repo.ExcludedBy) > 0:
					node.Explanation = "Not selected: an exclusion vetoes it on every subscription"
				case repo.Selected && repo.Auto:
					node.Explanation = "Selected automatically because the Hive is followed; turning it off excludes it so following will not add it back"
				case repo.Selected:
					node.Explanation = "Selected by saved membership"
				case repo.Unreviewed:
					node.Explanation = "Offered but not reviewed; not selected"
				default:
					node.Explanation = "Not selected"
				}
				view.Nodes = append(view.Nodes, node)
			}
		}
	}
	return view
}

func hivesTreeMark(sel hivectl.TreeSelection) string {
	switch {
	case sel.State == "all-current":
		return "x"
	case sel.State == "partial" || sel.Legacy:
		return "-"
	}
	return " "
}

func hivesTreeStateLabel(sel hivectl.TreeSelection) string {
	if sel.Legacy {
		return "Unrestricted — legacy"
	}
	switch sel.State {
	case "off":
		return "Off"
	case "partial":
		return "Partial"
	case "all-current":
		return "All current"
	}
	return sel.State
}

func hivesTreeBadges(sel hivectl.TreeSelection, sharedHubs int, disabled bool) []string {
	var badges []string
	if disabled {
		badges = append(badges, "Disabled")
	}
	if sel.Excluded > 0 {
		badges = append(badges, "Excluded")
	}
	if sel.Unreviewed > 0 {
		badges = append(badges, "Unreviewed")
	}
	if sharedHubs > 1 {
		badges = append(badges, fmt.Sprintf("Shared across %d hubs", sharedHubs))
	}
	if sel.Legacy {
		badges = append(badges, "Unrestricted — legacy")
	}
	if sel.MembershipIncomplete {
		badges = append(badges, "Membership incomplete")
	}
	return badges
}

func hivesTreeExplain(sel hivectl.TreeSelection, subscriptions int) string {
	if sel.Legacy && subscriptions == 1 {
		// An unrestricted subscription has no saved selection, so "0 of N selected" would read as "not contributing".
		text := "Unrestricted — legacy: receives every repository the Hive offers until restricted"
		if sel.MembershipIncomplete {
			text += "; Membership incomplete"
		}
		return text
	}
	text := fmt.Sprintf("%s: %d of %d repositories selected", hivesTreeStateLabel(sel), sel.Selected, sel.Repositories)
	if subscriptions != 1 {
		text += fmt.Sprintf(" across %d subscriptions", subscriptions)
	}
	if sel.MembershipIncomplete {
		text += "; Membership incomplete"
	}
	return text
}

func hivesTreeExclusions(sel hivectl.TreeSelection) string {
	if sel.Excluded == 0 {
		return "none"
	}
	return fmt.Sprintf("%d excluded; focus a repository to see which subscription excludes it", sel.Excluded)
}

func hivesTreeScope(sel hivectl.TreeSelection, subscriptions int) string {
	return hivesTreeExplain(sel, subscriptions)
}

// hivesTreeGroupExplain describes a hub or the whole tree. One unrestricted
// subscription must not stand in for its restricted siblings, so restricted
// and unrestricted Hives are reported separately.
func hivesTreeGroupExplain(sel hivectl.TreeSelection, members []hivectl.TreeSubscription) string {
	legacy, restricted, selected, total := 0, 0, 0, 0
	for _, m := range members {
		if m.Selection.Legacy {
			legacy++
			continue
		}
		restricted++
		selected += m.Selection.Selected
		total += m.Selection.Repositories
	}
	if legacy == 0 {
		return hivesTreeScope(sel, len(members))
	}
	var parts []string
	if restricted > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d selected in %d restricted Hive(s)", selected, total, restricted))
	}
	parts = append(parts, fmt.Sprintf("%d unrestricted — legacy (receives all)", legacy))
	if sel.MembershipIncomplete {
		parts = append(parts, "Membership incomplete")
	}
	return strings.Join(parts, "; ")
}

// hivesTreeProvenance names reviewed presets by ID and revision. A source with
// no matching preset record, or no source at all, is "attribution unavailable".
func hivesTreeProvenance(set *hivectl.ProfileSet, sources []string) string {
	if len(sources) == 0 || set == nil {
		return "attribution unavailable"
	}
	var parts []string
	for _, source := range sources {
		part := source + " (attribution unavailable)"
		for _, p := range set.Presets {
			if p.ID == source {
				part = "preset " + p.ID + " revision " + p.ReviewedRevision
				break
			}
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

func hivesTreeHubName(h hivectl.TreeHub) string {
	if h.Name != "" {
		return h.Name
	}
	return strings.TrimPrefix(strings.TrimPrefix(h.ID, "https://"), "http://")
}

// hivesTreeRepoName is the short owner/name of a canonical repository ID.
func hivesTreeRepoName(id string) string {
	parts := strings.Split(id, "/")
	if len(parts) >= 4 {
		return strings.Join(parts[2:], "/")
	}
	return id
}

// hivesTreeDiffLines renders the shared ContributionDiff. The summary line is
// worded as on the Web tree; every named target follows it.
func hivesTreeDiffLines(d hivectl.ContributionDiff) []string {
	if d.Stale {
		return []string{"Stale review — discard and review again before Apply."}
	}
	if d.Empty() {
		return []string{"No staged changes."}
	}
	lines := []string{fmt.Sprintf("+%d repositories selected, −%d deselected, %d exclusions added, %d cleared; follow changes: %d; enable/disable: %d; legacy conversions: %d.",
		d.SelectedCount, d.DeselectedCount, d.ExcludedCount, d.ClearedExclusionCount, len(d.Follow), len(d.Enabled), len(d.LegacyConversions))}
	for _, t := range d.Selected {
		lines = append(lines, "+ select "+t.Repository+" on "+t.Subscription)
	}
	for _, t := range d.Deselected {
		lines = append(lines, "− deselect "+t.Repository+" on "+t.Subscription)
	}
	for _, e := range d.Exclusions {
		verb := "clear exclusion of "
		if e.Excluded {
			verb = "exclude "
		}
		line := verb + e.Repository + " on " + e.Subscription
		if len(e.Sources) > 0 {
			line += "; still excluded by " + strings.Join(e.Sources, ", ")
		}
		if len(e.ScopedSubscriptions) > 0 {
			line += "; affects " + strings.Join(e.ScopedSubscriptions, ", ")
		}
		if len(e.LegacyUnrestricted) > 0 {
			line += "; Unrestricted — legacy still receives it on " + strings.Join(e.LegacyUnrestricted, ", ")
		}
		lines = append(lines, line)
	}
	for _, c := range d.Follow {
		lines = append(lines, fmt.Sprintf("follow new repositories on %s: %s → %s", c.Target, onOff(c.Before), onOff(c.After)))
	}
	for _, c := range d.Enabled {
		lines = append(lines, fmt.Sprintf("%s: %s → %s", c.Target, enabledWord(c.Before), enabledWord(c.After)))
	}
	for _, c := range d.HubFollow {
		lines = append(lines, fmt.Sprintf("hub %s follow: %s → %s", c.Hub, c.Before, c.After))
	}
	for _, name := range d.LegacyConversions {
		lines = append(lines, "review and restrict "+name+": Unrestricted — legacy → current membership")
	}
	for _, b := range d.SharedBranches {
		hubs := append([]string(nil), b.Hubs...)
		sort.Strings(hubs)
		lines = append(lines, "shared: "+b.Subscription+" also appears under "+strings.Join(hubs, ", "))
	}
	return lines
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func enabledWord(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}
