package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// TypeContext7 is the Context7 connector type.
const TypeContext7 = "context7"

// Context7APIKeyEnv is the environment variable the Context7 API key is read
// from when the connector names no auth.env or auth.file.
const Context7APIKeyEnv = "CONTEXT7_API_KEY"

// fetchContext7Docs is the existing Context7 llms.txt fetch; a var so tests
// can stub it.
var fetchContext7Docs = knowledge.FetchContext7Docs

// context7Connector syncs the default Context7 libraries as one fact each.
//
// Scope keys:
//   - library_ids (required): comma-separated Context7 library IDs
//     ("/org/project").
//   - topic: optional topic that narrows the fetched documentation.
//
// The API key is a reference (auth.env or auth.file); with neither set the
// CONTEXT7_API_KEY environment variable is used when present.
type context7Connector struct {
	cfg    ConnectorConfig
	logger *slog.Logger
}

func newContext7Connector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &context7Connector{cfg: cfg, logger: logger}, nil
}

func (c *context7Connector) Type() string { return TypeContext7 }

// FullListing: every sync re-fetches every configured library.
func (c *context7Connector) FullListing() bool { return true }

func (c *context7Connector) Validate(cfg ConnectorConfig) error {
	ids := scopeList(cfg, "library_ids")
	if len(ids) == 0 {
		return fmt.Errorf("scope.library_ids is required (comma-separated Context7 library IDs such as /vercel/next.js)")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !strings.HasPrefix(id, "/") || strings.ContainsAny(id, " \t?#") {
			return fmt.Errorf("scope.library_ids: %q must look like /org/project", id)
		}
		if seen[id] {
			return fmt.Errorf("scope.library_ids: %q listed twice", id)
		}
		seen[id] = true
	}
	return nil
}

func (c *context7Connector) apiKey() (string, error) {
	if c.cfg.Auth.Configured() {
		return c.cfg.Auth.Secret()
	}
	return strings.TrimSpace(os.Getenv(Context7APIKeyEnv)), nil
}

// Sync fetches each configured library and emits one page per library. The
// cursor is a content hash.
func (c *context7Connector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	key, err := c.apiKey()
	if err != nil {
		return cur, err
	}
	topic := c.cfg.ScopeValue("topic")
	h := sha256.New()
	for _, id := range scopeList(c.cfg, "library_ids") {
		res, err := fetchContext7Docs(ctx, id, topic, key)
		if err != nil {
			return cur, fmt.Errorf("library %s: %w", id, err)
		}
		if res.Omitted.Any() {
			// The content carries its own marker; this names the library so
			// the operator can raise HIVE_CONTEXT7_MAX_* if the cap bites.
			c.logger.Warn("context7: dropped snippets over the content cap",
				"connector", c.cfg.Name, "library", id,
				"omitted_snippets", res.Omitted.Snippets, "omitted_bytes", res.Omitted.Bytes)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", id, res.Content)
		p := Page{
			ID:       id,
			Title:    res.Title,
			URL:      "context7://" + id,
			Markdown: res.Content,
			Path:     []string{"Context7"},
		}
		if err := emit(p); err != nil {
			return cur, err
		}
	}
	return Cursor(hex.EncodeToString(h.Sum(nil))[:16]), nil
}
