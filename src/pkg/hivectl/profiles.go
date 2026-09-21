package hivectl

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Named hive profiles (#8097).
//
// Before this file, the set of hives a contributor lends a CLI to lived in
// `~/.config/hive/contributor.env` as three comma-separated, POSITION-ALIGNED
// lists — HIVE_HUB, HIVE_REGISTRATION_TOKEN, CONTRIBUTOR_ID — paired by index
// (bin/contributor-relay.js). That representation has no names, and one
// hand-edit that drops a single field transposes every hub/token pair after
// it: the relay's `FATAL: HIVE_HUB lists N hub(s) but HIVE_REGISTRATION_TOKEN
// lists M token(s)` refusal exists precisely because the format cannot detect
// a subtler misalignment, and the Justfile's MULTI-HUB PRESERVATION block
// refuses to append to a file whose lists already disagree.
//
// profiles.yml is the named, structured source of truth. contributor.env
// becomes a GENERATED PROJECTION of it (WriteEnvProjection), so nothing
// downstream — relay, container launch path, contribute-k8s — has to change:
// the relay keeps reading the same three variables, and a file this code wrote
// is aligned by construction.
//
// The active profile is projected FIRST in each list. The relay starts at
// activeHubIndex 0, so "first" is "the hub it solicits from when it starts" —
// which is what `hivectl hives use` needs to mean until the relay grows a
// live switch (phase 2 of #8097).

const (
	// ProfilesVersion is the schema version written into profiles.yml. A
	// reader that finds a HIGHER version refuses rather than guessing: the
	// file holds live registration tokens the hub will never reprint, so a
	// partial parse that then round-trips through Save would silently drop
	// fields an older binary does not know about.
	ProfilesVersion = 1

	profilesFileName       = "profiles.yml"
	contributorEnvFileName = "contributor.env"

	// profilesFileMode is asserted by tests, not merely applied: profiles.yml
	// carries HIVE_REGISTRATION_TOKEN for every hive, the sole long-lived
	// bearer credential for the contributor WebSocket. It matches the 0600 of
	// its siblings (contributor.env, gh-auth.env).
	profilesFileMode = os.FileMode(0o600)
	profilesDirMode  = os.FileMode(0o700)
)

// ErrNoProfiles reports that no profiles.yml exists and there was no legacy
// contributor.env to migrate from — the "you have not run contribute-setup
// yet" case, which callers turn into setup guidance rather than a stack trace.
var ErrNoProfiles = errors.New("no hive profiles configured")

// ErrProfileNotFound reports that no profile carries the requested name.
var ErrProfileNotFound = errors.New("no such hive profile")

// Profile is one named hive a contributor lends a CLI to.
//
// RegistrationToken is a secret. It is present in the struct because it is
// what the projection needs, and it is deliberately NOT tagged for JSON: the
// `hivectl hives` commands render profiles through the shared printer, and a
// json/yaml `-o` on `hives list` must not spray live tokens across a terminal
// or a CI log. Redacted() is what the commands print.
type Profile struct {
	Name              string    `yaml:"name" json:"name"`
	Hub               string    `yaml:"hub" json:"hub"`
	ContributorID     string    `yaml:"contributor_id,omitempty" json:"contributor_id,omitempty"`
	RegistrationToken string    `yaml:"registration_token" json:"-"`
	Session           string    `yaml:"session,omitempty" json:"session,omitempty"`
	Backend           string    `yaml:"backend,omitempty" json:"backend,omitempty"`
	Model             string    `yaml:"model,omitempty" json:"model,omitempty"`
	AddedAt           time.Time `yaml:"added_at,omitempty" json:"added_at,omitempty"`
}

// ProfileSet is the whole file: every hive, plus which one is active.
type ProfileSet struct {
	Version  int       `yaml:"version"`
	Active   string    `yaml:"active,omitempty"`
	Profiles []Profile `yaml:"profiles"`
}

// ProfileStore reads and writes profiles.yml and its contributor.env
// projection, both inside one config directory.
type ProfileStore struct {
	dir string
}

// NewProfileStore builds a store over an explicit config directory. Tests use
// this to stay off the developer's real credentials; production callers use
// DefaultProfileStore.
func NewProfileStore(dir string) *ProfileStore { return &ProfileStore{dir: dir} }

// DefaultProfileStore places the profiles beside the contributor credentials
// they describe: $HOME/.config/hive.
//
// This deliberately does NOT honour XDG_CONFIG_HOME the way DefaultSessionStore
// does. contributor.env's location is hard-coded to $HOME/.config/hive by the
// Justfile (`config_dir := env("HOME") + "/.config/hive"`), by
// src/compose-contributor.yaml's bind mount, and by the docs. profiles.yml is
// the source that file is generated FROM; putting the two in different
// directories on a machine that exports XDG_CONFIG_HOME would mean writing a
// projection the relay never reads.
func DefaultProfileStore() (*ProfileStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory for the hive profiles file: %w", err)
	}
	return NewProfileStore(filepath.Join(home, ".config", "hive")), nil
}

// Path reports where profiles.yml lives, for messages that name what was
// written.
func (s *ProfileStore) Path() string { return filepath.Join(s.dir, profilesFileName) }

// EnvPath reports where the generated contributor.env projection lives.
func (s *ProfileStore) EnvPath() string { return filepath.Join(s.dir, contributorEnvFileName) }

// Find returns the profile with the given name (case-insensitive, matching the
// uniqueness rule Validate enforces) and its index.
func (set *ProfileSet) Find(name string) (*Profile, int) {
	for i := range set.Profiles {
		if strings.EqualFold(set.Profiles[i].Name, name) {
			return &set.Profiles[i], i
		}
	}
	return nil, -1
}

// ActiveProfile returns the profile named by Active, or nil when the set is
// empty. A set with profiles but no Active resolves to the first one — the
// same hub the relay solicits from at activeHubIndex 0 — so "active" is never
// a lie about what a running relay is doing.
func (set *ProfileSet) ActiveProfile() *Profile {
	if len(set.Profiles) == 0 {
		return nil
	}
	if set.Active != "" {
		if p, _ := set.Find(set.Active); p != nil {
			return p
		}
	}
	return &set.Profiles[0]
}

// Ordered returns the profiles in projection order: the active one first, the
// rest in file order. This is the order WriteEnvProjection writes and the order
// `hivectl hives list` shows, so the list on screen matches the list the relay
// will walk.
func (set *ProfileSet) Ordered() []Profile {
	if len(set.Profiles) == 0 {
		return nil
	}
	active := set.ActiveProfile()
	out := make([]Profile, 0, len(set.Profiles))
	out = append(out, *active)
	for i := range set.Profiles {
		if strings.EqualFold(set.Profiles[i].Name, active.Name) {
			continue
		}
		out = append(out, set.Profiles[i])
	}
	return out
}

// Redacted returns a copy safe to print or serialize: the registration token is
// replaced by a fixed placeholder so `hives list -o json` never leaks it.
func (p Profile) Redacted() Profile {
	if p.RegistrationToken != "" {
		p.RegistrationToken = "(set)"
	}
	return p
}

// Validate enforces the invariants that make the projection safe to generate:
// every profile named and uniquely so, every hub a usable WebSocket/HTTP URL,
// and no field carrying a character that would break the comma-separated,
// shell-sourced env file the projection writes.
func (set *ProfileSet) Validate() error {
	if set.Version > ProfilesVersion {
		return fmt.Errorf("profiles file is version %d, but this hivectl understands up to version %d — upgrade hivectl rather than letting it rewrite a file it cannot fully read", set.Version, ProfilesVersion)
	}
	seen := map[string]string{}
	for i := range set.Profiles {
		p := &set.Profiles[i]
		if err := ValidateProfileName(p.Name); err != nil {
			return err
		}
		key := strings.ToLower(p.Name)
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("duplicate hive profile name %q (already used by %q); names must be unique, case-insensitively", p.Name, prev)
		}
		seen[key] = p.Name
		if err := ValidateHubURL(p.Hub); err != nil {
			return fmt.Errorf("profile %q: %w", p.Name, err)
		}
		if err := validateEnvValue("registration_token", p.RegistrationToken); err != nil {
			return fmt.Errorf("profile %q: %w", p.Name, err)
		}
		for field, value := range map[string]string{"session": p.Session, "backend": p.Backend, "model": p.Model} {
			if value == "" {
				continue
			}
			if err := validateEnvValue(field, value); err != nil {
				return fmt.Errorf("profile %q: %w", p.Name, err)
			}
		}
		if err := validateEnvValue("contributor_id", p.ContributorID); err != nil {
			return fmt.Errorf("profile %q: %w", p.Name, err)
		}
	}
	if set.Active != "" {
		if p, _ := set.Find(set.Active); p == nil {
			return fmt.Errorf("active hive %q is not one of the configured profiles", set.Active)
		}
	}
	return nil
}

// ValidateProfileName accepts the names that are safe to type, to match
// case-insensitively, and to embed in a shell-sourced file.
func ValidateProfileName(name string) error {
	if strings.TrimSpace(name) != name || name == "" {
		return fmt.Errorf("hive profile name %q must be non-empty and free of leading/trailing whitespace", name)
	}
	if len(name) > 64 {
		return fmt.Errorf("hive profile name %q is longer than 64 characters", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("hive profile name %q may use only letters, digits, '-', '_' and '.'", name)
		}
	}
	return nil
}

// ValidateHubURL accepts the hub spellings the relay accepts, and rejects the
// ones the projection cannot represent.
//
// The comma check is not cosmetic: the projection joins hubs with commas and
// the relay splits on them, so a comma inside a single URL would silently
// become two hubs and misalign every pair after it.
func ValidateHubURL(hub string) error {
	if strings.TrimSpace(hub) != hub || hub == "" {
		return fmt.Errorf("hub URL %q must be non-empty and free of surrounding whitespace", hub)
	}
	if err := validateEnvValue("hub", hub); err != nil {
		return err
	}
	u, err := url.Parse(hub)
	if err != nil {
		return fmt.Errorf("hub URL %q is not a valid URL: %w", hub, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "ws", "wss", "http", "https":
	default:
		return fmt.Errorf("hub URL %q must use ws://, wss://, http:// or https:// (got %q)", hub, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("hub URL %q has no host", hub)
	}
	return nil
}

// validateEnvValue rejects values that cannot survive a round trip through the
// comma-separated, shell-sourced contributor.env projection.
func validateEnvValue(field, value string) error {
	if strings.ContainsRune(value, ',') {
		return fmt.Errorf("%s may not contain a comma — the generated contributor.env separates hubs, tokens and ids with commas", field)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s may not contain control characters", field)
		}
		switch r {
		case ' ', '\t', '"', '\'', '$', '`', '\\', '\n', '\r':
			return fmt.Errorf("%s may not contain %q — the generated contributor.env is sourced by a shell", field, string(r))
		}
	}
	return nil
}

// ProfileNameFromHub derives a default profile name from a hub URL: the first
// label of its host, which is the part that actually distinguishes one hive
// from another ("acme" out of wss://acme.hive.hivecommons.dev/contribute).
//
// The result is a SUGGESTION. Callers that may produce several names from one
// list must pass them through uniqueProfileName, because two hives under
// different domains can share a first label.
func ProfileNameFromHub(hub string) string {
	u, err := url.Parse(hub)
	host := ""
	if err == nil {
		host = u.Hostname()
	}
	if host == "" {
		host = hub
	}
	host = strings.ToLower(host)
	if label, _, ok := strings.Cut(host, "."); ok && label != "" {
		host = label
	}
	cleaned := make([]rune, 0, len(host))
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			cleaned = append(cleaned, r)
		default:
			cleaned = append(cleaned, '-')
		}
	}
	name := strings.Trim(string(cleaned), "-")
	if name == "" {
		return "hive"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// uniqueProfileName appends a numeric suffix until the name is unused.
func uniqueProfileName(base string, taken map[string]struct{}) string {
	candidate := base
	for n := 2; ; n++ {
		if _, clash := taken[strings.ToLower(candidate)]; !clash {
			taken[strings.ToLower(candidate)] = struct{}{}
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, n)
	}
}

// Load reads profiles.yml. A missing file yields (nil, nil) so callers can
// decide between migrating and reporting ErrNoProfiles; an unreadable or
// invalid one is an error, never an empty set — the file holds tokens the hub
// will never reprint, and silently treating a corrupt file as "no hives" would
// invite the next Save to overwrite it with nothing.
func (s *ProfileStore) Load() (*ProfileSet, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read hive profiles %s: %w", s.Path(), err)
	}
	var set ProfileSet
	if err := yaml.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("hive profiles %s is not valid YAML: %w", s.Path(), err)
	}
	if err := set.Validate(); err != nil {
		return nil, fmt.Errorf("hive profiles %s: %w", s.Path(), err)
	}
	return &set, nil
}

// Save validates and writes profiles.yml with owner-only permissions, through
// a same-directory temp file and a rename so a crash mid-write cannot leave a
// half-written set of credentials and so the file is 0600 from its first byte.
func (s *ProfileStore) Save(set *ProfileSet) error {
	if set == nil {
		return errors.New("refusing to write a nil hive profile set")
	}
	if set.Version == 0 {
		set.Version = ProfilesVersion
	}
	if err := set.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(set)
	if err != nil {
		return fmt.Errorf("encode hive profiles: %w", err)
	}
	header := "# Named hive profiles for the Hive contributor relay (hivectl hives).\n" +
		"# Holds live registration tokens — keep this file mode 0600.\n" +
		"# contributor.env in this directory is GENERATED from this file.\n"
	return s.writeFile(s.Path(), "profiles-*.yml.tmp", append([]byte(header), data...))
}

// LoadOrMigrate returns the profile set, migrating a legacy positional
// contributor.env the first time it is called on a machine that has one.
//
// Migration does NOT rewrite contributor.env. A contributor who merely RUNS a
// `hivectl hives` command must end up with a profiles.yml and a byte-identical
// relay configuration; the projection is written only when a command actually
// changes which hives exist or which one is active.
//
// migrated reports whether a legacy file was converted, so the caller can say
// so once instead of on every subsequent run.
func (s *ProfileStore) LoadOrMigrate() (set *ProfileSet, migrated bool, err error) {
	existing, err := s.Load()
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}
	converted, err := s.migrateFromEnv()
	if err != nil {
		return nil, false, err
	}
	if converted == nil {
		return nil, false, ErrNoProfiles
	}
	if err := s.Save(converted); err != nil {
		return nil, false, err
	}
	return converted, true, nil
}

// migrateFromEnv builds a profile set from the legacy positional
// contributor.env, or returns nil when there is no such file to convert.
func (s *ProfileStore) migrateFromEnv() (*ProfileSet, error) {
	env, err := readEnvFile(s.EnvPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	hubs := splitList(env.value("HIVE_HUB"))
	tokens := splitList(env.value("HIVE_REGISTRATION_TOKEN"))
	ids := splitList(env.value("CONTRIBUTOR_ID"))
	if len(hubs) == 0 {
		return nil, nil
	}
	if len(tokens) != len(hubs) {
		// The exact misalignment the relay refuses to start on. Migrating it
		// would have to guess which token belongs to which hub, and guessing
		// wrong points a live credential at the wrong hive.
		return nil, fmt.Errorf(
			"%s lists %d hub(s) but %d registration token(s) — the positional lists are misaligned, so they cannot be migrated without guessing which token belongs to which hub; fix the file (one token per hub, same order) or re-run 'just contribute-setup' for the hive whose credential is missing, then try again",
			s.EnvPath(), len(hubs), len(tokens))
	}
	if len(ids) != 0 && len(ids) != len(hubs) {
		return nil, fmt.Errorf(
			"%s lists %d hub(s) but %d contributor id(s) — the positional lists are misaligned; fix the file (one id per hub, same order) or remove the CONTRIBUTOR_ID line, then try again",
			s.EnvPath(), len(hubs), len(ids))
	}
	addedAt := time.Time{}
	if info, statErr := os.Stat(s.EnvPath()); statErr == nil {
		// The env file's mtime is the closest thing to "when this contributor
		// registered" that survives on disk. Better than stamping every
		// migrated hive with the moment of migration.
		addedAt = info.ModTime().UTC().Truncate(time.Second)
	}
	session := env.value("HIVE_SESSION")
	taken := map[string]struct{}{}
	set := &ProfileSet{Version: ProfilesVersion}
	for i, hub := range hubs {
		if err := ValidateHubURL(hub); err != nil {
			return nil, fmt.Errorf("%s: %w", s.EnvPath(), err)
		}
		p := Profile{
			Name:              uniqueProfileName(ProfileNameFromHub(hub), taken),
			Hub:               hub,
			RegistrationToken: tokens[i],
			Session:           session,
			AddedAt:           addedAt,
		}
		if i < len(ids) {
			p.ContributorID = ids[i]
		}
		set.Profiles = append(set.Profiles, p)
	}
	// The relay starts at activeHubIndex 0, so the first hub in the legacy
	// list is the one it currently solicits from. Preserving that as the
	// active profile is what makes migration a no-op for a running relay.
	set.Active = set.Profiles[0].Name
	if err := set.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", s.EnvPath(), err)
	}
	return set, nil
}

// WriteEnvProjection regenerates contributor.env from the profile set.
//
// The three managed keys are replaced in place, preserving their position and
// every other line in the file — HIVE_LITELLM_ENDPOINT, CONTRIBUTOR_USERNAME,
// AGENT_BACKEND and any key a future setup adds are carried across rather than
// dropped, the same contract `just contribute-move` offers. The previous file
// is kept at contributor.env.bak, matching what contribute-setup does, because
// the values being replaced are credentials the hub will never reprint.
//
// An empty set truncates the three lists rather than deleting the file: a relay
// pointed at an empty HIVE_HUB fails loudly at startup, where a missing file
// would silently fall back to the public hub default.
func (s *ProfileStore) WriteEnvProjection(set *ProfileSet) error {
	if set == nil {
		return errors.New("refusing to project a nil hive profile set")
	}
	if err := set.Validate(); err != nil {
		return err
	}
	ordered := set.Ordered()
	hubs := make([]string, 0, len(ordered))
	tokens := make([]string, 0, len(ordered))
	ids := make([]string, 0, len(ordered))
	for _, p := range ordered {
		hubs = append(hubs, p.Hub)
		tokens = append(tokens, p.RegistrationToken)
		ids = append(ids, p.ContributorID)
	}

	env, err := readEnvFile(s.EnvPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if env == nil {
		env = &envFile{}
	}
	env.set("HIVE_REGISTRATION_TOKEN", strings.Join(tokens, ","))
	env.set("HIVE_HUB", strings.Join(hubs, ","))
	env.set("CONTRIBUTOR_ID", strings.Join(ids, ","))
	// The session label is per-profile from here on; project the active one so
	// a single-session contributor keeps the behaviour they had.
	if active := set.ActiveProfile(); active != nil && active.Session != "" {
		env.set("HIVE_SESSION", active.Session)
	}

	if _, statErr := os.Stat(s.EnvPath()); statErr == nil {
		if err := s.backupEnv(); err != nil {
			return err
		}
	}
	return s.writeFile(s.EnvPath(), "contributor-*.env.tmp", env.render())
}

func (s *ProfileStore) backupEnv() error {
	data, err := os.ReadFile(s.EnvPath())
	if err != nil {
		return fmt.Errorf("read %s before regenerating it: %w", s.EnvPath(), err)
	}
	return s.writeFile(s.EnvPath()+".bak", "contributor-*.env.bak.tmp", data)
}

func (s *ProfileStore) writeFile(path, pattern string, data []byte) error {
	if err := os.MkdirAll(s.dir, profilesDirMode); err != nil {
		return fmt.Errorf("create hive config dir %s: %w", s.dir, err)
	}
	tmp, err := os.CreateTemp(s.dir, pattern)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// CreateTemp already opens 0600; Chmod pins it against a future change to
	// that default, so the credential never exists on disk world-readable.
	if err := tmp.Chmod(profilesFileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("restrict permissions on %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// envFile is a line-preserving view of a KEY=VALUE file. Comments, blank lines,
// unknown keys and their original order all survive a read/modify/write cycle;
// only the keys the projection owns are rewritten.
type envFile struct {
	lines []string
}

func readEnvFile(path string) (*envFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return &envFile{}, nil
	}
	return &envFile{lines: strings.Split(text, "\n")}, nil
}

func (f *envFile) value(key string) string {
	prefix := key + "="
	for _, line := range f.lines {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), prefix))
		}
	}
	return ""
}

// set replaces the first assignment of key, or appends one when the key is
// absent. Later duplicate assignments of the same key are dropped: a shell
// sourcing the file would let the last one win, so leaving a stale duplicate
// behind would mean the projection had no effect.
func (f *envFile) set(key, value string) {
	prefix := key + "="
	replaced := false
	out := make([]string, 0, len(f.lines)+1)
	for _, line := range f.lines {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			if replaced {
				continue
			}
			out = append(out, prefix+value)
			replaced = true
			continue
		}
		out = append(out, line)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	f.lines = out
}

func (f *envFile) render() []byte {
	if len(f.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(f.lines, "\n") + "\n")
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// HubHTTPBase converts a contributor hub WebSocket URL into the HTTP base its
// REST endpoints live under, the same transformation `just contribute-setup`
// does in shell: wss->https, ws->http, and drop the trailing /contribute path.
func HubHTTPBase(hub string) (string, error) {
	if err := ValidateHubURL(hub); err != nil {
		return "", err
	}
	u, err := url.Parse(hub)
	if err != nil {
		return "", fmt.Errorf("hub URL %q is not a valid URL: %w", hub, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/contribute")
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}
