package hub

import (
	"sort"
	"strings"
)

// Repo-ownership overlap detection (kubestellar/hive#5691).
//
// A single hive serving a diverse org makes its agents compete over unrelated
// work, so operators split it into several subject-scoped spokes. The whole
// point of that split is that two fleets stop sharing a backlog — and nothing
// enforced it. The hub receives every spoke's repo list on every heartbeat and
// never compared them, so two spokes could manage the same repo and open
// competing PRs on it: the exact failure the split exists to prevent.
//
// Reported live in #5691 while splitting one 43-repo hive into two: an org
// watcher read one spoke's repo list, could not read the other's, concluded 18
// repos were unmanaged, and was one step from adding them to the first hive.
//
// This is READ-SIDE ONLY, derived per request from data the registry already
// holds. It is deliberately not persisted and adds nothing to the heartbeat
// protocol — the RFC sequences overlap detection first precisely because it
// needs neither, and it keeps the hub a directory rather than a control plane.
//
// It is also a WARNING, not a bar. Whether a duplicate should ever BLOCK
// assignment, and how to allowlist a repo two subject areas legitimately share
// (a docs repo is the RFC's own example), is an open question on #5691.
// Answering it in code before it is answered in the issue would be the wrong
// order, so this reports and does not refuse.

// RepoOverlap is one work item claimed by more than one spoke.
type RepoOverlap struct {
	// Host is the resolved GitHub host the claim sits on. Two spokes claiming
	// "org/repo" on DIFFERENT hosts are not in conflict — a github.com repo and
	// a github.ibm.com repo of the same name are different work — so the host is
	// part of the identity rather than decoration.
	Host string `json:"host"`
	// Repo is the canonical "owner/repo" path, in the spelling of whichever
	// claiming hive was seen first. Matching is case-insensitive because GitHub
	// treats owner and repo names so; this keeps a readable spelling to display.
	Repo string `json:"repo"`
	// Hives are the spokes claiming it, ordered by id so a diff of two
	// /api/registry responses is meaningful.
	Hives []OverlapClaim `json:"hives"`
}

// OverlapClaim names one spoke in an overlap. It carries the display name
// alongside the id so a consumer of /api/registry — or an operator's script —
// can report the conflict without a second lookup into hives[].
type OverlapClaim struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// repoClaimKey is the identity overlaps are computed on. Host and path are both
// lower-cased: GitHub owner/repo names are case-insensitive, and two spokes
// configured separately are exactly the pair most likely to disagree on casing.
func repoClaimKey(host, repo string) string {
	return strings.ToLower(strings.TrimSpace(host)) + "\x00" + strings.ToLower(strings.TrimSpace(repo))
}

// stripRepoRefHost removes the scheme and any leading host label from a repo
// reference, leaving the "owner/repo" or bare "repo" path. It is the complement
// of repoRefHost, which READS the same label instead of removing it, and uses
// that function's rule for what a host looks like (a first segment containing a
// dot) so the two can never disagree about where the path starts.
func stripRepoRefHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.Trim(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		parts = parts[1:]
	}
	return strings.Join(parts, "/")
}

// canonicalRepoClaim resolves one raw repo entry from a spoke's registry record
// into the (host, "owner/repo") pair an overlap is keyed on.
//
// The raw entry is whatever the spoke reported or an operator pasted, so it may
// be a bare name ("hive"), an owner-qualified path ("hivecommons/hive"), or a
// full URL ("https://github.ibm.com/z-aiops-unite/ui"). All three must land on
// the same identity when they name the same repo. That is not a nicety: two
// spokes are configured separately, so "one lists bare names, the other lists
// paths" is the most likely shape for a real overlap to take, and it is exactly
// the shape a naive string compare would miss.
//
// Host resolution follows the GitHubHost field's own documented order — the host
// the entry was pasted with, else the hive's reported host, else public GitHub —
// and resolves the empty case to the literal "github.com" so the map key cannot
// split what sameGitHubHost considers one host.
//
// Owner resolution delegates to repoDisplayLine rather than joining org and repo
// by hand. That function already carries the doubling-safe rule and the live
// fleet evidence behind it (a GHE hive recorded with org "castrojo.github.io"
// and primaryRepo "castrojo/endusers"): a ref that already contains a slash is a
// complete path and must NOT be re-qualified with the org.
//
// ok is false for a claim that cannot be compared: an empty ref, or one that
// resolves to a bare repo with no owner. Inventing an owner for the latter would
// manufacture overlaps between unrelated repos that happen to share a name.
func canonicalRepoClaim(h RegistryEntry, raw string) (host, repo string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	host = repoRefHost(raw)
	if host == "" {
		host = strings.TrimSpace(h.GitHubHost)
	}
	if host == "" || strings.EqualFold(host, publicGitHubHost) {
		host = publicGitHubHost
	}
	path := stripRepoRefHost(raw)
	if path == "" {
		return "", "", false
	}
	repo = repoDisplayLine(strings.TrimSpace(h.Org), path)
	if repo == "" || !strings.Contains(repo, "/") {
		return "", "", false
	}
	return strings.ToLower(host), repo, true
}

// repoClaimsOf returns the canonical claims one hive makes, de-duplicated.
//
// PrimaryRepo is included alongside Repos because a spoke's primary is a repo it
// manages; leaving it out would miss the overlap where one spoke's primary is
// another's ordinary repo. Self-duplication is collapsed here rather than at the
// comparison: a spoke that lists its own primary in repos[] — the normal shape —
// or lists a repo twice is claiming it ONCE, and must never look like a conflict
// with itself.
func repoClaimsOf(h RegistryEntry) map[string]RepoOverlap {
	claims := map[string]RepoOverlap{}
	refs := make([]string, 0, len(h.Repos)+1)
	if p := strings.TrimSpace(h.PrimaryRepo); p != "" {
		refs = append(refs, p)
	}
	refs = append(refs, h.Repos...)
	for _, raw := range refs {
		host, repo, ok := canonicalRepoClaim(h, raw)
		if !ok {
			continue
		}
		key := repoClaimKey(host, repo)
		if _, dup := claims[key]; dup {
			continue
		}
		claims[key] = RepoOverlap{Host: host, Repo: repo}
	}
	return claims
}

// computeRepoOverlaps returns every work item claimed by more than one spoke.
//
// Ordering is deterministic (host, then repo, then hive id) so two responses can
// be diffed and so the alert text does not churn between evaluations. Returns
// nil — not an empty slice — when the fleet is clean, so the field is omitted
// from JSON entirely rather than rendering as an empty array that reads like a
// section with nothing in it.
func computeRepoOverlaps(hives []RegistryEntry) []RepoOverlap {
	type agg struct {
		host  string
		repo  string
		names map[string]string
		ids   []string
	}
	byKey := map[string]*agg{}
	for _, h := range hives {
		id := strings.TrimSpace(h.ID)
		if id == "" {
			continue
		}
		for key, claim := range repoClaimsOf(h) {
			a := byKey[key]
			if a == nil {
				a = &agg{host: claim.Host, repo: claim.Repo, names: map[string]string{}}
				byKey[key] = a
			}
			if _, dup := a.names[id]; dup {
				continue
			}
			a.names[id] = strings.TrimSpace(h.Name)
			a.ids = append(a.ids, id)
		}
	}

	var out []RepoOverlap
	for _, a := range byKey {
		if len(a.ids) < 2 {
			continue
		}
		ids := append([]string(nil), a.ids...)
		sort.Strings(ids)
		claims := make([]OverlapClaim, 0, len(ids))
		for _, id := range ids {
			claims = append(claims, OverlapClaim{ID: id, Name: a.names[id]})
		}
		out = append(out, RepoOverlap{Host: a.host, Repo: a.repo, Hives: claims})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return strings.ToLower(out[i].Repo) < strings.ToLower(out[j].Repo)
	})
	return out
}

// repoOverlapsFor returns the overlaps that involve one hive, preserving the
// input ordering. Used to attach the fleet-level relationship to a per-hive
// surface (the alert rule) without recomputing the index per hive.
func repoOverlapsFor(hiveID string, overlaps []RepoOverlap) []RepoOverlap {
	id := strings.TrimSpace(hiveID)
	if id == "" {
		return nil
	}
	var out []RepoOverlap
	for _, o := range overlaps {
		for _, c := range o.Hives {
			if c.ID == id {
				out = append(out, o)
				break
			}
		}
	}
	return out
}

// repoOverlapAlertMaxRepos bounds how many repos one alert line names. A pair of
// spokes misconfigured onto the same org overlaps on EVERY repo, and a line
// listing forty of them is unreadable — the count carries that case, the names
// carry the ordinary one.
const repoOverlapAlertMaxRepos = 3

// repoOverlapAlertReason renders the alert line for one hive: which repos it
// shares and with whom. Written from the reading hive's point of view, because
// that is whose row the alert appears on.
func repoOverlapAlertReason(hiveID string, overlaps []RepoOverlap) string {
	if len(overlaps) == 0 {
		return ""
	}
	id := strings.TrimSpace(hiveID)

	// Other hives, de-duplicated across every shared repo and ordered so the
	// sentence is stable between evaluations.
	otherSeen := map[string]bool{}
	var others []string
	var repos []string
	for _, o := range overlaps {
		repos = append(repos, o.Repo)
		for _, c := range o.Hives {
			if c.ID == id || otherSeen[c.ID] {
				continue
			}
			otherSeen[c.ID] = true
			label := strings.TrimSpace(c.Name)
			if label == "" {
				label = c.ID
			}
			others = append(others, label)
		}
	}
	sort.Strings(others)

	shown := repos
	extra := 0
	if len(shown) > repoOverlapAlertMaxRepos {
		extra = len(shown) - repoOverlapAlertMaxRepos
		shown = shown[:repoOverlapAlertMaxRepos]
	}
	list := strings.Join(shown, ", ")
	if extra == 1 {
		list += " and 1 more"
	} else if extra > 1 {
		list += " and " + itoa(extra) + " more"
	}

	who := strings.Join(others, ", ")
	if who == "" {
		return "Also managed by another hive: " + list
	}
	return "Also managed by " + who + ": " + list
}
