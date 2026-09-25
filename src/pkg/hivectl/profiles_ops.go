package hivectl

import (
	"errors"
	"fmt"
	"strings"
)

// The five mutations `hivectl hives` and the TUI's Hives pane (#8128) share.
//
// Before this file the mutations lived in pkg/hivectl/commands as four-line
// sequences inside the cobra RunE bodies — find, set, Save, WriteEnvProjection.
// That was fine while the CLI was the only surface. It stops being fine the
// moment a second one exists, because every one of those sequences carries a
// rule that is easy to get subtly wrong in a copy: removing the ACTIVE profile
// has to re-elect one, renaming the active profile has to carry `Active` with
// it, and every mutation has to write profiles.yml BEFORE the projection.
//
// So the rules live here, on the set and the store, and both surfaces call
// them. The phrasing of the messages an operator reads stays with each surface
// — a TUI overlay and a shell command do not give the same advice — which is
// why these return sentinel errors rather than prose.

// ErrProfileExists reports that the requested name is already taken.
var ErrProfileExists = errors.New("hive profile already exists")

// Use marks name as the active profile.
//
// It does NOT write: callers pair it with Commit, so a validation failure
// cannot leave a half-applied switch on disk. already reports that the profile
// was active before the call, which callers use to say "was already active"
// rather than claiming a switch that did not happen.
func (set *ProfileSet) Use(name string) (profile Profile, already bool, err error) {
	target, _ := set.Find(name)
	if target == nil {
		return Profile{}, false, fmt.Errorf("%w: %q", ErrProfileNotFound, name)
	}
	already = strings.EqualFold(set.Active, target.Name)
	set.Active = target.Name
	return *target, already, nil
}

// Add appends a profile, optionally making it active.
//
// The FIRST profile added to an empty set is always made active regardless of
// the flag: a set with profiles and no active one resolves to the first entry
// anyway (ActiveProfile), so recording it is the difference between a stated
// fact and one the reader has to know a rule to infer.
func (set *ProfileSet) Add(profile Profile, activate bool) error {
	if err := ValidateProfileName(profile.Name); err != nil {
		return err
	}
	if clash, _ := set.Find(profile.Name); clash != nil {
		return fmt.Errorf("%w: %q (%s)", ErrProfileExists, clash.Name, clash.Hub)
	}
	if set.Version == 0 {
		set.Version = ProfilesVersion
	}
	set.Profiles = append(set.Profiles, profile)
	if activate || set.Active == "" {
		set.Active = profile.Name
	}
	return nil
}

// Rename changes a profile's name, carrying the active marker with it.
//
// A pure case change ("acme" -> "Acme") collides with itself under the
// case-insensitive uniqueness rule, so it is a clash only when the new name
// lands on a DIFFERENT profile.
func (set *ProfileSet) Rename(oldName, newName string) (Profile, error) {
	if err := ValidateProfileName(newName); err != nil {
		return Profile{}, err
	}
	target, _ := set.Find(oldName)
	if target == nil {
		return Profile{}, fmt.Errorf("%w: %q", ErrProfileNotFound, oldName)
	}
	if clash, _ := set.Find(newName); clash != nil && clash != target {
		return Profile{}, fmt.Errorf("%w: %q (%s)", ErrProfileExists, clash.Name, clash.Hub)
	}
	wasActive := strings.EqualFold(set.Active, target.Name)
	target.Name = newName
	if wasActive {
		set.Active = newName
	}
	return *target, nil
}

// Remove drops a profile and re-elects an active one when it was the active
// profile.
//
// The removed profile is returned by VALUE and before the slice is spliced, so
// a caller can still name the hive (and its hub) in the message it prints about
// a credential that no longer exists anywhere.
func (set *ProfileSet) Remove(name string) (removed Profile, wasActive bool, err error) {
	target, index := set.Find(name)
	if target == nil {
		return Profile{}, false, fmt.Errorf("%w: %q", ErrProfileNotFound, name)
	}
	removed = *target
	wasActive = strings.EqualFold(set.Active, removed.Name)
	set.Profiles = append(set.Profiles[:index], set.Profiles[index+1:]...)
	if wasActive {
		// An empty set keeps an empty Active rather than a dangling name:
		// Validate refuses an active that names no profile, so carrying the
		// removed name forward would make the very next Save fail.
		set.Active = ""
		if len(set.Profiles) > 0 {
			set.Active = set.Profiles[0].Name
		}
	}
	return removed, wasActive, nil
}

// Move shifts a profile by delta in The Commons rank order.
func (set *ProfileSet) Move(name string, delta int) (Profile, bool, error) {
	target, index := set.Find(name)
	if target == nil {
		return Profile{}, false, fmt.Errorf("%w: %q", ErrProfileNotFound, name)
	}
	next := index + delta
	if next < 0 {
		next = 0
	}
	if next >= len(set.Profiles) {
		next = len(set.Profiles) - 1
	}
	if next == index {
		return *target, false, nil
	}
	profile := set.Profiles[index]
	set.Profiles = append(set.Profiles[:index], set.Profiles[index+1:]...)
	set.Profiles = append(set.Profiles, Profile{})
	copy(set.Profiles[next+1:], set.Profiles[next:])
	set.Profiles[next] = profile
	if len(set.Profiles) > 0 {
		set.Active = set.Profiles[0].Name
	}
	return profile, true, nil
}

// SetCommonsStrategy records how the relay chooses the next subscribed hive.
func (set *ProfileSet) SetCommonsStrategy(strategy string) error {
	strategy = strings.ToLower(strings.TrimSpace(strategy))
	if err := ValidateCommonsStrategy(strategy); err != nil {
		return err
	}
	if strategy == "" {
		strategy = CommonsStrategyRanked
	}
	set.CommonsStrategy = strategy
	return nil
}

// Commit persists a mutated set: profiles.yml first, then the contributor.env
// projection regenerated from it.
//
// ORDER MATTERS and is the whole reason this is one function rather than two
// calls at each site. profiles.yml is the source of truth, so it is written
// first: if the projection then fails, the credentials are already safe on disk
// and re-running any mutating command regenerates the env file. Writing the
// projection first and failing to save would leave a contributor.env describing
// hives no file records — a relay soliciting from a hub the operator can no
// longer see, rename or remove.
func (s *ProfileStore) Commit(set *ProfileSet) error {
	if err := s.Save(set); err != nil {
		return err
	}
	return s.WriteEnvProjection(set)
}
