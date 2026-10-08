package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// This file exposes the small set of existing ingestion primitives that
// pkg/knowledge/connector reuses, so connectors share one SSRF policy, one
// front-matter sanitizer, one HTML converter and one document pipeline with
// knowledge.git_sources and knowledge.documents instead of growing copies.

// CheckRedirectNoPrivate is the http.Client CheckRedirect policy used for
// document fetches: at most 3 redirects, http(s) only, and never to a host
// that resolves to a private/internal address.
func CheckRedirectNoPrivate(req *http.Request, via []*http.Request) error {
	return knowledgeNoRedirectToPrivate(req, via)
}

// HostIsPrivate reports whether host is (or resolves to) a loopback, private,
// link-local or unspecified address. It fails closed on DNS errors.
func HostIsPrivate(ctx context.Context, host string) bool {
	return docRedirectHostIsPrivate(ctx, host)
}

// SanitizeFrontmatterValue makes s safe to embed as a single-line scalar in
// the hand-rolled vault front-matter.
func SanitizeFrontmatterValue(s string) string {
	return sanitizeFrontmatterValue(s)
}

// SanitizeFrontmatterList sanitizes items for an inline "[a, b]" list.
func SanitizeFrontmatterList(items []string) string {
	return sanitizeFrontmatterList(items)
}

// HTMLToMarkdown converts an HTML page to plain markdown text using the same
// stripping rules as document imports. It returns the extracted <title> (may
// be empty) and the body text with paragraphs separated by blank lines.
func HTMLToMarkdown(html []byte) (title, markdown string) {
	chunks, title := parseHTMLText(html)
	parts := make([]string, 0, len(chunks))
	for _, c := range chunks {
		parts = append(parts, c.Body)
	}
	return title, strings.Join(parts, "\n\n")
}

// FetchDocumentFacts runs the same fetch/read → parse → chunk pipeline as
// DocumentSource.Import, without writing artifacts, metadata or vault files.
// knowledgeDir bounds file_path reads exactly like a DocumentSource
// constructed with that base dir; empty falls back to the package default.
// It returns the extracted facts and the resolved document title.
func FetchDocumentFacts(ctx context.Context, cfg DocSourceConfig, knowledgeDir, context7Key string, logger *slog.Logger) ([]ExtractedFact, string, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ds := &DocumentSource{
		slug:           slugify(cfg.Name),
		config:         cfg,
		knowledgeDir:   knowledgeDir,
		logger:         logger,
		context7APIKey: context7Key,
	}

	var (
		content     []byte
		contentType string
		err         error
	)
	switch {
	case cfg.Context7ID != "":
		result, fetchErr := FetchContext7Docs(ctx, cfg.Context7ID, cfg.Name, context7Key)
		if fetchErr != nil {
			return nil, "", fmt.Errorf("fetching Context7 docs: %w", fetchErr)
		}
		content = []byte(result.Content)
		contentType = "text/plain"
	case cfg.URL != "":
		content, contentType, err = ds.fetchURL(ctx, cfg.URL)
		if err != nil {
			return nil, "", fmt.Errorf("fetching URL: %w", err)
		}
	case cfg.FilePath != "":
		if err := ds.validateFilePath(cfg.FilePath); err != nil {
			return nil, "", err
		}
		content, err = os.ReadFile(cfg.FilePath)
		if err != nil {
			return nil, "", fmt.Errorf("reading file: %w", err)
		}
		contentType = detectContentType(cfg.FilePath)
	default:
		return nil, "", fmt.Errorf("document source %q has no url, file_path, or context7_id", cfg.Name)
	}

	chunks, extractedTitle := ds.parseContent(content, contentType)
	title := cfg.Name
	if title == "" {
		title = extractedTitle
	}
	sourceURL := cfg.URL
	if sourceURL == "" && cfg.Context7ID != "" {
		sourceURL = "context7://" + cfg.Context7ID
	}
	return chunksToFacts(chunks, ds.slug, sourceURL, time.Now().UTC()), title, nil
}
