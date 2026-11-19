package customers

import (
	"strings"
)

// The discovery sweep. It reads what the hive already holds and makes no
// network call: the caller hands in dashboard users, links a trusted source
// proved, and sightings of identities next to artefacts. From those it
// keeps profiles for dashboard users, writes edges for people who are
// customers, keeps a bare suggestion for everyone else, assigns automatic
// tags, and prunes whatever points at an artefact that no longer exists.

// Rules that earn an automatic tag.
const (
	RuleRole          = "role"
	RuleAssociation   = "association"
	RuleAuthoredPR    = "history:authored a pull request"
	RuleReportedIssue = "history:reported an issue"
	RuleMeetingOrMail = "history:meetings or mail only"
)

// PredReviewRequested is a requested review on a pull request.
const PredReviewRequested = "review_requested"

// HiveUser is one dashboard user from the access list.
type HiveUser struct {
	Name string
	// Maintainer is true for the roles that run the project (owner and
	// merger); Rule says which, for the tag's provenance.
	Maintainer bool
	Rule       string
}

// ProvenLink is an identity a trusted source proved belongs to a dashboard
// user: a proven calendar email, the owner's connected GitHub account.
type ProvenLink struct {
	User     string
	Identity Identity
}

// Sighting is one identity seen next to an artefact.
type Sighting struct {
	Observation
	// Source names where it was seen ("github_poll", "calendar", ...).
	Source string
	// Maintainer is true when the source says the person is a project
	// member (a GitHub author association of owner, member or
	// collaborator).
	Maintainer bool
}

// SweepInput is everything one sweep saw.
type SweepInput struct {
	Users     []HiveUser
	Links     []ProvenLink
	Sightings []Sighting
	// OwnAddresses are this hive's own mail addresses (hive@<host>), which
	// are never suggested.
	OwnAddresses []string
}

// SweepResult counts what a sweep did.
type SweepResult struct {
	Users              int `json:"users"`
	Linked             int `json:"linked"`
	Edges              int `json:"edges"`
	Suggestions        int `json:"suggestions"`
	TagsChanged        int `json:"tags_changed"`
	PrunedEdges        int `json:"pruned_edges"`
	ExpiredSuggestions int `json:"expired_suggestions"`
	DroppedEvidence    int `json:"dropped_evidence"`
	Hints              int `json:"hints"`
}

// Sweep runs one discovery pass. It is safe to run repeatedly: the same
// input produces the same store.
func (s *Service) Sweep(in SweepInput) (SweepResult, error) {
	var res SweepResult
	maintainers := map[string]string{} // person id -> rule

	// A. Dashboard users are customers, and proven links are theirs.
	for _, u := range in.Users {
		if strings.TrimSpace(u.Name) == "" {
			continue
		}
		p, err := s.store.EnsureHiveUser(u.Name)
		if err != nil {
			continue // a malformed access-list entry must not stop the sweep
		}
		res.Users++
		if u.Maintainer {
			maintainers[p.ID] = RuleRole + ":" + u.Rule
		}
	}
	for _, l := range in.Links {
		p, ok := s.store.Lookup(KindHiveUser, l.User)
		if !ok {
			continue
		}
		linked, err := s.store.AutoLink(p.ID, l.Identity)
		if err != nil {
			continue
		}
		if linked {
			res.Linked++
		}
	}

	// B and C. Sightings become edges for customers and suggestions for
	// everyone else.
	// A calendar group, a mailing list, a service mailbox and the hive's own
	// address are never suggested; one a person already owns keeps its edges.
	sightings := make([]Sighting, 0, len(in.Sightings))
	for _, sg := range in.Sightings {
		if s.deniedIdentity(sg.Kind, sg.Value, in.OwnAddresses) {
			continue
		}
		sightings = append(sightings, sg)
	}
	in.Sightings = sightings
	obs := make([]Observation, 0, len(in.Sightings))
	for _, sg := range in.Sightings {
		obs = append(obs, sg.Observation)
	}
	owners, err := s.store.Observe(obs)
	if err != nil {
		return res, err
	}
	now := s.store.now().UTC()
	var edges []Edge
	for n, sg := range in.Sightings {
		pid, ok := owners[n]
		if !ok {
			continue
		}
		if sg.Maintainer {
			if _, set := maintainers[pid]; !set {
				maintainers[pid] = RuleAssociation + ":" + sg.Source
			}
		}
		if sg.Artefact == "" || sg.Predicate == "" {
			continue
		}
		at := sg.At
		if at.IsZero() {
			at = now
		}
		edges = append(edges, Edge{
			Person: pid, Predicate: sg.Predicate, Artefact: sg.Artefact,
			Via: IdentityKey(sg.Kind, sg.Value), Source: sg.Source, At: at, Confidence: 1,
		})
	}
	if s.index != nil && len(edges) > 0 {
		n, err := s.index.Put(edges)
		if err != nil {
			return res, err
		}
		res.Edges = n
	}

	// Retention: an edge, a suggestion or a piece of evidence does not
	// outlive its artefact.
	exists := s.resolvers.Exists
	if s.index != nil {
		n, err := s.index.Prune(exists)
		if err != nil {
			return res, err
		}
		res.PrunedEdges = n
	}
	if res.ExpiredSuggestions, err = s.store.ExpireSuggestions(exists); err != nil {
		return res, err
	}
	// A suggestion stored before a rule existed, or for an identity a
	// person has since come to own, expires here too.
	stale, err := s.store.ExpireSuggestionsWhere(func(sg Suggestion) bool {
		return IsNonPersonIdentity(sg.Kind, sg.Value, in.OwnAddresses)
	})
	if err != nil {
		return res, err
	}
	res.ExpiredSuggestions += stale
	if res.DroppedEvidence, err = s.store.PruneEvidence(exists); err != nil {
		return res, err
	}
	res.Suggestions = len(s.store.Suggestions())
	if res.Hints, err = s.store.DeriveHints(); err != nil {
		return res, err
	}

	// Automatic tags, from role, association and history. A manual tag is
	// never touched.
	for _, p := range s.store.People() {
		var counts map[string]int
		if s.index != nil {
			if counts, err = s.index.Counts(p.ID); err != nil {
				return res, err
			}
		}
		changed, err := s.store.ApplyAutoTags(p.ID, autoTags(counts, maintainers[p.ID]))
		if err != nil {
			return res, err
		}
		if changed {
			res.TagsChanged++
		}
	}
	return res, nil
}

// IsNonPersonIdentity applies IsNonPersonAddress to an email identity.
func IsNonPersonIdentity(kind IdentityKind, value string, own []string) bool {
	return kind == KindEmail && IsNonPersonAddress(value, own...)
}

// deniedIdentity reports whether a sighting must not become a suggestion: it
// is a non-person address that no customer owns.
func (s *Service) deniedIdentity(kind IdentityKind, value string, own []string) bool {
	if !IsNonPersonIdentity(kind, value, own) {
		return false
	}
	_, owned := s.store.Lookup(kind, value)
	return !owned
}

// githubPredicates are the edges that mean someone works in the repository.
var githubPredicates = []string{
	PredAuthored, PredAssigned, PredReviewed, PredMerged, PredClaimed, PredReported,
	PredCommentedOn, PredMentionedIn, PredReviewRequested,
}

// autoTags decides the automatic tags for one person from their edge counts
// and, when a role or an association says so, the maintainer rule.
func autoTags(counts map[string]int, maintainerRule string) map[string]string {
	tags := map[string]string{}
	if maintainerRule != "" {
		tags[TagMaintainer] = maintainerRule
	}
	github := 0
	for _, p := range githubPredicates {
		github += counts[p]
	}
	other := 0
	for _, n := range counts {
		other += n
	}
	other -= github
	switch {
	case counts[PredAuthored] > 0:
		tags[TagContributor] = RuleAuthoredPR
	case counts[PredReported] > 0:
		tags[TagEndUser] = RuleReportedIssue
	case github == 0 && other > 0 && maintainerRule == "":
		tags[TagStakeholder] = RuleMeetingOrMail
	}
	return tags
}

// mentionMax bounds the logins read out of one body.
const mentionMax = 20

// Mentions returns the distinct GitHub logins @-mentioned in text, in order
// of first appearance. An "@" inside an email address or a code span is not
// a mention.
func Mentions(text string) []string {
	var out []string
	seen := map[string]bool{}
	inCode := false
	for i := 0; i < len(text) && len(out) < mentionMax; i++ {
		c := text[i]
		if c == '`' {
			inCode = !inCode
			continue
		}
		if c != '@' || inCode {
			continue
		}
		if i > 0 && isLoginByte(text[i-1]) {
			continue // part of an email address or a word
		}
		j := i + 1
		for j < len(text) && isLoginByte(text[j]) {
			j++
		}
		login := strings.Trim(text[i+1:j], "-")
		if j < len(text) && (text[j] == '/' || (text[j] == '.' && j+1 < len(text) && isLoginByte(text[j+1]))) {
			// "@org/team" is a team; "@a.b" is not a login. A full stop
			// that ends the sentence is fine.
			i = j
			continue
		}
		i = j - 1
		if login == "" || len(login) > maxLoginLen {
			continue
		}
		key := strings.ToLower(login)
		if !seen[key] {
			seen[key] = true
			out = append(out, login)
		}
	}
	return out
}

// maxLoginLen is the longest GitHub login.
const maxLoginLen = 39

func isLoginByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

// IsBotLogin reports whether a GitHub login is an app or a bot account,
// which is never a person.
func IsBotLogin(login string) bool {
	l := strings.ToLower(strings.TrimSpace(login))
	return l == "" || strings.HasSuffix(l, "[bot]") || strings.HasSuffix(l, "-bot") || l == "ghost"
}
