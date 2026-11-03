package hivectl

import (
	"fmt"
	"sort"
)

// TreeDiscovery distinguishes an empty successful enumeration from missing,
// stale or failed discovery. Unknown is the zero value (saved state only).
type TreeDiscovery string

const (
	TreeUnknown     TreeDiscovery = ""
	TreeComplete    TreeDiscovery = "complete"
	TreeUnavailable TreeDiscovery = "unavailable"
	TreeStale       TreeDiscovery = "stale"
)

// TreeHubSnapshot is supplied by the hub discovery adapter. Origins match all
// saved subscriptions at that origin, without choosing or merging credentials.
// An unavailable hub may retain its last known HiveOrigins.
type TreeHubSnapshot struct {
	Name        string
	URL         string
	Follow      string
	Discovery   TreeDiscovery
	HiveOrigins []string
}

type TreeRepositorySnapshot struct {
	Discovery    TreeDiscovery
	Repositories []string
}

type TreeCounts struct {
	Repositories int `json:"repositories" yaml:"repositories"`
	Selected     int `json:"selected" yaml:"selected"`
	Excluded     int `json:"excluded" yaml:"excluded"`
	Unreviewed   int `json:"unreviewed" yaml:"unreviewed"`
	// Auto counts selected repositories that were added by following the Hive.
	Auto int `json:"auto" yaml:"auto"`
}

type TreeSelection struct {
	TreeCounts           `yaml:",inline"`
	State                string `json:"state" yaml:"state"`
	MembershipIncomplete bool   `json:"membership_incomplete" yaml:"membership_incomplete"`
	Legacy               bool   `json:"legacy_unrestricted" yaml:"legacy_unrestricted"`
}

// ContributionTree is a credential-free, derived read model. Hub appearances
// reference one subscription record; rollups deduplicate subscription/repository
// pairs, never repositories across independently operated Hives. Subscription
// keys are current profile names, not durable historical identities.
type ContributionTree struct {
	Hubs          []TreeHub          `json:"hubs" yaml:"hubs"`
	Subscriptions []TreeSubscription `json:"subscriptions" yaml:"subscriptions"`
	Selection     TreeSelection      `json:"selection" yaml:"selection"`
}
type TreeHub struct {
	ID            string        `json:"id" yaml:"id"`
	Name          string        `json:"name" yaml:"name"`
	Direct        bool          `json:"direct" yaml:"direct"`
	Follow        string        `json:"follow" yaml:"follow"`
	Discovery     TreeDiscovery `json:"discovery" yaml:"discovery"`
	Subscriptions []string      `json:"subscriptions" yaml:"subscriptions"`
	Selection     TreeSelection `json:"selection" yaml:"selection"`
}
type TreeSubscription struct {
	ID               string           `json:"id" yaml:"id"`
	HiveID           string           `json:"hive_id" yaml:"hive_id"`
	Disabled         bool             `json:"disabled" yaml:"disabled"`
	SharedAcrossHubs int              `json:"shared_across_hubs" yaml:"shared_across_hubs"`
	Follow           bool             `json:"follow_new_repositories" yaml:"follow_new_repositories"`
	Discovery        TreeDiscovery    `json:"discovery" yaml:"discovery"`
	Readiness        string           `json:"readiness" yaml:"readiness"`
	Activity         string           `json:"activity" yaml:"activity"`
	Sources          []string         `json:"selection_sources,omitempty" yaml:"selection_sources,omitempty"`
	Repositories     []TreeRepository `json:"repositories" yaml:"repositories"`
	Selection        TreeSelection    `json:"selection" yaml:"selection"`
}
type TreeRepository struct {
	ID         string   `json:"id" yaml:"id"`
	Selected   bool     `json:"selected" yaml:"selected"`
	ExcludedBy []string `json:"excluded_by,omitempty" yaml:"excluded_by,omitempty"`
	// Auto is true for a selected repository that was added by following the
	// Hive rather than chosen by hand.
	Auto       bool `json:"auto" yaml:"auto"`
	Unreviewed bool `json:"unreviewed" yaml:"unreviewed"`
	// Offered is unknown unless discovery is complete. Saved consent survives
	// an outage or removal; it is not evidence of runtime eligibility.
	Offered string `json:"offered" yaml:"offered"`
}

func finishTreeSelection(s *TreeSelection) {
	s.State = "off"
	if s.Selected > 0 {
		s.State = "partial"
		if s.Selected == s.Repositories && !s.MembershipIncomplete && !s.Legacy {
			s.State = "all-current"
		}
	}
}
func addTreeSelection(dst *TreeSelection, src TreeSelection) {
	dst.Repositories += src.Repositories
	dst.Selected += src.Selected
	dst.Excluded += src.Excluded
	dst.Unreviewed += src.Unreviewed
	dst.Auto += src.Auto
	dst.MembershipIncomplete = dst.MembershipIncomplete || src.MembershipIncomplete
	dst.Legacy = dst.Legacy || src.Legacy
}

// BuildContributionTree performs no I/O and never modifies consent. repos is
// keyed by saved subscription name, deliberately not by Hive origin. A caller
// must explicitly supply complete discovery; nil snapshots mean unknown, not
// empty. Selection sources are selection-level attribution, not repo ownership.
func BuildContributionTree(set *ProfileSet, hubs []TreeHubSnapshot, repos map[string]TreeRepositorySnapshot) (*ContributionTree, error) {
	if set == nil {
		return nil, fmt.Errorf("nil profile set")
	}
	tree := &ContributionTree{Hubs: []TreeHub{}, Subscriptions: []TreeSubscription{}}
	excluded := map[string][]string{}
	for _, p := range set.Profiles {
		if err := p.Selection.Validate(); err != nil {
			return nil, err
		}
		if p.Selection == nil {
			continue
		}
		seen := map[string]bool{}
		for _, raw := range p.Selection.Exclude {
			id, _ := NormalizeRepositoryID(raw)
			if !seen[id] {
				excluded[id] = append(excluded[id], p.Name)
				seen[id] = true
			}
		}
	}
	for id := range excluded {
		sort.Strings(excluded[id])
	}
	byID := map[string]int{}
	for _, p := range set.Profiles {
		if _, exists := byID[p.Name]; exists {
			return nil, fmt.Errorf("duplicate subscription name")
		}
		origin, err := NormalizeHiveID(p.Hub)
		if err != nil {
			return nil, fmt.Errorf("invalid hive origin for subscription %q", p.Name)
		}
		snapshot := repos[p.Name]
		if !validTreeDiscovery(snapshot.Discovery) {
			return nil, fmt.Errorf("invalid repository discovery state")
		}
		s := TreeSubscription{ID: p.Name, HiveID: origin, Disabled: p.Disabled, Discovery: snapshot.Discovery, Readiness: "unknown", Activity: "unavailable", Repositories: []TreeRepository{}}
		if p.Disabled {
			s.Readiness = "disabled"
		}
		s.Selection.Legacy = p.Selection == nil
		s.Selection.MembershipIncomplete = snapshot.Discovery != TreeComplete
		members, offered, selected := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, raw := range snapshot.Repositories {
			id, err := NormalizeRepositoryID(raw)
			if err != nil {
				return nil, err
			}
			members[id], offered[id] = true, true
		}
		if p.Selection != nil {
			s.Follow = p.Selection.FollowNewRepositories
			s.Sources = append([]string(nil), p.Selection.Sources...)
			for _, list := range [][]string{p.Selection.Repositories, p.Selection.MembershipSnapshot, p.Selection.Include, p.Selection.Exclude} {
				for _, raw := range list {
					id, _ := NormalizeRepositoryID(raw)
					members[id] = true
				}
			}
			effective, _ := p.Selection.EffectiveRepositories()
			for _, id := range effective {
				selected[id] = len(excluded[id]) == 0
			}
		}
		for id := range members {
			r := TreeRepository{ID: id, Selected: selected[id], Offered: "unknown"}
			if snapshot.Discovery == TreeComplete {
				r.Offered = "no"
				if offered[id] {
					r.Offered = "yes"
				}
			}
			if p.Selection != nil {
				r.ExcludedBy = append([]string(nil), excluded[id]...)
				r.Unreviewed = offered[id] && !selected[id] && len(r.ExcludedBy) == 0 && !treeSavedMember(p.Selection, id)
				r.Auto = r.Selected && p.Selection.IsAuto(id)
			}
			s.Repositories = append(s.Repositories, r)
			s.Selection.Repositories++
			if r.Selected {
				s.Selection.Selected++
			}
			if len(r.ExcludedBy) > 0 {
				s.Selection.Excluded++
			}
			if r.Unreviewed {
				s.Selection.Unreviewed++
			}
			if r.Auto {
				s.Selection.Auto++
			}
		}
		sort.Slice(s.Repositories, func(i, j int) bool { return s.Repositories[i].ID < s.Repositories[j].ID })
		finishTreeSelection(&s.Selection)
		byID[p.Name] = len(tree.Subscriptions)
		tree.Subscriptions = append(tree.Subscriptions, s)
		addTreeSelection(&tree.Selection, s.Selection)
	}
	seenHubs := map[string]bool{}
	for _, input := range hubs {
		id, err := NormalizeHiveID(input.URL)
		if err != nil {
			return nil, fmt.Errorf("invalid hub origin")
		}
		if seenHubs[id] {
			return nil, fmt.Errorf("duplicate hub origin")
		}
		seenHubs[id] = true
		if input.Follow == "" {
			input.Follow = "off"
		}
		switch input.Follow {
		case "off", "current", "current-and-future-repos":
		default:
			return nil, fmt.Errorf("invalid hub follow policy")
		}
		if !validTreeDiscovery(input.Discovery) {
			return nil, fmt.Errorf("invalid hub discovery state")
		}
		h := TreeHub{ID: id, Name: input.Name, Follow: input.Follow, Discovery: input.Discovery, Subscriptions: []string{}}
		origins := map[string]bool{}
		for _, raw := range input.HiveOrigins {
			origin, err := NormalizeHiveID(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid advertised hive origin")
			}
			origins[origin] = true
		}
		for i := range tree.Subscriptions {
			s := &tree.Subscriptions[i]
			if origins[s.HiveID] {
				h.Subscriptions = append(h.Subscriptions, s.ID)
				s.SharedAcrossHubs++
				addTreeSelection(&h.Selection, s.Selection)
			}
		}
		h.Selection.MembershipIncomplete = h.Selection.MembershipIncomplete || input.Discovery != TreeComplete
		tree.Selection.MembershipIncomplete = tree.Selection.MembershipIncomplete || h.Selection.MembershipIncomplete
		sort.Strings(h.Subscriptions)
		finishTreeSelection(&h.Selection)
		tree.Hubs = append(tree.Hubs, h)
	}
	sort.Slice(tree.Hubs, func(i, j int) bool { return tree.Hubs[i].ID < tree.Hubs[j].ID })
	direct := TreeHub{ID: "direct", Name: "Direct Hives", Direct: true, Follow: "off", Discovery: TreeComplete, Subscriptions: []string{}}
	for _, s := range tree.Subscriptions {
		if s.SharedAcrossHubs == 0 {
			direct.Subscriptions = append(direct.Subscriptions, s.ID)
			addTreeSelection(&direct.Selection, s.Selection)
		}
	}
	if len(direct.Subscriptions) > 0 {
		sort.Strings(direct.Subscriptions)
		finishTreeSelection(&direct.Selection)
		tree.Hubs = append(tree.Hubs, direct)
	}
	sort.Slice(tree.Subscriptions, func(i, j int) bool { return tree.Subscriptions[i].ID < tree.Subscriptions[j].ID })
	finishTreeSelection(&tree.Selection)
	return tree, nil
}
func validTreeDiscovery(s TreeDiscovery) bool {
	return s == TreeUnknown || s == TreeComplete || s == TreeUnavailable || s == TreeStale
}
func treeSavedMember(s *RepositorySelection, id string) bool {
	base := s.Repositories
	if s.Mode == SelectionAllCurrent {
		base = s.MembershipSnapshot
	}
	for _, list := range [][]string{base, s.Include} {
		for _, raw := range list {
			canonical, _ := NormalizeRepositoryID(raw)
			if canonical == id {
				return true
			}
		}
	}
	return false
}
