package wavefront_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/worksource"
	"github.com/hivecommons/hive/pkg/worksource/wavefront"
)

const (
	fixtureDir   = "testdata/wavefront-fixture"
	fixtureGraph = fixtureDir + "/graph.json"
	fixtureRev8  = fixtureDir + "/graph-rev-8.json"
	fixtureName  = "crustify-fixture"
	fixtureRev   = "rev-7"
	testRepo     = "acme/crust"
)

var fixedNow = time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)

func loadFixture(t *testing.T, path string) wavefront.Graph {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	g, err := wavefront.ParseGraph(data)
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}
	return g
}

func newFileSource(t *testing.T) *wavefront.Source {
	t.Helper()
	src, err := wavefront.New(wavefront.Options{
		Repo: testRepo, Path: fixtureGraph, ReceiptsDir: t.TempDir(),
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return src
}

func externalIDs(items []worksource.Issue) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ExternalID)
	}
	return out
}

func mustComplete(t *testing.T, src *wavefront.Source, node string) wavefront.Receipt {
	t.Helper()
	r, err := src.Complete(context.Background(), wavefront.ExternalID(fixtureName, node), fixtureRev, nil, fixedNow.Add(-time.Minute))
	if err != nil {
		t.Fatalf("Complete(%s): %v", node, err)
	}
	return r
}

// TestFixtureListsReadyAndWithholdsBlocked proves the fixture's initial frontier:
// the two done nodes are never listed, the four nodes whose only dependency is
// done are listed, and the explicitly blocked node plus its dependent are
// withheld, as is everything deeper in the DAG.
func TestFixtureListsReadyAndWithholdsBlocked(t *testing.T) {
	src := newFileSource(t)
	items, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	want := []string{
		"crustify-fixture:parse-ast", "crustify-fixture:wrap-alloc",
		"crustify-fixture:wrap-io", "crustify-fixture:wrap-logging",
	}
	if got := externalIDs(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("ready items = %v, want %v", got, want)
	}
	item := items[0]
	if item.SourceType != worksource.SourceTypeRun || item.Number != 0 || item.Repo != testRepo ||
		item.Stage != worksource.RunStageImplement || item.Title != "implement: Port the AST parser to Rust" ||
		item.State != "open" || item.Author != wavefront.Engine {
		t.Fatalf("item shape = %+v", item)
	}
	for _, l := range []string{"hive-run", "stage/implement", wavefront.LabelWavefront, "graph-rev/rev-7", "wavefront/port"} {
		if !contains(item.Labels, l) {
			t.Fatalf("labels %v lack %q", item.Labels, l)
		}
	}
	if rev, ok := wavefront.RevisionFromLabels(item.Labels); !ok || rev != fixtureRev {
		t.Fatalf("RevisionFromLabels = %q, %v", rev, ok)
	}
	if len(item.DependsOn) != 1 || !item.DependsOn[0].Resolved ||
		item.DependsOn[0].Ref.Key() != testRepo+"!crustify-fixture:build-graph" {
		t.Fatalf("dependency edge = %+v", item.DependsOn)
	}
	if src.SourceType() != wavefront.SourceType {
		t.Fatalf("SourceType = %q", src.SourceType())
	}
}

func TestBurndownMatchesRunKeyAndReceipts(t *testing.T) {
	src := newFileSource(t)
	key := testRepo + "!" + wavefront.ExternalID(fixtureName, "parse-ast")
	bd, ok, err := src.Burndown(context.Background(), key)
	if err != nil {
		t.Fatalf("Burndown: %v", err)
	}
	if !ok {
		t.Fatal("Burndown did not match fixture key")
	}
	if bd.Scope != 24 || bd.Satisfied != 2 || bd.Remaining != 22 || bd.Unknown != 0 {
		t.Fatalf("initial burndown = %+v", bd)
	}
	mustComplete(t, src, "parse-ast")
	bd, ok, err = src.Burndown(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("Burndown after receipt = %+v, %v, %v", bd, ok, err)
	}
	if bd.Satisfied != 3 || bd.Remaining != 21 {
		t.Fatalf("receipt burndown = %+v", bd)
	}
	if _, ok, err := src.Burndown(context.Background(), testRepo+"!other:parse-ast"); err != nil || ok {
		t.Fatalf("non-matching graph ok=%v err=%v", ok, err)
	}
}

func TestMarkUnknownIsTerminalButNotSatisfied(t *testing.T) {
	src := newFileSource(t)
	if _, err := src.MarkUnknown(context.Background(), wavefront.ExternalID(fixtureName, "parse-ast"), "lease expired", fixedNow.Add(-time.Minute)); err != nil {
		t.Fatalf("MarkUnknown: %v", err)
	}
	if src.Receipts().Completed(fixtureName, "parse-ast", fixtureRev) {
		t.Fatal("unknown receipt must not satisfy dependencies")
	}
	if !src.Receipts().Unknown(fixtureName, "parse-ast", fixtureRev) {
		t.Fatal("unknown receipt not recorded")
	}
	bd, ok, err := src.Burndown(context.Background(), testRepo+"!"+wavefront.ExternalID(fixtureName, "parse-ast"))
	if err != nil || !ok {
		t.Fatalf("Burndown: %+v ok=%v err=%v", bd, ok, err)
	}
	if bd.Unknown != 1 || bd.Satisfied != 2 || bd.Remaining != 21 {
		t.Fatalf("burndown with unknown = %+v", bd)
	}
	_, ready, err := src.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, n := range ready {
		if n.ID == "parse-ast" {
			t.Fatalf("unknown node was re-listed: %+v", ready)
		}
	}
}

// TestDiamondCompletionUnblocksDependent walks the diamond parse-ast ->
// {lower-types, lower-exprs} -> emit-ir: the join node stays withheld until
// BOTH arms carry a receipt, completed nodes are never re-listed, a retry of a
// completion is idempotent, and receipts survive a store reopen.
func TestDiamondCompletionUnblocksDependent(t *testing.T) {
	dir := t.TempDir()
	src, err := wavefront.New(wavefront.Options{Repo: testRepo, Path: fixtureGraph, ReceiptsDir: dir, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := src.Complete(ctx, wavefront.ExternalID(fixtureName, "emit-ir"), fixtureRev, nil, fixedNow); !errors.Is(err, wavefront.ErrNotReady) {
		t.Fatalf("completing a blocked join node = %v, want ErrNotReady", err)
	}
	first := mustComplete(t, src, "parse-ast")
	again := mustComplete(t, src, "parse-ast")
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("retrying a completion minted a different receipt:\n%+v\n%+v", first, again)
	}
	items, err := src.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	ids := externalIDs(items)
	for _, want := range []string{"crustify-fixture:lower-types", "crustify-fixture:lower-exprs"} {
		if !contains(ids, want) {
			t.Fatalf("after parse-ast, %s should be ready: %v", want, ids)
		}
	}
	for _, absent := range []string{"crustify-fixture:parse-ast", "crustify-fixture:emit-ir"} {
		if contains(ids, absent) {
			t.Fatalf("%s must not be listed: %v", absent, ids)
		}
	}
	mustComplete(t, src, "lower-types")
	items, _ = src.ListIssues(ctx)
	if contains(externalIDs(items), "crustify-fixture:emit-ir") {
		t.Fatalf("emit-ir listed with one arm still open: %v", externalIDs(items))
	}
	mustComplete(t, src, "lower-exprs")
	items, _ = src.ListIssues(ctx)
	if !contains(externalIDs(items), "crustify-fixture:emit-ir") {
		t.Fatalf("emit-ir should be ready once both arms hold receipts: %v", externalIDs(items))
	}

	// Receipt shape: schema-valid #8295 stage receipt bound to this revision.
	if first.Graph != fixtureName || first.Node != "parse-ast" || first.Revision != fixtureRev || first.Digest == "" {
		t.Fatalf("receipt identity = %+v", first)
	}
	sr := first.Receipt
	if sr.WorkKey != testRepo+"!crustify-fixture:parse-ast" || sr.AssignmentID != "crustify-fixture:parse-ast" ||
		sr.Stage != worksource.RunStageImplement || sr.ContractRevision != wavefront.ContractRevision ||
		sr.InputRevision != "wavefront-graph@"+first.Digest || sr.ResultClass != outputschema.ReceiptResultCompleted ||
		sr.Engine == nil || sr.Engine.Name != wavefront.Engine || len(sr.Artifacts) != 1 ||
		sr.Artifacts[0].Path != "wavefront/crustify-fixture/parse-ast" || sr.StartedAt == sr.EndedAt {
		t.Fatalf("stage receipt = %+v", sr)
	}
	report, err := json.Marshal(outputschema.AgentReport{
		Lane: wavefront.Engine, Kind: outputschema.KindStageReceipt, Summary: "node completed",
		Findings: []outputschema.Finding{}, PRsOpened: []outputschema.PROpened{}, BeadsFiled: []outputschema.BeadFiled{},
		Receipt: &sr,
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if _, err := outputschema.Validate(report); err != nil {
		t.Fatalf("receipt does not validate against the stage-receipt schema: %v", err)
	}

	// Durable: the file exists and a fresh store reloads it.
	if _, err := os.Stat(filepath.Join(dir, fixtureName, "parse-ast.json")); err != nil {
		t.Fatalf("receipt file: %v", err)
	}
	reopened, err := wavefront.NewReceiptStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reopened.Completed(fixtureName, "parse-ast", fixtureRev) || reopened.Completed(fixtureName, "parse-ast", "rev-8") {
		t.Fatalf("reloaded receipt must count at rev-7 only")
	}
	if !src.Receipts().Completed(fixtureName, "lower-exprs", fixtureRev) {
		t.Fatalf("Receipts() should expose the live store")
	}
}

// TestStaleRevisionRefused is the stale-plan guard: once the graph moves to a
// new revision, an item minted at the old one is refused (never re-derived),
// and a receipt recorded under the old revision no longer satisfies anything.
func TestStaleRevisionRefused(t *testing.T) {
	rev7 := loadFixture(t, fixtureGraph)
	rev8 := loadFixture(t, fixtureRev8)
	current := &rev7
	src, err := wavefront.New(wavefront.Options{
		Repo:   testRepo,
		Loader: func(context.Context) (wavefront.Graph, error) { return *current, nil },
		Now:    func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	items, err := src.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	minted := items[0]
	mintedRev, _ := wavefront.RevisionFromLabels(minted.Labels)
	mustComplete(t, src, "parse-ast")

	current = &rev8
	_, err = src.Complete(ctx, minted.ExternalID, mintedRev, nil, fixedNow)
	if !errors.Is(err, wavefront.ErrStaleRevision) {
		t.Fatalf("stale item completion = %v, want ErrStaleRevision", err)
	}
	if _, _, err := src.Verify(ctx, minted.ExternalID, mintedRev); !errors.Is(err, wavefront.ErrStaleRevision) {
		t.Fatalf("Verify stale = %v", err)
	}
	items, err = src.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues rev-8: %v", err)
	}
	ids := externalIDs(items)
	if !contains(ids, "crustify-fixture:parse-ast") || contains(ids, "crustify-fixture:lower-types") {
		t.Fatalf("rev-7 receipt must not satisfy rev-8: %v", ids)
	}

	// Unknown identities are refused too, distinctly from staleness.
	for _, bad := range []string{"other-graph:parse-ast", "crustify-fixture:no-such-node", "malformed", ":x", "x:"} {
		if _, _, err := src.Verify(ctx, bad, rev8.Revision); !errors.Is(err, wavefront.ErrUnknownNode) {
			t.Fatalf("Verify(%q) = %v, want ErrUnknownNode", bad, err)
		}
	}
	if _, err := src.Complete(ctx, "crustify-fixture:build-graph", rev8.Revision, nil, fixedNow); !errors.Is(err, wavefront.ErrNotReady) {
		t.Fatalf("completing a node Wavefront already marked done = %v, want ErrNotReady", err)
	}
}

// TestKeyRoundTrip proves every listed node has a canonical worksource key that
// survives ParseKey unchanged and is never mistaken for a GitHub issue.
func TestKeyRoundTrip(t *testing.T) {
	src := newFileSource(t)
	items, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	for _, item := range items {
		ref := worksource.RefFromIssue(item)
		key := ref.Key()
		if key != testRepo+"!"+item.ExternalID {
			t.Fatalf("key = %q", key)
		}
		parsed, ok := worksource.ParseKey(key)
		if !ok || parsed.Key() != key || parsed.IsGitHubIssue() || parsed.Display() != item.ExternalID {
			t.Fatalf("round trip of %q = %+v, ok=%v", key, parsed, ok)
		}
		graph, node, ok := wavefront.SplitExternalID(parsed.ExternalID)
		if !ok || graph != fixtureName || node == "" {
			t.Fatalf("SplitExternalID(%q) = %q, %q, %v", parsed.ExternalID, graph, node, ok)
		}
		if worksource.TaskKey(item) != key {
			t.Fatalf("TaskKey disagrees with Ref.Key")
		}
	}
}

// TestFactoryComposesWithPrimary proves the config flag wires the adapter
// behind the primary source through the registry, and that a bad block fails
// FromConfig instead of silently listing nothing.
func TestFactoryComposesWithPrimary(t *testing.T) {
	cfg := config.WorkSourceConfig{Wavefront: config.WavefrontSourceConfig{
		Enabled: true, Path: fixtureGraph, Repo: testRepo, ReceiptsDir: t.TempDir(),
	}}
	ws, err := worksource.FromConfig(cfg, nil, "", "acme", slog.Default())
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if _, ok := ws.(*worksource.Composite); !ok {
		t.Fatalf("enabled wavefront should compose, got %T", ws)
	}
	if ws.SourceType() != "github" {
		t.Fatalf("composite must keep the primary's type, got %q", ws.SourceType())
	}

	primary := staticSource{issues: []worksource.Issue{{SourceType: "github", Repo: testRepo, ExternalID: "7", Number: 7, Title: "issue"}}}
	composed, err := worksource.AppendAdditive(primary, cfg)
	if err != nil {
		t.Fatalf("AppendAdditive: %v", err)
	}
	items, err := composed.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(items) != 5 || items[0].Number != 7 || items[1].ExternalID != "crustify-fixture:parse-ast" {
		t.Fatalf("composed items = %v", externalIDs(items))
	}

	if _, err := worksource.FromConfig(config.WorkSourceConfig{Wavefront: config.WavefrontSourceConfig{Enabled: true}}, nil, "", "", slog.Default()); err == nil {
		t.Fatal("enabled wavefront without a location must fail FromConfig")
	}
	if _, err := wavefront.FromConfig(config.WorkSourceConfig{Wavefront: config.WavefrontSourceConfig{Enabled: true, Path: fixtureGraph, Repo: testRepo, ReceiptsDir: filepath.Join(fixtureGraph, "not-a-dir")}}, nil); err == nil {
		t.Fatal("unusable receipts dir must fail FromConfig")
	}
}

// TestFlagOffGolden pins that a hive with the block absent or disabled gets the
// primary source back untouched and byte-identical ListIssues output.
func TestFlagOffGolden(t *testing.T) {
	primary := staticSource{issues: []worksource.Issue{{
		SourceType: "github", Repo: "hivecommons/hive", ExternalID: "42", Number: 42,
		Title: "regular issue", State: "open",
	}}}
	for name, cfg := range map[string]config.WorkSourceConfig{
		"absent":   {},
		"disabled": {Wavefront: config.WavefrontSourceConfig{Enabled: false, Path: fixtureGraph, Repo: testRepo}},
	} {
		ws, err := worksource.AppendAdditive(primary, cfg)
		if err != nil {
			t.Fatalf("%s: AppendAdditive: %v", name, err)
		}
		if _, ok := ws.(staticSource); !ok {
			t.Fatalf("%s: flag off must return the primary itself, got %T", name, ws)
		}
		got, err := ws.ListIssues(context.Background())
		if err != nil {
			t.Fatalf("%s: ListIssues: %v", name, err)
		}
		data, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want, err := os.ReadFile("../testdata/list_issues_flag_off.json")
		if err != nil {
			t.Fatalf("read golden: %v", err)
		}
		if string(data)+"\n" != string(want) {
			t.Fatalf("%s: flag-off output changed:\n%s", name, data)
		}
	}
}

// githubTrap is an http.RoundTripper that fails the test if any request is
// addressed to GitHub and counts every request it does see.
type githubTrap struct {
	t     *testing.T
	seen  atomic.Int64
	hosts []string
}

func (g *githubTrap) RoundTrip(req *http.Request) (*http.Response, error) {
	g.seen.Add(1)
	if strings.Contains(req.URL.Host, "github") {
		g.t.Fatalf("GitHub call from the wavefront adapter: %s %s", req.Method, req.URL)
	}
	g.hosts = append(g.hosts, req.URL.Host)
	return http.DefaultTransport.RoundTrip(req)
}

// TestZeroGitHubNetwork proves a file-backed source makes no HTTP request at
// all, and a URL-backed source talks only to the configured graph endpoint.
func TestZeroGitHubNetwork(t *testing.T) {
	trap := &githubTrap{t: t}
	client := &http.Client{Transport: trap}
	fileSrc, err := wavefront.New(wavefront.Options{Repo: testRepo, Path: fixtureGraph, HTTPClient: client, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := fileSrc.ListIssues(context.Background()); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	mustComplete(t, fileSrc, "parse-ast")
	if n := trap.seen.Load(); n != 0 {
		t.Fatalf("file-backed source made %d HTTP requests", n)
	}

	graphData, err := os.ReadFile(fixtureGraph)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/broken" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(graphData)
	}))
	defer srv.Close()
	urlSrc, err := wavefront.New(wavefront.Options{Repo: testRepo, URL: srv.URL + "/graph.json", HTTPClient: client, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	items, err := urlSrc.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues over HTTP: %v", err)
	}
	if len(items) != 4 || hits.Load() != 1 {
		t.Fatalf("items=%d hits=%d", len(items), hits.Load())
	}
	mustComplete(t, urlSrc, "wrap-io")
	wantHost := strings.TrimPrefix(srv.URL, "http://")
	for _, h := range trap.hosts {
		if h != wantHost {
			t.Fatalf("request to %q, want only %q", h, wantHost)
		}
	}
	if int64(len(trap.hosts)) != hits.Load() {
		t.Fatalf("trap saw %d requests, server saw %d", len(trap.hosts), hits.Load())
	}

	broken, _ := wavefront.New(wavefront.Options{Repo: testRepo, URL: srv.URL + "/broken", HTTPClient: client})
	if _, err := broken.ListIssues(context.Background()); err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("non-200 graph fetch = %v", err)
	}
	unreachable, _ := wavefront.New(wavefront.Options{Repo: testRepo, URL: "http://127.0.0.1:1/graph.json"})
	if _, err := unreachable.ListIssues(context.Background()); err == nil {
		t.Fatal("unreachable endpoint should error")
	}
	badURL, _ := wavefront.New(wavefront.Options{Repo: testRepo, URL: "http://[::1]:namedport/graph.json"})
	if _, err := badURL.ListIssues(context.Background()); err == nil {
		t.Fatal("unparseable URL should error")
	}
}

// TestPlanImportAdmitsGraphWithoutLLM proves the graph becomes an epic's child
// beads through DecomposeFromOutput alone: every node is a child, node IDs are
// the plan refs, the diamond's join depends on both arms, and a revision that
// does not match the graph is refused before anything is written.
func TestPlanImportAdmitsGraphWithoutLLM(t *testing.T) {
	g := loadFixture(t, fixtureGraph)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic, err := store.Create("Crustify migration", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	if _, err := wavefront.ImportPlan(store, epic, g, "rev-6", planning.Options{}); !errors.Is(err, wavefront.ErrStaleRevision) {
		t.Fatalf("mismatched revision = %v, want ErrStaleRevision", err)
	}
	if kids := store.List(beads.ListFilter{}); len(kids) != 1 {
		t.Fatalf("refused import must write nothing, store has %d beads", len(kids))
	}
	res, err := wavefront.ImportPlan(store, epic, g, fixtureRev, planning.Options{Actor: "hive-wavefront-lane"})
	if err != nil {
		t.Fatalf("ImportPlan: %v", err)
	}
	if len(res.Children) != len(g.Nodes) {
		t.Fatalf("children = %d, want %d", len(res.Children), len(g.Nodes))
	}
	byRef := map[string]*beads.Bead{}
	for _, c := range res.Children {
		byRef[c.Meta(planning.MetaPlanRef)] = c
	}
	join, ok := byRef["emit-ir"]
	if !ok {
		t.Fatalf("emit-ir child missing; refs = %v", keys(byRef))
	}
	if !contains(join.DependsOn, byRef["lower-types"].ID) || !contains(join.DependsOn, byRef["lower-exprs"].ID) || len(join.DependsOn) != 2 {
		t.Fatalf("emit-ir DependsOn = %v", join.DependsOn)
	}
	if byRef["survey-headers"].DependsOn != nil {
		t.Fatalf("root node should have no dependencies: %v", byRef["survey-headers"].DependsOn)
	}
	if byRef["parse-ast"].Title != "Port the AST parser to Rust" {
		t.Fatalf("title = %q", byRef["parse-ast"].Title)
	}
	updated, err := store.Get(epic.ID)
	if err != nil || updated.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft {
		t.Fatalf("epic plan_status = %q, %v", updated.Meta(planning.MetaPlanStatus), err)
	}

	out, err := wavefront.PlanOutput(g)
	if err != nil {
		t.Fatalf("PlanOutput: %v", err)
	}
	if !strings.Contains(out, "[emit-ir] Port IR emission (depends: lower-types, lower-exprs) [agent_suitable]") {
		t.Fatalf("PlanOutput line shape:\n%s", out)
	}
	if !strings.HasPrefix(out, "Imported Wavefront plan crustify-fixture@rev-7\n1. [survey-headers] ") {
		t.Fatalf("PlanOutput header/order:\n%s", out)
	}

	cyclic := wavefront.Graph{Name: "c", Revision: "r", Nodes: []wavefront.Node{{ID: "a", DependsOn: []string{"b"}}, {ID: "b", DependsOn: []string{"a"}}}}
	if _, err := wavefront.PlanOutput(cyclic); err == nil {
		t.Fatal("PlanOutput must refuse a cycle")
	}
	if _, err := wavefront.ImportPlan(store, epic, cyclic, "r", planning.Options{}); err == nil {
		t.Fatal("ImportPlan must refuse a cycle")
	}
	untitled := wavefront.Graph{Name: "u", Revision: "r", Nodes: []wavefront.Node{{ID: "only"}}}
	out, err = wavefront.PlanOutput(untitled)
	if err != nil || !strings.Contains(out, "[only] only [agent_suitable]") {
		t.Fatalf("untitled node falls back to its id: %q, %v", out, err)
	}
}

func TestParseGraphRejectsStructuralDefects(t *testing.T) {
	cases := map[string]string{
		"bad json":      `{`,
		"no name":       `{"revision":"r","nodes":[{"id":"a"}]}`,
		"no revision":   `{"graph":"g","nodes":[{"id":"a"}]}`,
		"no nodes":      `{"graph":"g","revision":"r","nodes":[]}`,
		"empty id":      `{"graph":"g","revision":"r","nodes":[{"id":""}]}`,
		"reserved char": `{"graph":"g","revision":"r","nodes":[{"id":"a:b"}]}`,
		"reserved name": `{"graph":"g!x","revision":"r","nodes":[{"id":"a"}]}`,
		"duplicate id":  `{"graph":"g","revision":"r","nodes":[{"id":"a"},{"id":"a"}]}`,
		"undefined dep": `{"graph":"g","revision":"r","nodes":[{"id":"a","depends_on":["zz"]}]}`,
		"self dep":      `{"graph":"g","revision":"r","nodes":[{"id":"a","depends_on":["a"]}]}`,
		"cycle":         `{"graph":"g","revision":"r","nodes":[{"id":"a","depends_on":["b"]},{"id":"b","depends_on":["a"]}]}`,
		"slash in id":   `{"graph":"g","revision":"r","nodes":[{"id":"../x"}]}`,
		"space in id":   `{"graph":"g","revision":"r","nodes":[{"id":"a b"}]}`,
	}
	for name, doc := range cases {
		if _, err := wavefront.ParseGraph([]byte(doc)); err == nil {
			t.Errorf("%s: ParseGraph accepted %s", name, doc)
		}
	}
	if _, err := wavefront.ParseGraph([]byte(cases["undefined dep"])); !errors.Is(err, wavefront.ErrUndefinedEdge) || errors.Is(err, wavefront.ErrCycle) {
		t.Fatalf("undefined edge = %v, want ErrUndefinedEdge and not ErrCycle", err)
	}
	if _, err := wavefront.ParseGraph([]byte(cases["cycle"])); !errors.Is(err, wavefront.ErrCycle) {
		t.Fatalf("cycle = %v, want ErrCycle", err)
	}
	dangling := wavefront.Graph{Name: "d", Revision: "r", Nodes: []wavefront.Node{{ID: "a", DependsOn: []string{"ghost"}}, {ID: "b"}}}
	if waves, err := dangling.Waves(); err != nil || len(waves) != 1 || len(waves[0]) != 2 {
		t.Fatalf("Waves must skip unknown ids, not report a cycle: %v, %v", waves, err)
	}
	g, err := wavefront.ParseGraph([]byte(`{"graph":"g","revision":"r","nodes":[{"id":"c","depends_on":["a","b"]},{"id":"b"},{"id":"a"},{"id":"d","depends_on":["c"]}]}`))
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}
	waves, err := g.Waves()
	if err != nil {
		t.Fatalf("Waves: %v", err)
	}
	if len(waves) != 3 || waves[0][0].ID != "a" || waves[0][1].ID != "b" || waves[1][0].ID != "c" || waves[2][0].ID != "d" {
		t.Fatalf("waves = %+v", waves)
	}
	if g.Digest() == (wavefront.Graph{Name: "g", Revision: "r2", Nodes: g.Nodes}).Digest() {
		t.Fatal("digest must change with the revision")
	}
	if _, ok := g.Node("nope"); ok {
		t.Fatal("Node(nope) should be absent")
	}
	if id := wavefront.ExternalID("g", "a"); id != "g:a" {
		t.Fatalf("ExternalID = %q", id)
	}
}

func TestSourceConstructionAndLoaderErrors(t *testing.T) {
	if _, err := wavefront.New(wavefront.Options{Path: fixtureGraph}); err == nil {
		t.Fatal("New without repo should fail")
	}
	if _, err := wavefront.New(wavefront.Options{Repo: testRepo}); err == nil {
		t.Fatal("New without a location should fail")
	}
	if _, err := wavefront.New(wavefront.Options{Repo: testRepo, Path: fixtureGraph, ReceiptsDir: filepath.Join(fixtureGraph, "x")}); err == nil {
		t.Fatal("receipts dir under a file should fail")
	}
	var nilSrc *wavefront.Source
	if _, err := nilSrc.Graph(context.Background()); err == nil {
		t.Fatal("nil source Graph should fail")
	}
	missing, err := wavefront.New(wavefront.Options{Repo: testRepo, Path: filepath.Join(t.TempDir(), "missing.json")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := missing.ListIssues(context.Background()); err == nil {
		t.Fatal("missing graph file should error")
	}
	if _, _, err := missing.Verify(context.Background(), "g:a", "r"); err == nil {
		t.Fatal("Verify with an unreadable graph should error")
	}
	failing, _ := wavefront.New(wavefront.Options{Repo: testRepo, Loader: func(context.Context) (wavefront.Graph, error) { return wavefront.Graph{}, errors.New("boom") }})
	if _, _, err := failing.Ready(context.Background()); err == nil {
		t.Fatal("loader error should surface from Ready")
	}
	// A loader may return an unvalidated graph; Ready must still refuse a cycle.
	cyclic, _ := wavefront.New(wavefront.Options{Repo: testRepo, Loader: func(context.Context) (wavefront.Graph, error) {
		return wavefront.Graph{Name: "c", Revision: "r", Nodes: []wavefront.Node{{ID: "a", DependsOn: []string{"b"}}, {ID: "b", DependsOn: []string{"a"}}}}, nil
	}})
	if _, _, err := cyclic.Ready(context.Background()); err == nil {
		t.Fatal("cyclic graph must not list anything")
	}
	// A dependency on an undefined node (unvalidated loader) withholds the node.
	dangling, _ := wavefront.New(wavefront.Options{Repo: testRepo, Loader: func(context.Context) (wavefront.Graph, error) {
		return wavefront.Graph{Name: "d", Revision: "r", Nodes: []wavefront.Node{{ID: "a", DependsOn: []string{"ghost"}}, {ID: "b"}}}, nil
	}})
	items, err := dangling.ListIssues(context.Background())
	if err != nil || len(items) != 1 || items[0].ExternalID != "d:b" {
		t.Fatalf("dangling edge handling = %v, %v", externalIDs(items), err)
	}
	if _, err := dangling.Complete(context.Background(), "d:a", "r", nil, fixedNow); !errors.Is(err, wavefront.ErrNotReady) {
		t.Fatalf("completing a node with a dangling edge = %v", err)
	}
	huge, _ := wavefront.New(wavefront.Options{Repo: testRepo, Path: writeFile(t, strings.Repeat(" ", 16<<20+2))})
	if _, err := huge.ListIssues(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize graph = %v", err)
	}
}

func TestCompleteArtifactsAndClock(t *testing.T) {
	src := newFileSource(t)
	artifacts := []outputschema.Artifact{
		{Repo: testRepo, Path: "src/parser.rs", Description: "ported parser"},
		{Repo: testRepo, Path: "src/ast.rs", Description: "ported ast"},
	}
	// A start time after the clock is clamped to the end time.
	r, err := src.Complete(context.Background(), "crustify-fixture:wrap-alloc", fixtureRev, artifacts, fixedNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if r.Receipt.StartedAt != r.Receipt.EndedAt || len(r.Receipt.Artifacts) != 2 {
		t.Fatalf("receipt times/artifacts = %+v", r.Receipt)
	}
	report, _ := json.Marshal(outputschema.AgentReport{
		Lane: wavefront.Engine, Kind: outputschema.KindStageReceipt, Summary: "done",
		Findings: []outputschema.Finding{}, PRsOpened: []outputschema.PROpened{}, BeadsFiled: []outputschema.BeadFiled{},
		Receipt: &r.Receipt,
	})
	if _, err := outputschema.Validate(report); err != nil {
		t.Fatalf("custom-artifact receipt digest must validate: %v", err)
	}
	// Zero start time falls back to the clock.
	r2, err := src.Complete(context.Background(), "crustify-fixture:wrap-io", fixtureRev, nil, time.Time{})
	if err != nil || r2.Receipt.StartedAt != fixedNow.Format(time.RFC3339Nano) {
		t.Fatalf("zero start = %+v, %v", r2.Receipt.StartedAt, err)
	}
	// A default clock still produces a valid receipt.
	defaultClock, _ := wavefront.New(wavefront.Options{Repo: testRepo, Path: fixtureGraph})
	if _, err := defaultClock.Complete(context.Background(), "crustify-fixture:wrap-io", fixtureRev, nil, time.Time{}); err != nil {
		t.Fatalf("default clock Complete: %v", err)
	}
}

func TestReceiptStoreErrors(t *testing.T) {
	mem, err := wavefront.NewReceiptStore("")
	if err != nil {
		t.Fatalf("NewReceiptStore: %v", err)
	}
	if err := mem.Put(wavefront.Receipt{Graph: "bad:name", Node: "n"}); err == nil {
		t.Fatal("reserved graph name must be refused")
	}
	if err := mem.Put(wavefront.Receipt{Graph: "g", Node: "../n"}); err == nil {
		t.Fatal("reserved node id must be refused")
	}
	if err := mem.Put(wavefront.Receipt{Graph: "g", Node: "n", Revision: "r"}); err != nil {
		t.Fatalf("in-memory Put: %v", err)
	}
	if _, ok := mem.Get("g", "n"); !ok {
		t.Fatal("in-memory receipt should be readable")
	}
	if mem.Completed("g", "n", "r") {
		t.Fatal("a receipt without a completed result class must not count")
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "g"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "g", "notes.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wavefront.NewReceiptStore(dir); err != nil {
		t.Fatalf("non-receipt files must be ignored: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "g", "corrupt.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wavefront.NewReceiptStore(dir); err == nil {
		t.Fatal("corrupt receipt must fail the open, not be silently dropped")
	}
	if err := os.WriteFile(filepath.Join(dir, "g", "corrupt.json"), []byte(`{"graph":"","node":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wavefront.NewReceiptStore(dir); err == nil {
		t.Fatal("receipt without identity must fail the open")
	}
	if err := os.Remove(filepath.Join(dir, "g", "corrupt.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "g", "n.json"), []byte(`{"graph":"g","node":"n","revision":"r","stage_receipt":{"result_class":"completed"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	disk, err := wavefront.NewReceiptStore(dir)
	if err != nil {
		t.Fatalf("NewReceiptStore: %v", err)
	}
	if !disk.Completed("g", "n", "r") {
		t.Fatal("hand-written completed receipt should load")
	}
	// A graph directory that cannot be listed fails the open.
	if err := os.Chmod(filepath.Join(dir, "g"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "g"), 0o755) })
	if os.Getuid() != 0 {
		if _, err := wavefront.NewReceiptStore(dir); err == nil {
			t.Fatal("unreadable graph dir should fail the open")
		}
	}
	_ = os.Chmod(filepath.Join(dir, "g"), 0o755)

	// A store rooted at a file cannot be read.
	file := writeFile(t, "x")
	if _, err := wavefront.NewReceiptStore(file); err == nil {
		t.Fatal("store rooted at a file should fail")
	}
	// Put into a store whose graph dir is shadowed by a file fails on mkdir.
	shadowed := t.TempDir()
	blocked, err := wavefront.NewReceiptStore(shadowed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shadowed, "g"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := blocked.Put(wavefront.Receipt{Graph: "g", Node: "n"}); err == nil {
		t.Fatal("Put under a file-shadowed graph dir should fail")
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func keys(m map[string]*beads.Bead) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

type staticSource struct{ issues []worksource.Issue }

func (staticSource) SourceType() string { return "github" }
func (s staticSource) ListIssues(context.Context) ([]worksource.Issue, error) {
	return append([]worksource.Issue(nil), s.issues...), nil
}
