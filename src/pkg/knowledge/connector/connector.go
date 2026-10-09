// Package connector is the single seam through which external knowledge
// systems (git repositories, standalone documents, and later Notion,
// Confluence, SharePoint, Google Drive and GitHub Wiki) feed the hive's file
// vault. A Connector lists and fetches upstream pages; the Syncer runs each
// configured connector on its interval and the FactWriter turns the emitted
// pages into markdown facts with connector front-matter.
package connector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Page is one upstream document as emitted by a connector.
type Page struct {
	// ID is the stable upstream identifier. It must not change across syncs:
	// the fact slug is derived from it.
	ID       string
	Title    string
	URL      string
	Markdown string
	// UpdatedAt is the upstream last-modified time, zero when unknown.
	UpdatedAt time.Time
	// Archived marks a page that is archived/deprecated upstream; its fact is
	// written with `status: deprecated` so stale context stops reaching agents.
	Archived bool
	// Path is the page's location in the upstream hierarchy (space, parent
	// pages, folder), outermost first.
	Path  []string
	Attrs map[string]string
}

// Cursor is an opaque, connector-defined incremental sync position
// (updated-since timestamp, delta token, ETag, commit SHA...). The empty
// cursor requests a full listing.
type Cursor string

// Connector is implemented by every external knowledge system.
type Connector interface {
	// Type returns the connector type name used in `knowledge.connectors[].type`.
	Type() string
	// Validate checks the type-specific parts of cfg (scope keys, auth).
	Validate(cfg ConnectorConfig) error
	// Sync emits every page changed since cur and returns the new cursor.
	// When cur is empty, or the connector implements FullLister and reports
	// true, the emitted pages are a full listing: previously synced pages
	// that are not emitted are marked deprecated.
	Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error)
}

// FullLister is optionally implemented by connectors whose every Sync emits
// the complete page set (for example a git checkout or a single document).
type FullLister interface {
	FullListing() bool
}

// Auth names where a connector's credential lives. Inline secrets are never
// accepted; at most one of Env or File may be set.
type Auth struct {
	Env  string
	File string
}

// ErrNoCredential is returned by Auth.Secret when no credential is configured.
var ErrNoCredential = errors.New("connector: no credential configured (set auth.env or auth.file)")

// Configured reports whether a credential source is set.
func (a Auth) Configured() bool { return a.Env != "" || a.File != "" }

// Secret resolves the credential from the environment variable or file.
func (a Auth) Secret() (string, error) {
	switch {
	case a.Env != "" && a.File != "":
		return "", fmt.Errorf("connector: auth.env and auth.file are mutually exclusive")
	case a.Env != "":
		v := strings.TrimSpace(os.Getenv(a.Env))
		if v == "" {
			return "", fmt.Errorf("connector: auth.env %s is empty or unset", a.Env)
		}
		return v, nil
	case a.File != "":
		data, err := os.ReadFile(a.File)
		if err != nil {
			return "", fmt.Errorf("connector: reading auth.file: %w", err)
		}
		v := strings.TrimSpace(string(data))
		if v == "" {
			return "", fmt.Errorf("connector: auth.file %s is empty", a.File)
		}
		return v, nil
	default:
		return "", ErrNoCredential
	}
}

// DefaultInterval is used when a connector config leaves interval unset.
const DefaultInterval = 15 * time.Minute

// ConnectorConfig is the runtime form of one `knowledge.connectors` entry.
type ConnectorConfig struct {
	Name     string
	Type     string
	Enabled  bool
	Interval time.Duration
	Layer    string
	Scope    map[string]string
	Auth     Auth
}

// ScopeValue returns the trimmed scope entry for key.
func (c ConnectorConfig) ScopeValue(key string) string {
	return strings.TrimSpace(c.Scope[key])
}

// EffectiveInterval returns Interval, or DefaultInterval when unset.
func (c ConnectorConfig) EffectiveInterval() time.Duration {
	if c.Interval <= 0 {
		return DefaultInterval
	}
	return c.Interval
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// validLayers mirrors knowledge.LayerType.
var validLayers = map[string]bool{"personal": true, "project": true, "org": true, "community": true}

// validateCommon checks the fields every connector shares.
func validateCommon(cfg ConnectorConfig) error {
	if !nameRe.MatchString(cfg.Name) {
		return fmt.Errorf("connector name %q: must match %s", cfg.Name, nameRe.String())
	}
	if !nameRe.MatchString(cfg.Type) {
		return fmt.Errorf("connector %s: type %q: must match %s", cfg.Name, cfg.Type, nameRe.String())
	}
	if !validLayers[cfg.Layer] {
		return fmt.Errorf("connector %s: layer %q must be personal, project, org or community", cfg.Name, cfg.Layer)
	}
	if cfg.Auth.Env != "" && cfg.Auth.File != "" {
		return fmt.Errorf("connector %s: auth.env and auth.file are mutually exclusive", cfg.Name)
	}
	return nil
}

// Deps carries the runtime dependencies handed to connector factories.
type Deps struct {
	// KnowledgeDir is the knowledge data root (/data/knowledge in production).
	KnowledgeDir string
	// StateDir is a private, per-connector working directory (clones, caches).
	StateDir string
	Logger   *slog.Logger
	// HTTP is the shared SSRF-hardened client; nil means NewHTTPClient(HTTPOptions{}).
	HTTP *HTTPClient
}

func (d Deps) logger() *slog.Logger {
	if d.Logger == nil {
		return slog.Default()
	}
	return d.Logger
}

func (d Deps) httpClient() *HTTPClient {
	if d.HTTP == nil {
		return NewHTTPClient(HTTPOptions{})
	}
	return d.HTTP
}

// Factory builds a connector for cfg.
type Factory func(cfg ConnectorConfig, deps Deps) (Connector, error)

// Registry maps connector type names to factories.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

// DefaultRegistry returns a registry with the built-in connector types
// (`git` and `document`) registered.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	_ = r.Register(TypeGit, newGitConnector)
	_ = r.Register(TypeDocument, newDocumentConnector)
	_ = r.Register(TypeGitHubWiki, newGitHubWikiConnector)
	_ = r.Register(TypeGoogleDrive, newGoogleDriveConnector)
	_ = r.Register(TypeSharePoint, newSharePointConnector)
	return r
}

// Register adds a factory for typ. Registering a type twice is an error.
func (r *Registry) Register(typ string, f Factory) error {
	if !nameRe.MatchString(typ) {
		return fmt.Errorf("connector: invalid type name %q", typ)
	}
	if f == nil {
		return fmt.Errorf("connector: nil factory for type %q", typ)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[typ]; dup {
		return fmt.Errorf("connector: type %q already registered", typ)
	}
	r.factories[typ] = f
	return nil
}

// Types lists registered type names, sorted.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for t := range r.factories {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// New validates cfg and builds its connector.
func (r *Registry) New(cfg ConnectorConfig, deps Deps) (Connector, error) {
	if err := validateCommon(cfg); err != nil {
		return nil, err
	}
	r.mu.RLock()
	f, ok := r.factories[cfg.Type]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("connector %s: unknown type %q (known: %s)", cfg.Name, cfg.Type, strings.Join(r.Types(), ", "))
	}
	c, err := f(cfg, deps)
	if err != nil {
		return nil, fmt.Errorf("connector %s: %w", cfg.Name, err)
	}
	if c.Type() != cfg.Type {
		return nil, fmt.Errorf("connector %s: factory for %q returned type %q", cfg.Name, cfg.Type, c.Type())
	}
	if err := c.Validate(cfg); err != nil {
		return nil, fmt.Errorf("connector %s: %w", cfg.Name, err)
	}
	return c, nil
}

// isFullListing reports whether a sync from cur on c returns every page.
func isFullListing(c Connector, cur Cursor) bool {
	if cur == "" {
		return true
	}
	fl, ok := c.(FullLister)
	return ok && fl.FullListing()
}

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// httpStatusRetryable reports whether status is a rate-limit/transient code.
func httpStatusRetryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable ||
		status == http.StatusBadGateway || status == http.StatusGatewayTimeout
}
