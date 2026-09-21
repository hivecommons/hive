// Package profiles implements named hive profiles for contributors
// (hivecommons/hive#8097, phase 1).
//
// A contributor lending a CLI to more than one hive used to manage a single
// positional, comma-separated contributor.env (HIVE_HUB /
// HIVE_REGISTRATION_TOKEN / CONTRIBUTOR_ID, paired by index). This package
// replaces that as the source of truth with a named, structured
// ~/.config/hive/profiles.yml, one entry per hive. contributor.env stays as a
// generated, read-only projection for the relay and the container launch path
// so nothing downstream changes; see Project.
package profiles

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the profiles file, kept next to contributor.env.
const FileName = "profiles.yml"

// EnvFileName is the generated projection consumed by bin/contributor-relay.js
// and the container launch path.
const EnvFileName = "contributor.env"

// Profile is one saved hive: where it is, who the contributor is there, and
// the credential that proves it. RegistrationToken is the sole long-lived
// bearer credential for the contributor WebSocket; the hub stores only a hash
// and can never reprint it, which is why this file is written 0600.
type Profile struct {
	Name              string `yaml:"name"`
	Hub               string `yaml:"hub"`
	ContributorID     string `yaml:"contributor_id"`
	RegistrationToken string `yaml:"registration_token"`
	// Session is the optional HIVE_SESSION label so two relays against the
	// same hive get independent session-scoped identities.
	Session string `yaml:"session,omitempty"`
	// Backend/Model are optional per-profile defaults for AGENT_BACKEND and
	// AGENT_MODEL.
	Backend string `yaml:"backend,omitempty"`
	Model   string `yaml:"model,omitempty"`
	AddedAt string `yaml:"added_at,omitempty"`
}

// File is the whole profiles.yml: the profile set plus which one is active.
// The active profile is projected FIRST into contributor.env, which is the
// hub the relay solicits from at start (activeHubIndex starts at 0).
type File struct {
	// Active names the profile `hivectl hives use` selected. Empty means the
	// first profile is active.
	Active string `yaml:"active,omitempty"`
	// Username is the GitHub login shared by every profile (registration is
	// keyed on it hub-side).
	Username string    `yaml:"username,omitempty"`
	Profiles []Profile `yaml:"profiles"`
}

// nameRe deliberately excludes commas (the projection is comma-joined), spaces
// and path separators so a profile name is always safe to print, join and use
// as a lookup key.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidName reports whether name is usable as a profile name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// DefaultDir is where profiles.yml and contributor.env live, matching the
// Justfile's config_dir (env("HOME") + "/.config/hive").
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".config", "hive"), nil
}

// Path returns the profiles.yml path inside dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// EnvPath returns the contributor.env path inside dir.
func EnvPath(dir string) string { return filepath.Join(dir, EnvFileName) }

// Load reads profiles.yml from dir. A missing file is (nil, os.ErrNotExist)
// so callers can distinguish "never migrated" from a malformed file.
func Load(dir string) (*File, error) {
	data, err := os.ReadFile(Path(dir))
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", Path(dir), err)
	}
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(dir), err)
	}
	return &f, nil
}

// Save writes profiles.yml atomically with owner-only permissions: it carries
// registration tokens, the same secret contributor.env holds at 0600.
func Save(dir string, f *File) error {
	if err := f.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("encoding profiles: %w", err)
	}
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing profiles: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("securing %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, Path(dir)); err != nil {
		return fmt.Errorf("replacing %s: %w", Path(dir), err)
	}
	return nil
}

func (f *File) validate() error {
	seen := make(map[string]bool, len(f.Profiles))
	for i := range f.Profiles {
		p := &f.Profiles[i]
		if !ValidName(p.Name) {
			return fmt.Errorf("profile %d has invalid name %q", i, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate profile name %q", p.Name)
		}
		seen[p.Name] = true
		if p.Hub == "" {
			return fmt.Errorf("profile %q has no hub", p.Name)
		}
	}
	if f.Active != "" && !seen[f.Active] {
		return fmt.Errorf("active profile %q does not exist", f.Active)
	}
	return nil
}

// ErrNotFound is returned by lookups for a name with no profile.
var ErrNotFound = errors.New("no such profile")

// Find returns the profile with the given name.
func (f *File) Find(name string) (*Profile, error) {
	for i := range f.Profiles {
		if f.Profiles[i].Name == name {
			return &f.Profiles[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
}

// ActiveProfile resolves the active profile: the one named by Active, else
// the first. Nil when there are no profiles.
func (f *File) ActiveProfile() *Profile {
	if len(f.Profiles) == 0 {
		return nil
	}
	if f.Active != "" {
		if p, err := f.Find(f.Active); err == nil {
			return p
		}
	}
	return &f.Profiles[0]
}

// Use marks name active.
func (f *File) Use(name string) error {
	if _, err := f.Find(name); err != nil {
		return err
	}
	f.Active = name
	return nil
}

// Add appends a profile, refusing duplicates by name.
func (f *File) Add(p Profile) error {
	if !ValidName(p.Name) {
		return fmt.Errorf("invalid profile name %q (letters, digits, '.', '_' and '-' only)", p.Name)
	}
	if _, err := f.Find(p.Name); err == nil {
		return fmt.Errorf("profile %q already exists", p.Name)
	}
	if p.AddedAt == "" {
		p.AddedAt = time.Now().UTC().Format(time.RFC3339)
	}
	f.Profiles = append(f.Profiles, p)
	return nil
}

// Remove deletes the named profile. If it was active, the first remaining
// profile becomes active.
func (f *File) Remove(name string) error {
	for i := range f.Profiles {
		if f.Profiles[i].Name == name {
			f.Profiles = append(f.Profiles[:i], f.Profiles[i+1:]...)
			if f.Active == name {
				f.Active = ""
				if len(f.Profiles) > 0 {
					f.Active = f.Profiles[0].Name
				}
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrNotFound, name)
}

// Rename changes a profile's name, keeping the active marker with it.
func (f *File) Rename(oldName, newName string) error {
	if !ValidName(newName) {
		return fmt.Errorf("invalid profile name %q (letters, digits, '.', '_' and '-' only)", newName)
	}
	if _, err := f.Find(newName); err == nil {
		return fmt.Errorf("profile %q already exists", newName)
	}
	p, err := f.Find(oldName)
	if err != nil {
		return err
	}
	p.Name = newName
	if f.Active == oldName {
		f.Active = newName
	}
	return nil
}
