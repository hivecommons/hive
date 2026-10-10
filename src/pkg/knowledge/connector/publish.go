package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// Publisher is optionally implemented by connectors that can mirror vault
// facts out to their external system. Publish upserts every page under root:
// the page is located by Page.Attrs[PublishKeyAttr] (derived from the
// hive_fact_id marker in Page.ID), so publishing the same pages twice leaves
// the external system unchanged.
type Publisher interface {
	Publish(ctx context.Context, root string, pages []Page) error
}

// Publish page attributes and markers.
const (
	// PublishMarker names the fact id embedded in every published page.
	PublishMarker = "hive_fact_id"
	// PublishKeyAttr is the Page.Attrs key holding the upstream page name.
	PublishKeyAttr = "page_key"
	// DefaultPublishInterval is the publish mirror's sweep cadence.
	DefaultPublishInterval = 24 * time.Hour
	// defaultProposeVia is the footer's fallback when ProposeVia is unset.
	defaultProposeVia = "the Hive knowledge vault"
)

// ErrPublishInProgress is returned when a publish run is already active.
var ErrPublishInProgress = errors.New("connector: publish already in progress")

// PublishConfig is the runtime form of `knowledge.publish`.
type PublishConfig struct {
	Connector    string
	Layers       []string
	Root         string
	IncludeTypes []string
	DryRun       bool
	ProposeVia   string
}

// PublishKey returns the deterministic upstream page name for a fact id.
func PublishKey(factID string) string {
	layer, slug, _ := strings.Cut(factID, "/")
	return Slug("hive", layer, slug)
}

// NewPublisher builds the connector cfg through reg and returns it as a
// Publisher, or an error when its type cannot publish.
func NewPublisher(reg *Registry, cfg ConnectorConfig, deps Deps) (Publisher, error) {
	if reg == nil {
		reg = DefaultRegistry()
	}
	c, err := reg.New(cfg, deps)
	if err != nil {
		return nil, err
	}
	p, ok := c.(Publisher)
	if !ok {
		return nil, fmt.Errorf("connector %s: type %q does not support publishing", cfg.Name, cfg.Type)
	}
	return p, nil
}

// RenderPublishPage renders the markdown for one published fact: the
// hive_fact_id marker, an optional deprecation banner, the fact and the
// "Maintained by Hive" footer.
func RenderPublishPage(factID, title, body string, deprecated bool, banner, proposeVia string) string {
	if strings.TrimSpace(proposeVia) == "" {
		proposeVia = defaultProposeVia
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- %s: %s -->\n\n", PublishMarker, strings.ReplaceAll(factID, "--", "-"))
	if deprecated {
		if banner == "" {
			banner = "this fact is deprecated and no longer reflects current guidance."
		}
		fmt.Fprintf(&b, "> **Deprecated:** %s\n\n", banner)
	}
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "# ") {
		fmt.Fprintf(&b, "# %s\n\n", strings.TrimSpace(title))
	}
	if body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "---\n\n_Maintained by Hive — edits here are overwritten; propose changes via %s._\n", proposeVia)
	return b.String()
}

// PublishReport is the audit record of one publish batch.
type PublishReport struct {
	At        time.Time `json:"at"`
	Connector string    `json:"connector"`
	Root      string    `json:"root"`
	DryRun    bool      `json:"dry_run"`
	// Created, Updated and Deprecated list fact ids sent upstream.
	Created    []string `json:"created,omitempty"`
	Updated    []string `json:"updated,omitempty"`
	Deprecated []string `json:"deprecated,omitempty"`
	Unchanged  int      `json:"unchanged"`
	// Collisions lists facts skipped because another fact owns their page.
	Collisions []string `json:"collisions,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// Changed is the number of pages sent (or, in a dry run, to be sent) upstream.
func (r PublishReport) Changed() int {
	return len(r.Created) + len(r.Updated) + len(r.Deprecated)
}

// PublishStatus is the mirror state surfaced to operators.
type PublishStatus struct {
	Connector   string         `json:"connector"`
	Root        string         `json:"root"`
	DryRun      bool           `json:"dry_run"`
	LastRun     time.Time      `json:"last_run,omitempty"`
	LastSuccess time.Time      `json:"last_success,omitempty"`
	Pages       int            `json:"pages"`
	LastError   string         `json:"last_error,omitempty"`
	Running     bool           `json:"running"`
	LastReport  *PublishReport `json:"last_report,omitempty"`
}

// MirrorOptions configures NewMirror.
type MirrorOptions struct {
	Config    PublishConfig
	Publisher Publisher
	// VaultDir maps a layer name to the vault directory to read facts from.
	VaultDir func(layer string) (string, error)
	// StatePath, when set, persists published page hashes across restarts so
	// unchanged facts are never re-sent.
	StatePath string
	// Interval is the sweep cadence; zero means DefaultPublishInterval.
	Interval time.Duration
	Logger   *slog.Logger
	Now      func() time.Time
	// Audit, when set, receives one report per publish batch (dry runs too).
	Audit func(PublishReport)
}

type publishedPage struct {
	Key        string `json:"key"`
	Hash       string `json:"hash"`
	Title      string `json:"title"`
	Deprecated bool   `json:"deprecated,omitempty"`
}

type publishState struct {
	Pages  map[string]publishedPage `json:"pages"`
	Status PublishStatus            `json:"status"`
}

// Mirror publishes curator-promoted vault facts one way into an external
// system. A fact qualifies when its `source` front-matter starts with
// knowledge.PromotedSourcePrefix, it lives in a configured layer (never
// personal) and its type is in IncludeTypes (when set).
type Mirror struct {
	opts    MirrorOptions
	logger  *slog.Logger
	trigger chan struct{}
	pageKey func(factID string) string

	runMu sync.Mutex
	mu    sync.Mutex
	state publishState
}

// NewMirror validates opts and loads persisted publish state.
func NewMirror(opts MirrorOptions) (*Mirror, error) {
	if opts.Publisher == nil {
		return nil, fmt.Errorf("connector: MirrorOptions.Publisher is required")
	}
	if opts.VaultDir == nil {
		return nil, fmt.Errorf("connector: MirrorOptions.VaultDir is required")
	}
	if len(opts.Config.Layers) == 0 {
		return nil, fmt.Errorf("connector: publish layers are required")
	}
	for _, l := range opts.Config.Layers {
		if l == string(knowledge.LayerPersonal) || !validLayers[l] {
			return nil, fmt.Errorf("connector: publish layer %q must be project, org or community", l)
		}
	}
	if strings.Trim(opts.Config.Root, "/ ") == "" {
		return nil, fmt.Errorf("connector: publish root is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultPublishInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	m := &Mirror{
		opts:    opts,
		logger:  logger,
		trigger: make(chan struct{}, 1),
		pageKey: PublishKey,
		state:   publishState{Pages: map[string]publishedPage{}},
	}
	m.loadState()
	st := &m.state.Status
	st.Connector, st.Root, st.DryRun, st.Running = opts.Config.Connector, opts.Config.Root, opts.Config.DryRun, false
	st.Pages = len(m.state.Pages)
	return m, nil
}

// Status returns a snapshot of the mirror state.
func (m *Mirror) Status() PublishStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state.Status
	if st.LastReport != nil {
		r := *st.LastReport
		st.LastReport = &r
	}
	return st
}

// Trigger requests a publish run from the Start loop (for example after a
// curator promotion). It never blocks; requests coalesce.
func (m *Mirror) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

// Start publishes once, then on every Interval tick and Trigger, until ctx
// is cancelled.
func (m *Mirror) Start(ctx context.Context) {
	t := time.NewTicker(m.opts.Interval)
	defer t.Stop()
	for {
		if _, err := m.Run(ctx); err != nil && ctx.Err() == nil {
			m.logger.Warn("knowledge publish failed", "connector", m.opts.Config.Connector, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.trigger:
		}
	}
}

type publishFact struct {
	id, title, body string
	deprecated      bool
}

// Run performs one publish batch: it renders every qualifying fact, skips
// pages whose content hash is unchanged, publishes the rest (unless DryRun)
// and records the outcome.
func (m *Mirror) Run(ctx context.Context) (PublishReport, error) {
	if !m.runMu.TryLock() {
		return PublishReport{}, ErrPublishInProgress
	}
	defer m.runMu.Unlock()
	cfg := m.opts.Config
	report := PublishReport{At: m.opts.Now().UTC(), Connector: cfg.Connector, Root: cfg.Root, DryRun: cfg.DryRun}
	m.mu.Lock()
	m.state.Status.Running = true
	prev := make(map[string]publishedPage, len(m.state.Pages))
	for k, v := range m.state.Pages {
		prev[k] = v
	}
	m.mu.Unlock()

	pages, next, err := m.plan(prev, &report)
	if err == nil && len(pages) > 0 && !cfg.DryRun {
		err = m.opts.Publisher.Publish(ctx, cfg.Root, pages)
	}
	if err != nil {
		report.Error = err.Error()
	}
	m.finish(report, next, err == nil && !cfg.DryRun)
	if m.opts.Audit != nil {
		m.opts.Audit(report)
	}
	m.logger.Info("knowledge publish batch",
		"connector", cfg.Connector, "root", cfg.Root, "dry_run", cfg.DryRun,
		"created", len(report.Created), "updated", len(report.Updated), "deprecated", len(report.Deprecated),
		"unchanged", report.Unchanged, "collisions", len(report.Collisions), "error", report.Error)
	return report, err
}

// plan renders every qualifying fact (plus tombstones for previously
// published facts that left the vault) and returns the pages to send and the
// state to keep once they are sent.
func (m *Mirror) plan(prev map[string]publishedPage, report *PublishReport) ([]Page, map[string]publishedPage, error) {
	facts, err := m.collect()
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, f := range facts {
		seen[f.id] = true
	}
	for id, p := range prev {
		if !seen[id] {
			facts = append(facts, publishFact{id: id, title: p.Title, deprecated: true, body: "This fact was removed from the Hive knowledge vault."})
		}
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].id < facts[j].id })

	owner := map[string]string{}
	for id, p := range prev {
		owner[p.Key] = id
	}
	next := map[string]publishedPage{}
	var pages []Page
	for _, f := range facts {
		key := m.pageKey(f.id)
		if o, taken := owner[key]; taken && o != f.id {
			report.Collisions = append(report.Collisions, fmt.Sprintf("%s (page %s owned by %s)", f.id, key, o))
			continue
		}
		owner[key] = f.id
		banner := ""
		if !seen[f.id] {
			banner = "this fact was removed from the Hive knowledge vault and is kept here for reference only."
		}
		md := RenderPublishPage(f.id, f.title, f.body, f.deprecated, banner, m.opts.Config.ProposeVia)
		sum := sha256.Sum256([]byte(f.title + "\x00" + md))
		rec := publishedPage{Key: key, Hash: hex.EncodeToString(sum[:]), Title: f.title, Deprecated: f.deprecated}
		next[f.id] = rec
		old, had := prev[f.id]
		switch {
		case had && old.Hash == rec.Hash:
			report.Unchanged++
			continue
		case !had:
			report.Created = append(report.Created, f.id)
		case f.deprecated && !old.Deprecated:
			report.Deprecated = append(report.Deprecated, f.id)
		default:
			report.Updated = append(report.Updated, f.id)
		}
		layer, _, _ := strings.Cut(f.id, "/")
		pages = append(pages, Page{
			ID: f.id, Title: f.title, Markdown: md, Archived: f.deprecated, Path: []string{layer},
			Attrs: map[string]string{PublishKeyAttr: key, PublishMarker: f.id},
		})
	}
	return pages, next, nil
}

func (m *Mirror) finish(report PublishReport, next map[string]publishedPage, commit bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := &m.state.Status
	st.Running = false
	st.LastRun = report.At
	st.LastError = report.Error
	r := report
	st.LastReport = &r
	if report.Error == "" {
		st.LastSuccess = report.At
	}
	if commit {
		m.state.Pages = next
	}
	st.Pages = len(m.state.Pages)
	m.saveState()
}

// collect reads every qualifying fact from the configured layers' vaults.
func (m *Mirror) collect() ([]publishFact, error) {
	include := map[string]bool{}
	for _, t := range m.opts.Config.IncludeTypes {
		include[strings.TrimSpace(t)] = true
	}
	scanned := map[string]bool{}
	var out []publishFact
	for _, layer := range m.opts.Config.Layers {
		dir, err := m.opts.VaultDir(layer)
		if err != nil {
			return nil, fmt.Errorf("vault for layer %s: %w", layer, err)
		}
		if dir == "" || scanned[filepath.Clean(dir)] {
			continue
		}
		scanned[filepath.Clean(dir)] = true
		err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == dir && errors.Is(err, fs.ErrNotExist) {
					return fs.SkipAll
				}
				return err
			}
			if d.IsDir() {
				if path != dir && strings.HasPrefix(d.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, path)
			if f, ok := promotedFact(layer, filepath.ToSlash(strings.TrimSuffix(rel, ".md")), string(data), include); ok {
				out = append(out, f)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("reading %s vault: %w", layer, err)
		}
	}
	return out, nil
}

// promotedFact parses content and reports whether it is a publishable,
// curator-promoted fact.
func promotedFact(layer, slug, content string, include map[string]bool) (publishFact, bool) {
	fm := frontMatter(content)
	if !strings.HasPrefix(fm["source"], knowledge.PromotedSourcePrefix) {
		return publishFact{}, false
	}
	if fm["layer"] == string(knowledge.LayerPersonal) {
		return publishFact{}, false
	}
	if len(include) > 0 && !include[fm["type"]] {
		return publishFact{}, false
	}
	body := content
	if end := strings.Index(content[4:], "\n---"); end >= 0 {
		body = content[4+end+4:]
	}
	title := fm["title"]
	if title == "" {
		title = slug
	}
	state := strings.ToLower(fm["state"])
	return publishFact{
		id:         layer + "/" + slug,
		title:      title,
		body:       strings.TrimSpace(body),
		deprecated: fm["status"] == StatusDeprecated || state == "deprecated" || state == "superseded",
	}, true
}

func (m *Mirror) loadState() {
	if m.opts.StatePath == "" {
		return
	}
	data, err := os.ReadFile(m.opts.StatePath)
	if err != nil {
		if !os.IsNotExist(err) {
			m.logger.Warn("knowledge publish: reading state", "path", m.opts.StatePath, "error", err)
		}
		return
	}
	var st publishState
	if err := json.Unmarshal(data, &st); err != nil {
		m.logger.Warn("knowledge publish: ignoring corrupt state", "path", m.opts.StatePath, "error", err)
		return
	}
	if st.Pages == nil {
		st.Pages = map[string]publishedPage{}
	}
	m.state = st
}

// saveState persists state; the caller holds m.mu.
func (m *Mirror) saveState() {
	if m.opts.StatePath == "" {
		return
	}
	data, _ := json.MarshalIndent(m.state, "", "  ") // plain structs cannot fail to marshal
	if err := writeAtomic(m.opts.StatePath, string(data)); err != nil {
		m.logger.Warn("knowledge publish: saving state", "path", m.opts.StatePath, "error", err)
	}
}
