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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kubestellar/hive/pkg/knowledge"
)

// fakeWikiLayer is a minimal llm-wiki-compatible endpoint: GET
// /api/pages/<slug> serves the configured pages, POST /api/ingest records the
// payload and answers with ingestStatus.
type fakeWikiLayer struct {
	mu           sync.Mutex
	pages        map[string]map[string]any
	ingestStatus int
	ingested     []string
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
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/pages/"):
			slug := strings.TrimPrefix(r.URL.Path, "/api/pages/")
			page, ok := f.pages[slug]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(page)
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
	if payloads := dst.ingestedPayloads(); len(payloads) != 1 {
		t.Errorf("expected the vault fact to be ingested into org, got %d ingests", len(payloads))
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
