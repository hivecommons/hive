package hivectl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PresetReviewID derives the local provenance key for a preset from its name.
// It is what Selection.Sources and PresetProvenance.ID record.
func PresetReviewID(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	id := b.String()
	if len(id) > 64 {
		id = id[:64]
	}
	id = strings.Trim(id, "-")
	if id == "" {
		return "preset"
	}
	return id
}

// PresetRevision combines the publisher's opaque revision label with a digest
// of the validated document, so an edited preset under an unchanged label is
// still a new revision to review.
func PresetRevision(preset *Preset) (string, error) {
	data, err := json.Marshal(preset)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])[:16]
	if preset.Revision != "" {
		return preset.Revision + "@" + digest, nil
	}
	return digest, nil
}

// PresetHiveRepositories returns the canonical IDs of an explicit
// recommendation. all-current needs live discovery and is the caller's job.
func PresetHiveRepositories(hive PresetHive) ([]string, error) {
	ids := make([]string, 0, len(hive.Repositories.Repos))
	for _, repo := range hive.Repositories.Repos {
		ids = append(ids, repo.Forge+"/"+repo.Host+"/"+repo.Repo)
	}
	return canonicalRepositorySet(ids)
}

// PresetDiff compares a recommendation with a hive's saved reviewed membership.
// Additions never include personally excluded repositories (from any profile,
// matching projection precedence); those are reported in Excluded instead.
// Removals are saved members the recommendation no longer contains.
type PresetDiff struct {
	Additions []string `json:"additions,omitempty" yaml:"additions,omitempty"`
	Removals  []string `json:"removals,omitempty" yaml:"removals,omitempty"`
	Excluded  []string `json:"excluded,omitempty" yaml:"excluded,omitempty"`
}

func (selection *RepositorySelection) reviewedBase() []string {
	if selection == nil {
		return nil
	}
	if selection.Mode == SelectionAllCurrent {
		return selection.MembershipSnapshot
	}
	return selection.Repositories
}

// DiffPreset computes the review diff for one hive. A legacy unrestricted
// profile has no reviewed membership, so nothing is a removal.
func (set *ProfileSet) DiffPreset(name string, recommended []string) (PresetDiff, error) {
	var diff PresetDiff
	p, _ := set.Find(name)
	if p == nil {
		return diff, fmt.Errorf("unknown hive %q", name)
	}
	recommended, err := canonicalRepositorySet(recommended)
	if err != nil {
		return diff, err
	}
	excluded := map[string]bool{}
	for _, profile := range set.Profiles {
		if profile.Selection != nil {
			for _, id := range profile.Selection.Exclude {
				canonical, _ := NormalizeRepositoryID(id)
				excluded[canonical] = true
			}
		}
	}
	known := map[string]bool{}
	base, _ := canonicalRepositorySet(p.Selection.reviewedBase())
	for _, id := range base {
		known[id] = true
	}
	if p.Selection != nil {
		for _, id := range p.Selection.Include {
			canonical, _ := NormalizeRepositoryID(id)
			known[canonical] = true
		}
	}
	wanted := map[string]bool{}
	for _, id := range recommended {
		wanted[id] = true
		switch {
		case excluded[id]:
			diff.Excluded = append(diff.Excluded, id)
		case !known[id]:
			diff.Additions = append(diff.Additions, id)
		}
	}
	for _, id := range base {
		if !wanted[id] {
			diff.Removals = append(diff.Removals, id)
		}
	}
	return diff, nil
}

// ApplyPreset records reviewed preset membership for one hive: accepted
// additions join the reviewed base, and only removals passed in are dropped.
// Personal inclusions and exclusions are never touched. A legacy unrestricted
// profile becomes an explicit selection, which may be empty (idle), never a
// wildcard. It reports whether anything changed.
func (set *ProfileSet) ApplyPreset(name, presetID string, allCurrent bool, add, remove []string) (bool, error) {
	p, _ := set.Find(name)
	if p == nil {
		return false, fmt.Errorf("unknown hive %q", name)
	}
	if err := ValidateProfileName(presetID); err != nil {
		return false, fmt.Errorf("preset id: %w", err)
	}
	add, err := canonicalRepositorySet(add)
	if err != nil {
		return false, err
	}
	remove, err = canonicalRepositorySet(remove)
	if err != nil {
		return false, err
	}
	next := RepositorySelection{Mode: SelectionSelected}
	if allCurrent {
		next.Mode = SelectionAllCurrent
	}
	if p.Selection != nil {
		next = *p.Selection
	}
	base, err := canonicalRepositorySet(append(append([]string{}, next.reviewedBase()...), add...))
	if err != nil {
		return false, err
	}
	for _, id := range remove {
		base = withoutRepository(base, id)
		dropAuto(&next, id)
	}
	if len(base) == 0 {
		base = nil
	}
	if next.Mode == SelectionAllCurrent {
		next.MembershipSnapshot = base
	} else {
		next.Repositories = base
	}
	sources := append([]string{}, next.Sources...)
	found := false
	for _, source := range sources {
		if strings.EqualFold(source, presetID) {
			found = true
		}
	}
	if !found {
		sources = append(sources, presetID)
		sort.Strings(sources)
	}
	next.Sources = sources
	if err := next.Validate(); err != nil {
		return false, err
	}
	changed := p.Selection == nil || !found || len(add) > 0 || len(remove) > 0
	p.Selection = &next
	return changed, nil
}

// HasPresetSource reports whether a hive's selection was adopted from a preset.
func (selection *RepositorySelection) HasPresetSource(presetID string) bool {
	if selection == nil {
		return false
	}
	for _, source := range selection.Sources {
		if strings.EqualFold(source, presetID) {
			return true
		}
	}
	return false
}

// SetFollowNewRepositories stores the per-hive "include future additions"
// policy. It needs a reviewed selection: following never turns a profile into
// an unrestricted wildcard.
func (set *ProfileSet) SetFollowNewRepositories(name string, follow bool) (bool, error) {
	p, _ := set.Find(name)
	if p == nil {
		return false, fmt.Errorf("unknown hive %q", name)
	}
	if p.Selection == nil {
		return false, fmt.Errorf("hive %q has no reviewed selection to follow", name)
	}
	if p.Selection.FollowNewRepositories == follow {
		return false, nil
	}
	p.Selection.FollowNewRepositories = follow
	return true, nil
}

// FindPreset returns the recorded review of a preset, if any.
func (set *ProfileSet) FindPreset(id string) *PresetProvenance {
	for i := range set.Presets {
		if strings.EqualFold(set.Presets[i].ID, id) {
			return &set.Presets[i]
		}
	}
	return nil
}

// RecordPresetReview stores the reviewed preset revision. Re-recording the same
// source and revision is a no-op, so idempotent re-adoption leaves the scope
// generation alone.
func (set *ProfileSet) RecordPresetReview(id, source, revision string, at time.Time) bool {
	if existing := set.FindPreset(id); existing != nil {
		if existing.Source == source && existing.ReviewedRevision == revision {
			return false
		}
		existing.Source, existing.ReviewedRevision, existing.AcceptedAt = source, revision, at
		return true
	}
	set.Presets = append(set.Presets, PresetProvenance{ID: id, Source: source, ReviewedRevision: revision, AcceptedAt: at})
	return true
}
