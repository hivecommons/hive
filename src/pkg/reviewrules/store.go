package reviewrules

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DirEnvVar names the directory the rule index lives in. It is the review
	// feedback sweep's own state directory, so one volume holds both.
	DirEnvVar = "HIVE_REVIEW_FEEDBACK_DIR"
	// DefaultDir sits on the /data volume so the index survives pod rolls.
	DefaultDir = "/data/review-feedback"
	// VaultEnvVar overrides the knowledge vault the rules are written to.
	VaultEnvVar = "HIVE_REVIEW_FEEDBACK_RULES_VAULT"
	// DefaultVaultDir is the local knowledge channel the rules live in.
	DefaultVaultDir = "/data/vaults/review-rules"
	// VaultName is the channel name the vault is connected under.
	VaultName = "review-rules"
	// DisableEnvVar turns rule capture off when set to a false-like value. The
	// sweep itself keeps running.
	DisableEnvVar = "HIVE_REVIEW_FEEDBACK_RULES"
	// TrustedAssociationsEnvVar lists the author associations whose rules are
	// captured at once; every other reviewer's rule is queued for a person.
	TrustedAssociationsEnvVar = "HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS"
	// DefaultTrustedAssociations are the associations that may state a rule
	// on their own authority. CONTRIBUTOR is left out: the sweep routes a
	// contributor's feedback, but a rule changes what every later run is told.
	DefaultTrustedAssociations = "OWNER,MEMBER,COLLABORATOR"
	// MaxRulesEnvVar bounds how many live (active or pending) rules are kept.
	MaxRulesEnvVar = "HIVE_REVIEW_FEEDBACK_RULES_MAX"
	// DefaultMaxRules keeps the pinned set small enough to read.
	DefaultMaxRules = 200
	// MergeThresholdEnvVar overrides how similar two rules' texts must be to
	// merge, as a fraction between 0 and 1.
	MergeThresholdEnvVar = "HIVE_REVIEW_FEEDBACK_RULE_MERGE_THRESHOLD"
	// DefaultMergeThreshold is the token-overlap (Jaccard) at which two rules
	// of the same scope are the same rule.
	DefaultMergeThreshold = 0.8

	indexFile = "rules.json"

	// Confidence levels recorded with a rule: what the hive can claim about
	// how sure it is that the text is what the reviewer meant.
	confidenceSweep         = 0.85
	confidenceAgent         = 0.9
	confidencePending       = 0.6
	confidenceAgentFromSeen = 0.7

	// maxSeenComments bounds the remembered review comments.
	maxSeenComments = 500
	// maxEvaluatedComments bounds the comment ids rule capture classified.
	// Evicting the oldest only means one more, idempotent, classification.
	maxEvaluatedComments = 5000
	// maxRepliesKept bounds the remembered applied agent replies.
	maxRepliesKept = 500
	// titleRunes bounds a rule's entry title.
	titleRunes = 70
	// vaultAuthor is the author recorded on rule entries. No agent has this
	// name, so a pending (draft) rule never reaches an agent through the
	// author-matched draft path.
	vaultAuthor = "review-feedback"
)

// Status is where a rule is in its life.
type Status string

const (
	// StatusActive rules are approved entries the primer loads.
	StatusActive Status = "active"
	// StatusPending rules wait for a person: they are draft entries.
	StatusPending Status = "pending"
	// StatusRetracted rules were withdrawn, by a person or by removing or
	// deprecating the entry.
	StatusRetracted Status = "retracted"
	// StatusMerged rules were found to restate another rule.
	StatusMerged Status = "merged"
)

// Outcome is what a capture did.
type Outcome string

const (
	OutcomeNotRule   Outcome = "not_rule"
	OutcomeCaptured  Outcome = "captured"
	OutcomeQueued    Outcome = "queued"
	OutcomeMerged    Outcome = "merged"
	OutcomeDuplicate Outcome = "duplicate"
	OutcomeRefused   Outcome = "refused"
	OutcomeRefined   Outcome = "refined"
	OutcomeRejected  Outcome = "rejected"
)

// ErrFull is returned when the live rule set is at its bound.
var ErrFull = errors.New("the review-rule set is full; retract a rule to make room")

// Provenance records one comment a rule came from.
type Provenance struct {
	Repo        string    `json:"repo"`
	PR          int       `json:"pr"`
	PRURL       string    `json:"pr_url"`
	CommentID   string    `json:"comment_id"`
	CommentURL  string    `json:"comment_url"`
	Reviewer    string    `json:"reviewer"`
	Association string    `json:"association"`
	At          time.Time `json:"at"`
	// Source is "sweep" for a rule the sweep extracted from the comment and
	// "agent" for one the kicked agent stated in its reply block.
	Source string `json:"source"`
	// Merged marks a comment that restated a rule that already existed.
	Merged bool `json:"merged,omitempty"`
	// Acked is set once the reviewer was told on the PR, or when no
	// notice is owed (a queued rule).
	Acked bool `json:"acked,omitempty"`
}

// Rule is one captured rule.
type Rule struct {
	ID          string       `json:"id"`
	Num         int          `json:"num"`
	Text        string       `json:"text"`
	Scope       Scope        `json:"scope"`
	AppliesTo   string       `json:"applies_to,omitempty"`
	Lane        string       `json:"lane,omitempty"`
	Status      Status       `json:"status"`
	RetractedBy string       `json:"retracted_by,omitempty"`
	MergedInto  string       `json:"merged_into,omitempty"`
	Confidence  float64      `json:"confidence"`
	Provenance  []Provenance `json:"provenance"`
	ForeignHome string       `json:"foreign_home,omitempty"`
	HomeNoted   bool         `json:"home_noted,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// Slug is the knowledge entry id of the rule.
func (r *Rule) Slug() string { return "rule-" + strings.ToLower(r.ID) }

type seenComment struct {
	CommentID   string    `json:"comment_id"`
	URL         string    `json:"url"`
	Reviewer    string    `json:"reviewer"`
	Association string    `json:"association"`
	Repo        string    `json:"repo"`
	PR          int       `json:"pr"`
	Lane        string    `json:"lane"`
	At          time.Time `json:"at"`
}

type index struct {
	Next     int                    `json:"next"`
	Rules    []*Rule                `json:"rules"`
	Comments map[string]string      `json:"comments"`
	Seen     map[string]seenComment `json:"seen"`
	Replies  map[string]time.Time   `json:"replies"`
	// Evaluated are the comments rule capture has classified, keyed like
	// Comments, with when. It is separate from the review-feedback sweep's
	// handled ids, so a comment the agent already answered is still read for
	// rules once, and an index from before this field backfills on its first
	// pass (hivecommons/hive#19136).
	Evaluated map[string]time.Time `json:"evaluated,omitempty"`
}

// Config is the capture policy.
type Config struct {
	// Trusted are the upper-case author associations that may state a rule
	// on their own authority.
	Trusted map[string]bool
	// MaxRules bounds live rules.
	MaxRules int
	// MergeThreshold is the text similarity at which same-scope rules merge.
	MergeThreshold float64
}

// ConfigFromEnv reads the knobs, falling back to the Default* constants.
func ConfigFromEnv() Config {
	cfg := Config{MaxRules: DefaultMaxRules, MergeThreshold: DefaultMergeThreshold}
	list := strings.TrimSpace(os.Getenv(TrustedAssociationsEnvVar))
	if list == "" {
		list = DefaultTrustedAssociations
	}
	cfg.Trusted = ParseAssociations(list)
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(MaxRulesEnvVar))); err == nil && n > 0 {
		cfg.MaxRules = n
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(MergeThresholdEnvVar)), 64); err == nil && f > 0 && f <= 1 {
		cfg.MergeThreshold = f
	}
	return cfg
}

// ParseAssociations parses a comma-separated association list.
func ParseAssociations(list string) map[string]bool {
	out := map[string]bool{}
	for _, a := range strings.Split(list, ",") {
		if a = strings.ToUpper(strings.TrimSpace(a)); a != "" {
			out[a] = true
		}
	}
	return out
}

// Normalized fills unset knobs with the defaults.
func (c Config) Normalized() Config {
	if c.Trusted == nil {
		c.Trusted = ParseAssociations(DefaultTrustedAssociations)
	}
	if c.MaxRules <= 0 {
		c.MaxRules = DefaultMaxRules
	}
	if c.MergeThreshold <= 0 || c.MergeThreshold > 1 {
		c.MergeThreshold = DefaultMergeThreshold
	}
	return c
}

// Enabled reports whether rule capture runs. It is on by default; set
// HIVE_REVIEW_FEEDBACK_RULES to 0, false, off or no to stop it.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(DisableEnvVar))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// Dir resolves the index directory.
func Dir() string {
	if d := strings.TrimSpace(os.Getenv(DirEnvVar)); d != "" {
		return d
	}
	return DefaultDir
}

// VaultDir resolves the knowledge vault the rules are written to.
func VaultDir() string {
	if d := strings.TrimSpace(os.Getenv(VaultEnvVar)); d != "" {
		return d
	}
	return DefaultVaultDir
}

// Store persists captured rules.
type Store struct {
	mu    sync.Mutex
	dir   string
	vault string
	cfg   Config
	st    index
	// Reindex, when set, is called after the vault changed so the primer's
	// view is current without waiting for its periodic refresh.
	Reindex func()
}

// Open loads the index from dir and writes entries to vault. A missing index
// is an empty store; an unreadable one is returned with the error and still
// usable in memory.
func Open(dir, vault string, cfg Config) (*Store, error) {
	s := &Store{dir: dir, vault: vault, cfg: cfg.Normalized()}
	s.st = newIndex()
	b, err := os.ReadFile(filepath.Join(dir, indexFile))
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	var loaded index
	if err := json.Unmarshal(b, &loaded); err != nil {
		return s, err
	}
	s.st = loaded
	s.fillMaps()
	return s, nil
}

func newIndex() index {
	return index{Next: 1, Comments: map[string]string{}, Seen: map[string]seenComment{}, Replies: map[string]time.Time{}, Evaluated: map[string]time.Time{}}
}

func (s *Store) fillMaps() {
	if s.st.Next < 1 {
		s.st.Next = 1
	}
	if s.st.Comments == nil {
		s.st.Comments = map[string]string{}
	}
	if s.st.Seen == nil {
		s.st.Seen = map[string]seenComment{}
	}
	if s.st.Replies == nil {
		s.st.Replies = map[string]time.Time{}
	}
	if s.st.Evaluated == nil {
		s.st.Evaluated = map[string]time.Time{}
	}
}

// Comment is one review comment offered for capture.
type Comment struct {
	Repo        string
	PR          int
	CommentID   string
	URL         string
	Reviewer    string
	Association string
	Body        string
	At          time.Time
	// Lane is the base name of the agent that authored the PR; a rule with no
	// sharper scope applies to that agent's future work.
	Lane string
	// ManagedRepo says whether the hive manages Repo. A rule whose proper
	// home is the reviewed project's own files is only proposed, never
	// written, when the hive does not manage it.
	ManagedRepo bool
}

func (c Comment) key() string { return commentKey(c.Repo, c.PR, c.CommentID) }

func commentKey(repo string, pr int, id string) string {
	return fmt.Sprintf("%s#%d:%s", repo, pr, id)
}

func (c Comment) prURL() string { return fmt.Sprintf("https://github.com/%s/pull/%d", c.Repo, c.PR) }

// Result is what one capture did.
type Result struct {
	Outcome Outcome
	// Rule is a copy of the rule the comment became or joined; nil for
	// OutcomeNotRule and OutcomeRefused.
	Rule *Rule
	// Signals are the classification signals that matched.
	Signals []string
	Err     error
}

func (s *Store) trusted(assoc string) bool {
	return s.cfg.Trusted[strings.ToUpper(strings.TrimSpace(assoc))]
}

func (s *Store) live() int {
	n := 0
	for _, r := range s.st.Rules {
		if r.Status == StatusActive || r.Status == StatusPending {
			n++
		}
	}
	return n
}

func (s *Store) byID(id string) *Rule {
	for _, r := range s.st.Rules {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// resolve follows MergedInto to the rule a comment finally belongs to.
func (s *Store) resolve(r *Rule) *Rule {
	for i := 0; r != nil && r.Status == StatusMerged && i < len(s.st.Rules); i++ {
		r = s.byID(r.MergedInto)
	}
	return r
}

func scopeKey(sc Scope, appliesTo string) string {
	return string(sc) + "|" + strings.ToLower(appliesTo)
}

func (r *Rule) scopeKey() string { return scopeKey(r.Scope, r.AppliesTo) }

var tokenRe = regexp.MustCompile(`[a-z0-9_./-]+`)

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tokenRe.FindAllString(strings.ToLower(s), -1) {
		if t = strings.Trim(t, "./-"); t != "" {
			out[t] = true
		}
	}
	return out
}

// similarity is the Jaccard overlap of two texts' token sets.
func similarity(a, b string) float64 {
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

// nearDuplicate finds a live rule of the same scope whose text restates text.
func (s *Store) nearDuplicate(text, key string, skip *Rule) *Rule {
	for _, r := range s.st.Rules {
		if r == skip || (r.Status != StatusActive && r.Status != StatusPending) || r.scopeKey() != key {
			continue
		}
		if similarity(r.Text, text) >= s.cfg.MergeThreshold {
			return r
		}
	}
	return nil
}

// Remember records that the hive saw a review comment, so an agent's reply that
// cites it as evidence can be tied to its reviewer and association.
func (s *Store) Remember(c Comment, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.URL == "" {
		return nil
	}
	return s.txn(func() []*Rule {
		s.st.Seen[c.URL] = seenComment{CommentID: c.CommentID, URL: c.URL, Reviewer: c.Reviewer, Association: c.Association,
			Repo: c.Repo, PR: c.PR, Lane: c.Lane, At: now}
		if len(s.st.Seen) > maxSeenComments {
			urls := make([]string, 0, len(s.st.Seen))
			for u := range s.st.Seen {
				urls = append(urls, u)
			}
			sort.Slice(urls, func(i, j int) bool { return s.st.Seen[urls[i]].At.Before(s.st.Seen[urls[j]].At) })
			for _, u := range urls[:len(s.st.Seen)-maxSeenComments] {
				delete(s.st.Seen, u)
			}
		}
		return nil
	})
}

// CaptureComment classifies a comment and, when it states a rule, captures it.
// It is idempotent per comment: a comment id that was captured, queued or
// merged before returns OutcomeDuplicate and changes nothing.
func (s *Store) CaptureComment(c Comment, now time.Time) Result {
	cl := Classify(c.Body)
	if cl.Kind != KindRule {
		return Result{Outcome: OutcomeNotRule}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.st.Comments[c.key()]; ok {
		return Result{Outcome: OutcomeDuplicate, Rule: s.copyOf(s.resolve(s.byID(id))), Signals: cl.Signals}
	}
	text := ExtractRuleText(c.Body)
	if text == "" {
		return Result{Outcome: OutcomeRefused, Signals: cl.Signals, Err: errors.New("the comment has no text to record")}
	}
	home := ""
	if !c.ManagedRepo && ForeignHomeHint(c.Body) {
		home = c.Repo
	}
	return s.captureLocked(c, text, ScopeAgent, c.Lane, home, "sweep", confidenceSweep, cl.Signals, now)
}

// Evaluated reports whether rule capture already classified this comment.
func (s *Store) Evaluated(repo string, pr int, commentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.st.Evaluated[commentKey(repo, pr, commentID)]
	return ok
}

// MarkEvaluated records that rule capture classified these comments, so a
// later pass does not read them again.
func (s *Store) MarkEvaluated(repo string, pr int, commentIDs []string, now time.Time) error {
	if len(commentIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.txn(func() []*Rule {
		for _, id := range commentIDs {
			s.st.Evaluated[commentKey(repo, pr, id)] = now
		}
		if len(s.st.Evaluated) > maxEvaluatedComments {
			keys := make([]string, 0, len(s.st.Evaluated))
			for k := range s.st.Evaluated {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				a, b := s.st.Evaluated[keys[i]], s.st.Evaluated[keys[j]]
				if !a.Equal(b) {
					return a.Before(b)
				}
				return keys[i] < keys[j]
			})
			for _, k := range keys[:len(s.st.Evaluated)-maxEvaluatedComments] {
				delete(s.st.Evaluated, k)
			}
		}
		return nil
	})
}

// RuleForComment returns a copy of the rule this comment was captured as or
// merged into, following merges, or nil when it became none.
func (s *Store) RuleForComment(repo string, pr int, commentID string) *Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.st.Comments[commentKey(repo, pr, commentID)]
	if !ok {
		return nil
	}
	return s.copyOf(s.resolve(s.byID(id)))
}

// captureLocked creates a rule, or joins an existing near-identical one.
func (s *Store) captureLocked(c Comment, text string, sc Scope, appliesTo, home, source string, conf float64, signals []string, now time.Time) Result {
	trusted := s.trusted(c.Association)
	prov := Provenance{Repo: c.Repo, PR: c.PR, PRURL: c.prURL(), CommentID: c.CommentID, CommentURL: c.URL,
		Reviewer: c.Reviewer, Association: strings.ToUpper(c.Association), At: now, Source: source}
	key := scopeKey(sc, appliesTo)
	var res Result
	err := s.txn(func() []*Rule {
		if dup := s.nearDuplicate(text, key, nil); dup != nil {
			prov.Merged = true
			prov.Acked = !trusted
			dup.Provenance = append(dup.Provenance, prov)
			if trusted && dup.Status == StatusPending {
				dup.Status = StatusActive
			}
			if home != "" && dup.ForeignHome == "" {
				dup.ForeignHome = home
			}
			dup.UpdatedAt = now
			s.st.Comments[c.key()] = dup.ID
			res = Result{Outcome: OutcomeMerged, Rule: s.copyOf(dup), Signals: signals}
			return []*Rule{dup}
		}
		if s.live() >= s.cfg.MaxRules {
			res = Result{Outcome: OutcomeRefused, Signals: signals, Err: ErrFull}
			return nil
		}
		r := &Rule{ID: "R-" + strconv.Itoa(s.st.Next), Num: s.st.Next, Text: text, Scope: sc, AppliesTo: appliesTo, Lane: c.Lane,
			Status: StatusActive, Confidence: conf, ForeignHome: home, CreatedAt: now, UpdatedAt: now}
		s.st.Next++
		outcome := OutcomeCaptured
		if !trusted {
			r.Status, r.Confidence, outcome = StatusPending, confidencePending, OutcomeQueued
			prov.Acked = true
		}
		r.Provenance = []Provenance{prov}
		s.st.Rules = append(s.st.Rules, r)
		s.st.Comments[c.key()] = r.ID
		res = Result{Outcome: outcome, Rule: s.copyOf(r), Signals: signals}
		return []*Rule{r}
	})
	if err != nil {
		return Result{Outcome: OutcomeRefused, Signals: signals, Err: err}
	}
	return res
}

func (s *Store) copyOf(r *Rule) *Rule {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Provenance = append([]Provenance(nil), r.Provenance...)
	return &cp
}

// Applied is what one entry of an agent's block did.
type Applied struct {
	Outcome Outcome
	Rule    *Rule
	Reason  string
}

// ApplyReply applies the structured block in an agent reply. replyID names the
// reply, so the same reply is applied once. An entry must cite, as its
// evidence, a review comment the hive saw (Remember): the reviewer's trust
// comes from that comment, never from anything the block claims. managed says
// whether the hive manages a repo.
func (s *Store) ApplyReply(replyID, body string, managed func(repo string) bool, now time.Time) []Applied {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, done := s.st.Replies[replyID]; done {
		return nil
	}
	rules, errs := ParseBlocks(body)
	var out []Applied
	for _, e := range errs {
		out = append(out, Applied{Outcome: OutcomeRejected, Reason: e.Error()})
	}
	for _, br := range rules {
		out = append(out, s.applyOneLocked(br, managed, now))
	}
	if len(rules)+len(errs) > 0 || strings.Contains(body, BlockMarker) {
		_ = s.txn(func() []*Rule {
			s.st.Replies[replyID] = now
			if len(s.st.Replies) > maxRepliesKept {
				ids := make([]string, 0, len(s.st.Replies))
				for id := range s.st.Replies {
					ids = append(ids, id)
				}
				sort.Slice(ids, func(i, j int) bool { return s.st.Replies[ids[i]].Before(s.st.Replies[ids[j]]) })
				for _, id := range ids[:len(s.st.Replies)-maxRepliesKept] {
					delete(s.st.Replies, id)
				}
			}
			return nil
		})
	}
	return out
}

func (s *Store) applyOneLocked(br BlockRule, managed func(string) bool, now time.Time) Applied {
	seen, ok := s.st.Seen[br.EvidenceURL]
	if !ok {
		return Applied{Outcome: OutcomeRejected, Reason: "evidence_url does not name a review comment the hive saw"}
	}
	sc, appliesTo := Scope(br.Scope), br.AppliesTo
	if sc == "" {
		sc = ScopeAgent
	}
	switch sc {
	case ScopeAgent:
		if appliesTo == "" {
			appliesTo = seen.Lane
		}
	case ScopeGlobal:
		appliesTo = ""
	}
	home := ""
	if sc == ScopeRepo && managed != nil && !managed(appliesTo) {
		home = appliesTo
	}
	key := commentKey(seen.Repo, seen.PR, seen.CommentID)
	c := Comment{Repo: seen.Repo, PR: seen.PR, CommentID: seen.CommentID, URL: seen.URL, Reviewer: seen.Reviewer,
		Association: seen.Association, Lane: seen.Lane}
	id, known := s.st.Comments[key]
	if !known {
		res := s.captureLocked(c, br.Text, sc, appliesTo, home, "agent", confidenceAgentFromSeen, nil, now)
		return Applied{Outcome: res.Outcome, Rule: res.Rule, Reason: errText(res.Err)}
	}
	r := s.resolve(s.byID(id))
	if r == nil || (r.Status != StatusActive && r.Status != StatusPending) {
		return Applied{Outcome: OutcomeRejected, Reason: "the rule this comment became is no longer live"}
	}
	var out Applied
	err := s.txn(func() []*Rule {
		touched := []*Rule{r}
		r.Text, r.Scope, r.AppliesTo, r.UpdatedAt = br.Text, sc, appliesTo, now
		if s.trusted(seen.Association) {
			r.Confidence = confidenceAgent
		}
		if home != "" && r.ForeignHome != home {
			r.ForeignHome, r.HomeNoted = home, false
		}
		out = Applied{Outcome: OutcomeRefined, Rule: s.copyOf(r)}
		if dup := s.nearDuplicate(r.Text, r.scopeKey(), r); dup != nil {
			keep, drop := dup, r
			if drop.Num < keep.Num {
				keep, drop = drop, keep
			}
			drop.Status, drop.MergedInto, drop.UpdatedAt = StatusMerged, keep.ID, now
			keep.Provenance = append(keep.Provenance, drop.Provenance...)
			if keep.Status == StatusPending && s.anyTrusted(keep.Provenance) {
				keep.Status = StatusActive
			}
			if keep.ForeignHome == "" {
				keep.ForeignHome = drop.ForeignHome
			}
			for ck, rid := range s.st.Comments {
				if rid == drop.ID {
					s.st.Comments[ck] = keep.ID
				}
			}
			touched = []*Rule{keep, drop}
			out = Applied{Outcome: OutcomeMerged, Rule: s.copyOf(keep)}
		}
		return touched
	})
	if err != nil {
		return Applied{Outcome: OutcomeRejected, Reason: err.Error()}
	}
	return out
}

func (s *Store) anyTrusted(p []Provenance) bool {
	for _, pr := range p {
		if s.trusted(pr.Association) {
			return true
		}
	}
	return false
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Retract withdraws a rule: its entry stays in the vault as deprecated, so the
// history is readable, and the primer stops loading it.
func (s *Store) Retract(id, by string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byID(strings.ToUpper(strings.TrimSpace(id)))
	if r == nil {
		return fmt.Errorf("no rule %q", id)
	}
	if r.Status == StatusRetracted {
		return nil
	}
	return s.txn(func() []*Rule {
		r.Status, r.RetractedBy, r.UpdatedAt = StatusRetracted, by, now
		return []*Rule{r}
	})
}

// Get returns a copy of the rule with this id, or nil.
func (s *Store) Get(id string) *Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copyOf(s.byID(strings.ToUpper(strings.TrimSpace(id))))
}

// List returns copies of every rule, in id order.
func (s *Store) List() []*Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Rule, 0, len(s.st.Rules))
	for _, r := range s.st.Rules {
		out = append(out, s.copyOf(r))
	}
	return out
}

// Ack is a capture the reviewer has not been told about yet.
type Ack struct {
	Rule      *Rule
	CommentID string
	URL       string
	// Merged is true when the comment restated an existing rule.
	Merged bool
}

// UnackedFor lists the captures from PR repo#pr whose reviewer was not told.
func (s *Store) UnackedFor(repo string, pr int) []Ack {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Ack
	for _, r := range s.st.Rules {
		if r.Status != StatusActive {
			continue
		}
		for _, p := range r.Provenance {
			if p.Repo == repo && p.PR == pr && !p.Acked {
				out = append(out, Ack{Rule: s.copyOf(r), CommentID: p.CommentID, URL: p.CommentURL, Merged: p.Merged})
			}
		}
	}
	return out
}

// MarkAcked records that the reviewer was told about these comments' captures.
func (s *Store) MarkAcked(repo string, pr int, commentIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[string]bool{}
	for _, id := range commentIDs {
		want[id] = true
	}
	return s.txn(func() []*Rule {
		for _, r := range s.st.Rules {
			for i := range r.Provenance {
				p := &r.Provenance[i]
				if p.Repo == repo && p.PR == pr && want[p.CommentID] {
					p.Acked = true
				}
			}
		}
		return nil
	})
}

// HomeProposals lists live rules whose proper home is a repo the hive does not
// manage and whose proposal was not posted yet. One proposal per rule.
func (s *Store) HomeProposals() []*Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Rule
	for _, r := range s.st.Rules {
		if r.Status == StatusActive && r.ForeignHome != "" && !r.HomeNoted {
			out = append(out, s.copyOf(r))
		}
	}
	return out
}

// MarkHomeNoted records the proposal for rule id was posted.
func (s *Store) MarkHomeNoted(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byID(id)
	if r == nil {
		return fmt.Errorf("no rule %q", id)
	}
	return s.txn(func() []*Rule {
		r.HomeNoted = true
		return nil
	})
}

// Reconcile reads the lifecycle state of each rule's vault entry and brings the
// index in line, so an operator's `hivectl knowledge state <id> deprecated`, or
// deleting the entry, retracts the rule, and approving a queued (draft) entry
// activates it. It returns one line per change, for the log.
func (s *Store) Reconcile(now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var changes []string
	var touched []*Rule
	for _, r := range s.st.Rules {
		if r.Status != StatusActive && r.Status != StatusPending {
			continue
		}
		state, found := s.entryState(r)
		switch {
		case !found:
			r.Status, r.RetractedBy = StatusRetracted, "knowledge entry removed"
			changes = append(changes, r.ID+" retracted: its knowledge entry was removed")
		case state == "deprecated" || state == "superseded":
			r.Status, r.RetractedBy = StatusRetracted, "knowledge entry "+state
			changes = append(changes, r.ID+" retracted: its knowledge entry is "+state)
		case state == "draft" && r.Status == StatusActive:
			r.Status = StatusPending
			changes = append(changes, r.ID+" moved back to the review queue: its knowledge entry is a draft")
		case state == "approved" && r.Status == StatusPending:
			r.Status = StatusActive
			for i := range r.Provenance {
				r.Provenance[i].Acked = false
			}
			changes = append(changes, r.ID+" confirmed by a person: its knowledge entry is approved")
		default:
			continue
		}
		r.UpdatedAt = now
		touched = append(touched, r)
	}
	if len(touched) > 0 {
		if err := s.saveIndex(); err != nil {
			changes = append(changes, "index not saved: "+err.Error())
		}
	}
	return changes
}

// txn runs a mutation, then writes the touched rules' entries and the index.
// When a write fails the in-memory state is restored, so memory never claims
// something the disk does not hold. The caller holds s.mu.
func (s *Store) txn(fn func() []*Rule) error {
	snap, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	touched := fn()
	werr := s.flush(touched)
	if werr != nil {
		var restored index
		if json.Unmarshal(snap, &restored) == nil {
			s.st = restored
			s.fillMaps()
		}
	}
	return werr
}

func (s *Store) flush(touched []*Rule) error {
	if len(touched) > 0 {
		for _, r := range touched {
			if err := s.writeEntry(r); err != nil {
				return err
			}
		}
		if s.Reindex != nil {
			s.Reindex()
		}
	}
	return s.saveIndex()
}

func (s *Store) saveIndex() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.st, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, indexFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, indexFile))
}
