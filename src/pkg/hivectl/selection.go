package hivectl

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	SelectionSelected   = "selected"
	SelectionAllCurrent = "all-current"
)

// RepositorySelection records reviewed consent, not live discovery. Following
// future repositories is a policy for a later reviewed refresh; projection never
// treats it as a wildcard or fetches membership on its own.
type RepositorySelection struct {
	Mode                  string   `yaml:"mode" json:"mode"`
	Repositories          []string `yaml:"repositories,omitempty" json:"repositories,omitempty"`
	Include               []string `yaml:"include,omitempty" json:"include,omitempty"`
	Exclude               []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
	FollowNewRepositories bool     `yaml:"follow_new_repositories,omitempty" json:"follow_new_repositories,omitempty"`
	MembershipSnapshot    []string `yaml:"membership_snapshot,omitempty" json:"membership_snapshot,omitempty"`
	Sources               []string `yaml:"sources,omitempty" json:"sources,omitempty"`
	// Auto lists members that were added because the Hive is followed, not
	// chosen by hand. It is provenance only: it never widens EffectiveRepositories
	// and is dropped as soon as the member is touched by hand (included,
	// excluded, deselected, or the whole selection is re-chosen). An older
	// hivectl ignores the field and keeps the membership, so it degrades to
	// "chosen by hand".
	Auto []string `yaml:"auto,omitempty" json:"auto,omitempty"`
}

// IsAuto reports whether id was added automatically by following the Hive.
func (selection *RepositorySelection) IsAuto(id string) bool {
	if selection == nil {
		return false
	}
	for _, raw := range selection.Auto {
		if canonical, _ := NormalizeRepositoryID(raw); canonical == id {
			return true
		}
	}
	return false
}

// PresetProvenance is local review history. It contains no credentials and is
// deliberately separate from the credential-free public preset wire format.
type PresetProvenance struct {
	ID               string    `yaml:"id"`
	Source           string    `yaml:"source"`
	ReviewedRevision string    `yaml:"reviewed_revision"`
	AcceptedAt       time.Time `yaml:"accepted_at,omitempty"`
}

// NormalizeRepositoryID requires a forge, host and full repository path. Nested
// namespaces are retained (GitLab); display names and URLs are not identities.
func NormalizeRepositoryID(id string) (string, error) {
	id = strings.ToLower(id)
	parts := strings.Split(id, "/")
	if len(parts) < 4 {
		return "", fmt.Errorf("repository id %q must be <forge>/<host>/<owner>/<name>", id)
	}
	switch parts[0] {
	case "github", "gitlab", "gitea":
	default:
		return "", fmt.Errorf("unsupported repository forge %q", parts[0])
	}
	host, err := canonicalHost(parts[1], "")
	if err != nil {
		return "", fmt.Errorf("repository id %q: %w", id, err)
	}
	parts[1] = host
	for _, part := range parts[2:] {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid repository path in %q", id)
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return "", fmt.Errorf("invalid repository path in %q", id)
			}
		}
	}
	return strings.Join(parts, "/"), nil
}

func canonicalHost(host, scheme string) (string, error) {
	u, err := url.Parse("https://" + host)
	if err != nil || u.Host != host || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(host, ":") {
		return "", fmt.Errorf("invalid host %q", host)
	}
	name := strings.ToLower(u.Hostname())
	if net.ParseIP(name) == nil {
		for _, r := range name {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
				return "", fmt.Errorf("invalid host %q", host)
			}
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid port in %q", host)
		}
		port = strconv.Itoa(n)
	}
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(name, port), nil
	}
	if strings.Contains(name, ":") {
		return "[" + name + "]", nil
	}
	return name, nil
}

// NormalizeHiveID is the HTTP origin of a hub, never its display name or path.
// Different schemes and non-default ports remain distinct credential boundaries.
func NormalizeHiveID(hub string) (string, error) {
	base, err := HubHTTPBase(hub)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.User != nil {
		return "", fmt.Errorf("hive identity must not contain URL credentials")
	}
	scheme := strings.ToLower(u.Scheme)
	// HubHTTPBase historically preserves case for HTTP spellings.
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	host, err := canonicalHost(u.Host, scheme)
	if err != nil {
		return "", err
	}
	return scheme + "://" + host, nil
}

func (selection *RepositorySelection) Validate() error {
	if selection == nil {
		return nil
	}
	switch selection.Mode {
	case SelectionSelected, SelectionAllCurrent:
	default:
		return fmt.Errorf("selection mode %q must be selected or all-current", selection.Mode)
	}
	for _, list := range [][]string{selection.Repositories, selection.Include, selection.Exclude, selection.MembershipSnapshot, selection.Auto} {
		for _, id := range list {
			if _, err := NormalizeRepositoryID(id); err != nil {
				return err
			}
		}
	}
	for _, source := range selection.Sources {
		if err := ValidateProfileName(source); err != nil {
			return fmt.Errorf("selection source: %w", err)
		}
	}
	return nil
}

// EffectiveRepositories returns a sorted canonical set. nil selection alone is
// legacy/unrestricted; an empty returned set for an explicit selection is NONE.
func (selection *RepositorySelection) EffectiveRepositories() ([]string, error) {
	if err := selection.Validate(); err != nil {
		return nil, err
	}
	if selection == nil {
		return nil, nil
	}
	base := selection.Repositories
	if selection.Mode == SelectionAllCurrent {
		base = selection.MembershipSnapshot
	}
	members := map[string]bool{}
	for _, list := range [][]string{base, selection.Include} {
		for _, id := range list {
			canonical, _ := NormalizeRepositoryID(id)
			members[canonical] = true
		}
	}
	for _, id := range selection.Exclude {
		canonical, _ := NormalizeRepositoryID(id)
		delete(members, canonical)
	}
	result := make([]string, 0, len(members))
	for id := range members {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

// RepositoryScopes follows projection order, without coalescing legacy profiles
// that intentionally have different contributor identities at the same URL.
func (set *ProfileSet) RepositoryScopes() ([]string, error) {
	excluded := map[string]bool{}
	for _, p := range set.Profiles {
		if p.Selection != nil {
			for _, id := range p.Selection.Exclude {
				canonical, err := NormalizeRepositoryID(id)
				if err != nil {
					return nil, err
				}
				excluded[canonical] = true
			}
		}
	}
	scopes := make([]string, 0, len(set.Profiles))
	for _, p := range set.Ordered() {
		if p.Selection == nil {
			scopes = append(scopes, "*")
			continue
		}
		repos, err := p.Selection.EffectiveRepositories()
		if err != nil {
			return nil, err
		}
		filtered := make([]string, 0, len(repos))
		for _, id := range repos {
			if !excluded[id] {
				filtered = append(filtered, id)
			}
		}
		scopes = append(scopes, strings.Join(filtered, ";"))
	}
	return scopes, nil
}
