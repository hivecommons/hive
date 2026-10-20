package hivectl

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
)

var (
	ErrStageIncomplete = errors.New("membership incomplete; refresh before bulk inclusion")
	ErrStageLegacy     = errors.New("unrestricted subscription; review and restrict first")
	ErrStageSave       = errors.New("profiles were not saved")
	ErrStageProjection = errors.New("profiles saved but projection failed; retry Apply before further edits")
	ErrStageReload     = errors.New("saved for next start; relay reload pending/failed")
)

// StagedEdits owns a private copy of consent and discovery. It is a single UI
// session, not safe for concurrent calls. Neither construction nor edits do I/O.
// Both tree surfaces use Diff and Apply rather than deriving their own receipts.
type StagedEdits struct {
	before, current          *ProfileSet
	revision                 string
	hubs                     []TreeHubSnapshot
	repos                    map[string]TreeRepositorySnapshot
	stale, projectionPending bool
	signal                   func(context.Context, *ProfileStore) (RelaySwitchResult, error)
}

// NewStagedEdits freezes the same snapshots used by BuildContributionTree,
// together with the loaded ProfileSet.Revision. Opening a session never migrates
// legacy consent. Refresh/review after a stale Apply requires a new session.
func NewStagedEdits(set *ProfileSet, hubs []TreeHubSnapshot, repos map[string]TreeRepositorySnapshot) (*StagedEdits, error) {
	if set == nil {
		return nil, ErrNoProfiles
	}
	// Validation errors can embed credential values. Do not forward them to UIs.
	if err := set.Validate(); err != nil {
		return nil, errors.New("invalid contribution profiles")
	}
	s := &StagedEdits{before: cloneStageSet(set), current: cloneStageSet(set), revision: set.Revision(), signal: SignalRunningRelay}
	s.hubs = append([]TreeHubSnapshot(nil), hubs...)
	for i := range s.hubs {
		s.hubs[i].HiveOrigins = append([]string(nil), hubs[i].HiveOrigins...)
	}
	s.repos = make(map[string]TreeRepositorySnapshot, len(repos))
	for name, snapshot := range repos {
		snapshot.Repositories = append([]string(nil), snapshot.Repositories...)
		s.repos[name] = snapshot
	}
	if _, err := s.Tree(); err != nil {
		return nil, errors.New("invalid contribution discovery snapshot")
	}
	return s, nil
}

func cloneStageSet(set *ProfileSet) *ProfileSet {
	next := *set
	next.Profiles = append([]Profile(nil), set.Profiles...)
	next.Presets = append([]PresetProvenance(nil), set.Presets...)
	next.Hubs = append([]HubEntry(nil), set.Hubs...)
	for i := range next.Profiles {
		next.Profiles[i].Selection = cloneStageSelection(set.Profiles[i].Selection)
	}
	return &next
}

func cloneStageSelection(selection *RepositorySelection) *RepositorySelection {
	if selection == nil {
		return nil
	}
	next := *selection
	// Normalize set spellings/order and nil/empty lists for semantic no-op diffs.
	next.Repositories, _ = canonicalRepositorySet(selection.Repositories)
	next.MembershipSnapshot, _ = canonicalRepositorySet(selection.MembershipSnapshot)
	next.Include, _ = canonicalRepositorySet(selection.Include)
	next.Exclude, _ = canonicalRepositorySet(selection.Exclude)
	next.Sources = append([]string(nil), selection.Sources...)
	return &next
}

// Tree returns a fresh credential-free snapshot, so callers cannot mutate the
// staged state through a returned slice. Saved membership survives disablement.
func (s *StagedEdits) Tree() (*ContributionTree, error) {
	hubs := append([]TreeHubSnapshot(nil), s.hubs...)
	for i := range hubs {
		id, _ := NormalizeHiveID(hubs[i].URL)
		for _, h := range s.current.Hubs {
			origin, _ := NormalizeHiveID(h.URL)
			if id == origin {
				hubs[i].Follow = h.EffectiveFollow()
			}
		}
	}
	return BuildContributionTree(s.current, hubs, s.repos)
}

func (s *StagedEdits) editable() error {
	if s.stale {
		return ErrStaleRevision
	}
	if s.projectionPending {
		return ErrStageProjection
	}
	return nil
}

func (s *StagedEdits) profile(name string, scoped bool) (*Profile, error) {
	if err := s.editable(); err != nil {
		return nil, err
	}
	p, _ := s.current.Find(name)
	if p == nil {
		return nil, ErrProfileNotFound
	}
	if scoped && p.Selection == nil {
		return nil, ErrStageLegacy
	}
	return p, nil
}

// SelectRepository changes saved membership only, never an exclusion or follow
// policy. Selecting an excluded target keeps the veto visible in the tree/diff.
func (s *StagedEdits) SelectRepository(name, repository string, selected bool) error {
	p, err := s.profile(name, true)
	if err != nil {
		return err
	}
	id, err := NormalizeRepositoryID(repository)
	if err != nil {
		return errors.New("invalid repository identity")
	}
	if selected {
		if treeSavedMember(p.Selection, id) {
			return nil
		}
		return s.current.IncludeRepo(p.Name, id)
	}
	p.Selection.Repositories = withoutRepository(p.Selection.Repositories, id)
	p.Selection.MembershipSnapshot = withoutRepository(p.Selection.MembershipSnapshot, id)
	p.Selection.Include = withoutRepository(p.Selection.Include, id)
	return nil
}

// SetExcluded clears only the exclusion, preserving personal inclusions. In
// particular it must not call ClearRepoOverride, which also removes inclusion.
func (s *StagedEdits) SetExcluded(name, repository string, excluded bool) error {
	p, err := s.profile(name, true)
	if err != nil {
		return err
	}
	id, err := NormalizeRepositoryID(repository)
	if err != nil {
		return errors.New("invalid repository identity")
	}
	if excluded {
		return s.current.ExcludeRepo(p.Name, id)
	}
	p.Selection.Exclude = withoutRepository(p.Selection.Exclude, id)
	return nil
}

func (s *StagedEdits) SetFollow(name string, follow bool) error {
	p, err := s.profile(name, true)
	if err != nil {
		return err
	}
	p.Selection.FollowNewRepositories = follow
	return nil
}

// SetHiveEnabled is deliberately allowed for legacy profiles without conversion.
func (s *StagedEdits) SetHiveEnabled(name string, enabled bool) error {
	p, err := s.profile(name, false)
	if err != nil {
		return err
	}
	return s.current.SetDisabled(p.Name, !enabled)
}

// branch resolves one subscription or an entire hub by canonical tree ID.
// Search/filter state is never accepted as branch membership.
func (s *StagedEdits) branch(id string, hub bool) ([]string, bool, error) {
	if err := s.editable(); err != nil {
		return nil, false, err
	}
	tree, err := s.Tree()
	if err != nil {
		return nil, false, errors.New("invalid contribution tree")
	}
	if hub {
		for _, h := range tree.Hubs {
			if h.ID == id {
				return h.Subscriptions, h.Selection.MembershipIncomplete, nil
			}
		}
		return nil, false, ErrHubNotFound
	}
	for _, p := range tree.Subscriptions {
		if p.ID == id {
			return []string{p.ID}, p.Selection.MembershipIncomplete, nil
		}
	}
	return nil, false, ErrProfileNotFound
}

// SelectCurrent operates atomically over the full named branch. Legacy profiles
// require ReviewAndRestrict first, including during deselection. DeselectCurrent
// retains follow policy; it is never SetSelectionMode("none").
func (s *StagedEdits) SelectCurrent(id string, hub, selected bool) error {
	names, incomplete, err := s.branch(id, hub)
	if err != nil {
		return err
	}
	if selected && incomplete {
		return ErrStageIncomplete
	}
	for _, name := range names {
		p, _ := s.current.Find(name)
		if p.Selection == nil {
			return ErrStageLegacy
		}
	}
	next := cloneStageSet(s.current)
	for _, name := range names {
		if selected {
			p, _ := next.Find(name)
			members := append([]string(nil), s.repos[name].Repositories...)
			// Selection is additive over the named branch. Keep saved targets
			// absent from discovery and preserve the existing refresh-source mode;
			// changing a preset selection to all-current could broaden following.
			if p.Selection.Mode == SelectionAllCurrent {
				members = append(members, p.Selection.MembershipSnapshot...)
			} else {
				members = append(members, p.Selection.Repositories...)
			}
			err = next.SetSelectionMode(name, p.Selection.Mode, members)
		} else {
			err = next.DeselectCurrent(name)
		}
		if err != nil {
			return errors.New("could not edit branch membership")
		}
	}
	s.current = next
	return nil
}

// ReviewAndRestrict is the explicit legacy conversion step. The complete
// discovery snapshot becomes scoped current membership with following off.
func (s *StagedEdits) ReviewAndRestrict(name string) error {
	p, err := s.profile(name, false)
	if err != nil {
		return err
	}
	if p.Selection != nil {
		return nil
	}
	snapshot := s.repos[p.Name]
	if snapshot.Discovery != TreeComplete {
		return ErrStageIncomplete
	}
	return s.current.SetSelectionMode(p.Name, SelectionAllCurrent, snapshot.Repositories)
}

// SetHubEnabled fans out to canonical shared subscriptions, retaining scope and
// Hive following. Disable also stops this tracked hub's future enrollment.
// Enable requires complete hub discovery (but not repository discovery).
func (s *StagedEdits) SetHubEnabled(id string, enabled bool) error {
	names, _, err := s.branch(id, true)
	if err != nil {
		return err
	}
	tree, _ := s.Tree()
	for _, h := range tree.Hubs {
		if h.ID == id && enabled && h.Discovery != TreeComplete {
			return ErrStageIncomplete
		}
	}
	var tracked *HubEntry
	for i := range s.current.Hubs {
		origin, _ := NormalizeHiveID(s.current.Hubs[i].URL)
		if origin == id {
			tracked = &s.current.Hubs[i]
			break
		}
	}
	// A discovery-only snapshot with no persisted policy cannot truthfully promise
	// to stop future enrollment. Direct Hives have no enrollment policy to stop.
	if tracked == nil && id != "direct" {
		return ErrHubNotFound
	}
	if !enabled && tracked != nil {
		tracked.Follow = HubFollowOff
	}
	for _, name := range names {
		_ = s.current.SetDisabled(name, !enabled)
	}
	return nil
}

// Discard drops only unsaved edits. Once profiles have been saved, projection
// repair must complete first. Discard never makes an already-stale review fresh.
func (s *StagedEdits) Discard() error {
	if s.projectionPending {
		return ErrStageProjection
	}
	s.current = cloneStageSet(s.before)
	return nil
}

// StageTarget names participation at one subscription, not a cross-Hive task ID.
type StageTarget struct {
	Subscription string `json:"subscription"`
	Repository   string `json:"repository"`
}

type StageSelectionChange struct {
	Subscription string               `json:"subscription"`
	Before       *RepositorySelection `json:"before"`
	After        *RepositorySelection `json:"after"`
}

type StageBoolChange struct {
	Target string `json:"target"`
	Before bool   `json:"before"`
	After  bool   `json:"after"`
}

type StageHubFollowChange struct {
	Hub    string `json:"hub"`
	Before string `json:"before"`
	After  string `json:"after"`
}

type StageSharedBranch struct {
	Subscription string   `json:"subscription"`
	Hubs         []string `json:"hubs"`
}

// StageExclusionEffect lists all remaining veto sources, scoped subscriptions
// with saved membership suppressed by the union, and enabled legacy profiles
// which can still receive this repository. Clearing one source need not restore
// scope if another source remains. Disabled scoped subscriptions are included.
type StageExclusionEffect struct {
	StageTarget
	Excluded            bool     `json:"excluded"`
	Sources             []string `json:"sources"`
	ScopedSubscriptions []string `json:"scoped_subscriptions"`
	LegacyUnrestricted  []string `json:"legacy_unrestricted"`
}

type ContributionDiff struct {
	Stale                 bool          `json:"stale"`
	Selected              []StageTarget `json:"selected"`
	Deselected            []StageTarget `json:"deselected"`
	SelectedCount         int           `json:"selected_count"`
	DeselectedCount       int           `json:"deselected_count"`
	ExcludedCount         int           `json:"excluded_count"`
	ClearedExclusionCount int           `json:"cleared_exclusion_count"`
	// Membership changes remain visible even when an exclusion masks the effect.
	Membership        []StageSelectionChange `json:"membership"`
	Exclusions        []StageExclusionEffect `json:"exclusions"`
	Follow            []StageBoolChange      `json:"follow"`
	Enabled           []StageBoolChange      `json:"enabled"`
	HubFollow         []StageHubFollowChange `json:"hub_follow"`
	LegacyConversions []string               `json:"legacy_conversions"`
	SharedBranches    []StageSharedBranch    `json:"shared_branches"`
}

func (d ContributionDiff) Empty() bool {
	return len(d.Membership) == 0 && len(d.Enabled) == 0 && len(d.HubFollow) == 0
}

func stageScopes(set *ProfileSet) map[string]map[string]bool {
	scopes, _ := set.RepositoryScopes() // All session state was validated at entry.
	result := map[string]map[string]bool{}
	for i, p := range set.Ordered() {
		result[p.Name] = map[string]bool{}
		if scopes[i] != "" && scopes[i] != "*" {
			for _, id := range strings.Split(scopes[i], ";") {
				result[p.Name][id] = true
			}
		}
	}
	return result
}

// Diff is deterministic and contains only consent, canonical IDs and profile
// names. No profiles, credentials, endpoints with queries, or relay errors escape.
func (s *StagedEdits) Diff() ContributionDiff {
	d := ContributionDiff{Stale: s.stale}
	beforeScopes, afterScopes := stageScopes(s.before), stageScopes(s.current)
	changed := map[string]bool{}
	profiles := append([]Profile(nil), s.current.Profiles...)
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	for _, p := range profiles {
		old, _ := s.before.Find(p.Name)
		before, after := cloneStageSelection(old.Selection), cloneStageSelection(p.Selection)
		if !reflect.DeepEqual(before, after) {
			d.Membership = append(d.Membership, StageSelectionChange{p.Name, before, after})
			changed[p.Name] = true
		}
		if old.Selection == nil && p.Selection != nil {
			d.LegacyConversions = append(d.LegacyConversions, p.Name)
		}
		oldFollow, newFollow := before != nil && before.FollowNewRepositories, after != nil && after.FollowNewRepositories
		if oldFollow != newFollow {
			d.Follow = append(d.Follow, StageBoolChange{p.Name, oldFollow, newFollow})
		}
		if old.Disabled != p.Disabled {
			d.Enabled = append(d.Enabled, StageBoolChange{p.Name, !old.Disabled, !p.Disabled})
			changed[p.Name] = true
		}
		for _, id := range stageChangedIDs(beforeScopes[p.Name], afterScopes[p.Name]) {
			target := StageTarget{p.Name, id}
			if afterScopes[p.Name][id] {
				d.Selected = append(d.Selected, target)
			} else {
				d.Deselected = append(d.Deselected, target)
			}
			changed[p.Name] = true
		}
		oldEx, newEx := stageExclusions(before), stageExclusions(after)
		for _, id := range stageChangedIDs(oldEx, newEx) {
			effect := StageExclusionEffect{StageTarget: StageTarget{p.Name, id}, Excluded: newEx[id]}
			if effect.Excluded {
				d.ExcludedCount++
			} else {
				d.ClearedExclusionCount++
			}
			for _, other := range profiles {
				if other.Selection == nil {
					if !other.Disabled {
						effect.LegacyUnrestricted = append(effect.LegacyUnrestricted, other.Name)
					}
					continue
				}
				if stageExclusions(other.Selection)[id] {
					effect.Sources = append(effect.Sources, other.Name)
				}
				// Include memberships already suppressed by another source, and targets
				// just removed, so every relevant linked effect can be reviewed.
				previous, _ := s.before.Find(other.Name)
				if treeSavedMember(other.Selection, id) || previous.Selection != nil && treeSavedMember(previous.Selection, id) {
					effect.ScopedSubscriptions = append(effect.ScopedSubscriptions, other.Name)
					changed[other.Name] = true
				}
			}
			d.Exclusions = append(d.Exclusions, effect)
		}
	}
	for _, h := range s.current.Hubs {
		old, _ := s.before.FindHub(h.Name)
		if old.EffectiveFollow() != h.EffectiveFollow() {
			id, _ := NormalizeHiveID(h.URL)
			d.HubFollow = append(d.HubFollow, StageHubFollowChange{id, old.EffectiveFollow(), h.EffectiveFollow()})
		}
	}
	sort.Slice(d.HubFollow, func(i, j int) bool { return d.HubFollow[i].Hub < d.HubFollow[j].Hub })
	tree, _ := s.Tree()
	for _, p := range tree.Subscriptions {
		if !changed[p.ID] || p.SharedAcrossHubs < 2 {
			continue
		}
		shared := StageSharedBranch{Subscription: p.ID}
		for _, h := range tree.Hubs {
			for _, name := range h.Subscriptions {
				if name == p.ID && !h.Direct {
					shared.Hubs = append(shared.Hubs, h.ID)
				}
			}
		}
		d.SharedBranches = append(d.SharedBranches, shared)
	}
	d.SelectedCount, d.DeselectedCount = len(d.Selected), len(d.Deselected)
	return d
}

func stageExclusions(selection *RepositorySelection) map[string]bool {
	ids := map[string]bool{}
	if selection != nil {
		for _, raw := range selection.Exclude {
			id, _ := NormalizeRepositoryID(raw)
			ids[id] = true
		}
	}
	return ids
}

func stageChangedIDs(before, after map[string]bool) []string {
	var result []string
	for id := range before {
		if !after[id] {
			result = append(result, id)
		}
	}
	for id := range after {
		if !before[id] {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}

const (
	StageNotSaved           = "not-saved"
	StageUnchanged          = "unchanged"
	StageProfilesSaved      = "profiles-saved/projection-failed"
	StageSaved              = "profiles-and-projection-saved"
	StageReloadNotRequested = "not-requested"
	StageReloadNotified     = "notified"
	StageReloadFailed       = "pending-failed"
	StageReloadNoManaged    = "no-managed-relay-found — saved for next start"
)

// ApplyReceipt reports three independent facts. ScopeVersion identifies the
// saved generation, but is not evidence that any relay adopted that generation.
// Raw persistence/command errors are intentionally excluded (they may be secret).
type ApplyReceipt struct {
	Saved        string `json:"saved"`
	RelayReload  string `json:"relay_reload"`
	Acknowledged string `json:"acknowledged"`
	ScopeVersion uint64 `json:"scope_version,omitempty"`
}

// Apply saves through the guarded shared store, then requests relay reload.
// Empty diffs do neither. A partial save retains the reviewed diff and advances
// only the expected revision, allowing an explicit retry without overwriting a
// concurrent edit. Successful persistence consumes the diff even if reload fails.
func (s *StagedEdits) Apply(ctx context.Context, store *ProfileStore) (ApplyReceipt, error) {
	receipt := ApplyReceipt{Saved: StageNotSaved, RelayReload: StageReloadNotRequested, Acknowledged: "unknown"}
	if s.projectionPending {
		receipt.Saved, receipt.ScopeVersion = StageProfilesSaved, s.current.RepoScopeVersion
	}
	if s.stale {
		return receipt, ErrStaleRevision
	}
	if s.Diff().Empty() {
		receipt.Saved = StageUnchanged
		return receipt, nil
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if store == nil {
		return receipt, ErrStageSave
	}
	previous := s.current.Revision()
	err := store.CommitExpecting(s.current, s.revision)
	if err != nil {
		if errors.Is(err, ErrStaleRevision) {
			s.stale = true
			return receipt, ErrStaleRevision
		}
		// Save advances Revision only after profiles.yml is written; Commit always
		// increments the scope generation, so a successful save changes its hash.
		if s.current.Revision() != previous {
			s.revision = s.current.Revision()
			s.projectionPending = true
			receipt.Saved, receipt.ScopeVersion = StageProfilesSaved, s.current.RepoScopeVersion
			return receipt, ErrStageProjection
		}
		return receipt, ErrStageSave
	}
	s.revision = s.current.Revision()
	s.projectionPending = false
	s.before = cloneStageSet(s.current)
	receipt.Saved, receipt.ScopeVersion = StageSaved, s.current.RepoScopeVersion
	receipt.Acknowledged = "pending"
	result, err := s.signal(ctx, store)
	if err != nil {
		receipt.RelayReload = StageReloadFailed
		return receipt, ErrStageReload
	}
	if result.Running {
		receipt.RelayReload = StageReloadNotified
	} else {
		receipt.RelayReload = StageReloadNoManaged
	}
	return receipt, nil
}
