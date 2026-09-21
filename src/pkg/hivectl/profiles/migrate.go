package profiles

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// envValues is a minimal KEY=VALUE view of contributor.env, preserving every
// line so the projection can re-emit keys this package does not own (for
// example HIVE_LITELLM_ENDPOINT) unchanged.
type envValues struct {
	values map[string]string
	// order of first appearance, for stable projection output.
	order []string
}

func parseEnvFile(path string) (*envValues, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ev := &envValues{values: map[string]string{}}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if _, dup := ev.values[key]; !dup {
			ev.order = append(ev.order, key)
		}
		ev.values[key] = value
	}
	return ev, nil
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// nameForHub derives a stable, human-recognizable profile name from the hub
// URL: the hostname, falling back to a sanitized form of the whole value.
func nameForHub(hub string, taken map[string]bool) string {
	base := hub
	if u, err := url.Parse(hub); err == nil && u.Hostname() != "" {
		base = u.Hostname()
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if !ValidName(name) {
		name = "hive"
	}
	candidate := name
	for i := 2; taken[candidate]; i++ {
		candidate = fmt.Sprintf("%s-%d", name, i)
	}
	taken[candidate] = true
	return candidate
}

// Migrate reads the positional contributor.env in dir and converts it to a
// named profiles.yml, once. It never deletes or rewrites contributor.env — the
// file it read stays valid, and the first Project call regenerates it from
// the profiles.
//
// Fail closed on misalignment: a file whose HIVE_HUB /
// HIVE_REGISTRATION_TOKEN / CONTRIBUTOR_ID lists disagree in length is
// malformed (the relay refuses it too), and guessing which token belongs to
// which hub could hand a live credential to the wrong host. The one benign
// exception is a single CONTRIBUTOR_ID shared across hubs, which older setups
// wrote.
func Migrate(dir string) (*File, error) {
	ev, err := parseEnvFile(EnvPath(dir))
	if err != nil {
		return nil, err
	}
	hubs := splitList(ev.values["HIVE_HUB"])
	tokens := splitList(ev.values["HIVE_REGISTRATION_TOKEN"])
	ids := splitList(ev.values["CONTRIBUTOR_ID"])
	if len(hubs) == 0 {
		return nil, fmt.Errorf("%s has no HIVE_HUB; nothing to migrate", EnvPath(dir))
	}
	if len(tokens) != len(hubs) {
		return nil, fmt.Errorf("%s is misaligned: %d hub(s) but %d registration token(s); refusing to guess which token belongs to which hub — re-run contribute-setup for the affected hive",
			EnvPath(dir), len(hubs), len(tokens))
	}
	switch {
	case len(ids) == len(hubs):
	case len(ids) == 1:
		for len(ids) < len(hubs) {
			ids = append(ids, ids[0])
		}
	case len(ids) == 0:
		ids = make([]string, len(hubs))
	default:
		return nil, fmt.Errorf("%s is misaligned: %d hub(s) but %d contributor id(s); refusing to guess — re-run contribute-setup for the affected hive",
			EnvPath(dir), len(hubs), len(ids))
	}

	f := &File{Username: ev.values["CONTRIBUTOR_USERNAME"]}
	taken := map[string]bool{}
	for i, hub := range hubs {
		f.Profiles = append(f.Profiles, Profile{
			Name:              nameForHub(hub, taken),
			Hub:               hub,
			ContributorID:     ids[i],
			RegistrationToken: tokens[i],
			Backend:           ev.values["AGENT_BACKEND"],
		})
	}
	f.Active = f.Profiles[0].Name
	if err := Save(dir, f); err != nil {
		return nil, err
	}
	return f, nil
}

// LoadOrMigrate returns the profiles file, migrating the positional
// contributor.env the first time. When neither file exists it returns an
// empty File so `hives add` can start from nothing.
func LoadOrMigrate(dir string) (*File, error) {
	f, err := Load(dir)
	if err == nil {
		return f, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	f, err = Migrate(dir)
	if err == nil {
		return f, nil
	}
	if os.IsNotExist(err) {
		return &File{}, nil
	}
	return nil, err
}
