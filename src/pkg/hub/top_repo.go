package hub

import (
	"sort"
	"strings"
)

const (
	topRepoMembershipScore  = 100
	topRepoOwnerScore       = 300
	topRepoActiveScore      = 75
	topRepoCurrentTaskScore = 50
	topRepoCompletedScore   = 10
	topRepoFailedScore      = 3
)

// userTopRepo returns the org/repo (or org-only fallback) that best represents
// the user's strongest hub-known association. The score is intentionally based
// only on data the hub already keeps per hive: explicit membership/ownership,
// the hive's project mapping, and per-hive contributor/leaderboard activity.
func userTopRepo(u *SaaSUser, hives []RegistryEntry) string {
	return userTopRepoAssociation(u, hives).Label
}

type topRepoAssociation struct {
	Label string
	URL   string
}

func userTopRepoAssociation(u *SaaSUser, hives []RegistryEntry) topRepoAssociation {
	if u == nil || len(u.Hives) == 0 || len(hives) == 0 {
		return topRepoAssociation{}
	}
	usernames := topRepoUsernames(u)
	byID := make(map[string]RegistryEntry, len(hives))
	for _, h := range hives {
		if h.ID != "" {
			byID[h.ID] = h
		}
	}
	scores := map[string]int{}
	urls := map[string]string{}
	for hiveID, role := range u.Hives {
		h, ok := byID[hiveID]
		if !ok {
			continue
		}
		assoc := topRepoCandidate(h)
		if assoc.Label == "" {
			continue
		}
		score := topRepoMembershipScore + topRepoRoleScore(role)
		if topRepoNameMatches(h.Owner, usernames) {
			score += topRepoOwnerScore
		}
		for _, e := range h.Leaderboard {
			if !topRepoNameMatches(e.GitHubUsername, usernames) {
				continue
			}
			score += e.TasksCompleted*topRepoCompletedScore + e.TasksFailed*topRepoFailedScore
			if e.Active {
				score += topRepoActiveScore
			}
			if strings.TrimSpace(e.CurrentTask) != "" {
				score += topRepoCurrentTaskScore
			}
		}
		scores[assoc.Label] += score
		if assoc.URL != "" {
			urls[assoc.Label] = assoc.URL
		}
	}
	if len(scores) == 0 {
		return topRepoAssociation{}
	}
	labels := make([]string, 0, len(scores))
	for label := range scores {
		labels = append(labels, label)
	}
	sort.Slice(labels, func(i, j int) bool {
		if scores[labels[i]] != scores[labels[j]] {
			return scores[labels[i]] > scores[labels[j]]
		}
		return topRepoLess(labels[i], labels[j])
	})
	return topRepoAssociation{Label: labels[0], URL: urls[labels[0]]}
}

func topRepoCandidate(h RegistryEntry) topRepoAssociation {
	label := topRepoLabel(h)
	if label == "" {
		return topRepoAssociation{}
	}
	return topRepoAssociation{Label: label, URL: topRepoURL(h, label)}
}

func topRepoLabel(h RegistryEntry) string {
	if label := repoDisplayLine(h.Org, h.PrimaryRepo); label != "" {
		return label
	}
	if len(h.Repos) == 0 {
		return strings.TrimSpace(h.Org)
	}
	candidates := make([]string, 0, len(h.Repos))
	for _, repo := range h.Repos {
		if label := repoDisplayLine(h.Org, repo); label != "" {
			candidates = append(candidates, label)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return topRepoLess(candidates[i], candidates[j])
	})
	if len(candidates) == 0 {
		return strings.TrimSpace(h.Org)
	}
	return candidates[0]
}

func topRepoURL(h RegistryEntry, label string) string {
	label = strings.Trim(label, "/ ")
	if label == "" {
		return ""
	}
	host := strings.TrimSpace(h.GitHubHost)
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.Trim(host, "/ ")
	if host == "" {
		host = "github.com"
	}
	if !strings.Contains(label, "/") && strings.Contains(label, ".") {
		return ""
	}
	return "https://" + host + "/" + label
}

func topRepoRoleScore(role string) int {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "owner", "admin":
		return topRepoOwnerScore
	case "write", "maintainer", "member":
		return topRepoMembershipScore
	default:
		return 0
	}
}

func topRepoUsernames(u *SaaSUser) map[string]bool {
	names := map[string]bool{}
	for _, name := range []string{u.GitHubUsername, u.LinkedGitHubLogin} {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			names[name] = true
		}
	}
	return names
}

func topRepoNameMatches(name string, names map[string]bool) bool {
	return names[strings.ToLower(strings.TrimSpace(name))]
}

func topRepoLess(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}
