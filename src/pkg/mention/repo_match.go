package mention

import "strings"

// RepoRef is a repository reference reduced to its lower-cased owner and name.
// Owner is empty for a bare name.
type RepoRef struct {
	Owner string
	Name  string
}

// ParseRepoRef normalises the forms a repository can arrive in — `owner/name`,
// a bare `name`, `https://github.com/owner/name`, `github.com/owner/name`,
// and `git@github.com:owner/name` — with optional `.git` suffix, surrounding
// slashes, and whitespace. Comparison is case-insensitive, so both fields are
// lower-cased. The zero RepoRef is returned for blank input.
func ParseRepoRef(raw string) RepoRef {
	s := strings.TrimSpace(raw)
	for _, prefix := range []string{"https://", "http://", "ssh://"} {
		if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
			s = s[len(prefix):]
			break
		}
	}
	if strings.HasPrefix(strings.ToLower(s), "git@") {
		s = s[len("git@"):]
	}
	if idx := strings.IndexAny(s, "/:"); idx > 0 {
		host := s[:idx]
		if strings.EqualFold(host, "github.com") || strings.EqualFold(host, "www.github.com") {
			s = s[idx+1:]
		}
	}
	s = strings.Trim(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.ToLower(s)
	if s == "" {
		return RepoRef{}
	}
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 1:
		return RepoRef{Name: parts[0]}
	default:
		// Anything past owner/name (for example `/issues/12`) is a URL tail.
		return RepoRef{Owner: parts[0], Name: parts[1]}
	}
}

// RepoMatches reports whether an event repository refers to a configured
// repository. defaultOwner is the config's `org`, used to qualify a bare
// configured entry; when neither side names an owner, names alone decide.
// A configured owner (explicit or via defaultOwner) must agree with the event
// owner when the event carries one, so `kubestellar/console` does not match a
// hive governing `someone-else/console`.
func RepoMatches(configured, event, defaultOwner string) bool {
	want := ParseRepoRef(configured)
	got := ParseRepoRef(event)
	if want.Name == "" || got.Name == "" || want.Name != got.Name {
		return false
	}
	wantOwner := want.Owner
	if wantOwner == "" {
		wantOwner = strings.ToLower(strings.TrimSpace(defaultOwner))
	}
	if wantOwner == "" || got.Owner == "" {
		return true
	}
	return wantOwner == got.Owner
}

// RepoConfigured reports whether event matches any entry in configured,
// qualifying bare entries with defaultOwner. See RepoMatches.
func RepoConfigured(configured []string, event, defaultOwner string) bool {
	for _, c := range configured {
		if RepoMatches(c, event, defaultOwner) {
			return true
		}
	}
	return false
}
