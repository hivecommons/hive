package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestOriginLabelDefinitionsShareOneColour(t *testing.T) {
	if !IsOriginLabel("origin/review") || !IsOriginLabel(" Origin/Selfreport ") || IsOriginLabel("origin/") || IsOriginLabel("from-review") || IsOriginLabel("agent/reviewer") {
		t.Fatal("IsOriginLabel prefix check")
	}
	known, ok := originLabelDefinition(OriginReviewerLabel)
	if !ok || known.color != OriginLabelColor || !strings.Contains(known.description, "reviewer") {
		t.Fatalf("origin/reviewer definition = %+v, %v", known, ok)
	}
	other, ok := originLabelDefinition("origin/sentinel")
	if !ok || other.color != OriginLabelColor || other.description == "" {
		t.Fatalf("unknown origin source should still get the family colour: %+v, %v", other, ok)
	}
	if _, ok := originLabelDefinition("agent/reviewer"); ok {
		t.Fatal("agent/* is not a provenance label")
	}
	if def, ok := issueLabelDefinitionFor("meta"); !ok || def.color != "5319e7" {
		t.Fatalf("escalation definitions still win: %+v %v", def, ok)
	}
	if reviewBacklogLabel != "origin/reviewer" || LegacyOriginReviewLabel != "origin/review" || LegacyFromReviewLabel != "from-review" {
		t.Fatalf("backlog label = %q legacy = %q, %q", reviewBacklogLabel, LegacyOriginReviewLabel, LegacyFromReviewLabel)
	}
	for _, retired := range []string{"from-review", "origin/review", " Origin/Review "} {
		if got, ok := CanonicalOriginLabel(retired); !ok || got != OriginReviewerLabel {
			t.Fatalf("CanonicalOriginLabel(%q) = %q, %v", retired, got, ok)
		}
	}
	if !strings.HasSuffix(originLabelMigrationFileStem, "-v3") || !strings.HasSuffix(OriginLabelMigrationID, "-v3") {
		t.Fatalf("migration marker/id must be versioned so v1/v2 hives re-run: %q %q", originLabelMigrationFileStem, OriginLabelMigrationID)
	}
}

func TestEnsureOriginLabelRefusesRetiredNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("retired origin label must not reach GitHub: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	c := newTestClient(t, server, "o", []string{"r"})
	for _, retired := range []string{"origin/review", "Origin/Review"} {
		err := c.ensureOriginLabel(context.Background(), "o", "r", retired)
		if err == nil || !strings.Contains(err.Error(), OriginReviewerLabel) {
			t.Fatalf("ensureOriginLabel(%q) = %v, want retired error naming %s", retired, err, OriginReviewerLabel)
		}
	}
}

func TestEnsureOriginLabelCreatesThenRecoloursIdempotently(t *testing.T) {
	org, repo := "o", "r"
	var mu sync.Mutex
	var existing map[string]any
	var creates, edits int
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels/origin/reviewer", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			if existing == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(existing)
		case http.MethodPatch:
			edits++
			var label map[string]any
			_ = json.NewDecoder(r.Body).Decode(&label)
			existing = label
			_ = json.NewEncoder(w).Encode(label)
		default:
			t.Errorf("unexpected %s", r.Method)
		}
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		creates++
		var label map[string]any
		_ = json.NewDecoder(r.Body).Decode(&label)
		existing = label
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(label)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})
	ctx := context.Background()

	if err := c.EnsureIssueLabel(ctx, org+"/"+repo, "origin/reviewer", "ff0000", "ignored: origin/* owns its colour"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if creates != 1 || edits != 0 || existing["color"] != OriginLabelColor {
		t.Fatalf("after create: creates=%d edits=%d label=%v", creates, edits, existing)
	}
	if err := c.ensureCreateIssueLabel(ctx, org, repo, "origin/reviewer"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if creates != 1 || edits != 0 {
		t.Fatalf("matching colour must be a no-op: creates=%d edits=%d", creates, edits)
	}
	existing["color"] = "d93f0b"
	if err := c.ensureCreateIssueLabel(ctx, org, repo, "origin/reviewer"); err != nil {
		t.Fatalf("recolour: %v", err)
	}
	if creates != 1 || edits != 1 || existing["color"] != OriginLabelColor {
		t.Fatalf("drifted colour should be edited back: creates=%d edits=%d label=%v", creates, edits, existing)
	}
}

// originMigrationServer serves a repo whose open issues carry the given
// labels and records every label add/remove/create/delete the migration
// makes. listQueries collects the labels= filter of each issue list call.
type originMigrationServer struct {
	mu          sync.Mutex
	added       map[int][]string
	removed     map[int][]string
	created     map[string]string
	deleted     []string
	listQueries []string
}

func newOriginMigrationServer(t *testing.T, org, repo string, issues []wireIssue) (*originMigrationServer, *httptest.Server) {
	t.Helper()
	st := &originMigrationServer{added: map[int][]string{}, removed: map[int][]string{}, created: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		filter := r.URL.Query().Get("labels")
		st.mu.Lock()
		st.listQueries = append(st.listQueries, filter)
		st.mu.Unlock()
		var matched []wireIssue
		for _, is := range issues {
			if filter == "" {
				matched = append(matched, is)
				continue
			}
			for _, l := range is.Labels {
				if l.Name == filter {
					matched = append(matched, is)
					break
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(t, matched))
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/repos/"+org+"/"+repo+"/labels/")
		switch r.Method {
		case http.MethodGet:
			http.NotFound(w, r)
		case http.MethodDelete:
			st.mu.Lock()
			st.deleted = append(st.deleted, name)
			st.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		var label map[string]any
		_ = json.NewDecoder(r.Body).Decode(&label)
		st.mu.Lock()
		st.created[label["name"].(string)], _ = label["color"].(string)
		st.mu.Unlock()
		_ = json.NewEncoder(w).Encode(label)
	})
	for _, is := range issues {
		n := is.Number
		mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(n)+"/labels", func(w http.ResponseWriter, r *http.Request) {
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			st.mu.Lock()
			st.added[n] = append(st.added[n], labels...)
			st.mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		})
		mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(n)+"/labels/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
			st.mu.Lock()
			st.removed[n] = append(st.removed[n], strings.TrimPrefix(r.URL.Path, "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(n)+"/labels/"))
			st.mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		})
	}
	return st, httptest.NewServer(mux)
}

func originMigrationOpts(t *testing.T) OriginLabelMigrationOptions {
	t.Helper()
	dir := t.TempDir()
	return OriginLabelMigrationOptions{
		DataDir:    dir,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MarkerPath: filepath.Join(dir, "marker"),
		ReportPath: filepath.Join(dir, "report.json"),
	}
}

func TestMigrateOriginLabelsSwapsBothRetiredNamesOnceWithoutDoubleLabelling(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 1, Title: "only from-review", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "from-review"}, {Name: "agent/scanner"}}, CreatedAt: hoursAgo(4)},
		{Number: 2, Title: "from-review plus new", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "from-review"}, {Name: "origin/reviewer"}}, CreatedAt: hoursAgo(3)},
		{Number: 3, Title: "only origin/review (v1 hive)", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "origin/review"}, {Name: "agent/reviewer"}}, CreatedAt: hoursAgo(2)},
		{Number: 4, Title: "origin/review plus new", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "origin/review"}, {Name: "origin/reviewer"}}, CreatedAt: hoursAgo(1)},
	}
	st, server := newOriginMigrationServer(t, org, repo, issues)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})
	opts := originMigrationOpts(t)

	report, err := c.MigrateOriginLabels(context.Background(), opts)
	if err != nil {
		t.Fatalf("MigrateOriginLabels: %v", err)
	}
	if report == nil || len(report.Migrated) != 4 || len(report.Errors) != 0 {
		t.Fatalf("report = %+v", report)
	}
	// Both retired names in order, then the unfiltered list the origin
	// backfill scans.
	if strings.Join(st.listQueries, ",") != "from-review,origin/review," {
		t.Fatalf("list filters = %v, want both retired names in order then the backfill scan", st.listQueries)
	}
	for _, n := range []int{1, 3} {
		if strings.Join(st.added[n], ",") != "origin/reviewer" {
			t.Fatalf("issue %d added = %v, want origin/reviewer", n, st.added[n])
		}
	}
	for _, n := range []int{2, 4} {
		if _, ok := st.added[n]; ok {
			t.Fatalf("issue %d already carried origin/reviewer; must not be added again: %v", n, st.added[n])
		}
	}
	if strings.Join(st.removed[1], ",") != "from-review" || strings.Join(st.removed[2], ",") != "from-review" {
		t.Fatalf("removed = %v, want from-review off 1 and 2", st.removed)
	}
	if strings.Join(st.removed[3], ",") != "origin/review" || strings.Join(st.removed[4], ",") != "origin/review" {
		t.Fatalf("removed = %v, want origin/review off 3 and 4", st.removed)
	}
	if st.created["origin/reviewer"] != OriginLabelColor || len(st.created) != 1 {
		t.Fatalf("created labels = %v, want only origin/reviewer in the family colour", st.created)
	}
	if strings.Join(st.deleted, ",") != "origin/review" {
		t.Fatalf("deleted definitions = %v, want only origin/review (from-review is kept for closed-item history)", st.deleted)
	}
	if strings.Join(report.DefinitionsDeleted, ",") != org+"/"+repo+":origin/review" {
		t.Fatalf("report.DefinitionsDeleted = %v", report.DefinitionsDeleted)
	}
	for i, want := range []struct {
		from string
		had  bool
	}{{"from-review", false}, {"from-review", true}, {"origin/review", false}, {"origin/review", true}} {
		got := report.Migrated[i]
		if got.From != want.from || got.To != "origin/reviewer" || got.AlreadyHadNew != want.had {
			t.Fatalf("migrated[%d] = %+v, want from=%s already_had_new=%v", i, got, want.from, want.had)
		}
	}
	if _, err := os.Stat(opts.MarkerPath); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if data, err := os.ReadFile(opts.ReportPath); err != nil || !strings.Contains(string(data), OriginLabelMigrationID) {
		t.Fatalf("report file = %q, %v", data, err)
	}

	before := len(st.listQueries)
	again, err := c.MigrateOriginLabels(context.Background(), opts)
	if err != nil || again != nil {
		t.Fatalf("second run = %+v, %v; want a no-op once the marker exists", again, err)
	}
	if len(st.listQueries) != before {
		t.Fatalf("second run listed issues again (%d lists)", len(st.listQueries)-before)
	}
}

// A hive that already ran the v1 from-review → origin/review swap has the
// old marker in /data. The v2 marker lives under a different name, so the
// v2 migration still runs once and moves origin/review → origin/reviewer.
func TestMigrateOriginLabelsRunsOnHiveThatCompletedV1(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 7, Title: "v1 output", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "origin/review"}}, CreatedAt: hoursAgo(2)},
	}
	st, server := newOriginMigrationServer(t, org, repo, issues)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})

	dataDir := t.TempDir()
	// Markers of both earlier versions must not suppress v3.
	for _, marker := range []string{"origin-label-migration.done", "origin-label-migration-v2.done"} {
		if err := os.WriteFile(filepath.Join(dataDir, marker), []byte("2026-01-01T00:00:00Z\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opts := OriginLabelMigrationOptions{DataDir: dataDir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	report, err := c.MigrateOriginLabels(context.Background(), opts)
	if err != nil {
		t.Fatalf("MigrateOriginLabels: %v", err)
	}
	if report == nil || len(report.Migrated) != 1 || report.Migrated[0].From != "origin/review" || report.Migrated[0].To != "origin/reviewer" {
		t.Fatalf("v1/v2 markers must not suppress v3: report = %+v", report)
	}
	if strings.Join(st.added[7], ",") != "origin/reviewer" || strings.Join(st.removed[7], ",") != "origin/review" {
		t.Fatalf("issue 7 added=%v removed=%v", st.added[7], st.removed[7])
	}
	if strings.Join(st.deleted, ",") != "origin/review" {
		t.Fatalf("deleted definitions = %v", st.deleted)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "origin-label-migration-v3.done")); err != nil {
		t.Fatalf("v3 marker not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "origin-label-migration-v3.json")); err != nil {
		t.Fatalf("v3 report not written: %v", err)
	}
	if again, err := c.MigrateOriginLabels(context.Background(), opts); err != nil || again != nil {
		t.Fatalf("second run = %+v, %v; want no-op", again, err)
	}
}

// When swapping an item fails the retired definition must stay on that repo
// (deleting it would strip the label from the un-migrated item) and the
// marker must not be written so the next boot retries.
func TestMigrateOriginLabelsKeepsRetiredDefinitionWhenAnItemFails(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 9, Title: "stuck", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "origin/review"}}, CreatedAt: hoursAgo(2)},
	}
	st, server := newOriginMigrationServer(t, org, repo, issues)
	defer server.Close()
	// Override the add-labels route to fail so the item never migrates.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/"+org+"/"+repo+"/issues/9/labels" {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		server.Config.Handler.ServeHTTP(w, r)
	}))
	defer failing.Close()
	c := newTestClient(t, failing, org, []string{repo})
	opts := originMigrationOpts(t)

	report, err := c.MigrateOriginLabels(context.Background(), opts)
	if err == nil || report == nil || len(report.Errors) != 1 || len(report.Migrated) != 0 {
		t.Fatalf("report = %+v, err = %v; want one error and nothing migrated", report, err)
	}
	if len(st.deleted) != 0 || len(report.DefinitionsDeleted) != 0 {
		t.Fatalf("retired definition deleted despite a failed item: %v %v", st.deleted, report.DefinitionsDeleted)
	}
	if _, err := os.Stat(opts.MarkerPath); !os.IsNotExist(err) {
		t.Fatalf("marker must not be written after errors: %v", err)
	}
}

// The v3 backfill gives open hive-authored issues that predate the universal
// origin axis their first origin/* label: origin/<lane> from agent/<lane>,
// origin/selfreport from hive/selfreport (which wins over a lane), nothing on
// human-filed issues, issues that already carry an origin label, or PRs.
func TestMigrateOriginLabelsBackfillsLaneAndSelfReportOrigins(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 11, Title: "lane only", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "agent/scanner"}, {Name: "kind/bug"}}, CreatedAt: hoursAgo(5)},
		{Number: 12, Title: "selfreport with lane", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "agent/quality"}, {Name: "hive/selfreport"}}, CreatedAt: hoursAgo(4)},
		{Number: 13, Title: "already has origin", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "agent/scanner"}, {Name: "origin/dashboard"}}, CreatedAt: hoursAgo(3)},
		{Number: 14, Title: "human filed", User: wireUser{"alice"}, Labels: []wireLabel{{Name: "agent/scanner"}}, CreatedAt: hoursAgo(2)},
		{Number: 15, Title: "no marker", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "kind/bug"}}, CreatedAt: hoursAgo(1)},
		{Number: 16, Title: "a PR", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "agent/scanner"}}, CreatedAt: hoursAgo(1), PullRequest: &struct{}{}},
	}
	st, server := newOriginMigrationServer(t, org, repo, issues)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})
	c.additionalBotLogins = []string{"hivecommons-hive[bot]"}

	report, err := c.MigrateOriginLabels(context.Background(), originMigrationOpts(t))
	if err != nil {
		t.Fatalf("MigrateOriginLabels: %v", err)
	}
	if len(report.Errors) != 0 || len(report.Migrated) != 0 {
		t.Fatalf("report = %+v", report)
	}
	if strings.Join(st.added[11], ",") != "origin/scanner" || strings.Join(st.added[12], ",") != "origin/selfreport" {
		t.Fatalf("added = %v, want origin/scanner on 11 and origin/selfreport on 12", st.added)
	}
	for _, n := range []int{13, 14, 15, 16} {
		if got, ok := st.added[n]; ok {
			t.Fatalf("issue %d must not be touched, added %v", n, got)
		}
	}
	if len(st.removed) != 0 {
		t.Fatalf("backfill must never remove labels: %v", st.removed)
	}
	if st.created["origin/scanner"] != OriginLabelColor || st.created["origin/selfreport"] != OriginLabelColor {
		t.Fatalf("created = %v, want both new labels in the family colour", st.created)
	}
	if len(report.Backfilled) != 2 || report.Backfilled[0].DerivedFrom != "agent/scanner" || report.Backfilled[1].DerivedFrom != "hive/selfreport" {
		t.Fatalf("backfilled = %+v", report.Backfilled)
	}
}

// Every origin/* label the hive can create resolves to the shared colour and
// a per-source description, so a label created on demand for a new lane is
// never a bare default-coloured label.
func TestOriginLabelDefinitionCoversEverySource(t *testing.T) {
	for _, source := range config.KnownOriginSources() {
		def, ok := originLabelDefinition(config.OriginLabel(source))
		if !ok || def.color != OriginLabelColor || !strings.Contains(def.description, "provenance") {
			t.Fatalf("origin/%s definition = %+v ok=%v", source, def, ok)
		}
	}
	if def, ok := originLabelDefinition("origin/brand-new-lane"); !ok || !strings.Contains(def.description, "brand-new-lane agent lane") {
		t.Fatalf("unknown lane definition = %+v ok=%v", def, ok)
	}
}
