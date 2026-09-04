package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const localKnowledgeDir = "/data/knowledge"

// knowledgeBaseDir is the root for imported document metadata and document
// vaults. Production uses localKnowledgeDir; tests override it for hermetic
// temp-dir-backed ingestion.
var knowledgeBaseDir = localKnowledgeDir

// vaultsBaseDir is where user-created knowledge channels (vaults) are stored on
// the spoke's data volume, one directory per channel. It mirrors the location the
// bead synthesizer uses for its own vault. A var (not const) only so purge/create
// tests can point it at a temp dir; production never reassigns it.
var vaultsBaseDir = "/data/vaults"

// reservedVaultName is the automation vault the bead synthesizer writes into. It
// must never be offered to users as an import target nor be creatable/writable by
// hand: importing into it would corrupt the synthesizer's own store, and it is
// deliberately hidden from every user-facing channel picker. See issue #3581,
// where the Import Facts dropdown surfaced this vault as the only option and every
// import failed with "layer bead-synth-wiki has no configured endpoint".
const reservedVaultName = "bead-synth-wiki"

// KnowledgeAPI provides a unified interface for dashboard endpoints to query
// across all configured wiki layers.
type KnowledgeAPI struct {
	mu             sync.RWMutex
	layers         []layerClient
	config         KnowledgeConfig
	promoter       *Promoter
	subscriptions  []Subscription
	vaults         []*FileStore
	gitSources     []*GitSource
	docSources     []*DocumentSource
	graphStore     *GraphStore
	logger         *slog.Logger
	context7APIKey string
}

// Subscription represents a user-added wiki endpoint.
type Subscription struct {
	URL   string    `json:"url"`
	Layer LayerType `json:"layer"`
	Name  string    `json:"name"`
	Added time.Time `json:"added"`
}

// NewKnowledgeAPI creates a dashboard-facing API from the full knowledge config.
func NewKnowledgeAPI(layers []LayerConfig, config KnowledgeConfig, logger *slog.Logger) *KnowledgeAPI {
	var clients []layerClient
	for _, l := range layers {
		endpoint := l.Endpoint()
		if endpoint == "" {
			continue
		}
		clients = append(clients, layerClient{
			layerType: l.Type,
			client:    NewClient(endpoint, logger),
		})
	}

	promoter := NewPromoter(layers, config.Curator, logger)

	api := &KnowledgeAPI{
		layers:         clients,
		config:         config,
		promoter:       promoter,
		logger:         logger,
		context7APIKey: os.Getenv("CONTEXT7_API_KEY"),
	}
	api.reloadDocuments()
	api.autoImportContext7()
	return api
}

// SetContext7APIKey configures the API key for Context7 library imports.
func (k *KnowledgeAPI) SetContext7APIKey(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.context7APIKey = key
}

// WireContext7Suggester attaches a Context7 suggestion callback to the primer.
// When the primer builds knowledge for an agent kick, it checks if Context7
// has docs for any of the task's keywords that aren't already imported.
func (k *KnowledgeAPI) WireContext7Suggester(primer *Primer) {
	if k.context7APIKey == "" {
		return
	}
	primer.SetContext7Suggester(func(ctx context.Context, keyword string) string {
		k.mu.RLock()
		for _, ds := range k.docSources {
			if strings.Contains(strings.ToLower(ds.metadata.Title), strings.ToLower(keyword)) {
				k.mu.RUnlock()
				return ""
			}
		}
		k.mu.RUnlock()

		results, err := SearchContext7(ctx, keyword, k.context7APIKey)
		if err != nil || len(results) == 0 {
			return ""
		}
		best := results[0]
		return fmt.Sprintf("Context7 docs available for **%s** (%s) — run `bd kb import-ctx7 %s` to import", best.Title, best.ID, best.ID)
	})
}

// LayerStatus describes the health of a single wiki layer.
type LayerStatus struct {
	Type    LayerType `json:"type"`
	URL     string    `json:"url"`
	Healthy bool      `json:"healthy"`
	Pages   int       `json:"pages,omitempty"`
}

// SearchAll queries all reachable layers and returns tagged results.
func (k *KnowledgeAPI) SearchAll(ctx context.Context, query string, typeFilter string, limit int) []Fact {
	var all []Fact
	for _, lc := range k.layers {
		results, err := lc.client.Search(ctx, query, typeFilter, limit)
		if err != nil {
			k.logger.Warn("knowledge search failed", "layer", lc.layerType, "error", err)
			continue
		}
		for _, r := range results {
			all = append(all, Fact{
				Slug:       r.Slug,
				Title:      r.Title,
				Type:       FactType(r.Type),
				Body:       r.Snippet,
				Confidence: r.Confidence,
				Status:     r.Status,
				Tags:       r.Tags,
				Layer:      lc.layerType,
			})
		}
	}
	return all
}

// LayerFacts returns facts from a specific layer.
func (k *KnowledgeAPI) LayerFacts(ctx context.Context, layer LayerType, typeFilter string) []Fact {
	for _, lc := range k.layers {
		if lc.layerType != layer {
			continue
		}
		results, err := lc.client.ListPages(ctx, typeFilter)
		if err != nil {
			k.logger.Warn("layer list failed", "layer", layer, "error", err)
			return nil
		}
		facts := make([]Fact, len(results))
		for i, r := range results {
			facts[i] = Fact{
				Slug:       r.Slug,
				Title:      r.Title,
				Type:       FactType(r.Type),
				Body:       r.Snippet,
				Confidence: r.Confidence,
				Status:     r.Status,
				Tags:       r.Tags,
				Layer:      layer,
			}
		}
		return facts
	}
	return nil
}

// ReadFact returns a single fact from the first layer that has it.
func (k *KnowledgeAPI) ReadFact(ctx context.Context, slug string) (*Fact, error) {
	for _, lc := range k.layers {
		page, err := lc.client.ReadPage(ctx, slug)
		if err != nil {
			continue
		}
		return &Fact{
			Slug:       page.Slug,
			Title:      page.Title,
			Type:       FactType(page.Type),
			Body:       page.Body,
			Confidence: page.Confidence,
			Status:     page.Status,
			Tags:       page.Tags,
			Layer:      lc.layerType,
		}, nil
	}
	if f, err := k.VaultFact(slug); err == nil {
		return f, nil
	}
	return nil, nil
}

// Health checks all configured layers and returns their status.
func (k *KnowledgeAPI) Health(ctx context.Context) []LayerStatus {
	statuses := make([]LayerStatus, len(k.layers))
	for i, lc := range k.layers {
		statuses[i] = LayerStatus{
			Type:    lc.layerType,
			URL:     lc.client.baseURL,
			Healthy: lc.client.Healthy(ctx),
		}
		if statuses[i].Healthy {
			stats, err := lc.client.Stats(ctx)
			if err == nil {
				statuses[i].Pages = stats.TotalPages
			}
		}
	}
	return statuses
}

// Stats returns aggregate stats across all layers.
func (k *KnowledgeAPI) Stats(ctx context.Context) map[string]interface{} {
	result := map[string]interface{}{
		"enabled": k.config.Enabled,
		"engine":  k.config.Engine,
	}

	layerStats := make([]map[string]interface{}, 0, len(k.layers)+len(k.vaults))
	for _, lc := range k.layers {
		ls := map[string]interface{}{
			"type":    lc.layerType,
			"url":     lc.client.baseURL,
			"healthy": false,
		}
		stats, err := lc.client.Stats(ctx)
		if err == nil {
			ls["healthy"] = true
			ls["total_pages"] = stats.TotalPages
			ls["by_type"] = stats.ByType
			ls["by_status"] = stats.ByStatus
			ls["stale"] = stats.Stale
			ls["orphaned"] = stats.Orphaned
		}
		layerStats = append(layerStats, ls)
	}

	for _, v := range k.vaults {
		vs := v.Stats()
		layerStats = append(layerStats, map[string]interface{}{
			"type":        v.Name(),
			"healthy":     true,
			"total_pages": vs.TotalPages,
		})
	}

	for _, gs := range k.gitSources {
		info := gs.Info()
		layerStats = append(layerStats, map[string]interface{}{
			"type":        "git_source:" + info.Name,
			"layer":       info.Layer,
			"url":         info.URL,
			"subpath":     info.Subpath,
			"healthy":     info.Ready,
			"total_pages": info.Pages,
		})
	}

	result["layers"] = layerStats
	result["layers_count"] = len(layerStats)

	return result
}

// CreateFactRequest is the payload for creating a new fact.
type CreateFactRequest struct {
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Type       string   `json:"type"`
	Tags       []string `json:"tags"`
	Layer      string   `json:"layer"`
	Confidence float64  `json:"confidence"`
}

// CreateFact ingests a new fact into the specified layer.
func (k *KnowledgeAPI) CreateFact(ctx context.Context, req CreateFactRequest) error {
	layer := LayerType(req.Layer)
	client := k.clientForLayer(layer)
	if client == nil {
		return fmt.Errorf("layer %s has no configured endpoint", req.Layer)
	}

	fact := ExtractedFact{
		Title:      req.Title,
		Body:       req.Body,
		Type:       FactType(req.Type),
		Confidence: req.Confidence,
		Tags:       req.Tags,
		SourcePR:   "manual",
		SourceDate: time.Now(),
	}

	if err := client.IngestFacts(ctx, []ExtractedFact{fact}); err != nil {
		return fmt.Errorf("ingesting fact: %w", err)
	}

	k.logger.Info("fact created", "title", req.Title, "layer", req.Layer, "type", req.Type)
	return nil
}

// UpdateFactRequest is the payload for updating an existing fact.
type UpdateFactRequest struct {
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Type       string   `json:"type"`
	Tags       []string `json:"tags"`
	Status     string   `json:"status"`
	Confidence float64  `json:"confidence"`
}

// UpdateFact modifies an existing fact in the specified layer.
func (k *KnowledgeAPI) UpdateFact(ctx context.Context, layer LayerType, slug string, req UpdateFactRequest) error {
	client := k.clientForLayer(layer)
	if client == nil {
		return fmt.Errorf("layer %s has no configured endpoint", layer)
	}

	update := pageUpdateRequest{
		Title:      req.Title,
		Body:       req.Body,
		Type:       req.Type,
		Confidence: req.Confidence,
		Tags:       req.Tags,
		Status:     req.Status,
	}

	if err := client.UpdatePage(ctx, slug, update); err != nil {
		return fmt.Errorf("updating fact %s: %w", slug, err)
	}

	k.logger.Info("fact updated", "slug", slug, "layer", layer)
	return nil
}

// DeleteFact removes a fact from the specified layer.
func (k *KnowledgeAPI) DeleteFact(ctx context.Context, layer LayerType, slug string) error {
	client := k.clientForLayer(layer)
	if client == nil {
		return fmt.Errorf("layer %s has no configured endpoint", layer)
	}

	if err := client.DeletePage(ctx, slug); err != nil {
		return fmt.Errorf("deleting fact %s: %w", slug, err)
	}

	k.logger.Info("fact deleted", "slug", slug, "layer", layer)
	return nil
}

// Promoter exposes the layer promoter so the scheduled promotion loop can be
// built over the same clients the dashboard's manual promote path uses.
func (k *KnowledgeAPI) Promoter() *Promoter {
	return k.promoter
}

// PromoteFact promotes a fact from one layer to another (upward only).
func (k *KnowledgeAPI) PromoteFact(ctx context.Context, req PromoteRequest) PromoteResult {
	return k.promoter.Promote(ctx, req)
}

// ImportFacts parses markdown or JSON content and ingests extracted facts.
func (k *KnowledgeAPI) ImportFacts(ctx context.Context, layer LayerType, content string, format string) (int, error) {
	// The reserved automation vault is never a valid user import target — writing
	// into it would corrupt the bead synthesizer's store (issue #3581).
	if string(layer) == reservedVaultName {
		return 0, fmt.Errorf("%q is a reserved automation channel and cannot be imported into; pick or create another channel", reservedVaultName)
	}

	var facts []ExtractedFact
	switch format {
	case "json":
		if err := parseJSONFacts(content, &facts); err != nil {
			return 0, fmt.Errorf("parsing JSON: %w", err)
		}
	case "markdown", "md":
		facts = parseMarkdownFacts(content)
	default:
		facts = parseMarkdownFacts(content)
	}
	if len(facts) == 0 {
		return 0, nil
	}

	// Prefer a local vault (channel) with this name — the common case on a spoke
	// with no remote wiki layers configured, and the fix for #3581 where import
	// used to fail because only remote "layers" were writable. Fall back to a
	// configured remote layer endpoint when no vault matches.
	if v := k.vaultByName(string(layer)); v != nil {
		if err := v.WriteFacts(facts); err != nil {
			return 0, fmt.Errorf("writing facts to channel %q: %w", layer, err)
		}
		k.triggerVaultReindex(v.RootDir())
		k.logger.Info("facts imported to vault", "count", len(facts), "channel", layer, "format", format)
		return len(facts), nil
	}

	client := k.clientForLayer(layer)
	if client == nil {
		return 0, fmt.Errorf("no writable channel named %q — create a channel first, or configure a wiki endpoint for that layer", layer)
	}
	if err := client.IngestFacts(ctx, facts); err != nil {
		return 0, fmt.Errorf("ingesting imported facts: %w", err)
	}
	k.logger.Info("facts imported", "count", len(facts), "layer", layer, "format", format)
	return len(facts), nil
}

// vaultByName returns the connected vault whose name matches (case-sensitive),
// or nil. The reserved automation vault is intentionally NOT matchable here — it
// is filtered so it can never be resolved as an import target.
func (k *KnowledgeAPI) vaultByName(name string) *FileStore {
	if name == "" || name == reservedVaultName {
		return nil
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	for _, v := range k.vaults {
		if v.Name() == name {
			return v
		}
	}
	return nil
}

// WritableVaults returns the connected vaults a user may import into — every
// vault except the reserved automation one. This is the source of truth for the
// dashboard's channel pickers (issue #3581).
func (k *KnowledgeAPI) WritableVaults() []VaultInfo {
	all := k.Vaults()
	out := make([]VaultInfo, 0, len(all))
	for _, v := range all {
		if v.Name == reservedVaultName {
			continue
		}
		out = append(out, v)
	}
	return out
}

// CreateVault creates a brand-new local knowledge channel (vault) under
// vaultsBaseDir and connects it so users can import facts into it. The name must
// be path-safe and must not collide with the reserved automation vault. Creating
// an already-existing channel is idempotent (it just connects it).
func (k *KnowledgeAPI) CreateVault(name string) (VaultInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return VaultInfo{}, fmt.Errorf("channel name is required")
	}
	if name == reservedVaultName {
		return VaultInfo{}, fmt.Errorf("%q is reserved for automation and cannot be created", reservedVaultName)
	}
	if !isSafeVaultName(name) {
		return VaultInfo{}, fmt.Errorf("invalid channel name %q: use letters, numbers, spaces, dashes or underscores", name)
	}

	dir := filepath.Join(vaultsBaseDir, slugify(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return VaultInfo{}, fmt.Errorf("creating channel dir: %w", err)
	}
	// ConnectVault is idempotent-ish: it errors if this exact dir is already
	// connected, which for a create-or-connect flow we treat as success.
	if err := k.ConnectVault(dir, name); err != nil && !strings.Contains(err.Error(), "already connected") {
		return VaultInfo{}, err
	}
	for _, v := range k.Vaults() {
		if v.RootDir == dir {
			return v, nil
		}
	}
	return VaultInfo{Name: name, RootDir: dir}, nil
}

// isSafeVaultName rejects names that could escape vaultsBaseDir or break the
// on-disk layout. Deliberately conservative: a channel name is a human label.
func isSafeVaultName(name string) bool {
	if len(name) > 64 || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ' ' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// Subscriptions returns the current list of subscribed wiki endpoints.
func (k *KnowledgeAPI) Subscriptions() []Subscription {
	subs := make([]Subscription, len(k.subscriptions))
	copy(subs, k.subscriptions)
	return subs
}

// AddSubscription adds a new wiki endpoint and connects a client for it.
func (k *KnowledgeAPI) AddSubscription(sub Subscription) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	for _, existing := range k.subscriptions {
		if existing.URL == sub.URL {
			return fmt.Errorf("subscription already exists: %s", sub.URL)
		}
	}

	sub.Added = time.Now()
	k.subscriptions = append(k.subscriptions, sub)

	k.layers = append(k.layers, layerClient{
		layerType: sub.Layer,
		client:    NewClient(sub.URL, k.logger),
	})

	k.logger.Info("subscription added", "url", sub.URL, "layer", sub.Layer, "name", sub.Name)
	return nil
}

// RemoveSubscription disconnects a wiki endpoint by URL.
func (k *KnowledgeAPI) RemoveSubscription(url string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	found := false
	newSubs := make([]Subscription, 0, len(k.subscriptions))
	for _, s := range k.subscriptions {
		if s.URL == url {
			found = true
			continue
		}
		newSubs = append(newSubs, s)
	}
	if !found {
		return fmt.Errorf("subscription not found: %s", url)
	}
	k.subscriptions = newSubs

	newLayers := make([]layerClient, 0, len(k.layers))
	for _, lc := range k.layers {
		if lc.client.baseURL == url {
			continue
		}
		newLayers = append(newLayers, lc)
	}
	k.layers = newLayers

	k.logger.Info("subscription removed", "url", url)
	return nil
}

// VaultInfo describes a connected Obsidian/file-based vault for the dashboard.
type VaultInfo struct {
	Name        string         `json:"name"`
	RootDir     string         `json:"root_dir"`
	Pages       int            `json:"pages"`
	LastIndexed time.Time      `json:"last_indexed"`
	TagCounts   map[string]int `json:"tag_counts,omitempty"`
}

// ConnectVault adds a file-based vault (Obsidian, MindStudio export, or any
// directory of markdown files) as a knowledge source.
func (k *KnowledgeAPI) ConnectVault(rootDir string, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	for _, v := range k.vaults {
		if v.RootDir() == rootDir {
			return fmt.Errorf("vault already connected: %s", rootDir)
		}
	}

	store, err := NewFileStore(rootDir, name, k.logger)
	if err != nil {
		return fmt.Errorf("connecting vault: %w", err)
	}

	k.vaults = append(k.vaults, store)
	k.logger.Info("vault connected", "name", name, "dir", rootDir, "pages", store.Stats().TotalPages)
	return nil
}

// DisconnectVault removes a file-based vault by root directory.
func (k *KnowledgeAPI) DisconnectVault(rootDir string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	found := false
	newVaults := make([]*FileStore, 0, len(k.vaults))
	for _, v := range k.vaults {
		if v.RootDir() == rootDir {
			found = true
			continue
		}
		newVaults = append(newVaults, v)
	}
	if !found {
		return fmt.Errorf("vault not found: %s", rootDir)
	}
	k.vaults = newVaults
	k.logger.Info("vault disconnected", "dir", rootDir)
	return nil
}

// PurgeVault disconnects a vault AND deletes its files from disk. This is the
// destructive "delete channel" path (vs DisconnectVault, which only un-indexes
// and leaves files in place). It is guarded hard: the directory MUST live under
// vaultsBaseDir and MUST NOT be the reserved automation vault — a purge outside
// the vaults tree, or of bead-synth-wiki, is refused. The disconnect happens
// first so agents stop reading it before the files vanish.
func (k *KnowledgeAPI) PurgeVault(rootDir string) error {
	clean := filepath.Clean(rootDir)
	base := filepath.Clean(vaultsBaseDir)
	// Must be strictly inside vaultsBaseDir (not the base itself, not a sibling).
	if clean == base || !strings.HasPrefix(clean, base+string(filepath.Separator)) {
		return fmt.Errorf("refusing to purge %q: not inside %s", rootDir, base)
	}
	if filepath.Base(clean) == reservedVaultName {
		return fmt.Errorf("%q is reserved for automation and cannot be purged", reservedVaultName)
	}
	// Disconnect first (best effort — a vault may be purged even if it was never
	// indexed as a connected vault, e.g. a leftover dir), then remove the files.
	if err := k.DisconnectVault(rootDir); err != nil {
		k.logger.Info("purge: vault not connected, removing files anyway", "dir", clean, "err", err)
	}
	if err := os.RemoveAll(clean); err != nil {
		return fmt.Errorf("deleting channel files: %w", err)
	}
	k.logger.Info("vault purged", "dir", clean)
	return nil
}

// Vaults returns info about all connected file-based vaults.
func (k *KnowledgeAPI) Vaults() []VaultInfo {
	infos := make([]VaultInfo, len(k.vaults))
	for i, v := range k.vaults {
		stats := v.Stats()
		infos[i] = VaultInfo{
			Name:        stats.Name,
			RootDir:     stats.RootDir,
			Pages:       stats.TotalPages,
			LastIndexed: stats.LastIndexed,
			TagCounts:   stats.TagCounts,
		}
	}
	return infos
}

// GetVaultStore returns the underlying FileStore for a vault by root directory.
// This is used by the git syncer to trigger reindex after pulls.
func (k *KnowledgeAPI) GetVaultStore(rootDir string) *FileStore {
	for _, v := range k.vaults {
		if v.RootDir() == rootDir {
			return v
		}
	}
	return nil
}

// SetGraphStore attaches the knowledge graph for relationship-based queries.
func (k *KnowledgeAPI) SetGraphStore(gs *GraphStore) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.graphStore = gs
}

// GraphStore returns the attached graph store, or nil.
func (k *KnowledgeAPI) GraphStore() *GraphStore {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.graphStore
}

// FileStores returns all connected vault FileStores.
func (k *KnowledgeAPI) FileStores() []*FileStore {
	k.mu.RLock()
	defer k.mu.RUnlock()
	result := make([]*FileStore, len(k.vaults))
	copy(result, k.vaults)
	return result
}

// ReindexVault forces a re-scan of a specific vault.
func (k *KnowledgeAPI) ReindexVault(rootDir string) error {
	for _, v := range k.vaults {
		if v.RootDir() == rootDir {
			v.Reindex()
			return nil
		}
	}
	return fmt.Errorf("vault not found: %s", rootDir)
}

// SearchAllWithVaults queries wiki layers, file-based vaults, and git sources.
func (k *KnowledgeAPI) SearchAllWithVaults(ctx context.Context, query string, typeFilter string, limit int) []Fact {
	k.mu.RLock()
	vaults := k.vaults
	sources := k.gitSources
	k.mu.RUnlock()

	results := k.SearchAll(ctx, query, typeFilter, limit)

	for _, v := range vaults {
		if query == "" {
			results = append(results, v.ListPages(typeFilter)...)
		} else {
			results = append(results, v.Search(query, limit)...)
		}
	}

	for _, gs := range sources {
		if !gs.Ready() {
			continue
		}
		store := gs.Store()
		var facts []Fact
		if query == "" {
			facts = store.ListPages(typeFilter)
		} else {
			facts = store.Search(query, limit)
		}
		for i := range facts {
			facts[i].Layer = gs.Config().Layer
		}
		results = append(results, facts...)
	}

	return results
}

// VaultFacts returns all facts from a specific vault by name.
func (k *KnowledgeAPI) VaultFacts(name string) []Fact {
	for _, v := range k.vaults {
		if v.Name() == name {
			return v.ListPages("")
		}
	}
	return nil
}

// VaultFact reads a single fact from any connected vault.
func (k *KnowledgeAPI) VaultFact(slug string) (*Fact, error) {
	for _, v := range k.vaults {
		fact, err := v.ReadPage(slug)
		if err == nil {
			return fact, nil
		}
	}
	return nil, fmt.Errorf("fact not found in any vault: %s", slug)
}

// Layers returns the configured layer types for the frontend.
func (k *KnowledgeAPI) Layers() []LayerType {
	seen := make(map[LayerType]bool)
	var result []LayerType
	for _, lc := range k.layers {
		if !seen[lc.layerType] {
			seen[lc.layerType] = true
			result = append(result, lc.layerType)
		}
	}
	return result
}

// ConnectGitSource adds a remote git repo as a knowledge source. It clones the
// repo (sparse if subpath is set), indexes the markdown files, and starts a
// periodic sync loop. Any layer level can have git sources.
func (k *KnowledgeAPI) ConnectGitSource(ctx context.Context, config GitSourceConfig) error {
	config.URL = strings.TrimSpace(config.URL)
	if err := ValidateGitSourceURLContext(ctx, config.URL); err != nil {
		return fmt.Errorf("invalid git source url: %w", err)
	}

	k.mu.RLock()
	for _, gs := range k.gitSources {
		if gs.Config().URL == config.URL && gs.Config().Subpath == config.Subpath {
			k.mu.RUnlock()
			return fmt.Errorf("git source already connected: %s (subpath: %s)", config.URL, config.Subpath)
		}
	}
	k.mu.RUnlock()

	gs := newGitSourceForConnect(config, localKnowledgeDir, k.logger)
	if err := initGitSourceForConnect(gs, ctx); err != nil {
		return err
	}

	k.mu.Lock()
	k.gitSources = append(k.gitSources, gs)
	k.mu.Unlock()

	go startGitSourceSyncLoopForConnect(gs, ctx)

	return nil
}

var (
	newGitSourceForConnect           = NewGitSource
	initGitSourceForConnect          = (*GitSource).Init
	startGitSourceSyncLoopForConnect = (*GitSource).StartSyncLoop
)

// GetGitSourceStore returns the underlying FileStore for a git source by name.
func (k *KnowledgeAPI) GetGitSourceStore(name string) *FileStore {
	for _, gs := range k.gitSources {
		if gs.Config().Name == name && gs.Ready() {
			return gs.Store()
		}
	}
	return nil
}

// DisconnectGitSource removes a git source by URL and subpath.
func (k *KnowledgeAPI) DisconnectGitSource(url, subpath string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	found := false
	newSources := make([]*GitSource, 0, len(k.gitSources))
	for _, gs := range k.gitSources {
		if gs.Config().URL == url && gs.Config().Subpath == subpath {
			found = true
			continue
		}
		newSources = append(newSources, gs)
	}
	if !found {
		return fmt.Errorf("git source not found: %s (subpath: %s)", url, subpath)
	}
	k.gitSources = newSources
	k.logger.Info("git source disconnected", "url", url, "subpath", subpath)
	return nil
}

// GitSources returns info about all connected git sources.
func (k *KnowledgeAPI) GitSources() []GitSourceInfo {
	infos := make([]GitSourceInfo, len(k.gitSources))
	for i, gs := range k.gitSources {
		infos[i] = gs.Info()
	}
	return infos
}

// ImportDocument fetches/reads an external document, extracts facts into the
// vault, and registers graph relationships.
func (k *KnowledgeAPI) ImportDocument(ctx context.Context, config DocSourceConfig) (*DocMetadata, error) {
	k.mu.Lock()
	for _, ds := range k.docSources {
		if ds.slug == slugify(config.Name) {
			k.mu.Unlock()
			return nil, fmt.Errorf("document %q already imported", config.Name)
		}
	}
	k.mu.Unlock()

	vaultDir := k.docVaultDir()
	ds := NewDocumentSource(config, knowledgeBaseDir, vaultDir, k.graphStore, k.logger, k.context7APIKey)
	meta, err := ds.Import(ctx)
	if err != nil {
		return nil, err
	}

	k.mu.Lock()
	k.docSources = append(k.docSources, ds)
	k.mu.Unlock()

	k.triggerVaultReindex(vaultDir)
	return meta, nil
}

// ListDocuments returns metadata for all imported documents.
func (k *KnowledgeAPI) ListDocuments() []DocMetadata {
	k.mu.RLock()
	defer k.mu.RUnlock()

	metas := make([]DocMetadata, len(k.docSources))
	for i, ds := range k.docSources {
		metas[i] = ds.Metadata()
	}
	return metas
}

// GetDocument returns metadata for a specific imported document.
func (k *KnowledgeAPI) GetDocument(slug string) (*DocMetadata, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()

	for _, ds := range k.docSources {
		if ds.slug == slug {
			meta := ds.Metadata()
			return &meta, nil
		}
	}
	return nil, fmt.Errorf("document %q not found", slug)
}

// DeleteDocument removes an imported document and its extracted facts.
func (k *KnowledgeAPI) DeleteDocument(slug string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	for i, ds := range k.docSources {
		if ds.slug == slug {
			if err := ds.Delete(); err != nil {
				return err
			}
			k.docSources = append(k.docSources[:i], k.docSources[i+1:]...)
			k.triggerVaultReindex(k.docVaultDir())
			return nil
		}
	}
	return fmt.Errorf("document %q not found", slug)
}

// ReimportDocument re-fetches a document and replaces its extracted facts.
func (k *KnowledgeAPI) ReimportDocument(ctx context.Context, slug string) (*DocMetadata, error) {
	k.mu.RLock()
	var target *DocumentSource
	for _, ds := range k.docSources {
		if ds.slug == slug {
			target = ds
			break
		}
	}
	k.mu.RUnlock()

	if target == nil {
		return nil, fmt.Errorf("document %q not found", slug)
	}

	meta, err := target.Reimport(ctx)
	if err != nil {
		return nil, err
	}

	k.triggerVaultReindex(k.docVaultDir())
	return meta, nil
}

// SearchContext7Libraries searches Context7 for libraries matching the query.
func (k *KnowledgeAPI) SearchContext7Libraries(ctx context.Context, query string) ([]Context7SearchResult, error) {
	return SearchContext7(ctx, query, k.context7APIKey)
}

// reloadDocuments scans the documents directory for previously imported
// documents and restores them into memory from their metadata.json files.
func (k *KnowledgeAPI) reloadDocuments() {
	docsDir := filepath.Join(knowledgeBaseDir, docStorageDir)
	entries, err := os.ReadDir(docsDir)
	if err != nil {
		return
	}

	vaultDir := k.docVaultDir()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(docsDir, entry.Name())
		ds, err := LoadDocumentSource(dir, vaultDir, k.graphStore, k.logger, k.context7APIKey)
		if err != nil {
			k.logger.Warn("failed to reload document", "dir", entry.Name(), "error", err)
			continue
		}
		k.docSources = append(k.docSources, ds)
		k.logger.Info("reloaded document from disk", "slug", ds.slug, "facts", ds.metadata.FactCount)
	}
}

// autoImportContext7 imports any Context7 libraries from config that aren't
// already present as document sources. Runs at startup after reloadDocuments.
func (k *KnowledgeAPI) autoImportContext7() {
	if k.context7APIKey == "" {
		return
	}
	for _, item := range k.config.Context7.AutoImport {
		if item.ID == "" {
			continue
		}
		name := strings.TrimLeft(item.ID, "/")
		name = strings.ReplaceAll(name, "/", "-")
		slug := slugify(name)

		alreadyImported := false
		k.mu.RLock()
		for _, ds := range k.docSources {
			if ds.slug == slug {
				alreadyImported = true
				break
			}
		}
		k.mu.RUnlock()

		if alreadyImported {
			k.logger.Info("context7 auto-import: already present", "id", item.ID, "slug", slug)
			continue
		}

		config := DocSourceConfig{
			Name:       name,
			Context7ID: item.ID,
			Layer:      LayerCommunity,
		}
		ctx, cancel := context.WithTimeout(context.Background(), context7Timeout)
		meta, err := k.ImportDocument(ctx, config)
		cancel()
		if err != nil {
			k.logger.Warn("context7 auto-import failed", "id", item.ID, "error", err)
			continue
		}
		k.logger.Info("context7 auto-imported", "id", item.ID, "facts", meta.FactCount)
	}
}

// CleanupOrphanedDocFacts scans vault directories for doc-import fact files
// that are not claimed by any active document source and removes them.
func (k *KnowledgeAPI) CleanupOrphanedDocFacts() (int, error) {
	k.mu.RLock()
	owned := make(map[string]bool)
	for _, ds := range k.docSources {
		for _, slug := range ds.metadata.FactSlugs {
			owned[slug] = true
		}
	}
	k.mu.RUnlock()

	vaultDir := k.docVaultDir()
	entries, err := os.ReadDir(vaultDir)
	if err != nil {
		return 0, fmt.Errorf("reading vault dir: %w", err)
	}

	const docFactPrefix = "doc-"
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		slug := strings.TrimSuffix(name, ".md")
		if slug == name {
			continue
		}
		if !strings.HasPrefix(slug, docFactPrefix) {
			continue
		}
		if owned[slug] {
			continue
		}
		path := filepath.Join(vaultDir, name)
		if err := os.Remove(path); err != nil {
			k.logger.Warn("failed to remove orphaned doc fact", "path", path, "error", err)
			continue
		}
		removed++
	}

	if removed > 0 {
		k.triggerVaultReindex(vaultDir)
		k.logger.Info("cleaned up orphaned doc facts", "removed", removed)
	}
	return removed, nil
}

func (k *KnowledgeAPI) docVaultDir() string {
	for _, v := range k.vaults {
		if v.rootDir != "" {
			return v.rootDir
		}
	}
	return filepath.Join(knowledgeBaseDir, "documents-vault")
}

// ObsidianSyncRequest is the payload from the Obsidian Post Webhook plugin.
// The plugin flattens frontmatter into the top level alongside content/filename.
type ObsidianSyncRequest struct {
	Filename    string                 `json:"filename"`
	Filepath    string                 `json:"filepath"`
	Content     string                 `json:"content"`
	Frontmatter map[string]interface{} `json:"frontmatter"`
	Timestamp   json.Number            `json:"timestamp,omitempty"`
	CreatedAt   json.Number            `json:"createdAt,omitempty"`
	ModifiedAt  json.Number            `json:"modifiedAt,omitempty"`
	Overflow    map[string]interface{} `json:"-"`
}

// UnmarshalJSON captures the flat frontmatter fields the plugin sends at top level.
func (r *ObsidianSyncRequest) UnmarshalJSON(data []byte) error {
	type plain ObsidianSyncRequest
	if err := json.Unmarshal(data, (*plain)(r)); err != nil {
		return err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	known := map[string]bool{
		"filename": true, "filepath": true, "content": true,
		"frontmatter": true, "timestamp": true, "createdAt": true,
		"modifiedAt": true, "attachments": true, "renderedHtml": true,
		"path": true, "vault": true, "modified": true,
	}
	if r.Frontmatter == nil {
		r.Frontmatter = make(map[string]interface{})
	}
	for k, v := range raw {
		if !known[k] {
			r.Frontmatter[k] = v
		}
	}
	if v, ok := raw["vault"]; ok {
		if s, ok := v.(string); ok {
			r.Overflow = map[string]interface{}{"vault": s}
		}
	}
	if v, ok := raw["path"]; ok {
		if s, ok := v.(string); ok && r.Filepath == "" {
			r.Filepath = s
		}
	}
	return nil
}

// Vault returns the vault name from either the explicit vault field or overflow.
func (r *ObsidianSyncRequest) Vault() string {
	if r.Overflow != nil {
		if v, ok := r.Overflow["vault"]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// ObsidianSyncResult describes the outcome of an Obsidian sync operation.
type ObsidianSyncResult struct {
	Slug   string `json:"slug"`
	Action string `json:"action"` // "created" or "updated"
	Fact   Fact   `json:"fact"`
}

// defaultObsidianConfidence is used when frontmatter omits a confidence value.
const defaultObsidianConfidence = 0.7

// ObsidianSync accepts a Post Webhook payload and upserts it as a knowledge fact.
func (k *KnowledgeAPI) ObsidianSync(ctx context.Context, req ObsidianSyncRequest) (*ObsidianSyncResult, error) {
	slug := strings.TrimSuffix(req.Filename, ".md")
	slug = strings.TrimSuffix(slug, ".markdown")
	slug = strings.ReplaceAll(slug, "\\", "/")
	if strings.Contains(slug, "..") {
		return nil, fmt.Errorf("filename must not contain path traversal sequences")
	}

	// Extract metadata from frontmatter with defaults
	title := extractFrontmatterString(req.Frontmatter, "title", "")
	if title == "" {
		title = extractTitleFromContent(req.Content, slug)
	}

	tags := extractFrontmatterStringSlice(req.Frontmatter, "tags")
	factType := extractFrontmatterString(req.Frontmatter, "type", "pattern")
	layer := extractFrontmatterString(req.Frontmatter, "layer", "project")
	layer = filepath.Base(layer)
	if layer == "" || layer == "." || layer == ".." {
		layer = "project"
	}
	confidence := extractFrontmatterFloat(req.Frontmatter, "confidence", defaultObsidianConfidence)

	layerType := LayerType(layer)
	client := k.clientForLayer(layerType)

	var action string
	if client != nil {
		_, readErr := client.ReadPage(ctx, slug)
		action = "created"

		if readErr == nil {
			action = "updated"
			update := pageUpdateRequest{
				Title:      title,
				Body:       req.Content,
				Type:       factType,
				Confidence: confidence,
				Tags:       tags,
			}
			if err := client.UpdatePage(ctx, slug, update); err != nil {
				return nil, fmt.Errorf("updating fact %s: %w", slug, err)
			}
		} else {
			fact := ExtractedFact{
				Title:      title,
				Body:       req.Content,
				Type:       FactType(factType),
				Confidence: confidence,
				Tags:       tags,
				SourcePR:   "obsidian:" + req.Vault(),
				SourceDate: time.Now(),
			}
			if err := client.IngestFacts(ctx, []ExtractedFact{fact}); err != nil {
				return nil, fmt.Errorf("creating fact %s: %w", slug, err)
			}
		}
	} else {
		action = k.obsidianSyncToFile(slug, title, factType, layer, confidence, tags, req)
	}

	resultFact := Fact{
		Slug:       slug,
		Title:      title,
		Type:       FactType(factType),
		Body:       req.Content,
		Confidence: confidence,
		Tags:       tags,
		Layer:      layerType,
	}

	k.logger.Info("obsidian sync", "slug", slug, "action", action, "vault", req.Vault(), "layer", layer)

	return &ObsidianSyncResult{
		Slug:   slug,
		Action: action,
		Fact:   resultFact,
	}, nil
}

func (k *KnowledgeAPI) obsidianSyncToFile(slug, title, factType, layer string, confidence float64, tags []string, req ObsidianSyncRequest) string {
	dir := filepath.Join(localKnowledgeDir, layer)
	_ = os.MkdirAll(dir, 0o755)

	filename := slug + ".md"
	filename = strings.ReplaceAll(filename, "/", "_")
	path := filepath.Join(dir, filename)

	var buf strings.Builder
	buf.WriteString("---\n")
	fmt.Fprintf(&buf, "title: %s\n", title)
	fmt.Fprintf(&buf, "type: %s\n", factType)
	fmt.Fprintf(&buf, "layer: %s\n", layer)
	fmt.Fprintf(&buf, "confidence: %.2f\n", confidence)
	if len(tags) > 0 {
		fmt.Fprintf(&buf, "tags: [%s]\n", strings.Join(tags, ", "))
	}
	fmt.Fprintf(&buf, "source: obsidian:%s\n", req.Vault())
	fmt.Fprintf(&buf, "synced: %s\n", time.Now().UTC().Format(time.RFC3339))
	buf.WriteString("---\n\n")
	buf.WriteString(req.Content)

	_, err := os.Stat(path)
	action := "created"
	if err == nil {
		action = "updated"
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(buf.String()), 0o644); err != nil {
		k.logger.Warn("obsidian file sync failed", "path", path, "error", err)
	} else if err := os.Rename(tmpPath, path); err != nil {
		k.logger.Warn("obsidian file rename failed", "path", path, "error", err)
	}

	k.triggerVaultReindex(dir)
	return action
}

func (k *KnowledgeAPI) triggerVaultReindex(dir string) {
	for _, v := range k.vaults {
		if v.RootDir() == dir {
			v.reindex()
			return
		}
	}
	store, err := NewFileStore(dir, filepath.Base(dir), k.logger)
	if err == nil {
		k.vaults = append(k.vaults, store)
	}
}

// extractTitleFromContent extracts a title from the first # heading in markdown
// content, or falls back to the slug basename.
func extractTitleFromContent(content string, fallbackSlug string) string {
	lines := strings.SplitN(content, "\n", 10)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			return strings.TrimSpace(trimmed[2:])
		}
	}
	// Use the last path component of the slug as fallback
	parts := strings.Split(fallbackSlug, "/")
	return parts[len(parts)-1]
}

// extractFrontmatterString reads a string value from frontmatter, returning
// the default if missing or wrong type.
func extractFrontmatterString(fm map[string]interface{}, key string, defaultVal string) string {
	if fm == nil {
		return defaultVal
	}
	v, ok := fm[key]
	if !ok {
		return defaultVal
	}
	s, ok := v.(string)
	if !ok {
		return defaultVal
	}
	return s
}

// extractFrontmatterStringSlice reads a []string from frontmatter. Accepts
// both []interface{} (JSON arrays) and []string.
func extractFrontmatterStringSlice(fm map[string]interface{}, key string) []string {
	if fm == nil {
		return nil
	}
	v, ok := fm[key]
	if !ok {
		return nil
	}
	switch typed := v.(type) {
	case []interface{}:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	case []string:
		return typed
	default:
		return nil
	}
}

// extractFrontmatterFloat reads a float64 from frontmatter, returning the
// default if missing or wrong type.
func extractFrontmatterFloat(fm map[string]interface{}, key string, defaultVal float64) float64 {
	if fm == nil {
		return defaultVal
	}
	v, ok := fm[key]
	if !ok {
		return defaultVal
	}
	switch typed := v.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case json.Number:
		f, err := typed.Float64()
		if err != nil {
			return defaultVal
		}
		return f
	default:
		return defaultVal
	}
}

func (k *KnowledgeAPI) clientForLayer(layer LayerType) *Client {
	for _, lc := range k.layers {
		if lc.layerType == layer {
			return lc.client
		}
	}
	return nil
}

func parseJSONFacts(content string, facts *[]ExtractedFact) error {
	return json.Unmarshal([]byte(content), facts)
}

// CreateIdeationFact validates that the type is an ideation type and ingests
// it into the project KB layer.
func (k *KnowledgeAPI) CreateIdeationFact(ctx context.Context, req CreateFactRequest) error {
	ft := FactType(req.Type)
	if !ft.IsIdeation() {
		return fmt.Errorf("fact type %q is not an ideation type", req.Type)
	}
	if req.Layer == "" {
		req.Layer = string(LayerProject)
	}
	return k.CreateFact(ctx, req)
}

// ListIdeationFacts returns all facts from the project layer whose type is an
// ideation type (idea, vision, constitution, requirement, constraint,
// stakeholder, acceptance).
func (k *KnowledgeAPI) ListIdeationFacts(ctx context.Context) []Fact {
	all := k.LayerFacts(ctx, LayerProject, "")
	var ideation []Fact
	for _, f := range all {
		if f.Type.IsIdeation() {
			ideation = append(ideation, f)
		}
	}
	return ideation
}

// GetConstitution returns the single constitution fact from the project layer,
// or nil if none exists.
func (k *KnowledgeAPI) GetConstitution(ctx context.Context) *Fact {
	all := k.LayerFacts(ctx, LayerProject, string(FactConstitution))
	if len(all) > 0 {
		return &all[0]
	}
	return nil
}

func parseMarkdownFacts(content string) []ExtractedFact {
	var facts []ExtractedFact
	lines := strings.Split(content, "\n")

	var current *ExtractedFact
	var bodyLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") {
			if current != nil && current.Title != "" {
				current.Body = strings.TrimSpace(strings.Join(bodyLines, "\n"))
				facts = append(facts, *current)
			}
			title := strings.TrimLeft(trimmed, "# ")
			current = &ExtractedFact{
				Title:      title,
				Type:       FactPattern,
				Confidence: 0.6,
				SourcePR:   "import",
				SourceDate: time.Now(),
			}
			bodyLines = nil
			continue
		}

		if strings.HasPrefix(trimmed, "- **") && strings.Contains(trimmed, "**") {
			if current != nil && current.Title != "" {
				current.Body = strings.TrimSpace(strings.Join(bodyLines, "\n"))
				facts = append(facts, *current)
			}
			title := trimmed[4:]
			if idx := strings.Index(title, "**"); idx > 0 {
				body := strings.TrimSpace(title[idx+2:])
				title = title[:idx]
				current = &ExtractedFact{
					Title:      title,
					Body:       body,
					Type:       FactPattern,
					Confidence: 0.6,
					SourcePR:   "import",
					SourceDate: time.Now(),
				}
				bodyLines = nil
			}
			continue
		}

		if current != nil && trimmed != "" {
			bodyLines = append(bodyLines, trimmed)
		}
	}

	if current != nil && current.Title != "" {
		current.Body = strings.TrimSpace(strings.Join(bodyLines, "\n"))
		facts = append(facts, *current)
	}

	return facts
}
