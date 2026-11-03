package hivectl

import (
	"fmt"
	"sort"
	"strings"
)

// SetSelectionMode changes reviewed membership without erasing personal exclusions.
// none is an explicit empty selection, never the legacy wildcard.
// Scope generations are advanced by ProfileStore.Commit, alongside persistence.
func (set *ProfileSet) SetSelectionMode(name, mode string, repositories []string) error {
	p, _ := set.Find(name)
	if p == nil {
		return fmt.Errorf("unknown hive %q", name)
	}
	if mode != SelectionSelected && mode != SelectionAllCurrent && mode != "none" {
		return fmt.Errorf("invalid selection mode %q", mode)
	}
	ids, err := canonicalRepositorySet(repositories)
	if err != nil {
		return err
	}
	next := RepositorySelection{Mode: SelectionSelected}
	if p.Selection != nil {
		next = *p.Selection
	}
	if mode == "none" {
		next.Mode = SelectionSelected
		next.Repositories, next.Include, next.MembershipSnapshot = nil, nil, nil
		next.FollowNewRepositories = false
	} else if mode == SelectionAllCurrent {
		next.Mode, next.MembershipSnapshot = mode, ids
	} else {
		next.Mode, next.Repositories = mode, ids
	}
	next.Auto = nil // re-choosing the membership by hand makes every member a deliberate one
	p.Selection = &next
	return nil
}

// DeselectCurrent clears reviewed membership and personal inclusions, but keeps
// exclusions, provenance and follow policy. A later approved refresh may select
// repositories again. Legacy unrestricted profiles require an explicit scope
// conversion instead of being silently migrated by a tree checkbox.
func (set *ProfileSet) DeselectCurrent(name string) error {
	p, _ := set.Find(name)
	if p == nil {
		return fmt.Errorf("unknown hive %q", name)
	}
	if p.Selection == nil {
		return fmt.Errorf("hive %q is unrestricted; review --select all-current first, or use --select none to select nothing and stop following", name)
	}
	next := *p.Selection
	next.Mode = SelectionSelected
	next.Repositories, next.Include, next.MembershipSnapshot = nil, nil, nil
	next.Auto = nil
	p.Selection = &next
	return nil
}

// AddAuto adds repositories that a followed Hive newly offers to the reviewed
// membership and records them as automatic. Anything already a member or
// excluded on this subscription is skipped, so a repository the user turned off
// by hand is never re-added. It returns how many were added. A legacy
// unrestricted profile has no reviewed membership to add to.
func (set *ProfileSet) AddAuto(name string, ids []string) (int, error) {
	p, _ := set.Find(name)
	if p == nil {
		return 0, fmt.Errorf("unknown hive %q", name)
	}
	if p.Selection == nil {
		return 0, fmt.Errorf("hive %q is unrestricted; restrict it before following", name)
	}
	canonical, err := canonicalRepositorySet(ids)
	if err != nil {
		return 0, err
	}
	next := *p.Selection
	known := map[string]bool{}
	// Same definition of "already a member" as treeSavedMember: the snapshot only
	// counts in all-current mode. A stale snapshot beside mode "selected" is not membership.
	for _, list := range [][]string{next.reviewedBase(), next.Include, next.Exclude} {
		for _, raw := range list {
			id, _ := NormalizeRepositoryID(raw)
			known[id] = true
		}
	}
	var fresh []string
	for _, id := range canonical {
		if !known[id] {
			fresh = append(fresh, id)
		}
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	if next.Mode == SelectionAllCurrent {
		next.MembershipSnapshot, err = canonicalRepositorySet(append(append([]string{}, next.MembershipSnapshot...), fresh...))
	} else {
		next.Repositories, err = canonicalRepositorySet(append(append([]string{}, next.Repositories...), fresh...))
	}
	if err != nil {
		return 0, err
	}
	if next.Auto, err = canonicalRepositorySet(append(append([]string{}, next.Auto...), fresh...)); err != nil {
		return 0, err
	}
	p.Selection = &next
	return len(fresh), nil
}

// MarkAuto records that members already in the reviewed membership were added
// by following the Hive (used by `preset refresh`, which adds them through
// ApplyPreset). Personal inclusions are never marked: those were chosen by hand.
func (set *ProfileSet) MarkAuto(name string, ids []string) error {
	p, _ := set.Find(name)
	if p == nil {
		return fmt.Errorf("unknown hive %q", name)
	}
	if p.Selection == nil {
		return nil
	}
	canonical, err := canonicalRepositorySet(ids)
	if err != nil {
		return err
	}
	inBase, inInclude := map[string]bool{}, map[string]bool{}
	for _, raw := range p.Selection.reviewedBase() {
		id, _ := NormalizeRepositoryID(raw)
		inBase[id] = true
	}
	for _, raw := range p.Selection.Include {
		id, _ := NormalizeRepositoryID(raw)
		inInclude[id] = true
	}
	var mark []string
	for _, id := range canonical {
		if inBase[id] && !inInclude[id] {
			mark = append(mark, id)
		}
	}
	if len(mark) == 0 {
		return nil
	}
	next := *p.Selection
	if next.Auto, err = canonicalRepositorySet(append(append([]string{}, next.Auto...), mark...)); err != nil {
		return err
	}
	p.Selection = &next
	return nil
}

// dropAuto forgets that id was automatic, keeping a nil slice when none remain
// so selections without automatic members stay byte-identical on disk.
func dropAuto(selection *RepositorySelection, id string) {
	if selection == nil || len(selection.Auto) == 0 {
		return
	}
	selection.Auto = withoutRepository(selection.Auto, id)
	if len(selection.Auto) == 0 {
		selection.Auto = nil
	}
}

func canonicalRepositorySet(repositories []string) ([]string, error) {
	members := map[string]bool{}
	for _, id := range repositories {
		canonical, err := NormalizeRepositoryID(id)
		if err != nil {
			return nil, err
		}
		members[canonical] = true
	}
	result := make([]string, 0, len(members))
	for id := range members {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func withoutRepository(ids []string, target string) []string {
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		canonical, _ := NormalizeRepositoryID(id)
		if canonical != target {
			result = append(result, canonical)
		}
	}
	return result
}

// ToggleRepoExclusion changes only the personal exclusion, retaining additions.
func (set *ProfileSet) ToggleRepoExclusion(name, id string) error {
	canonical, err := NormalizeRepositoryID(id)
	if err != nil {
		return err
	}
	p, _ := set.Find(name)
	if p == nil {
		return fmt.Errorf("unknown hive %q", name)
	}
	if p.Selection != nil {
		for _, excluded := range p.Selection.Exclude {
			normalized, _ := NormalizeRepositoryID(excluded)
			if normalized == canonical {
				p.Selection.Exclude = withoutRepository(p.Selection.Exclude, canonical)
				return nil
			}
		}
	}
	return set.ExcludeRepo(name, canonical)
}

func (set *ProfileSet) IncludeRepo(name, id string) error {
	return set.repoOverride(name, id, "include")
}
func (set *ProfileSet) ExcludeRepo(name, id string) error {
	return set.repoOverride(name, id, "exclude")
}
func (set *ProfileSet) ClearRepoOverride(name, id string) error {
	return set.repoOverride(name, id, "clear")
}

func (set *ProfileSet) repoOverride(name, id, operation string) error {
	canonical, err := NormalizeRepositoryID(id)
	if err != nil {
		return err
	}
	p, _ := set.Find(name)
	if p == nil {
		return fmt.Errorf("unknown hive %q", name)
	}
	if p.Selection == nil {
		if operation == "clear" {
			return nil
		}
		// An all-minus-one wildcard cannot be represented by a reviewed snapshot.
		if operation == "exclude" {
			return fmt.Errorf("hive %q is unrestricted; select all-current before excluding a repository", name)
		}
		p.Selection = &RepositorySelection{Mode: SelectionSelected}
	}
	switch operation {
	case "include":
		p.Selection.Include, err = canonicalRepositorySet(append(append([]string{}, p.Selection.Include...), canonical))
	case "exclude":
		p.Selection.Exclude, err = canonicalRepositorySet(append(append([]string{}, p.Selection.Exclude...), canonical))
	case "clear":
		p.Selection.Include = withoutRepository(p.Selection.Include, canonical)
		p.Selection.Exclude = withoutRepository(p.Selection.Exclude, canonical)
	}
	if err == nil && (operation == "include" || operation == "exclude") {
		dropAuto(p.Selection, canonical) // touched by hand: no longer automatic
	}
	return err
}

// ClearSelection is the only operation that restores legacy unrestricted scope.
// Interactive callers must obtain explicit confirmation before invoking it.
func (set *ProfileSet) ClearSelection(name string) error {
	p, _ := set.Find(name)
	if p == nil {
		return fmt.Errorf("unknown hive %q", name)
	}
	p.Selection = nil
	return nil
}

// SelectionSummary reflects the same cross-profile exclusion precedence as the
// generated relay projection, rather than just counting one profile's entries.
func (set *ProfileSet) SelectionSummary(name string) string {
	scopes, err := set.RepositoryScopes()
	if err != nil {
		return "invalid"
	}
	for i, p := range set.Ordered() {
		if strings.EqualFold(p.Name, name) {
			if scopes[i] == "*" {
				return "all"
			}
			if scopes[i] == "" {
				return "none"
			}
			return fmt.Sprintf("%d repos", len(strings.Split(scopes[i], ";")))
		}
	}
	return "invalid"
}

// RepositoryInput accepts a canonical ID or an owner/name with explicit forge
// defaults. A bare display name can never identify a repository.
func RepositoryInput(repository, forge, host string) (string, error) {
	lower := strings.ToLower(repository)
	if strings.Count(lower, "/") >= 3 && (strings.HasPrefix(lower, "github/") || strings.HasPrefix(lower, "gitlab/") || strings.HasPrefix(lower, "gitea/")) {
		return NormalizeRepositoryID(repository)
	}
	return NormalizeRepositoryID(forge + "/" + host + "/" + repository)
}
