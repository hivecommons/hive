package dashboard

// Tests for the dashboard knowledge graph and promote handlers
// (handleKnowledgeGraph, handleKnowledgePromote in api.go). These pin the
// previously-uncovered branches: graph building with root/depth query
// parameters, the promoter success path against fake wiki layer endpoints,
// and the vault → ObsidianSync fallback (both its success and error legs).
// Everything is hermetic: wiki layers are httptest servers, vaults and the
// graph store live in t.TempDir().

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// fakeWikiLayer serves configured pages and records writes.
type fakeWikiLayer struct {
	mu           sync.Mutex
	pages        map[string]map[string]any
	ingestStatus int
	ingested     []string
	mutations    []string
	srv          *httptest.Server
}

func newFakeWikiLayer(t *testing.T, pages map[string]map[string]any, ingestStatus int) *fakeWikiLayer {
	t.Helper()
	f := &fakeWikiLayer{pages: pages, ingestStatus: ingestStatus}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/ingest":
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.ingested = append(f.ingested, string(body))
			f.mu.Unlock()
			w.WriteHeader(f.ingestStatus)
		case r.Method == http.MethodGet && (r.URL.Path == "/api/search" || r.URL.Path == "/api/pages"):
			pages := make([]map[string]any, 0, len(f.pages))
			for _, page := range f.pages {
				pages = append(pages, page)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": pages})
		case r.Method == http.MethodGet && r.URL.Path == "/api/stats":
			_ = json.NewEncoder(w).Encode(map[string]int{"total_pages": len(f.pages)})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/pages/"):
			slug := strings.TrimPrefix(r.URL.Path, "/api/pages/")
			page, ok := f.pages[slug]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(page)
		case (r.Method == http.MethodPost || r.Method == http.MethodDelete) && strings.HasPrefix(r.URL.Path, "/api/pages/"):
			f.mu.Lock()
			f.mutations = append(f.mutations, r.Method+" "+r.URL.Path)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWikiLayer) ingestedPayloads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ingested...)
}

func (f *fakeWikiLayer) pageMutations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.mutations...)
}

// writeVaultFact writes a frontmatter fact file the FileStore indexer parses,
// so VaultFact/ReadPage can find it by slug (= filename without .md).
func writeVaultFact(t *testing.T, dir, slug, title string) {
	t.Helper()
	content := fmt.Sprintf(`---
title: %s
type: pattern
layer: project
confidence: 0.8
tags: [alpha, beta]
---

Body of %s.
`, title, slug)
	if err := os.WriteFile(filepath.Join(dir, slug+".md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write vault fact: %v", err)
	}
}

func newKnowledgeGraphServer(t *testing.T) (*Server, *knowledge.KnowledgeAPI, string) {
	t.Helper()
	srv := newMinimalServer(t)
	api := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	srv.deps.Knowledge = api

	vaultDir := t.TempDir()
	writeVaultFact(t, vaultDir, "fact-a", "Fact A")
	writeVaultFact(t, vaultDir, "fact-b", "Fact B")
	if err := api.ConnectVault(vaultDir, "testvault"); err != nil {
		t.Fatalf("connect vault: %v", err)
	}

	gs, err := knowledge.NewGraphStore(filepath.Join(t.TempDir(), "graph.db"), slog.Default())
	if err != nil {
		t.Fatalf("new graph store: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	if err := gs.AddTriple(knowledge.Triple{Subject: "fact-a", Predicate: knowledge.PredicateRelatedTo, Object: "fact-b"}); err != nil {
		t.Fatalf("add triple: %v", err)
	}
	api.SetGraphStore(gs)
	return srv, api, vaultDir
}

func getGraph(t *testing.T, srv *Server, url string) knowledge.GraphData {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeGraph(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: expected 200, got %d: %s", url, w.Code, w.Body.String())
	}
	var data knowledge.GraphData
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatalf("decode graph data: %v", err)
	}
	return data
}

// --- handleKnowledgeGraph ---

func TestHandleKnowledgeGraph_NilDeps(t *testing.T) {
	srv := NewServer(0, slog.Default())
	data := getGraph(t, srv, "/api/knowledge/graph")
	if len(data.Nodes) != 0 || len(data.Edges) != 0 {
		t.Errorf("expected empty graph with nil deps, got %d nodes / %d edges", len(data.Nodes), len(data.Edges))
	}
}

func TestHandleKnowledgeGraph_NilGraphStore(t *testing.T) {
	srv := newMinimalServer(t)
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	data := getGraph(t, srv, "/api/knowledge/graph")
	if len(data.Nodes) != 0 || len(data.Edges) != 0 {
		t.Errorf("expected empty graph with nil graph store, got %d nodes / %d edges", len(data.Nodes), len(data.Edges))
	}
}

func TestHandleKnowledgeGraph_FullGraphWithVaultTitles(t *testing.T) {
	srv, _, _ := newKnowledgeGraphServer(t)
	data := getGraph(t, srv, "/api/knowledge/graph")

	if len(data.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(data.Edges))
	}
	e := data.Edges[0]
	if e.From != "fact-a" || e.To != "fact-b" || e.Predicate != knowledge.PredicateRelatedTo {
		t.Errorf("unexpected edge: %+v", e)
	}
	if len(data.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(data.Nodes))
	}
	titles := map[string]string{}
	for _, n := range data.Nodes {
		titles[n.Slug] = n.Title
	}
	// Titles must be enriched from the connected vault, not echo the slug.
	if titles["fact-a"] != "Fact A" || titles["fact-b"] != "Fact B" {
		t.Errorf("expected vault-enriched titles, got %v", titles)
	}
}

func TestHandleKnowledgeGraph_RootAndDepthParams(t *testing.T) {
	srv, _, _ := newKnowledgeGraphServer(t)

	data := getGraph(t, srv, "/api/knowledge/graph?root=fact-a&depth=1")
	if len(data.Edges) != 1 || len(data.Nodes) != 2 {
		t.Errorf("root=fact-a depth=1: expected 1 edge / 2 nodes, got %d / %d", len(data.Edges), len(data.Nodes))
	}

	// A root with no triples yields an empty (but non-null) graph.
	data = getGraph(t, srv, "/api/knowledge/graph?root=fact-zzz&depth=1")
	if data.Nodes == nil || data.Edges == nil {
		t.Error("expected non-null empty nodes/edges for unknown root")
	}
	if len(data.Edges) != 0 {
		t.Errorf("expected 0 edges for unknown root, got %d", len(data.Edges))
	}
}

func TestHandleKnowledgeGraph_InvalidDepthFallsBackToDefault(t *testing.T) {
	srv, _, _ := newKnowledgeGraphServer(t)
	// Non-numeric and non-positive depths are ignored (default depth 2 applies),
	// so the fact-a → fact-b edge is still reachable from fact-a.
	for _, q := range []string{"depth=abc", "depth=0", "depth=-3"} {
		data := getGraph(t, srv, "/api/knowledge/graph?root=fact-a&"+q)
		found := false
		for _, e := range data.Edges {
			if e.From == "fact-a" && e.To == "fact-b" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: expected fact-a→fact-b edge via default depth, got %+v", q, data.Edges)
		}
	}
}

// --- handleKnowledgePromote ---

func postPromote(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/knowledge/promote", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgePromote(w, req)
	return w
}

func TestHandleKnowledgePromote_SuccessViaLayerEndpoints(t *testing.T) {
	src := newFakeWikiLayer(t, map[string]map[string]any{
		"my-fact": {
			"slug": "my-fact", "title": "My Fact", "body": "the body",
			"type": "pattern", "confidence": 0.9, "tags": []string{"a"},
		},
	}, http.StatusOK)
	dst := newFakeWikiLayer(t, nil, http.StatusOK)

	srv := newMinimalServer(t)
	layers := []knowledge.LayerConfig{
		{Type: knowledge.LayerProject, URL: src.srv.URL},
		{Type: knowledge.LayerOrg, URL: dst.srv.URL},
	}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, knowledge.KnowledgeConfig{Enabled: true}, slog.Default())

	w := postPromote(t, srv, `{"slug":"my-fact","from_layer":"project","to_layer":"org"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result knowledge.PromoteResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, got error %q", result.Error)
	}
	if result.Slug != "my-fact" || result.FromLayer != "project" || result.ToLayer != "org" {
		t.Errorf("unexpected result: %+v", result)
	}

	payloads := dst.ingestedPayloads()
	if len(payloads) != 1 {
		t.Fatalf("expected 1 ingest to target layer, got %d", len(payloads))
	}
	// Empty promoter defaults to "dashboard" and rides the provenance line.
	if !strings.Contains(payloads[0], "promoted from project by dashboard") {
		t.Errorf("expected provenance with default promoter in ingest payload, got %s", payloads[0])
	}
}

func TestHandleKnowledgePromote_VaultFallbackSyncsToLayer(t *testing.T) {
	// No endpoint for from_layer → the promoter fails; the fact exists in a
	// connected vault, so the handler falls back to ObsidianSync into org.
	dst := newFakeWikiLayer(t, nil, http.StatusOK)

	srv := newMinimalServer(t)
	layers := []knowledge.LayerConfig{{Type: knowledge.LayerOrg, URL: dst.srv.URL}}
	api := knowledge.NewKnowledgeAPI(layers, knowledge.KnowledgeConfig{Enabled: true}, slog.Default())
	srv.deps.Knowledge = api

	personal := t.TempDir()
	if err := os.WriteFile(filepath.Join(personal, "vault-fact.md"), []byte("---\ntitle: Personal Copy\nlayer: personal\n---\n\nPersonal body.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := api.ConnectVault(personal, "personal-notes"); err != nil {
		t.Fatal(err)
	}
	vaultDir := t.TempDir()
	writeVaultFact(t, vaultDir, "vault-fact", "Vault Fact")
	if err := api.ConnectVault(vaultDir, "testvault"); err != nil {
		t.Fatalf("connect vault: %v", err)
	}

	w := postPromote(t, srv, `{"slug":"vault-fact","from_layer":"project","to_layer":"org"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from vault fallback, got %d: %s", w.Code, w.Body.String())
	}
	var result knowledge.PromoteResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.Success {
		t.Errorf("expected fallback success, got error %q", result.Error)
	}
	if result.Slug != "vault-fact" || result.FromLayer != "project" || result.ToLayer != "org" {
		t.Errorf("unexpected result: %+v", result)
	}
	if payloads := dst.ingestedPayloads(); len(payloads) != 1 || !strings.Contains(payloads[0], "Vault Fact") {
		t.Errorf("expected the Project vault fact to be ingested into org, got %q", payloads)
	}
}

func TestHandleKnowledgePromote_ConfiguredSourceFailureDoesNotUseVault(t *testing.T) {
	src := newFakeWikiLayer(t, nil, http.StatusOK)
	dst := newFakeWikiLayer(t, nil, http.StatusOK)
	srv := newMinimalServer(t)
	api := knowledge.NewKnowledgeAPI([]knowledge.LayerConfig{
		{Type: knowledge.LayerProject, URL: src.srv.URL},
		{Type: knowledge.LayerOrg, URL: dst.srv.URL},
	}, knowledge.KnowledgeConfig{Enabled: true}, slog.Default())
	srv.deps.Knowledge = api
	dir := t.TempDir()
	writeVaultFact(t, dir, "shared-fact", "Unrelated Vault")
	if err := api.ConnectVault(dir, "vault"); err != nil {
		t.Fatal(err)
	}

	rec := postPromote(t, srv, `{"slug":"shared-fact","from_layer":"project","to_layer":"org"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "reading fact from project") {
		t.Fatalf("expected project read failure, got %d: %s", rec.Code, rec.Body)
	}
	if payloads := dst.ingestedPayloads(); len(payloads) != 0 {
		t.Fatalf("unexpected fallback promotion payloads: %q", payloads)
	}
}

func TestHandleKnowledgeFact_DuplicateVaultNamesHaveExactRoutes(t *testing.T) {
	srv := newMinimalServer(t)
	api := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true}, slog.Default())
	srv.deps.Knowledge = api
	first := t.TempDir()
	second := t.TempDir()
	writeVaultFact(t, first, "shared-fact", "First vault fact")
	writeVaultFact(t, second, "shared-fact", "Second vault fact")
	for _, dir := range []string{first, second} {
		if err := api.ConnectVault(dir, "Same Name"); err != nil {
			t.Fatal(err)
		}
	}

	read := func(selector string, want knowledge.Fact) {
		t.Helper()
		rec := doGet(srv, "/api/knowledge/"+url.PathEscape(selector)+"/shared-fact")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
		var fact knowledge.Fact
		if err := json.NewDecoder(rec.Body).Decode(&fact); err != nil {
			t.Fatal(err)
		}
		if fact.Title != want.Title || fact.SourceLayer != want.SourceLayer {
			t.Fatalf("wrong fact: %+v", fact)
		}
	}
	facts := api.SearchAllWithVaults(srv.deps.Ctx, "", "", 0)
	if len(facts) != 2 || facts[0].SourceLayer == facts[1].SourceLayer {
		t.Fatalf("vault rows lack distinct identities: %+v", facts)
	}
	for _, fact := range facts {
		source, ok := strings.CutPrefix(string(fact.SourceLayer), "vault:")
		if !ok || !strings.HasPrefix(source, "sha256:") {
			t.Fatalf("expected opaque vault ID: %+v", fact)
		}
		read(string(fact.SourceLayer), fact)
		filtered := api.VaultFacts(source)
		if len(filtered) != 1 || filtered[0].SourceLayer != fact.SourceLayer || filtered[0].Title != fact.Title {
			t.Fatalf("wrong vault listing: %+v", filtered)
		}
	}
	read("vault:Same Name", facts[0])
	if err := api.DisconnectVault(first); err != nil {
		t.Fatal(err)
	}
	read(string(facts[1].SourceLayer), facts[1])
	if rec := doGet(srv, "/api/knowledge/"+url.PathEscape(string(facts[0].SourceLayer))+"/shared-fact"); rec.Code != http.StatusNotFound {
		t.Fatalf("removed vault route = %d: %s", rec.Code, rec.Body)
	}
}

func TestHandleKnowledgePromote_UsesRequestedVault(t *testing.T) {
	dst := newFakeWikiLayer(t, nil, http.StatusOK)
	personal := newFakeWikiLayer(t, nil, http.StatusOK)
	srv := newMinimalServer(t)
	api := knowledge.NewKnowledgeAPI([]knowledge.LayerConfig{
		{Type: knowledge.LayerOrg, URL: dst.srv.URL},
		{Type: knowledge.LayerPersonal, URL: personal.srv.URL},
	}, knowledge.KnowledgeConfig{Enabled: true}, slog.Default())
	srv.deps.Knowledge = api
	for _, name := range []string{"Unrelated Vault", "Selected Vault"} {
		dir := t.TempDir()
		writeVaultFact(t, dir, "shared-fact", name)
		if err := api.ConnectVault(dir, name); err != nil {
			t.Fatal(err)
		}
	}

	source := api.VaultFacts("Selected Vault")[0].SourceLayer
	rec := postPromote(t, srv, fmt.Sprintf(`{"slug":"shared-fact","from_layer":%q,"to_layer":"org"}`, source))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	payloads := dst.ingestedPayloads()
	if len(payloads) != 1 || !strings.Contains(payloads[0], "Selected Vault") || strings.Contains(payloads[0], "Unrelated Vault") {
		t.Fatalf("wrong promotion payloads: %q", payloads)
	}
	rec = postPromote(t, srv, fmt.Sprintf(`{"slug":"shared-fact","from_layer":%q,"to_layer":"personal"}`, source))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "promotion only flows upward") {
		t.Fatalf("downward promotion status = %d: %s", rec.Code, rec.Body)
	}
	if len(personal.ingestedPayloads()) != 0 || len(personal.pageMutations()) != 0 {
		t.Fatal("downward promotion wrote to the Personal wiki")
	}
}

func TestHandleKnowledgeFact_UsesRequestedSource(t *testing.T) {
	wiki := func(title string) *fakeWikiLayer {
		return newFakeWikiLayer(t, map[string]map[string]any{
			"shared": {"slug": "shared", "title": title, "body": title},
		}, http.StatusOK)
	}
	srv := newMinimalServer(t)
	api := knowledge.NewKnowledgeAPI([]knowledge.LayerConfig{
		{Type: knowledge.LayerOrg, URL: wiki("Org").srv.URL},
		{Type: knowledge.LayerProject, URL: wiki("Project").srv.URL},
	}, knowledge.KnowledgeConfig{Enabled: true}, slog.Default())
	srv.deps.Knowledge = api
	for _, name := range []string{"my vault", "project"} {
		dir := t.TempDir()
		writeVaultFact(t, dir, "shared", name)
		writeVaultFact(t, dir, "vault-only", name)
		if err := api.ConnectVault(dir, name); err != nil {
			t.Fatal(err)
		}
	}
	sources := make(map[string]knowledge.LayerType)
	for _, f := range api.SearchAllWithVaults(srv.deps.Ctx, "", "", 0) {
		if f.Slug == "shared" {
			sources[f.Title] = f.SourceLayer
		}
	}

	for _, tc := range []struct {
		path, title, layer string
	}{
		{"project/shared", "Project", "project"},
		{"org/shared", "Org", "org"},
		{"all/shared", "Org", "org"},
		{"vault:project/shared", "project", "project"},
		{"my%20vault/shared", "my vault", "project"},
		{"vault:my%20vault/shared", "my vault", "project"},
		{"project/vault-only", "", ""},
		{"community/shared", "", ""},
		{"vault:missing/shared", "", ""},
		{"git_source:missing/shared", "", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := doGet(srv, "/api/knowledge/"+tc.path)
			if tc.title == "" {
				if rec.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
			}
			var fact knowledge.Fact
			if err := json.NewDecoder(rec.Body).Decode(&fact); err != nil {
				t.Fatal(err)
			}
			if fact.Title != tc.title || string(fact.Layer) != tc.layer || fact.SourceLayer == "" || fact.SourceLayer != sources[tc.title] {
				t.Fatalf("wrong fact: %+v", fact)
			}
		})
	}

	var vaultSource knowledge.LayerType
	for _, layer := range api.Stats(srv.deps.Ctx)["layers"].([]map[string]interface{}) {
		if layer["type"] == "project" {
			vaultSource, _ = layer["source_layer"].(knowledge.LayerType)
		}
	}
	selector, qualified := strings.CutPrefix(string(vaultSource), "vault:")
	if !qualified || vaultSource != sources["project"] {
		t.Fatalf("project vault selector = %q, want %q", vaultSource, sources["project"])
	}
	rec := doGet(srv, "/api/knowledge/vaults/"+url.PathEscape(selector)+"/facts")
	var listed []knowledge.Fact
	if rec.Code != http.StatusOK || json.NewDecoder(rec.Body).Decode(&listed) != nil || len(listed) != 2 {
		t.Fatalf("project vault listing = %d: %s", rec.Code, rec.Body)
	}
	for _, f := range listed {
		if f.Title != "project" || f.SourceLayer != vaultSource {
			t.Fatalf("listed fact was not from the project vault: %+v", f)
		}
	}
}

func TestKnowledgeActionsUseSelectedWikiEndpoint(t *testing.T) {
	wiki := func(title string) *fakeWikiLayer {
		return newFakeWikiLayer(t, map[string]map[string]any{
			"shared": {"slug": "shared", "title": title, "body": title},
		}, http.StatusOK)
	}
	first, second := wiki("First wiki"), wiki("Second wiki")
	target := newFakeWikiLayer(t, nil, http.StatusOK)
	srv := newMinimalServer(t)
	api := knowledge.NewKnowledgeAPI([]knowledge.LayerConfig{
		{Type: knowledge.LayerProject, URL: first.srv.URL},
		{Type: knowledge.LayerOrg, URL: target.srv.URL},
	}, knowledge.KnowledgeConfig{Enabled: true}, slog.Default())
	srv.deps.Knowledge = api
	if err := api.AddSubscription(knowledge.Subscription{Layer: knowledge.LayerProject, URL: second.srv.URL}); err != nil {
		t.Fatal(err)
	}
	facts := api.SearchAllWithVaults(srv.deps.Ctx, "", "", 0)
	if len(facts) != 2 || facts[0].SourceLayer == facts[1].SourceLayer || facts[1].Title != "Second wiki" {
		t.Fatalf("wiki search identities = %+v", facts)
	}
	source := facts[1].SourceLayer
	layerPath := "/api/knowledge/" + url.PathEscape(string(source))
	list := decodeJSON(t, doGet(srv, layerPath))
	if list["count"] != float64(1) {
		t.Fatalf("selected wiki listing = %v", list)
	}
	row := list["facts"].([]interface{})[0].(map[string]interface{})
	if row["title"] != "Second wiki" || row["source_layer"] != string(source) {
		t.Fatalf("selected wiki row = %v", row)
	}
	factPath := layerPath + "/shared"
	detail := decodeJSON(t, doGet(srv, factPath))
	if detail["title"] != "Second wiki" || detail["source_layer"] != string(source) {
		t.Fatalf("selected wiki detail = %v", detail)
	}
	if rec := doPut(srv, factPath, map[string]string{"title": "Edited"}); rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d: %s", rec.Code, rec.Body)
	}
	if rec := postPromote(t, srv, fmt.Sprintf(`{"slug":"shared","from_layer":%q,"to_layer":"org"}`, source)); rec.Code != http.StatusOK {
		t.Fatalf("promote status = %d: %s", rec.Code, rec.Body)
	}
	if payloads := target.ingestedPayloads(); len(payloads) != 1 || !strings.Contains(payloads[0], "Second wiki") {
		t.Fatalf("promoted wrong wiki fact: %q", payloads)
	}
	if rec := doDelete(srv, factPath, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body)
	}
	if got := strings.Join(second.pageMutations(), ", "); got != "POST /api/pages/shared, DELETE /api/pages/shared" || len(first.pageMutations()) != 0 {
		t.Fatalf("wiki writes = %q on first and %q on second", first.pageMutations(), got)
	}
	if err := api.RemoveSubscription(second.srv.URL); err != nil {
		t.Fatal(err)
	}
	if rec := doGet(srv, factPath); rec.Code != http.StatusNotFound {
		t.Fatalf("removed wiki route = %d: %s", rec.Code, rec.Body)
	}
}

func TestHandleKnowledgePromote_VaultFallbackSyncError(t *testing.T) {
	// Fallback path where ObsidianSync itself fails (target layer rejects the
	// ingest) must surface as a 500, not a silent success.
	dst := newFakeWikiLayer(t, nil, http.StatusInternalServerError)

	srv := newMinimalServer(t)
	layers := []knowledge.LayerConfig{{Type: knowledge.LayerOrg, URL: dst.srv.URL}}
	api := knowledge.NewKnowledgeAPI(layers, knowledge.KnowledgeConfig{Enabled: true}, slog.Default())
	srv.deps.Knowledge = api

	vaultDir := t.TempDir()
	writeVaultFact(t, vaultDir, "vault-fact", "Vault Fact")
	if err := api.ConnectVault(vaultDir, "testvault"); err != nil {
		t.Fatalf("connect vault: %v", err)
	}

	w := postPromote(t, srv, `{"slug":"vault-fact","from_layer":"project","to_layer":"org"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 when fallback sync fails, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleKnowledgePromote_FactNowhereReturnsPromoteError(t *testing.T) {
	// Promote fails and the fact is not in any vault: the original promoter
	// error comes back as a 400.
	srv := newMinimalServer(t)
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true}, slog.Default())

	w := postPromote(t, srv, `{"slug":"ghost","from_layer":"project","to_layer":"org"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unpromotable missing fact, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no configured endpoint") {
		t.Errorf("expected promoter error in body, got %s", w.Body.String())
	}
}
