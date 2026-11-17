// Package repoagents discovers agents a managed repo declares under
// .hive/agents/*.yaml and holds them for an owner's approval
// (hivecommons/hive#18491).
//
// A discovered definition never runs on its own. The scan lists the directory
// in every managed repo that passes the repo-sourced prompt allowlist (the same
// seed-only gate definition_source and prompt_source use), parses each file as
// a portable AgentDefinition, and records it as pending. The dashboard shows
// pending entries; an owner's approval creates the agent with definition_source
// (and prompt_source, when a sibling <name>.md exists) pointing at the file, so
// later edits re-apply through pkg/defsrc. Backend, model, mode and tools stay
// owner-controlled: see defsrc.KeepOwnerControlledFields.
//
// When an approved file disappears, the entry is marked orphaned and the agent
// is paused through OnOrphan, never deleted. A repo that drops off the
// allowlist is not scanned at all, so its entries are left exactly as they were.
package repoagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/defsrc"
)

// AgentsDir is the repo directory scanned for agent definitions.
const AgentsDir = ".hive/agents"

const (
	// DefaultInterval is how often every managed repo is rescanned when no
	// interval is configured. Push webhooks rescan a repo sooner.
	DefaultInterval = 15 * time.Minute
	// MinInterval is the shortest accepted rescan interval, so a typo cannot
	// turn the scan into a GitHub API hammer.
	MinInterval = time.Minute
	// maxFilesPerRepo bounds how many definition files one repo can enqueue.
	maxFilesPerRepo = 50
	// scanTimeout bounds one repo's scan.
	scanTimeout = 2 * time.Minute
	// triggerBuffer is how many webhook-requested rescans may queue; more
	// are dropped because the next timer pass covers them.
	triggerBuffer = 16
	// pushCommitsCap is the most commits GitHub lists in a push payload; a
	// push at the cap may hide changed paths, so it always rescans.
	pushCommitsCap = 20
	// stateFileMode keeps the state file owner-readable only.
	stateFileMode = 0o600
	stateDirMode  = 0o700
)

// Status is where a discovered definition stands.
type Status string

const (
	// StatusPending waits for an owner's approval.
	StatusPending Status = "pending"
	// StatusInvalid is a file that does not parse as an AgentDefinition. It
	// cannot be approved until it is fixed.
	StatusInvalid Status = "invalid"
	// StatusApproved has a registered agent.
	StatusApproved Status = "approved"
	// StatusRejected was declined by an owner; it stays declined until the
	// file is removed.
	StatusRejected Status = "rejected"
	// StatusOrphaned is an approved agent whose file was removed. The agent
	// is paused, not deleted.
	StatusOrphaned Status = "orphaned"
)

// Entry is one discovered definition file.
type Entry struct {
	ID          string     `json:"id"`
	Repo        string     `json:"repo"`
	Path        string     `json:"path"`
	PromptPath  string     `json:"prompt_path,omitempty"`
	Name        string     `json:"name"`
	DisplayName string     `json:"display_name,omitempty"`
	Description string     `json:"description,omitempty"`
	Emoji       string     `json:"emoji,omitempty"`
	Status      Status     `json:"status"`
	Agent       string     `json:"agent,omitempty"`
	Error       string     `json:"error,omitempty"`
	FirstSeen   time.Time  `json:"first_seen"`
	LastSeen    time.Time  `json:"last_seen"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	DecidedBy   string     `json:"decided_by,omitempty"`
	OrphanedAt  *time.Time `json:"orphaned_at,omitempty"`
}

// Owner is the owner half of Entry.Repo.
func (e Entry) Owner() string {
	o, _, _ := strings.Cut(e.Repo, "/")
	return o
}

// RepoName is the repo half of Entry.Repo.
func (e Entry) RepoName() string {
	_, r, _ := strings.Cut(e.Repo, "/")
	return r
}

// Client reads a repo's directory listing and file contents. It is satisfied
// by *github.Client, which reads through the hive's GitHub App token.
type Client interface {
	ListDirectoryFiles(ctx context.Context, owner, repo, path, ref string) ([]string, error)
	defsrc.Fetcher
}

// Options configures a Discoverer.
type Options struct {
	// Client may be nil (no GitHub client yet); every scan is then skipped.
	Client Client
	// Allow is the seed-only repo allowlist check; nil denies every repo.
	Allow defsrc.AllowFunc
	// Repos returns the managed repos as "owner/repo" slugs.
	Repos func() []string
	// StatePath is where entries persist; empty keeps them in memory only.
	StatePath string
	// OnOrphan is called, outside any lock, for each approved entry whose
	// file was removed. It pauses the agent.
	OnOrphan func(Entry)
	Logger   *slog.Logger
	Now      func() time.Time
}

// Discoverer scans managed repos for agent definitions. Safe for concurrent use.
type Discoverer struct {
	opts Options
	// saveMu serializes state writes so the newest snapshot always lands last.
	saveMu  sync.Mutex
	mu      sync.Mutex
	entries map[string]*Entry
	trigger chan string
}

// ErrNotFound is returned for an unknown entry id.
var ErrNotFound = errors.New("discovered agent not found")

// New builds a Discoverer and loads any persisted entries.
func New(opts Options) *Discoverer {
	if opts.Allow == nil {
		opts.Allow = func(string) bool { return false }
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	d := &Discoverer{opts: opts, entries: map[string]*Entry{}, trigger: make(chan string, triggerBuffer)}
	d.load()
	return d
}

// EntryID is the stable id of a definition file.
func EntryID(slug, filePath string) string { return slug + ":" + filePath }

// RepoSlug qualifies a project.repos entry with org when it has no owner.
func RepoSlug(org, repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.Contains(repo, "/") || org == "" {
		return repo
	}
	return org + "/" + repo
}

// AgentName is the agent key a definition name maps to, the same
// normalization the dashboard import uses.
func AgentName(defName string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(defName), " ", "-"))
}

// List returns every entry, sorted by repo then path.
func (d *Discoverer) List() []Entry {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Entry, 0, len(d.entries))
	for _, e := range d.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Get returns one entry.
func (d *Discoverer) Get(id string) (Entry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[id]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// FetchDefinition re-reads and parses an entry's file through the allowlist
// gate, so approval registers what the repo holds now, not a stale copy.
func (d *Discoverer) FetchDefinition(ctx context.Context, id string) (*defsrc.AgentDefinition, Entry, error) {
	e, ok := d.Get(id)
	if !ok {
		return nil, Entry{}, ErrNotFound
	}
	var fetcher defsrc.Fetcher
	if d.opts.Client != nil {
		fetcher = d.opts.Client
	}
	def, err := defsrc.FetchOnce(ctx, fetcher, d.opts.Allow, defsrc.Source{Owner: e.Owner(), Repo: e.RepoName(), Path: e.Path})
	return def, e, err
}

// Approve records that an owner registered the entry as agent.
func (d *Discoverer) Approve(id, agent, by string) (Entry, error) {
	return d.decide(id, by, func(e *Entry) error {
		if e.Status != StatusPending {
			return fmt.Errorf("only a pending definition can be approved (this one is %s)", e.Status)
		}
		e.Status = StatusApproved
		e.Agent = agent
		return nil
	})
}

// Reject records that an owner declined the entry.
func (d *Discoverer) Reject(id, by string) (Entry, error) {
	return d.decide(id, by, func(e *Entry) error {
		if e.Status != StatusPending && e.Status != StatusInvalid {
			return fmt.Errorf("only a pending definition can be rejected (this one is %s)", e.Status)
		}
		e.Status = StatusRejected
		return nil
	})
}

func (d *Discoverer) decide(id, by string, apply func(*Entry) error) (Entry, error) {
	d.mu.Lock()
	e, ok := d.entries[id]
	if !ok {
		d.mu.Unlock()
		return Entry{}, ErrNotFound
	}
	if err := apply(e); err != nil {
		d.mu.Unlock()
		return Entry{}, err
	}
	now := d.opts.Now()
	e.DecidedAt = &now
	e.DecidedBy = by
	out := *e
	d.mu.Unlock()
	d.save()
	return out, nil
}

// Trigger asks Run to rescan slug soon. It never blocks; a full queue drops
// the request because the next timer pass rescans every repo anyway.
func (d *Discoverer) Trigger(slug string) bool {
	if slug == "" {
		return false
	}
	select {
	case d.trigger <- slug:
		return true
	default:
		return false
	}
}

// Run scans every repo now and every interval after that, plus any repo a
// Trigger names, until ctx is done.
func (d *Discoverer) Run(ctx context.Context, interval time.Duration) {
	if interval < MinInterval {
		interval = MinInterval
	}
	d.ScanAll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.ScanAll(ctx)
		case slug := <-d.trigger:
			if err := d.ScanRepo(ctx, slug); err != nil {
				d.opts.Logger.Warn("repo agent discovery: rescan failed", "repo", slug, "error", err)
			}
		}
	}
}

// ScanAll scans every managed repo. A repo that fails is logged and skipped.
func (d *Discoverer) ScanAll(ctx context.Context) {
	if d.opts.Repos == nil {
		return
	}
	for _, slug := range d.opts.Repos() {
		if ctx.Err() != nil {
			return
		}
		if err := d.ScanRepo(ctx, slug); err != nil {
			d.opts.Logger.Warn("repo agent discovery: scan failed", "repo", slug, "error", err)
		}
	}
}

// ScanRepo lists slug's .hive/agents/ and reconciles its entries. A repo that
// is not allowlisted, or a scan without a client, is skipped without touching
// entries. A listing failure returns an error and also leaves entries alone,
// so a transient GitHub error never orphans an agent.
func (d *Discoverer) ScanRepo(ctx context.Context, slug string) error {
	owner, repo, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || repo == "" {
		return fmt.Errorf("repo %q is not owner/repo", slug)
	}
	if !d.opts.Allow(slug) || d.opts.Client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	names, err := d.opts.Client.ListDirectoryFiles(ctx, owner, repo, AgentsDir, "")
	if err != nil {
		return err
	}
	prompts := map[string]bool{}
	var defs []string
	for _, n := range names {
		switch strings.ToLower(path.Ext(n)) {
		case ".yaml", ".yml":
			defs = append(defs, n)
		case ".md":
			prompts[n] = true
		}
	}
	sort.Strings(defs)
	if len(defs) > maxFilesPerRepo {
		d.opts.Logger.Warn("repo agent discovery: too many definitions, ignoring the rest", "repo", slug, "found", len(defs), "max", maxFilesPerRepo)
		defs = defs[:maxFilesPerRepo]
	}

	type scanned struct {
		path, prompt string
		def          *defsrc.AgentDefinition
		parseErr     error
		fetchErr     error
	}
	results := make([]scanned, 0, len(defs))
	for _, n := range defs {
		res := scanned{path: AgentsDir + "/" + n}
		if p := strings.TrimSuffix(n, path.Ext(n)) + ".md"; prompts[p] {
			res.prompt = AgentsDir + "/" + p
		}
		raw, ferr := d.opts.Client.GetFileContentRef(ctx, owner, repo, res.path, "")
		if ferr != nil {
			res.fetchErr = ferr
		} else {
			res.def, res.parseErr = defsrc.ParseDefinition(raw)
		}
		results = append(results, res)
	}

	now := d.opts.Now()
	var orphaned []Entry
	d.mu.Lock()
	seen := map[string]bool{}
	for _, res := range results {
		id := EntryID(slug, res.path)
		seen[id] = true
		e, exists := d.entries[id]
		if !exists {
			e = &Entry{ID: id, Repo: slug, Path: res.path, Status: StatusPending, FirstSeen: now}
			d.entries[id] = e
		}
		e.LastSeen = now
		e.PromptPath = res.prompt
		if res.fetchErr != nil {
			e.Error = "could not read file: " + res.fetchErr.Error()
			if !exists {
				e.Status = StatusInvalid
			}
			continue
		}
		applyDefinition(e, res.def, res.parseErr)
		if e.Status == StatusOrphaned {
			// The file came back. The agent stays paused until an owner
			// resumes it; the entry tracks the file again.
			e.Status = StatusApproved
			e.OrphanedAt = nil
		}
	}
	for id, e := range d.entries {
		if e.Repo != slug || seen[id] {
			continue
		}
		switch e.Status {
		case StatusApproved:
			e.Status = StatusOrphaned
			at := now
			e.OrphanedAt = &at
			orphaned = append(orphaned, *e)
		case StatusOrphaned:
			// Already paused.
		default:
			delete(d.entries, id)
		}
	}
	d.mu.Unlock()
	d.save()

	for _, e := range orphaned {
		d.opts.Logger.Info("repo agent discovery: definition removed, pausing agent", "repo", e.Repo, "path", e.Path, "agent", e.Agent)
		if d.opts.OnOrphan != nil {
			d.opts.OnOrphan(e)
		}
	}
	return nil
}

// applyDefinition copies a parsed definition's presentation onto e and moves
// it between pending and invalid. Decided entries keep their status.
func applyDefinition(e *Entry, def *defsrc.AgentDefinition, parseErr error) {
	if parseErr != nil {
		e.Error = parseErr.Error()
		if e.Status == StatusPending {
			e.Status = StatusInvalid
		}
		return
	}
	e.Error = ""
	e.Name = AgentName(def.Metadata.Name)
	e.DisplayName = def.Metadata.DisplayName
	e.Description = def.Metadata.Description
	e.Emoji = def.Metadata.Emoji
	if e.Status == StatusInvalid {
		e.Status = StatusPending
	}
}

// PushRescanRepo reports the repo a push delivery should rescan: a push to the
// repo's default branch that touched .hive/agents/ (or listed too many
// commits to tell). Any other push returns "".
func PushRescanRepo(payload []byte) string {
	var p struct {
		Ref        string `json:"ref"`
		Repository struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
		Commits []struct {
			Added    []string `json:"added"`
			Removed  []string `json:"removed"`
			Modified []string `json:"modified"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	slug := p.Repository.FullName
	if slug == "" || p.Repository.DefaultBranch == "" || p.Ref != "refs/heads/"+p.Repository.DefaultBranch {
		return ""
	}
	if len(p.Commits) == 0 || len(p.Commits) >= pushCommitsCap {
		return slug
	}
	prefix := AgentsDir + "/"
	for _, c := range p.Commits {
		for _, list := range [][]string{c.Added, c.Removed, c.Modified} {
			for _, f := range list {
				if strings.HasPrefix(f, prefix) {
					return slug
				}
			}
		}
	}
	return ""
}

type stateFile struct {
	Entries []Entry `json:"entries"`
}

func (d *Discoverer) snapshot() []Entry {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Entry, 0, len(d.entries))
	for _, e := range d.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (d *Discoverer) load() {
	if d.opts.StatePath == "" {
		return
	}
	data, err := os.ReadFile(d.opts.StatePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			d.opts.Logger.Warn("repo agent discovery: state unreadable, starting empty", "path", d.opts.StatePath, "error", err)
		}
		return
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil {
		d.opts.Logger.Warn("repo agent discovery: state corrupt, starting empty", "path", d.opts.StatePath, "error", err)
		return
	}
	for i := range st.Entries {
		e := st.Entries[i]
		if e.ID == "" {
			continue
		}
		d.entries[e.ID] = &e
	}
}

func (d *Discoverer) save() {
	if d.opts.StatePath == "" {
		return
	}
	d.saveMu.Lock()
	defer d.saveMu.Unlock()
	if err := writeState(d.opts.StatePath, d.snapshot()); err != nil {
		d.opts.Logger.Warn("repo agent discovery: state not saved", "path", d.opts.StatePath, "error", err)
	}
}

func writeState(statePath string, entries []Entry) error {
	data, err := json.MarshalIndent(stateFile{Entries: entries}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(statePath), stateDirMode); err != nil {
		return err
	}
	tmp := statePath + ".tmp"
	if err := os.WriteFile(tmp, data, stateFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, statePath)
}
