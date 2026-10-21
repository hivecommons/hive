package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// The contribution tree's app-side half (RFC #13153, TUI surface).
//
// The tree screen edits a hivectl.StagedEdits session — the same model the
// Commons web page stages through — and commits it with StagedEdits.Apply,
// which takes the shared profiles.yml lock and refuses a stale revision. The
// overlay never writes between keypresses: every key mutates the in-memory
// session and re-projects the rows; only A → enter reaches the filesystem.

// hivesTreeDiscoveryTimeout bounds hub and repository discovery for one open.
const hivesTreeDiscoveryTimeout = hivesProbeTimeout

// hivesTreeRuntimeNoStore is the runtime line when the TUI has no profile
// store at all, so neither the relay status snapshot nor the activity store
// can be located. A label, not a claim.
const hivesTreeRuntimeNoStore = hivectl.TreeRuntimeUnavailable + " — no profile store"

var errHivesTreeRefreshPending = errors.New("staged edits are pending; apply (A) or discard (U) before refreshing discovery")

type (
	// hivesTreeOpenedMsg is a finished open: a session, or the reason there
	// is none. discovery carries per-branch unavailability reasons, keyed by
	// hub id or subscription name.
	hivesTreeOpenedMsg struct {
		overlayID uint64
		edits     *hivectl.StagedEdits
		discovery map[string]string
		err       error
	}

	// hivesTreeAppliedMsg is a finished Apply with its receipt.
	hivesTreeAppliedMsg struct {
		overlayID uint64
		receipt   hivectl.ApplyReceipt
		err       error
	}
)

// openHivesTree raises the tree screen and starts discovery.
func (m model) openHivesTree() (tea.Model, tea.Cmd) {
	next, ok := m.hives.BeginTree()
	if !ok {
		return m, nil
	}
	m.hives = &next
	m.hivesStage = nil
	return m, m.loadHivesTree(m.hivesID)
}

// loadHivesTree reads the set, discovers hubs and repositories in parallel,
// and opens a staged session. Discovery failures stay per-branch: a hub that
// does not answer is "unavailable", never an empty collection.
func (m model) loadHivesTree(overlayID uint64) tea.Cmd {
	env := m.hivesEnv
	return func() tea.Msg {
		if env.store == nil {
			return hivesTreeOpenedMsg{overlayID: overlayID, err: errNoHivesStore}
		}
		set, _, err := env.store.LoadOrMigrate()
		if err != nil {
			return hivesTreeOpenedMsg{overlayID: overlayID, err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), hivesTreeDiscoveryTimeout)
		defer cancel()
		hubs, repos, discovery := discoverHivesTree(ctx, env, set)
		edits, err := hivectl.NewStagedEdits(set, hubs, repos)
		if err != nil {
			return hivesTreeOpenedMsg{overlayID: overlayID, err: err}
		}
		if env.signalRelay != nil {
			edits.SetRelaySignal(func(ctx context.Context, _ *hivectl.ProfileStore) (hivectl.RelaySwitchResult, error) {
				return env.signalRelay(ctx)
			})
		}
		return hivesTreeOpenedMsg{overlayID: overlayID, edits: edits, discovery: discovery}
	}
}

// discoverHivesTree mirrors the web page's discovery: one hub registry read
// per saved hub, one repository read per subscription, all concurrent.
func discoverHivesTree(ctx context.Context, env hivesEnv, set *hivectl.ProfileSet) ([]hivectl.TreeHubSnapshot, map[string]hivectl.TreeRepositorySnapshot, map[string]string) {
	discovery := map[string]string{}
	hubs := make([]hivectl.TreeHubSnapshot, 0, len(set.Hubs))
	repos := make(map[string]hivectl.TreeRepositorySnapshot, len(set.Profiles))
	var mu sync.Mutex
	var wg sync.WaitGroup
	// Build every snapshot before any goroutine starts so the slice header
	// and the discovery map are only written concurrently under mu.
	hubIDs := make([]string, 0, len(set.Hubs))
	for _, h := range set.Hubs {
		id, err := hivectl.NormalizeHiveID(h.URL)
		if err != nil {
			discovery[h.Name] = "hub URL is not a usable origin"
			continue
		}
		hubs = append(hubs, hivectl.TreeHubSnapshot{Name: h.Name, URL: h.URL, Follow: h.EffectiveFollow(), Discovery: hivectl.TreeUnavailable})
		hubIDs = append(hubIDs, id)
	}
	if env.discoverHubHives == nil {
		for _, id := range hubIDs {
			discovery[id] = "hub discovery not configured"
		}
	}
	if env.discoverRepositories == nil {
		for _, p := range set.Profiles {
			repos[p.Name] = hivectl.TreeRepositorySnapshot{Discovery: hivectl.TreeUnavailable}
			discovery[p.Name] = "repository discovery not configured"
		}
	}
	for i := range hubs {
		if env.discoverHubHives == nil {
			break
		}
		wg.Add(1)
		go func(i int, id, url string) {
			defer wg.Done()
			hives, err := env.discoverHubHives(ctx, url)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				discovery[id] = hivesTreeDiscoveryReason(err)
				return
			}
			hubs[i].Discovery = hivectl.TreeComplete
			for _, hive := range hives {
				hubs[i].HiveOrigins = append(hubs[i].HiveOrigins, hive.HubURL)
			}
		}(i, hubIDs[i], hubs[i].URL)
	}
	for _, p := range set.Profiles {
		if env.discoverRepositories == nil {
			break
		}
		wg.Add(1)
		go func(name, hub string) {
			defer wg.Done()
			list, err := env.discoverRepositories(ctx, hub)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				repos[name] = hivectl.TreeRepositorySnapshot{Discovery: hivectl.TreeUnavailable}
				discovery[name] = hivesTreeDiscoveryReason(err)
				return
			}
			repos[name] = hivectl.TreeRepositorySnapshot{Discovery: hivectl.TreeComplete, Repositories: list}
		}(p.Name, p.Hub)
	}
	wg.Wait()
	return hubs, repos, discovery
}

// hivesTreeDiscoveryReason keeps a coarse cause. Transport errors embed the
// URL, which may carry a query string, so they are summarized rather than
// rendered.
func hivesTreeDiscoveryReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "discovery timed out"
	case errors.Is(err, context.Canceled):
		return "discovery cancelled"
	case errors.Is(err, hivectl.ErrHubUnavailable):
		return "hub registry unavailable"
	}
	msg := err.Error()
	if i := strings.Index(msg, ";"); i > 0 {
		msg = msg[:i]
	}
	if strings.Contains(msg, "://") || strings.Contains(msg, "?") {
		return "discovery unavailable"
	}
	return msg
}

// hiveTreeRows flattens the read model into the overlay's rows, carrying the
// staged diff's marks and the capability flags the key handler consults.
func hiveTreeRows(edits *hivectl.StagedEdits, discovery map[string]string, runtime *hivectl.TreeRuntime) ([]panes.HiveTreeRow, panes.HiveTreeSummary, error) {
	tree, err := edits.Tree()
	if err != nil {
		return nil, panes.HiveTreeSummary{}, err
	}
	live, durable := hivesTreeRuntimeNoStore, hivesTreeRuntimeNoStore
	if runtime != nil {
		tree.ApplyRuntime(*runtime)
		live, durable = runtime.Live.Label(), runtime.Durable.Label()
	}
	diff := edits.Diff()
	changedSubs, changedRepos := diff.Changed()
	subs := make(map[string]hivectl.TreeSubscription, len(tree.Subscriptions))
	for _, s := range tree.Subscriptions {
		subs[s.ID] = s
	}
	var rows []panes.HiveTreeRow
	for _, hub := range tree.Hubs {
		hubKey := "hub:" + hub.ID
		row := panes.HiveTreeRow{
			Kind: panes.HiveTreeHub, Key: hubKey, Level: 1, ID: hub.ID, Label: hub.Name, Direct: hub.Direct,
			Check:     treeCheck(hub.Selection, hub.Discovery == hivectl.TreeComplete || len(hub.Subscriptions) > 0),
			CanSelect: !hub.Selection.MembershipIncomplete && hub.Selection.Repositories > 0 && !hub.Selection.Legacy,
		}
		row.Badges = append(row.Badges, hivectl.SelectionLabel(hub.Selection, hub.Discovery))
		if !hub.Direct {
			row.Badges = append(row.Badges, "Future Hives: "+hub.Follow)
		}
		if reason, ok := discovery[hub.ID]; ok {
			row.Badges = append(row.Badges, "Discovery unavailable — "+reason)
			row.NeedsReview = true
		}
		if len(hub.Subscriptions) == 0 {
			row.Badges = append(row.Badges, "Empty collection")
		}
		anyEnabled := false
		for _, id := range hub.Subscriptions {
			if s, ok := subs[id]; ok {
				anyEnabled = anyEnabled || !s.Disabled
				row.Changed = row.Changed || changedSubs[id]
			}
		}
		row.Disabled = len(hub.Subscriptions) > 0 && !anyEnabled
		row.Detail = append(row.Detail, fmt.Sprintf("%d hive(s)", len(hub.Subscriptions)))
		if !hub.Direct {
			row.Detail = append(row.Detail, "hub id "+hub.ID)
		}
		rows = append(rows, row)
		for _, id := range hub.Subscriptions {
			s, ok := subs[id]
			if !ok {
				continue
			}
			rows = append(rows, hiveTreeSubscriptionRows(hubKey, s, discovery, changedSubs, changedRepos)...)
		}
	}
	for _, name := range sortedDiscoveryOrphans(discovery, tree) {
		rows = append(rows, panes.HiveTreeRow{Kind: panes.HiveTreePlaceholder, Key: "orphan:" + name, Level: 1, Label: name + " — " + discovery[name], NeedsReview: true})
	}
	summary := panes.HiveTreeSummary{Staged: !diff.Empty(), Lines: diff.Lines(), Live: live, Durable: durable}
	return rows, summary, nil
}

func hiveTreeSubscriptionRows(hubKey string, s hivectl.TreeSubscription, discovery map[string]string, changedSubs, changedRepos map[string]bool) []panes.HiveTreeRow {
	subKey := hubKey + "/sub:" + s.ID
	sel := s.Selection
	row := panes.HiveTreeRow{
		Kind: panes.HiveTreeHive, Key: subKey, ParentKey: hubKey, Level: 2, ID: s.ID, Label: s.ID,
		Check:     treeCheck(sel, s.Discovery == hivectl.TreeComplete),
		CanSelect: !sel.MembershipIncomplete && sel.Repositories > 0 && !sel.Legacy,
		Following: s.Follow, Disabled: s.Disabled, Legacy: sel.Legacy, Changed: changedSubs[s.ID],
	}
	row.Badges = append(row.Badges, hivectl.SelectionLabel(sel, s.Discovery))
	if s.Disabled {
		row.Badges = append(row.Badges, "Disabled — scope retained")
	}
	if s.SharedAcrossHubs > 1 {
		row.Badges = append(row.Badges, fmt.Sprintf("Shared across %d hubs", s.SharedAcrossHubs))
	}
	if s.Follow {
		row.Badges = append(row.Badges, "Following new repositories")
	} else {
		row.Badges = append(row.Badges, "Follow off")
	}
	if s.Discovery != hivectl.TreeComplete {
		reason := discovery[s.ID]
		if reason == "" {
			reason = string(s.Discovery)
		}
		row.Badges = append(row.Badges, "Discovery unavailable — "+reason)
		row.NeedsReview = true
	}
	if sel.Legacy {
		row.NeedsReview = true
	}
	row.Detail = append(row.Detail, "hive "+s.HiveID, "readiness: "+s.Readiness, "activity: "+s.Activity, "history: "+s.History)
	if len(s.Sources) > 0 {
		row.Detail = append(row.Detail, "selection sources: "+strings.Join(s.Sources, ", "))
	}
	rows := []panes.HiveTreeRow{row}
	if len(s.Repositories) == 0 {
		label := "Membership unknown — discovery " + string(s.Discovery)
		switch {
		case sel.Legacy:
			label = "Unrestricted — legacy; press R to review and restrict"
		case s.Discovery == hivectl.TreeComplete:
			label = "No repositories"
		}
		rows = append(rows, panes.HiveTreeRow{Kind: panes.HiveTreePlaceholder, Key: subKey + "/none", ParentKey: subKey, Level: 3, Label: label, NeedsReview: sel.Legacy || s.Discovery != hivectl.TreeComplete, Changed: row.Changed})
		return rows
	}
	for _, r := range s.Repositories {
		repo := panes.HiveTreeRow{
			Kind: panes.HiveTreeRepo, Key: subKey + "/repo:" + r.ID, ParentKey: subKey, Level: 3, ID: r.ID, Subscription: s.ID, Label: r.ID,
			Check: panes.HiveTreeCheckOff, CanSelect: !sel.Legacy, Legacy: sel.Legacy, Changed: changedRepos[s.ID+"|"+r.ID],
		}
		if r.Selected {
			repo.Check = panes.HiveTreeCheckAll
		}
		if sel.Legacy {
			repo.Check = panes.HiveTreeCheckDisabled
		}
		for _, by := range r.ExcludedBy {
			repo.Excluded = repo.Excluded || by == s.ID
		}
		if len(r.ExcludedBy) > 0 {
			repo.Badges = append(repo.Badges, "Excluded by "+strings.Join(r.ExcludedBy, ", "))
		}
		if r.Unreviewed {
			repo.Badges = append(repo.Badges, "Unreviewed")
			repo.NeedsReview = true
		}
		if r.Offered == "no" {
			repo.Badges = append(repo.Badges, "No longer offered")
			repo.NeedsReview = true
		}
		if repo.Changed {
			repo.Badges = append(repo.Badges, "Changed")
		}
		repo.Detail = append(repo.Detail, "offered: "+hiveDash(r.Offered), "activity: "+hiveDash(r.Activity), "history: "+hiveDash(r.History))
		rows = append(rows, repo)
	}
	return rows
}

func hiveDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "—"
	}
	return v
}

// sortedDiscoveryOrphans names saved hubs whose URL could not be keyed by
// origin: they are absent from the read model and would otherwise vanish.
func sortedDiscoveryOrphans(discovery map[string]string, tree *hivectl.ContributionTree) []string {
	known := map[string]bool{}
	for _, h := range tree.Hubs {
		known[h.ID] = true
	}
	for _, s := range tree.Subscriptions {
		known[s.ID] = true
	}
	var out []string
	for k := range discovery {
		if !known[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func treeCheck(sel hivectl.TreeSelection, known bool) panes.HiveTreeCheck {
	switch {
	case sel.Legacy || !known || sel.Repositories == 0:
		return panes.HiveTreeCheckDisabled
	case sel.State == "all-current":
		return panes.HiveTreeCheckAll
	case sel.State == "partial":
		return panes.HiveTreeCheckPartial
	}
	return panes.HiveTreeCheckOff
}

// updateHivesTree consumes every key while the tree screen is up.
func (m model) updateHivesTree(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.hives.TreeConfirming() {
		switch msg.String() {
		case "enter":
			return m.applyHivesTree()
		case "esc":
			next := m.hives.TreeConfirmCancel()
			m.hives = &next
		}
		return m, nil
	}
	if m.hives.TreeSearching() {
		switch msg.String() {
		case "esc":
			next := m.hives.TreeEndSearch(true)
			m.hives = &next
		case "enter":
			next := m.hives.TreeEndSearch(false)
			m.hives = &next
		case "backspace":
			next := m.hives.TreeSearchBackspace()
			m.hives = &next
		default:
			if (msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace) && !msg.Alt {
				text := string(msg.Runes)
				if msg.Type == tea.KeySpace {
					text = " "
				}
				next := m.hives.TreeSearchType(text)
				m.hives = &next
			}
		}
		return m, nil
	}
	switch msg.String() {
	case "esc":
		next, left := m.hives.TreeLeave()
		m.hives = &next
		if left {
			m.hivesStage = nil
		}
		return m, nil
	case "?":
		next := m.hives.TreeToggleHelp()
		m.hives = &next
		return m, nil
	case "j", "down":
		next := m.hives.TreeMove(1)
		m.hives = &next
	case "k", "up":
		next := m.hives.TreeMove(-1)
		m.hives = &next
	case "g", "home":
		next := m.hives.TreeJump(false)
		m.hives = &next
	case "G", "end":
		next := m.hives.TreeJump(true)
		m.hives = &next
	case "l", "right":
		next := m.hives.TreeExpand(true)
		m.hives = &next
	case "h", "left":
		next := m.hives.TreeExpand(false)
		m.hives = &next
	case "/":
		next := m.hives.TreeBeginSearch()
		m.hives = &next
	case "1", "2", "3":
		next := m.hives.TreeSetFilter(panes.HiveTreeFilter(int(msg.Runes[0] - '1')))
		m.hives = &next
	case "r":
		return m.refreshHivesTree()
	case "A":
		next, ok := m.hives.BeginTreeConfirm()
		m.hives = &next
		if !ok {
			return m.treeNote("nothing is staged")
		}
	case "U":
		return m.editHivesTree(func(edits *hivectl.StagedEdits, _ panes.HiveTreeRow) (string, error) {
			return "staged edits discarded", edits.Discard()
		})
	case " ", "f", "F", "x", "e", "R":
		return m.editHivesTree(hivesTreeEditFor(msg.String()))
	}
	return m, nil
}

// refreshHivesTree re-runs discovery. It is refused while edits are staged,
// because a refresh opens a new session and would drop them.
func (m model) refreshHivesTree() (tea.Model, tea.Cmd) {
	if m.hives.TreeStaged() {
		next := m.hives.SetTreeError(errHivesTreeRefreshPending)
		m.hives = &next
		return m, nil
	}
	next := m.hives.TreeClose()
	m.hives = &next
	return m.openHivesTree()
}

func (m model) treeNote(note string) (tea.Model, tea.Cmd) {
	next := m.hives.SetTreeNote(note)
	m.hives = &next
	return m, nil
}

// hivesTreeEdit mutates the session for the selected row and returns the
// staging hint to show. Nothing is written.
type hivesTreeEdit func(edits *hivectl.StagedEdits, row panes.HiveTreeRow) (string, error)

var errHivesTreeRowNotEditable = errors.New("this key does not apply to the selected row")

// hivesTreeEditFor maps a key to the edit it stages on the selected row.
func hivesTreeEditFor(key string) hivesTreeEdit {
	switch key {
	case " ":
		return func(edits *hivectl.StagedEdits, row panes.HiveTreeRow) (string, error) {
			switch row.Kind {
			case panes.HiveTreeRepo:
				selected := row.Check != panes.HiveTreeCheckAll
				if err := edits.SelectRepository(row.Subscription, row.ID, selected); err != nil {
					return "", err
				}
				if selected {
					return "staged: select " + row.ID + " at " + row.Subscription, nil
				}
				return "staged: deselect " + row.ID + " at " + row.Subscription, nil
			case panes.HiveTreeHive, panes.HiveTreeHub:
				if !row.CanSelect {
					return "", errors.New("no selectable repositories here: membership is unknown, empty, or legacy — nothing to check")
				}
				selected := row.Check != panes.HiveTreeCheckAll
				if err := edits.SelectCurrent(row.ID, row.Kind == panes.HiveTreeHub, selected); err != nil {
					return "", err
				}
				if selected {
					return "staged: select current repositories at " + row.Label, nil
				}
				note := "staged: deselect current at " + row.Label
				if row.Following {
					note += " — following stays on and may repopulate this Hive on refresh"
				}
				return note, nil
			}
			return "", errHivesTreeRowNotEditable
		}
	case "f":
		return func(edits *hivectl.StagedEdits, row panes.HiveTreeRow) (string, error) {
			if row.Kind != panes.HiveTreeHive {
				return "", errHivesTreeRowNotEditable
			}
			if err := edits.SetFollow(row.ID, !row.Following); err != nil {
				return "", err
			}
			if row.Following {
				return "staged: stop following new repositories at " + row.ID, nil
			}
			return "staged: follow new repositories at " + row.ID, nil
		}
	case "F":
		// Explicit stop: never turns following on, so it is safe to press
		// without reading the badge first.
		return func(edits *hivectl.StagedEdits, row panes.HiveTreeRow) (string, error) {
			if row.Kind != panes.HiveTreeHive {
				return "", errHivesTreeRowNotEditable
			}
			if !row.Following {
				return "not following new repositories at " + row.ID + "; nothing staged", nil
			}
			if err := edits.SetFollow(row.ID, false); err != nil {
				return "", err
			}
			return "staged: stop following new repositories at " + row.ID, nil
		}
	case "x":
		return func(edits *hivectl.StagedEdits, row panes.HiveTreeRow) (string, error) {
			if row.Kind != panes.HiveTreeRepo {
				return "", errHivesTreeRowNotEditable
			}
			if err := edits.SetExcluded(row.Subscription, row.ID, !row.Excluded); err != nil {
				return "", err
			}
			if row.Excluded {
				return "staged: exclusion cleared — remaining saved membership decides selection", nil
			}
			return "staged: persistent exclusion of " + row.ID + " (parent actions cannot clear it)", nil
		}
	case "e":
		return func(edits *hivectl.StagedEdits, row panes.HiveTreeRow) (string, error) {
			switch row.Kind {
			case panes.HiveTreeHive:
				if err := edits.SetHiveEnabled(row.ID, row.Disabled); err != nil {
					return "", err
				}
				if row.Disabled {
					return "staged: enable " + row.ID + " (saved scope restored)", nil
				}
				return "staged: disable " + row.ID + " (credentials and selections retained)", nil
			case panes.HiveTreeHub:
				if row.Direct {
					return "", errors.New("direct subscriptions are enabled per hive; press e on the hive")
				}
				if err := edits.SetHubEnabled(row.ID, row.Disabled); err != nil {
					return "", err
				}
				if row.Disabled {
					return "staged: enable subscriptions under " + row.Label, nil
				}
				return "staged: disable subscriptions under " + row.Label + "; future-Hive enrollment off", nil
			}
			return "", errHivesTreeRowNotEditable
		}
	case "R":
		return func(edits *hivectl.StagedEdits, row panes.HiveTreeRow) (string, error) {
			if row.Kind != panes.HiveTreeHive || !row.Legacy {
				return "", errors.New("review and restrict applies to a legacy unrestricted hive")
			}
			if err := edits.ReviewAndRestrict(row.ID); err != nil {
				return "", err
			}
			return "staged: legacy conversion for " + row.ID + " — currently discovered repositories become explicit membership with following off; review the diff before A", nil
		}
	}
	return func(*hivectl.StagedEdits, panes.HiveTreeRow) (string, error) { return "", errHivesTreeRowNotEditable }
}

// editHivesTree applies one staged edit to the selected row and re-projects.
func (m model) editHivesTree(edit hivesTreeEdit) (tea.Model, tea.Cmd) {
	if m.hivesStage == nil {
		return m, nil
	}
	row, ok := m.hives.TreeSelected()
	if !ok {
		return m, nil
	}
	note, err := edit(m.hivesStage, row)
	if err != nil {
		next := m.hives.SetTreeError(err)
		m.hives = &next
		return m, nil
	}
	return m.projectHivesTree(note)
}

// projectHivesTree redraws the tree from the session.
func (m model) projectHivesTree(note string) (tea.Model, tea.Cmd) {
	var runtime *hivectl.TreeRuntime
	if m.hivesEnv.treeRuntime != nil {
		rt := m.hivesEnv.treeRuntime()
		runtime = &rt
	}
	rows, summary, err := hiveTreeRows(m.hivesStage, m.hivesTreeDiscovery, runtime)
	if err != nil {
		next := m.hives.SetTreeError(err)
		m.hives = &next
		return m, nil
	}
	next := m.hives.SetTree(rows, summary)
	if note != "" {
		next = next.SetTreeNote(note)
	}
	m.hives = &next
	return m, nil
}

// applyHivesTree commits the reviewed diff.
func (m model) applyHivesTree() (tea.Model, tea.Cmd) {
	edits := m.hivesStage
	if edits == nil {
		return m, nil
	}
	next := m.hives.BeginTreeApply()
	m.hives = &next
	env := m.hivesEnv
	overlayID := m.hivesID
	return m, func() tea.Msg {
		if env.applyStaged == nil {
			return hivesTreeAppliedMsg{overlayID: overlayID, err: errNoHivesStore}
		}
		receipt, err := env.applyStaged(context.Background(), edits)
		return hivesTreeAppliedMsg{overlayID: overlayID, receipt: receipt, err: err}
	}
}

// hivesTreeReceipt renders the receipt truthfully: saved, relay reload and
// acknowledgement are three separate facts, and "saved" is never "live".
func hivesTreeReceipt(receipt hivectl.ApplyReceipt, err error) string {
	parts := []string{"saved: " + receipt.Saved, "relay reload: " + receipt.RelayReload, "acknowledged: " + receipt.Acknowledged}
	switch {
	case err == nil && receipt.Saved == hivectl.StageUnchanged:
		return "nothing to apply; " + strings.Join(parts, "; ")
	case err == nil && receipt.Acknowledged != "yes":
		return "✓ change saved — relay adoption pending; " + strings.Join(parts, "; ")
	case err == nil:
		return "✓ applied; " + strings.Join(parts, "; ")
	}
	return strings.Join(parts, "; ")
}

func (m model) handleHivesTreeOpened(msg hivesTreeOpenedMsg) (tea.Model, tea.Cmd) {
	if m.hives == nil || m.hivesID != msg.overlayID || !m.hives.InTree() {
		return m, nil
	}
	if msg.err != nil {
		next := m.hives.SetTreeError(msg.err)
		m.hives = &next
		return m, nil
	}
	m.hivesStage = msg.edits
	m.hivesTreeDiscovery = msg.discovery
	note := m.hivesTreeNote
	m.hivesTreeNote = ""
	return m.projectHivesTree(note)
}

func (m model) handleHivesTreeApplied(msg hivesTreeAppliedMsg) (tea.Model, tea.Cmd) {
	if m.hives == nil || m.hivesID != msg.overlayID || !m.hives.InTree() {
		return m, nil
	}
	next := m.hives.EndTreeApply()
	m.hives = &next
	note := hivesTreeReceipt(msg.receipt, msg.err)
	switch {
	case msg.err == nil, errors.Is(msg.err, hivectl.ErrStageReload):
		// Saved. The diff is consumed, so reopen the session against the new
		// revision; the note survives the reload.
		if msg.err != nil {
			note = "⚠ " + note + "; the running relay could not be signaled, so the change governs its next start"
		}
		m.hivesTreeNote = note
		next := m.hives.TreeClose()
		m.hives = &next
		return m.openHivesTree()
	case errors.Is(msg.err, hivectl.ErrStaleRevision):
		m.hivesTreeNote = "profiles.yml was changed by another hivectl, TUI or Commons web session since this review; nothing was saved — review the current tree and stage again"
		next := m.hives.TreeClose()
		m.hives = &next
		return m.openHivesTree()
	case errors.Is(msg.err, hivectl.ErrStageProjection):
		next := m.hives.SetTreeError(fmt.Errorf("%s; profiles.yml was saved but contributor.env was not regenerated — press A to retry before further edits", note))
		m.hives = &next
		return m.projectHivesTreeKeepError()
	}
	next = m.hives.SetTreeError(fmt.Errorf("%w; %s", msg.err, note))
	m.hives = &next
	return m, nil
}

// projectHivesTreeKeepError re-projects without clearing the error line.
func (m model) projectHivesTreeKeepError() (tea.Model, tea.Cmd) {
	err := m.hives.TreeError()
	next, cmd := m.projectHivesTree("")
	mm := next.(model)
	keep := mm.hives.SetTreeError(errors.New(err))
	mm.hives = &keep
	return mm, cmd
}
