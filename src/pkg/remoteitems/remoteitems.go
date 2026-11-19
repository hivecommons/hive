// Package remoteitems follows up on the issues and pull requests the hive
// opened in repositories it does not manage (hivecommons/hive#18199).
//
// A write-tier reference repo (project.reference_repos) gets no webhooks: the
// hive's App is not installed there, by design (hivecommons/hive#18196). Its
// items are not enumerated either: EnumerateActionable walks project.repos
// only. So a maintainer's comment or review on an issue or PR the hive opened
// there would never reach the agent that opened it. This package closes that
// gap with the smallest possible surface:
//
//   - it LISTS the open items in each write-tier reference repo authored by
//     the hive owner's login (the identity the writes appear under) and keeps
//     the ones whose body carries a hive marker (the "— hive:" attribution
//     trailer or the gh-wrapper's 🐝 identity footer);
//   - for each tracked item it fetches the comments, review comments and
//     reviews newer than its cursor, drops the owner's own and bots', and
//     delivers the rest to the agent the marker names: as a resume kick into
//     the agent's live session when it has one, else as a handoff section on
//     the agent's next kick;
//   - it prunes items that are no longer open.
//
// That is all. There is no backlog scan of the remote, no queue, no kick
// driven by remote items and no stage engine on them; a remote repo is
// participated in, never managed.
package remoteitems

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultPath is where the tracked-item store lives on the spoke; FileEnv
// overrides it.
const (
	DefaultPath = "/data/turn/remote-items.json"
	FileEnv     = "HIVE_REMOTE_ITEMS_FILE"
)

const (
	storeFileMode = 0o600
	storeDirMode  = 0o700
	// maxCommentsPerItemPerPass bounds how much feedback one pass routes for
	// one item, so a first pass over an item with a long history does not
	// flood the agent.
	maxCommentsPerItemPerPass = 5
	// maxDetailRunes bounds a quoted comment in the message.
	maxDetailRunes = 600
	// maxPendingPerAgent bounds the handoff queue per agent.
	maxPendingPerAgent = 20
)

// Path resolves the store path from the environment.
func Path() string {
	if p := strings.TrimSpace(os.Getenv(FileEnv)); p != "" {
		return p
	}
	return DefaultPath
}

// RemoteItem is one open issue or PR as the forge lists it.
type RemoteItem struct {
	Repo      string
	Number    int
	IsPR      bool
	Title     string
	Body      string
	URL       string
	Author    string
	CreatedAt time.Time
}

// Comment is one piece of feedback on a tracked item.
type Comment struct {
	// Kind is "comment", "review_comment" or "review".
	Kind      string
	Author    string
	Body      string
	URL       string
	CreatedAt time.Time
}

// Fetcher is the forge seam. Production reads under the owner token
// (pkg/github.RemoteItemsFetcher); tests fake it.
type Fetcher interface {
	// ListOwnOpenItems lists the open issues and PRs in repo created by login.
	ListOwnOpenItems(ctx context.Context, repo, login string) ([]RemoteItem, error)
	// CommentsSince lists the comments (and, for a PR, review comments and
	// reviews) on repo#number created after since, oldest first.
	CommentsSince(ctx context.Context, repo string, number int, isPR bool, since time.Time) ([]Comment, error)
}

// Resumer delivers a message into an agent's live session. crux.Runtime
// satisfies it.
type Resumer interface {
	SessionID(name string) (string, bool)
	SendResumeKick(name, message, sessionID string) error
}

// MarkerFunc reports whether body carries a hive marker and which agent it
// names ("" when the marker names none). pkg/github.HiveMarkerAgent is the
// production implementation.
type MarkerFunc func(body string) (agent string, marked bool)

// Item is one tracked remote item.
type Item struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	IsPR   bool   `json:"is_pr"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	// Agent is the configured agent the marker names; "" when it names none
	// or an unknown one, in which case the item is tracked but not routed.
	Agent string `json:"agent,omitempty"`
	// Cursor is the creation time of the newest feedback already routed.
	Cursor    time.Time `json:"cursor"`
	FirstSeen time.Time `json:"first_seen"`
}

// Key is the item's map key, "owner/repo#N" case-folded on the repo.
func (it Item) Key() string { return itemKey(it.Repo, it.Number) }

func itemKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// Pending is feedback queued for an agent's next fresh kick because its live
// session could not take a resume kick.
type Pending struct {
	Key      string    `json:"key"`
	Agent    string    `json:"agent"`
	Detail   string    `json:"detail"`
	Session  string    `json:"session,omitempty"`
	QueuedAt time.Time `json:"queued_at"`
}

// state is the on-disk shape.
type state struct {
	Items   map[string]Item `json:"items"`
	Pending []Pending       `json:"pending,omitempty"`
}

// Store is the file-backed tracker. One file, replaced atomically.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore returns a store over path ("" means Path()).
func NewStore(path string) *Store {
	if strings.TrimSpace(path) == "" {
		path = Path()
	}
	return &Store{path: path}
}

// Path is the store file.
func (s *Store) Path() string { return s.path }

func (s *Store) load() (state, error) {
	st := state{Items: map[string]Item{}}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return state{Items: map[string]Item{}}, fmt.Errorf("%s: %w", s.path, err)
	}
	if st.Items == nil {
		st.Items = map[string]Item{}
	}
	return st, nil
}

func (s *Store) save(st state) error {
	if err := os.MkdirAll(filepath.Dir(s.path), storeDirMode); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, storeFileMode); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Items returns the tracked items, sorted by key.
func (s *Store) Items() ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(st.Items))
	for _, it := range st.Items {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

// PendingFor returns the queued handoffs for any of names.
func (s *Store) PendingFor(names ...string) ([]Pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	var out []Pending
	for _, p := range st.Pending {
		if want[p.Agent] {
			out = append(out, p)
		}
	}
	return out, nil
}

// Outcome is what one pass did.
type Outcome struct {
	Repos     int
	Tracked   int
	Added     int
	Pruned    int
	Routed    int
	Queued    int
	Unrouted  int
	ListErrs  int
	FetchErrs int
}

// Poller runs passes.
type Poller struct {
	Store  *Store
	Fetch  Fetcher
	Marker MarkerFunc
	// ResolveAgent maps the name a marker carries (an agent name or display
	// name) to the configured agent to route to, "" when none matches; nil
	// accepts any non-empty name as-is.
	ResolveAgent func(name string) string
	Logger       *slog.Logger
}

// Target is one watched repo and the login the hive's writes there appear
// under: the hive owner's, or the App bot's when the repo writes as the bot
// (project.remote_write_identity, hivecommons/hive#18516).
type Target struct {
	Repo  string
	Login string
}

// Run performs one pass over repos for one login; see RunTargets.
func (p *Poller) Run(ctx context.Context, repos []string, login string, resumer Resumer, now time.Time) Outcome {
	targets := make([]Target, 0, len(repos))
	for _, r := range repos {
		targets = append(targets, Target{Repo: r, Login: login})
	}
	return p.RunTargets(ctx, targets, resumer, now)
}

// RunTargets performs one pass over targets, each listed under its own
// login: list, track, prune, fetch feedback, deliver. Errors on one repo or
// item never stop the pass. resumer may be nil (everything is queued as a
// handoff). A target without a login is skipped; a repo not in targets
// drops its tracked items.
func (p *Poller) RunTargets(ctx context.Context, targets []Target, resumer Resumer, now time.Time) Outcome {
	var out Outcome
	loginFor := map[string]string{}
	var repos []string
	for _, t := range targets {
		repo, login := strings.TrimSpace(t.Repo), strings.TrimSpace(t.Login)
		if repo == "" || login == "" {
			continue
		}
		if _, dup := loginFor[strings.ToLower(repo)]; dup {
			continue
		}
		loginFor[strings.ToLower(repo)] = login
		repos = append(repos, repo)
	}
	if p == nil || p.Store == nil || p.Fetch == nil || p.Marker == nil || len(repos) == 0 {
		return out
	}
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	p.Store.mu.Lock()
	defer p.Store.mu.Unlock()
	st, err := p.Store.load()
	if err != nil {
		logger.Warn("remote items: store unreadable, pass skipped", "path", p.Store.path, "error", err.Error())
		return out
	}
	seen := map[string]bool{}
	listed := map[string]bool{} // repos whose listing succeeded
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		out.Repos++
		login := loginFor[strings.ToLower(repo)]
		items, err := p.Fetch.ListOwnOpenItems(ctx, repo, login)
		if err != nil {
			out.ListErrs++
			logger.Warn("remote items: listing failed, keeping tracked items", "repo", repo, "error", err.Error())
			continue
		}
		listed[strings.ToLower(repo)] = true
		for _, ri := range items {
			if !strings.EqualFold(ri.Author, login) {
				continue
			}
			agent, marked := p.Marker(ri.Body)
			if !marked {
				continue
			}
			key := itemKey(repo, ri.Number)
			seen[key] = true
			cur, ok := st.Items[key]
			if !ok {
				cur = Item{Repo: repo, Number: ri.Number, IsPR: ri.IsPR, FirstSeen: now, Cursor: ri.CreatedAt}
				out.Added++
			}
			cur.Title, cur.URL, cur.IsPR = ri.Title, ri.URL, ri.IsPR
			cur.Agent = p.resolveAgent(agent)
			if cur.Agent == "" && agent != "" {
				logger.Debug("remote items: marker names an unknown agent, tracking only", "repo", repo, "number", ri.Number, "agent", agent)
			}
			st.Items[key] = cur
		}
	}
	// Prune: an item that a successful listing no longer shows is closed
	// (or lost its marker); a repo that is no longer a write-tier reference
	// repo drops its items too.
	wanted := map[string]bool{}
	for _, r := range repos {
		wanted[strings.ToLower(strings.TrimSpace(r))] = true
	}
	for key, it := range st.Items {
		repo := strings.ToLower(it.Repo)
		if !wanted[repo] || (listed[repo] && !seen[key]) {
			delete(st.Items, key)
			out.Pruned++
		}
	}
	out.Tracked = len(st.Items)

	// Feedback.
	keys := make([]string, 0, len(st.Items))
	for k := range st.Items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		it := st.Items[key]
		comments, err := p.Fetch.CommentsSince(ctx, it.Repo, it.Number, it.IsPR, it.Cursor)
		if err != nil {
			out.FetchErrs++
			logger.Warn("remote items: feedback fetch failed", "item", key, "error", err.Error())
			continue
		}
		var fresh []Comment
		login := loginFor[strings.ToLower(it.Repo)]
		for _, c := range comments {
			if !c.CreatedAt.After(it.Cursor) || strings.EqualFold(c.Author, login) || strings.HasSuffix(c.Author, "[bot]") || strings.TrimSpace(c.Body) == "" {
				continue
			}
			fresh = append(fresh, c)
		}
		sort.Slice(fresh, func(i, j int) bool { return fresh[i].CreatedAt.Before(fresh[j].CreatedAt) })
		if len(fresh) > maxCommentsPerItemPerPass {
			fresh = fresh[:maxCommentsPerItemPerPass]
		}
		if len(fresh) == 0 {
			// Advance past our own and bot comments so they are not re-read.
			for _, c := range comments {
				if c.CreatedAt.After(it.Cursor) {
					it.Cursor = c.CreatedAt
				}
			}
			st.Items[key] = it
			continue
		}
		it.Cursor = fresh[len(fresh)-1].CreatedAt
		if it.Agent == "" {
			out.Unrouted++
			logger.Info("remote items: feedback on an item whose marker names no configured agent", "item", key, "comments", len(fresh))
			st.Items[key] = it
			continue
		}
		msg := BuildMessage(it, fresh)
		if deliver(resumer, it.Agent, msg) {
			out.Routed++
			logger.Info("remote items: feedback resumed into the agent's session", "item", key, "agent", it.Agent, "comments", len(fresh))
		} else {
			session := ""
			if resumer != nil {
				session, _ = resumer.SessionID(it.Agent)
			}
			st.Pending = appendPending(st.Pending, Pending{Key: key, Agent: it.Agent, Detail: handoffDetail(it, fresh), Session: session, QueuedAt: now})
			out.Queued++
			logger.Info("remote items: feedback queued for the agent's next kick", "item", key, "agent", it.Agent, "comments", len(fresh))
		}
		st.Items[key] = it
	}
	st.Pending = reconcilePending(st.Pending, st.Items, resumer)
	if err := p.Store.save(st); err != nil {
		logger.Warn("remote items: store not saved", "path", p.Store.path, "error", err.Error())
	}
	return out
}

func (p *Poller) resolveAgent(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if p.ResolveAgent == nil {
		return name
	}
	return strings.TrimSpace(p.ResolveAgent(name))
}

// deliver tries the resume kick; false means queue it.
func deliver(r Resumer, agent, msg string) bool {
	if r == nil {
		return false
	}
	session, ok := r.SessionID(agent)
	if !ok {
		return false
	}
	return r.SendResumeKick(agent, msg, session) == nil
}

func appendPending(list []Pending, p Pending) []Pending {
	count := 0
	for _, q := range list {
		if q.Agent == p.Agent {
			count++
		}
	}
	if count >= maxPendingPerAgent {
		// Drop the oldest for this agent.
		for i, q := range list {
			if q.Agent == p.Agent {
				list = append(list[:i], list[i+1:]...)
				break
			}
		}
	}
	return append(list, p)
}

// reconcilePending drops queued handoffs that a fresh kick has since carried
// (the agent is on a different session than when it was queued) and those
// for items no longer tracked.
func reconcilePending(list []Pending, items map[string]Item, r Resumer) []Pending {
	var keep []Pending
	for _, p := range list {
		if _, ok := items[p.Key]; !ok {
			continue
		}
		if r != nil {
			current, _ := r.SessionID(p.Agent)
			if current != p.Session {
				continue
			}
		}
		keep = append(keep, p)
	}
	return keep
}

// kindLabel is the human word for a Comment.Kind.
func kindLabel(kind string) string {
	switch kind {
	case "review_comment":
		return "review comment"
	case "review":
		return "review"
	}
	return "comment"
}

func truncateRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func itemWord(it Item) string {
	if it.IsPR {
		return "PR"
	}
	return "issue"
}

// BuildMessage is the resume kick for new feedback on a remote item: what and
// where, each piece of feedback quoted, and the rule that the reply goes on
// that item (a push to the PR's branch on the owner's fork for a PR).
func BuildMessage(it Item, comments []Comment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REMOTE %s FOLLOW-UP: new activity on %s#%d", strings.ToUpper(itemWord(it)), it.Repo, it.Number)
	if it.URL != "" {
		fmt.Fprintf(&b, " (%s)", it.URL)
	}
	fmt.Fprintf(&b, ", the %s you opened in a REFERENCE repo as the hive owner. Continue from where you left off.\n", itemWord(it))
	for _, c := range comments {
		fmt.Fprintf(&b, "- %s from %s", kindLabel(c.Kind), c.Author)
		if c.URL != "" {
			fmt.Fprintf(&b, " (%s)", c.URL)
		}
		b.WriteString(": ")
		b.WriteString(truncateRunes(c.Body, maxDetailRunes))
		b.WriteString("\n")
	}
	if it.IsPR {
		b.WriteString("Read the feedback, push any fix to the SAME branch on the owner's fork, and reply on the PR. Do not open a new PR; do not label, assign, review or close anything there.")
	} else {
		b.WriteString("Read the feedback and reply on the issue. Do not open a new issue; do not label, assign or close anything there.")
	}
	return b.String()
}

// handoffDetail is the compact one-line form queued for a fresh kick.
func handoffDetail(it Item, comments []Comment) string {
	parts := make([]string, 0, len(comments))
	for _, c := range comments {
		parts = append(parts, fmt.Sprintf("%s from %s: %s", kindLabel(c.Kind), c.Author, truncateRunes(c.Body, maxDetailRunes/3)))
	}
	where := fmt.Sprintf("%s#%d", it.Repo, it.Number)
	if it.URL != "" {
		where += " (" + it.URL + ")"
	}
	return fmt.Sprintf("%s %s: %s", itemWord(it), where, strings.Join(parts, "; "))
}

// HandoffSection renders the block an agent's next kick carries for feedback
// that could not be resumed into its session. Empty when nothing is queued.
// It only reads the store and takes no lock, so the kick builder never waits
// on a pass.
func HandoffSection(path string, names ...string) string {
	s := NewStore(path)
	st, err := s.load()
	if err != nil || len(st.Pending) == 0 {
		return ""
	}
	want := map[string]bool{}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	var lines []string
	for _, p := range st.Pending {
		if want[p.Agent] {
			lines = append(lines, "- "+p.Detail)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("🌐 REMOTE ITEM FOLLOW-UP: people replied on issues or PRs you opened in a REFERENCE repo as the hive owner. Address each before new work, on the item itself (push to the same branch on the owner's fork for a PR); never label, assign, review or close anything there.\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n")
	return b.String()
}
