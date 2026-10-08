package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// fetchDocumentFacts is the existing knowledge.documents pipeline; a var so
// tests can stub it.
var fetchDocumentFacts = knowledge.FetchDocumentFacts

// documentConnector exposes a knowledge.documents-style source as a
// connector. Scope keys: exactly one of url, file_path or context7_id. For
// context7_id, auth (env|file) optionally supplies the Context7 API key.
type documentConnector struct {
	cfg          ConnectorConfig
	knowledgeDir string
	logger       *slog.Logger
	http         *HTTPClient
}

func newDocumentConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	return &documentConnector{
		cfg:          cfg,
		knowledgeDir: deps.KnowledgeDir,
		logger:       deps.logger(),
		http:         deps.httpClient(),
	}, nil
}

func (d *documentConnector) Type() string { return TypeDocument }

// FullListing: every sync re-reads the whole document.
func (d *documentConnector) FullListing() bool { return true }

func docSourceConfig(cfg ConnectorConfig) knowledge.DocSourceConfig {
	return knowledge.DocSourceConfig{
		Name:       cfg.Name,
		URL:        cfg.ScopeValue("url"),
		FilePath:   cfg.ScopeValue("file_path"),
		Context7ID: cfg.ScopeValue("context7_id"),
		Layer:      knowledge.LayerType(cfg.Layer),
	}
}

func (d *documentConnector) Validate(cfg ConnectorConfig) error {
	src := docSourceConfig(cfg)
	set := 0
	for _, v := range []string{src.URL, src.FilePath, src.Context7ID} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("scope must set exactly one of url, file_path or context7_id")
	}
	if src.URL != "" {
		lower := strings.ToLower(src.URL)
		if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
			return fmt.Errorf("scope.url must be http(s)")
		}
	}
	return nil
}

// Sync fetches and parses the document through the same code path as
// knowledge.documents and emits one page per extracted fact. The page ids
// (000, 001, ...) mirror the legacy doc-<slug>-NNN numbering. The cursor is a
// content hash.
func (d *documentConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	src := docSourceConfig(d.cfg)
	if src.URL != "" {
		// The legacy path relies on the dashboard handler's pre-fetch SSRF
		// check; a connector has no handler in front of it, so check here.
		if err := d.http.ValidateURL(ctx, src.URL); err != nil {
			return cur, fmt.Errorf("scope.url: %w", err)
		}
	}
	var key string
	if d.cfg.Auth.Configured() {
		secret, err := d.cfg.Auth.Secret()
		if err != nil {
			return cur, err
		}
		key = secret
	}
	facts, _, err := fetchDocumentFacts(ctx, src, d.knowledgeDir, key, d.logger)
	if err != nil {
		return cur, err
	}
	sourceURL := src.URL
	if sourceURL == "" && src.Context7ID != "" {
		sourceURL = "context7://" + src.Context7ID
	}
	h := sha256.New()
	for i, f := range facts {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", f.Title, f.Body)
		p := Page{
			ID:        fmt.Sprintf("%03d", i),
			Title:     f.Title,
			URL:       sourceURL,
			Markdown:  f.Body,
			UpdatedAt: f.SourceDate,
		}
		if err := emit(p); err != nil {
			return cur, err
		}
	}
	return Cursor(hex.EncodeToString(h.Sum(nil))[:16]), nil
}
