package github

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Cross-repo decomposition (hivecommons/hive#18680).
//
// A tracker in one managed repo can be split into children whose work lives
// in another managed repo — hivecommons/hive#18596 (the companion app
// roadmap) has children whose code is all in hivecommons/quorum-companion.
// The relay files such a child in the repo where its work lives and links it
// back to the parent in full cross-repo form: `Part of owner/repo#N` in the
// body and `<!-- hive:parent owner/repo#N -->` as the machine-readable
// marker. A bare `#N` resolves only inside the child's own repo, so a bare
// link on a cross-repo child names the wrong issue.

// ChildRef names one child of a decomposed parent. Repo is the child's
// "owner/name" when it lives in a different repo from the parent, and ""
// when it shares the parent's repo.
type ChildRef struct {
	Repo   string
	Number int
}

// String renders the ref the way GitHub resolves it from the parent: a bare
// `#N` for a same-repo child, `owner/repo#N` otherwise.
func (r ChildRef) String() string {
	if r.Repo == "" {
		return "#" + strconv.Itoa(r.Number)
	}
	return r.Repo + "#" + strconv.Itoa(r.Number)
}

// sortChildRefs orders same-repo children first, then by repo and number.
func sortChildRefs(refs []ChildRef) {
	sort.Slice(refs, func(i, j int) bool {
		a, b := refs[i], refs[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Number < b.Number
	})
}

// fullRepoName is the lowercased "owner/name" spelling repo resolves to,
// qualifying a bare name with org.
func fullRepoName(org, repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	if !strings.Contains(repo, "/") && org != "" {
		repo = org + "/" + repo
	}
	return strings.ToLower(repo)
}

// ParentMarkerFor returns the body marker for parent issue n in parentRepo.
// An empty parentRepo means the child's own repo and yields the bare form
// ParentMarker writes; any other repo yields `<!-- hive:parent owner/repo#N -->`.
func ParentMarkerFor(parentRepo string, n int) string {
	parentRepo = strings.TrimSpace(parentRepo)
	if parentRepo == "" {
		return ParentMarker(n)
	}
	return parentMarkerPrefix + parentRepo + "#" + strconv.Itoa(n) + " -->"
}

// ParseParentMarkerRef returns the parent a body's marker names: its repo
// ("" for the bare same-repo form) and number, or ("", 0) when there is no
// marker.
func ParseParentMarkerRef(body string) (repo string, n int) {
	m := parentMarkerRe.FindStringSubmatch(body)
	if m == nil {
		return "", 0
	}
	n, _ = strconv.Atoi(m[2])
	return m[1], n
}

// markerNamesParent reports whether body's parent marker names parent issue
// number in repoFull (lowercased "owner/name"): a bare marker always means
// the child's own repo, so it matches only when the child is enumerated
// from repoFull itself.
func markerNamesParent(body, repoFull string, parent int) bool {
	repo, n := ParseParentMarkerRef(body)
	if n != parent || n <= 0 {
		return false
	}
	return repo == "" || strings.EqualFold(repo, repoFull)
}

// bareParentRefRe matches a bare `#N` that is not already qualified with a
// repo (no word character or `/` before the `#`) and is not the prefix of a
// longer number.
func bareParentRefRe(n int) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^\w/#])#` + strconv.Itoa(n) + `\b`)
}

// withCrossRepoParentReference rewrites a cross-repo child's body so its
// link to the parent resolves from the child's repo (hivecommons/hive#18680):
// every bare `#N` naming the parent becomes `owner/repo#N`, and when the
// body has no `Part of owner/repo#N` line one is put first. A same-repo
// child (parentRepo "") is returned unchanged.
func withCrossRepoParentReference(body, parentRepo string, parent int) string {
	parentRepo = strings.TrimSpace(parentRepo)
	if parentRepo == "" || parent <= 0 {
		return body
	}
	full := parentRepo + "#" + strconv.Itoa(parent)
	// The marker is rewritten by withParentMarker; leave it alone here so a
	// bare marker is replaced rather than turned into a second spelling.
	marker := ""
	if m := parentMarkerRe.FindString(body); m != "" {
		marker = m
		body = strings.Replace(body, m, "\x00hive-parent-marker\x00", 1)
	}
	body = bareParentRefRe(parent).ReplaceAllString(body, "${1}"+full)
	if marker != "" {
		body = strings.Replace(body, "\x00hive-parent-marker\x00", marker, 1)
	}
	partOf := regexp.MustCompile(`(?i)\bpart\s+of\s+` + regexp.QuoteMeta(full) + `\b`)
	if !partOf.MatchString(body) {
		trimmed := strings.TrimLeft(body, "\n")
		if trimmed == "" {
			return "Part of " + full
		}
		body = "Part of " + full + "\n\n" + trimmed
	}
	return body
}

// openIssueSnapshots keeps the newest enumerated set of open issue numbers
// per repo (lowercased "owner/name"), so a parent's tracker progress can
// count a child that lives in another managed repo (hivecommons/hive#18680).
// Repos are enumerated one after another, so a parent may read another
// repo's set from the previous pass; that lag only affects the display
// count — closing a parent always re-reads its children live. Zero value is
// ready to use.
type openIssueSnapshots struct {
	mu   sync.RWMutex
	sets map[string]map[int]struct{}
}

func (s *openIssueSnapshots) store(repoFull string, open map[int]struct{}) {
	if s == nil || repoFull == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sets == nil {
		s.sets = map[string]map[int]struct{}{}
	}
	s.sets[strings.ToLower(repoFull)] = open
}

// isOpen reports whether repoFull#n was open in the newest snapshot of that
// repo; known is false when the repo has not been enumerated yet.
func (s *openIssueSnapshots) isOpen(repoFull string, n int) (open, known bool) {
	if s == nil {
		return false, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	set, ok := s.sets[strings.ToLower(repoFull)]
	if !ok {
		return false, false
	}
	_, open = set[n]
	return open, true
}

// crossRepoOpenFunc resolves whether a child in another repo is open; known
// is false when nothing is known about that repo.
type crossRepoOpenFunc func(repoFull string, n int) (open, known bool)

// crossRepoParent returns the parent's "owner/name" when req names a parent
// in a different repo from the child, or "" for a same-repo parent.
func (c *Client) crossRepoParent(req IssueRequest) string {
	parentRepo := strings.TrimSpace(req.ParentRepo)
	if req.Parent <= 0 || parentRepo == "" {
		return ""
	}
	pOwner, pName := c.splitRepo(parentRepo)
	cOwner, cName := c.splitRepo(strings.TrimSpace(req.Repo))
	if strings.EqualFold(pOwner+"/"+pName, cOwner+"/"+cName) {
		return ""
	}
	return pOwner + "/" + pName
}

// resolveChildIssueRepo settles which repo a child with a cross-repo parent
// is filed in (hivecommons/hive#18680). The child goes where its work lives
// (req.Repo) when this hive manages that repo; otherwise it falls back to the
// parent's repo, so the relay never files in an unmanaged repo. A ParentRepo
// that names the child's own repo is cleared. Returns the requested repo when
// it fell back, "" otherwise. The caller has already refused an unmanaged
// parent repo.
func (c *Client) resolveChildIssueRepo(req *IssueRequest) string {
	if req == nil || req.Parent <= 0 || strings.TrimSpace(req.ParentRepo) == "" {
		return ""
	}
	if c.crossRepoParent(*req) == "" {
		req.ParentRepo = ""
		return ""
	}
	if in, configured := c.repoInConfiguredSet(req.Repo); configured && !in {
		from := req.Repo
		req.Repo = strings.TrimSpace(req.ParentRepo)
		req.ParentRepo = ""
		return from
	}
	return ""
}

// configuredRepoSpelling returns the configured project.repos entry repo
// names (compared as full owner/name, case-insensitively), or repo
// unchanged when it is not configured.
func (c *Client) configuredRepoSpelling(repo string) string {
	repo = strings.TrimSpace(repo)
	if c == nil || repo == "" {
		return repo
	}
	owner, name := c.splitRepo(repo)
	want := strings.ToLower(owner + "/" + name)
	for _, r := range c.getRepos() {
		o, n := c.splitRepo(strings.TrimSpace(r))
		if strings.ToLower(o+"/"+n) == want {
			return r
		}
	}
	return repo
}

// canonicalizeClaimRepos keys every claim by the configured spelling of
// its issue's repo (hivecommons/hive#18676). A PR names an issue in another
// repo in full `owner/repo#N` form, so its claim carried "owner/repo" while
// enumeration keys issues by the configured spelling — the bare repo name
// on a default config. The ledger lookup then missed the claim entirely: a
// quorum-companion PR with `Refs hivecommons/hive#18660` left #18660
// Actionable with no PR link while the PR sat at needs-decision.
func (c *Client) canonicalizeClaimRepos(claims []IssueClaim) {
	for i := range claims {
		claims[i].Repo = c.configuredRepoSpelling(claims[i].Repo)
	}
}
