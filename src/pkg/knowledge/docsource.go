package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	docStorageDir    = "documents"
	docFetchTimeout  = 30 * time.Second
	docUserAgent     = "HiveKnowledge/1.0"
	docMetadataFile  = "metadata.json"
	docSourceFile    = "source"
	docFactPrefix    = "doc-"
	docMaxFetchBytes = 50 * 1024 * 1024 // 50MB
)

// DocMetadata describes an imported document and the facts extracted from it.
type DocMetadata struct {
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Author      string    `json:"author,omitempty"`
	SourceURL   string    `json:"source_url,omitempty"`
	SourceFile  string    `json:"source_file,omitempty"`
	ContentType string    `json:"content_type"`
	FetchedAt   time.Time `json:"fetched_at"`
	PageCount   int       `json:"page_count,omitempty"`
	FactCount   int       `json:"fact_count"`
	FactSlugs   []string  `json:"fact_slugs"`
}

// DocumentSource manages a single imported document: fetch/read, parse, extract
// facts to the vault, and maintain graph relationships.
type DocumentSource struct {
	slug           string
	config         DocSourceConfig
	metadata       DocMetadata
	storageDir     string
	knowledgeDir   string
	vaultDir       string
	graphStore     *GraphStore
	logger         *slog.Logger
	context7APIKey string
}

// NewDocumentSource creates a document source. Call Import to fetch and extract.
func NewDocumentSource(config DocSourceConfig, baseDir, vaultDir string, graphStore *GraphStore, logger *slog.Logger, context7Key string) *DocumentSource {
	slug := slugify(config.Name)
	return &DocumentSource{
		slug:           slug,
		config:         config,
		storageDir:     filepath.Join(baseDir, docStorageDir, slug),
		knowledgeDir:   baseDir,
		vaultDir:       vaultDir,
		graphStore:     graphStore,
		logger:         logger,
		context7APIKey: context7Key,
	}
}

// LoadDocumentSource restores a DocumentSource from its on-disk metadata.json.
// Used at startup to reload previously imported documents.
func LoadDocumentSource(dir, vaultDir string, graphStore *GraphStore, logger *slog.Logger, context7Key string) (*DocumentSource, error) {
	metaPath := filepath.Join(dir, docMetadataFile)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("reading metadata: %w", err)
	}

	var meta DocMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parsing metadata: %w", err)
	}

	config := DocSourceConfig{Name: meta.Title, URL: meta.SourceURL, FilePath: meta.SourceFile}
	if strings.HasPrefix(meta.SourceURL, "context7://") {
		config.Context7ID = strings.TrimPrefix(meta.SourceURL, "context7://")
		config.URL = ""
	}

	ds := &DocumentSource{
		slug:           meta.Slug,
		config:         config,
		metadata:       meta,
		storageDir:     dir,
		vaultDir:       vaultDir,
		graphStore:     graphStore,
		logger:         logger,
		context7APIKey: context7Key,
	}
	return ds, nil
}

// Import fetches (or reads) the document, parses it into chunks, writes
// extracted facts to the vault, and stores the original artifact + metadata.
func (ds *DocumentSource) Import(ctx context.Context) (*DocMetadata, error) {
	if err := os.MkdirAll(ds.storageDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating document storage dir: %w", err)
	}
	if err := os.MkdirAll(ds.vaultDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating vault dir: %w", err)
	}

	var (
		content     []byte
		contentType string
		err         error
	)

	if ds.config.Context7ID != "" {
		result, fetchErr := FetchContext7Docs(ctx, ds.config.Context7ID, ds.config.Name, ds.context7APIKey)
		if fetchErr != nil {
			return nil, fmt.Errorf("fetching Context7 docs: %w", fetchErr)
		}
		content = []byte(result.Content)
		contentType = "text/plain"
	} else if ds.config.URL != "" {
		content, contentType, err = ds.fetchURL(ctx, ds.config.URL)
		if err != nil {
			return nil, fmt.Errorf("fetching URL: %w", err)
		}
	} else if ds.config.FilePath != "" {
		if err := ds.validateFilePath(ds.config.FilePath); err != nil {
			return nil, err
		}
		content, err = os.ReadFile(ds.config.FilePath)
		if err != nil {
			return nil, fmt.Errorf("reading file: %w", err)
		}
		contentType = detectContentType(ds.config.FilePath)
	} else {
		return nil, fmt.Errorf("document source %q has no url, file_path, or context7_id", ds.config.Name)
	}

	ext := extensionForType(contentType)
	artifactPath := filepath.Join(ds.storageDir, docSourceFile+ext)
	if err := os.WriteFile(artifactPath, content, 0o644); err != nil {
		return nil, fmt.Errorf("storing artifact: %w", err)
	}

	chunks, extractedTitle := ds.parseContent(content, contentType)
	title := ds.config.Name
	if title == "" {
		title = extractedTitle
	}

	sourceURL := ds.config.URL
	if sourceURL == "" && ds.config.Context7ID != "" {
		sourceURL = "context7://" + ds.config.Context7ID
	}

	now := time.Now().UTC()
	facts := chunksToFacts(chunks, ds.slug, sourceURL, now)

	factSlugs, err := ds.writeFactsToVault(facts, now)
	if err != nil {
		return nil, fmt.Errorf("writing facts to vault: %w", err)
	}

	ds.emitGraphTriples(factSlugs)

	ds.metadata = DocMetadata{
		Slug:        ds.slug,
		Title:       title,
		SourceURL:   sourceURL,
		SourceFile:  ds.config.FilePath,
		ContentType: contentType,
		FetchedAt:   now,
		PageCount:   countPages(chunks),
		FactCount:   len(factSlugs),
		FactSlugs:   factSlugs,
	}

	if err := ds.writeMetadata(); err != nil {
		ds.logger.Warn("failed to write document metadata", "slug", ds.slug, "error", err)
	}

	ds.logger.Info("document imported",
		"slug", ds.slug,
		"title", title,
		"content_type", contentType,
		"facts", len(factSlugs),
	)
	return &ds.metadata, nil
}

// Delete removes the document's artifact, metadata, and extracted vault facts.
func (ds *DocumentSource) Delete() error {
	for _, slug := range ds.metadata.FactSlugs {
		path := filepath.Join(ds.vaultDir, slug+".md")
		_ = os.Remove(path)
	}

	if ds.graphStore != nil {
		// Best-effort like the vault-file removal above: a failure here leaves a
		// dangling triple pointing at a fact whose file is already gone, which is
		// a graph-consistency issue worth logging but not worth blocking the
		// document delete over.
		for _, slug := range ds.metadata.FactSlugs {
			if err := ds.graphStore.RemoveTriple(Triple{Subject: slug, Predicate: PredicateDerivedFrom, Object: "doc:" + ds.slug}); err != nil {
				ds.logger.Warn("failed to remove derived-from triple", "slug", slug, "doc", ds.slug, "error", err)
			}
		}
		for i := 1; i < len(ds.metadata.FactSlugs); i++ {
			if err := ds.graphStore.RemoveTriple(Triple{
				Subject:   ds.metadata.FactSlugs[i-1],
				Predicate: PredicateRelatedTo,
				Object:    ds.metadata.FactSlugs[i],
			}); err != nil {
				ds.logger.Warn("failed to remove related-to triple",
					"subject", ds.metadata.FactSlugs[i-1], "object", ds.metadata.FactSlugs[i], "error", err)
			}
		}
	}

	if err := os.RemoveAll(ds.storageDir); err != nil {
		return fmt.Errorf("removing document storage: %w", err)
	}

	ds.logger.Info("document deleted", "slug", ds.slug, "facts_removed", len(ds.metadata.FactSlugs))
	return nil
}

// Reimport deletes existing facts and re-imports the document.
func (ds *DocumentSource) Reimport(ctx context.Context) (*DocMetadata, error) {
	if err := ds.Delete(); err != nil {
		ds.logger.Warn("cleanup before reimport failed", "slug", ds.slug, "error", err)
	}
	return ds.Import(ctx)
}

// Metadata returns the current document metadata.
func (ds *DocumentSource) Metadata() DocMetadata {
	return ds.metadata
}

// docRedirectResolver resolves a redirect target's hostname to IPs. A package
// var so tests can point it at a stub instead of real DNS.
var docRedirectResolver = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// docRedirectHostIsPrivate reports whether a redirect target points at a
// private/internal address, RESOLVING DNS to do so.
//
// A redirect target is chosen by the remote server at fetch time, is fully
// attacker-controlled, and has no operator gate. An IP-literal-only check would
// let a public URL 302 to a hostname that resolves to 169.254.169.254 and reach
// cloud metadata, which is exactly the bypass the pre-fetch isPrivateURL guard
// in the API handler (which does resolve) was added to close.
//
// Fails closed: an unparseable target or a DNS error is treated as private.
func docRedirectHostIsPrivate(ctx context.Context, host string) bool {
	if host == "" {
		return true
	}
	if docRedirectAddressIsPrivate(host) {
		return true
	}
	addrs, err := docRedirectResolver(ctx, host)
	if err != nil || len(addrs) == 0 {
		return true
	}
	for _, addr := range addrs {
		if docRedirectAddressIsPrivate(addr) {
			return true
		}
	}
	return false
}

func docRedirectAddressIsPrivate(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" || host == "localhost" {
		return host == "localhost"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// docNoRedirectToPrivate is a CheckRedirect policy that prevents the HTTP
// client from following redirects to private/internal hosts. A public URL that
// returns a 302 to an internal address (e.g. 169.254.169.254 or 10.x) would
// otherwise bypass the pre-fetch isPrivateURL guard in the API handler.
func docNoRedirectToPrivate(req *http.Request, via []*http.Request) error {
	return knowledgeNoRedirectToPrivate(req, via)
}

// knowledgeNoRedirectToPrivate is a CheckRedirect policy for user-configurable
// knowledge HTTP endpoints. It keeps redirect validation as strong as the
// dashboard preflight checks on the original URL.
func knowledgeNoRedirectToPrivate(req *http.Request, via []*http.Request) error {
	const maxRedirects = 3
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	// Only http(s) may be followed: a redirect to file://, gopher:// or similar
	// is never legitimate for a document fetch and is a classic SSRF pivot.
	if scheme := strings.ToLower(req.URL.Scheme); scheme != "http" && scheme != "https" {
		return fmt.Errorf("redirect to non-http(s) scheme blocked: %s", scheme)
	}
	if docRedirectHostIsPrivate(req.Context(), req.URL.Hostname()) {
		return fmt.Errorf("redirect to private/internal host blocked: %s", req.URL.Host)
	}
	return nil
}

func (ds *DocumentSource) fetchURL(ctx context.Context, url string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, docFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", docUserAgent)

	client := &http.Client{CheckRedirect: docNoRedirectToPrivate}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("HTTP GET: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, docMaxFetchBytes))
	if err != nil {
		return nil, "", fmt.Errorf("reading response: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = detectContentType(url)
	}
	return body, normalizeContentType(ct), nil
}

// allowedFilePrefixes is the list of directory prefixes from which the hive
// may read local documents. Restricting to /data/knowledge prevents an
// authenticated caller from using file_path to read arbitrary server files
// (e.g. /data/hive.yaml, /secrets/*, /etc/passwd).
// Tests that use a DocumentSource with a file outside the knowledge dir must
// override this variable for the duration of the test.
var allowedFilePrefixes = []string{localKnowledgeDir + "/"}

// validateLocalFilePath rejects any file_path that falls outside the
// allowed knowledge directory. The path is cleaned before comparison to
// block ../ traversal.
func validateLocalFilePath(path string) error {
	clean := filepath.Clean(path)
	for _, prefix := range allowedFilePrefixes {
		if strings.HasPrefix(clean, prefix) {
			return nil
		}
	}
	return fmt.Errorf("file_path %q is outside the allowed knowledge directory (%s)", path, localKnowledgeDir)
}

// validateFilePath checks the file_path on this DocumentSource. Uses the
// instance's knowledgeDir (set at construction time) when available, so that
// unit tests constructing a DocumentSource with a temporary baseDir get their
// correct prefix automatically. Falls back to the global allowedFilePrefixes.
func (ds *DocumentSource) validateFilePath(path string) error {
	if ds.knowledgeDir != "" {
		clean := filepath.Clean(path)
		prefix := ds.knowledgeDir + string(filepath.Separator)
		if strings.HasPrefix(clean, prefix) {
			return nil
		}
		return fmt.Errorf("file_path %q is outside the allowed knowledge directory (%s)", path, ds.knowledgeDir)
	}
	return validateLocalFilePath(path)
}

func (ds *DocumentSource) parseContent(content []byte, contentType string) ([]DocChunk, string) {
	switch contentType {
	case "application/pdf":
		return parsePDF(content)
	case "text/html":
		return parseHTMLText(content)
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return parseDocx(content)
	default:
		chunks := parseRawText(string(content))
		title := ""
		if len(chunks) > 0 {
			title = chunks[0].Title
		}
		return chunks, title
	}
}

func (ds *DocumentSource) writeFactsToVault(facts []ExtractedFact, synthesized time.Time) ([]string, error) {
	var slugs []string
	for i, fact := range facts {
		factSlug := fmt.Sprintf("%s%s-%03d", docFactPrefix, ds.slug, i)
		filename := factSlug + ".md"
		path := filepath.Join(ds.vaultDir, filename)

		var buf strings.Builder
		buf.WriteString("---\n")
		fmt.Fprintf(&buf, "title: %s\n", fact.Title)
		fmt.Fprintf(&buf, "type: %s\n", string(fact.Type))
		fmt.Fprintf(&buf, "layer: %s\n", string(ds.config.Layer))
		fmt.Fprintf(&buf, "confidence: %.2f\n", fact.Confidence)
		if len(fact.Tags) > 0 {
			fmt.Fprintf(&buf, "tags: [%s]\n", strings.Join(fact.Tags, ", "))
		}
		fmt.Fprintf(&buf, "source: %s\n", fact.SourcePR)
		if ds.config.URL != "" {
			fmt.Fprintf(&buf, "source_url: %s\n", ds.config.URL)
		}
		fmt.Fprintf(&buf, "synthesized: %s\n", synthesized.Format(time.RFC3339))
		buf.WriteString("---\n\n")
		buf.WriteString(fact.Body)

		tmpPath := path + ".tmp"
		if err := os.WriteFile(tmpPath, []byte(buf.String()), 0o644); err != nil {
			return slugs, fmt.Errorf("writing fact %d: %w", i, err)
		}
		if err := os.Rename(tmpPath, path); err != nil {
			return slugs, fmt.Errorf("renaming fact %d: %w", i, err)
		}
		slugs = append(slugs, factSlug)
	}
	return slugs, nil
}

func (ds *DocumentSource) emitGraphTriples(factSlugs []string) {
	if ds.graphStore == nil || len(factSlugs) == 0 {
		return
	}

	docNode := "doc:" + ds.slug
	for _, slug := range factSlugs {
		if err := ds.graphStore.AddTriple(Triple{
			Subject: slug, Predicate: PredicateDerivedFrom, Object: docNode,
		}); err != nil {
			ds.logger.Warn("graph triple failed", "triple", slug+"→"+docNode, "error", err)
		}
	}

	for i := 1; i < len(factSlugs); i++ {
		if err := ds.graphStore.AddTriple(Triple{
			Subject: factSlugs[i-1], Predicate: PredicateRelatedTo, Object: factSlugs[i],
		}); err != nil {
			ds.logger.Warn("graph triple failed", "from", factSlugs[i-1], "to", factSlugs[i], "error", err)
		}
	}
}

func (ds *DocumentSource) writeMetadata() error {
	data, err := json.MarshalIndent(ds.metadata, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(ds.storageDir, docMetadataFile), data, 0o644)
}

func detectContentType(pathOrURL string) string {
	lower := strings.ToLower(pathOrURL)
	switch {
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(lower, ".html"), strings.HasSuffix(lower, ".htm"):
		return "text/html"
	case strings.HasSuffix(lower, ".md"), strings.HasSuffix(lower, ".markdown"):
		return "text/plain"
	case strings.HasSuffix(lower, ".txt"):
		return "text/plain"
	case strings.HasSuffix(lower, ".docx"):
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	default:
		return "text/plain"
	}
}

const docxMIME = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

func normalizeContentType(ct string) string {
	ct = strings.ToLower(ct)
	switch {
	case strings.Contains(ct, "pdf"):
		return "application/pdf"
	case strings.Contains(ct, "html"):
		return "text/html"
	case strings.Contains(ct, "wordprocessingml"), strings.Contains(ct, "docx"):
		return docxMIME
	default:
		return "text/plain"
	}
}

func extensionForType(contentType string) string {
	switch contentType {
	case "application/pdf":
		return ".pdf"
	case "text/html":
		return ".html"
	case docxMIME:
		return ".docx"
	default:
		return ".txt"
	}
}

func countPages(chunks []DocChunk) int {
	seen := make(map[int]bool)
	for _, c := range chunks {
		if c.PageNum > 0 {
			seen[c.PageNum] = true
		}
	}
	return len(seen)
}
