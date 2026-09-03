package knowledge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

func synthTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const floatTolerance = 1e-9

func confEq(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < floatTolerance
}

func newTestBead(id, title string, beadType beads.BeadType, priority beads.Priority, actor string) *beads.Bead {
	return &beads.Bead{
		ID:       id,
		Title:    title,
		Type:     beadType,
		Status:   beads.StatusDone,
		Priority: priority,
		Actor:    actor,
		Metadata: make(map[string]interface{}),
	}
}

// --- Classification Tests ---

func TestClassifyBead_BugToGotcha(t *testing.T) {
	b := newTestBead("b1", "nil pointer in handler", beads.TypeBug, beads.PriorityMedium, "scanner")
	ft, conf := ClassifyBead(b)
	if ft != FactGotcha {
		t.Errorf("type = %s, want gotcha", ft)
	}
	if !confEq(conf, 0.7) {
		t.Errorf("confidence = %f, want 0.7", conf)
	}
}

func TestClassifyBead_BugRegressionMetadata(t *testing.T) {
	b := newTestBead("b2", "webhook port lost again", beads.TypeBug, beads.PriorityMedium, "scanner")
	b.Metadata["finding_type"] = "regression"
	ft, conf := ClassifyBead(b)
	if ft != FactRegression {
		t.Errorf("type = %s, want regression", ft)
	}
	if !confEq(conf, 0.8) {
		t.Errorf("confidence = %f, want 0.8", conf)
	}
}

func TestClassifyBead_DecisionBead(t *testing.T) {
	b := newTestBead("b3", "use zustand for state mgmt", beads.TypeDecision, beads.PriorityMedium, "architect")
	ft, conf := ClassifyBead(b)
	if ft != FactDecision {
		t.Errorf("type = %s, want decision", ft)
	}
	if !confEq(conf, 0.7) {
		t.Errorf("confidence = %f, want 0.7", conf)
	}
}

func TestClassifyBead_AdvisoryPattern(t *testing.T) {
	b := newTestBead("b4", "use factory functions for mocks", beads.TypeAdvisory, beads.PriorityMedium, "quality")
	b.Metadata["finding_type"] = "pattern"
	ft, conf := ClassifyBead(b)
	if ft != FactPattern {
		t.Errorf("type = %s, want pattern", ft)
	}
	if !confEq(conf, 0.6) {
		t.Errorf("confidence = %f, want 0.6", conf)
	}
}

func TestClassifyBead_AdvisorySecurity(t *testing.T) {
	b := newTestBead("b5", "SQL injection in query builder", beads.TypeAdvisory, beads.PriorityMedium, "sec-check")
	b.Metadata["finding_type"] = "security"
	ft, conf := ClassifyBead(b)
	if ft != FactGotcha {
		t.Errorf("type = %s, want gotcha", ft)
	}
	if !confEq(conf, 0.8) {
		t.Errorf("confidence = %f, want 0.8", conf)
	}
}

func TestClassifyBead_AdvisoryTest(t *testing.T) {
	b := newTestBead("b6", "missing test for edge case", beads.TypeAdvisory, beads.PriorityMedium, "quality")
	b.Metadata["finding_type"] = "test"
	ft, conf := ClassifyBead(b)
	if ft != FactTestScaff {
		t.Errorf("type = %s, want test_scaffold", ft)
	}
	if !confEq(conf, 0.5) {
		t.Errorf("confidence = %f, want 0.5", conf)
	}
}

func TestClassifyBead_AdvisoryConvention(t *testing.T) {
	b := newTestBead("b6b", "naming convention for hooks", beads.TypeAdvisory, beads.PriorityMedium, "quality")
	b.Metadata["finding_type"] = "convention"
	ft, conf := ClassifyBead(b)
	if ft != FactPattern {
		t.Errorf("type = %s, want pattern", ft)
	}
	if !confEq(conf, 0.6) {
		t.Errorf("confidence = %f, want 0.6", conf)
	}
}

func TestClassifyBead_AdvisoryVulnerability(t *testing.T) {
	b := newTestBead("b6c", "XSS in user input", beads.TypeAdvisory, beads.PriorityMedium, "sec-check")
	b.Metadata["finding_type"] = "vulnerability"
	ft, conf := ClassifyBead(b)
	if ft != FactGotcha {
		t.Errorf("type = %s, want gotcha", ft)
	}
	if !confEq(conf, 0.8) {
		t.Errorf("confidence = %f, want 0.8", conf)
	}
}

func TestClassifyBead_AdvisoryCoverage(t *testing.T) {
	b := newTestBead("b6d", "low coverage in auth module", beads.TypeAdvisory, beads.PriorityMedium, "quality")
	b.Metadata["finding_type"] = "coverage"
	ft, conf := ClassifyBead(b)
	if ft != FactTestScaff {
		t.Errorf("type = %s, want test_scaffold", ft)
	}
	if !confEq(conf, 0.5) {
		t.Errorf("confidence = %f, want 0.5", conf)
	}
}

func TestClassifyBead_AdvisoryDefaultFallback(t *testing.T) {
	b := newTestBead("b6e", "misc observation", beads.TypeAdvisory, beads.PriorityMedium, "scanner")
	b.Metadata["finding_type"] = "other"
	ft, conf := ClassifyBead(b)
	if ft != FactPattern {
		t.Errorf("type = %s, want pattern", ft)
	}
	if !confEq(conf, 0.5) {
		t.Errorf("confidence = %f, want 0.5", conf)
	}
}

func TestClassifyBead_AdvisoryDefaultWithKeywords(t *testing.T) {
	b := newTestBead("b6f", "advisory with regression keyword", beads.TypeAdvisory, beads.PriorityMedium, "scanner")
	b.Metadata["finding_type"] = "other"
	b.Notes = "This broke again after the deploy"
	ft, conf := ClassifyBead(b)
	if ft != FactRegression {
		t.Errorf("type = %s, want regression (from keyword scan)", ft)
	}
	if !confEq(conf, 0.8) {
		t.Errorf("confidence = %f, want 0.8", conf)
	}
}

func TestClassifyBead_ChoreSkipped(t *testing.T) {
	b := newTestBead("b7", "update dependencies", beads.TypeChore, beads.PriorityMinor, "ci-maintainer")
	ft, conf := ClassifyBead(b)
	if ft != "" {
		t.Errorf("type = %s, want empty (chore should be skipped)", ft)
	}
	if conf != 0 {
		t.Errorf("confidence = %f, want 0", conf)
	}
}

func TestClassifyBead_EpicToDecision(t *testing.T) {
	b := newTestBead("b8", "migrate to new auth system", beads.TypeEpic, beads.PriorityMedium, "architect")
	ft, conf := ClassifyBead(b)
	if ft != FactDecision {
		t.Errorf("type = %s, want decision", ft)
	}
	if !confEq(conf, 0.6) {
		t.Errorf("confidence = %f, want 0.6", conf)
	}
}

func TestClassifyBead_TaskLowConfidence(t *testing.T) {
	b := newTestBead("b9", "add logging to handler", beads.TypeTask, beads.PriorityMinor, "scanner")
	ft, conf := ClassifyBead(b)
	if ft != FactPattern {
		t.Errorf("type = %s, want pattern", ft)
	}
	if !confEq(conf, 0.4) {
		t.Errorf("confidence = %f, want 0.4", conf)
	}
}

func TestClassifyBead_FeatureToPattern(t *testing.T) {
	b := newTestBead("b9b", "add retry logic", beads.TypeFeature, beads.PriorityMedium, "scanner")
	ft, conf := ClassifyBead(b)
	if ft != FactPattern {
		t.Errorf("type = %s, want pattern", ft)
	}
	if !confEq(conf, 0.5) {
		t.Errorf("confidence = %f, want 0.5", conf)
	}
}

func TestClassifyBead_PriorityBoost(t *testing.T) {
	b := newTestBead("b10", "critical auth bypass", beads.TypeBug, beads.PriorityCritical, "sec-check")
	ft, conf := ClassifyBead(b)
	if ft != FactGotcha {
		t.Errorf("type = %s, want gotcha", ft)
	}
	if !confEq(conf, 0.8) {
		t.Errorf("confidence = %f, want 0.8 (0.7 + 0.1 boost)", conf)
	}
}

func TestClassifyBead_PriorityBoostHigh(t *testing.T) {
	b := newTestBead("b10b", "high priority bug", beads.TypeBug, beads.PriorityHigh, "scanner")
	_, conf := ClassifyBead(b)
	if !confEq(conf, 0.8) {
		t.Errorf("confidence = %f, want 0.8 (0.7 + 0.1 boost for high priority)", conf)
	}
}

func TestClassifyBead_PriorityBoostCappedAt1(t *testing.T) {
	b := newTestBead("b10c", "critical regression broke again", beads.TypeBug, beads.PriorityCritical, "scanner")
	b.Metadata["finding_type"] = "regression"
	_, conf := ClassifyBead(b)
	if !confEq(conf, 0.9) {
		t.Errorf("confidence = %f, want 0.9 (0.8 + 0.1 boost)", conf)
	}
}

func TestClassifyBead_KeywordRefinement(t *testing.T) {
	b := newTestBead("b11", "misc task", beads.TypeTask, beads.PriorityMinor, "scanner")
	b.Notes = "This is a regression that broke the build again after the last deploy."
	ft, conf := ClassifyBead(b)
	if ft != FactRegression {
		t.Errorf("type = %s, want regression (keyword refinement)", ft)
	}
	if !confEq(conf, 0.8) {
		t.Errorf("confidence = %f, want 0.8 (keyword refinement)", conf)
	}
}

func TestClassifyBead_NilMetadata(t *testing.T) {
	b := &beads.Bead{
		ID:       "b12",
		Title:    "nil metadata bead",
		Type:     beads.TypeBug,
		Status:   beads.StatusDone,
		Priority: beads.PriorityMedium,
		Metadata: nil,
	}
	ft, conf := ClassifyBead(b)
	if ft != FactGotcha {
		t.Errorf("type = %s, want gotcha", ft)
	}
	if !confEq(conf, 0.7) {
		t.Errorf("confidence = %f, want 0.7", conf)
	}
}

func TestClassifyBead_UnknownType(t *testing.T) {
	b := &beads.Bead{
		ID:       "b13",
		Title:    "unknown type bead",
		Type:     beads.BeadType("unknown"),
		Status:   beads.StatusDone,
		Priority: beads.PriorityMedium,
		Metadata: make(map[string]interface{}),
	}
	ft, _ := ClassifyBead(b)
	if ft != "" {
		t.Errorf("type = %s, want empty (unknown type should be skipped)", ft)
	}
}

// --- classifyByKeywords Tests ---

func TestClassifyByKeywords_Empty(t *testing.T) {
	ft, _ := classifyByKeywords("")
	if ft != "" {
		t.Errorf("type = %s, want empty for empty text", ft)
	}
}

func TestClassifyByKeywords_Regression(t *testing.T) {
	ft, conf := classifyByKeywords("This broke again after deploy")
	if ft != FactRegression {
		t.Errorf("type = %s, want regression", ft)
	}
	if !confEq(conf, 0.8) {
		t.Errorf("confidence = %f, want 0.8", conf)
	}
}

func TestClassifyByKeywords_Gotcha(t *testing.T) {
	ft, conf := classifyByKeywords("You must always validate inputs")
	if ft != FactGotcha {
		t.Errorf("type = %s, want gotcha", ft)
	}
	if !confEq(conf, 0.7) {
		t.Errorf("confidence = %f, want 0.7", conf)
	}
}

func TestClassifyByKeywords_Decision(t *testing.T) {
	ft, conf := classifyByKeywords("We decided to use Redis going forward")
	if ft != FactDecision {
		t.Errorf("type = %s, want decision", ft)
	}
	if !confEq(conf, 0.7) {
		t.Errorf("confidence = %f, want 0.7", conf)
	}
}

func TestClassifyByKeywords_Pattern(t *testing.T) {
	ft, conf := classifyByKeywords("The convention is to use PascalCase")
	if ft != FactPattern {
		t.Errorf("type = %s, want pattern", ft)
	}
	if !confEq(conf, 0.6) {
		t.Errorf("confidence = %f, want 0.6", conf)
	}
}

func TestClassifyByKeywords_TestScaffold(t *testing.T) {
	ft, conf := classifyByKeywords("Add a test for this mock behavior")
	if ft != FactTestScaff {
		t.Errorf("type = %s, want test_scaffold", ft)
	}
	if !confEq(conf, 0.5) {
		t.Errorf("confidence = %f, want 0.5", conf)
	}
}

func TestClassifyByKeywords_NoMatch(t *testing.T) {
	ft, _ := classifyByKeywords("Generic comment about stuff")
	if ft != "" {
		t.Errorf("type = %s, want empty (no keyword match)", ft)
	}
}

// --- Body Building Tests ---

func TestBuildFactBody_AllFields(t *testing.T) {
	b := newTestBead("b20", "ListDisks misses LUKS volumes", beads.TypeBug, beads.PriorityHigh, "scanner")
	b.Notes = "probe.go:142 only globs /dev/disk/by-path"
	b.Metadata["file"] = "internal/fcos/probe.go"
	b.Metadata["detail"] = "The glob pattern misses /dev/mapper entries"
	b.Metadata["severity"] = "High"
	b.Metadata["recommendation"] = "Add /dev/mapper to glob list"
	b.ExternalRef = "gh-645"

	body := BuildFactBody(b, "scanner")

	if !strings.Contains(body, "probe.go:142") {
		t.Error("body missing notes")
	}
	if !strings.Contains(body, "File: internal/fcos/probe.go") {
		t.Error("body missing file metadata")
	}
	if !strings.Contains(body, "The glob pattern misses") {
		t.Error("body missing detail metadata")
	}
	if !strings.Contains(body, "Severity: High") {
		t.Error("body missing severity metadata")
	}
	if !strings.Contains(body, "Recommendation: Add /dev/mapper") {
		t.Error("body missing recommendation metadata")
	}
	if !strings.Contains(body, "External: gh-645") {
		t.Error("body missing external ref")
	}
	if !strings.Contains(body, "Source: bead:scanner/b20") {
		t.Error("body missing source tracking")
	}
}

func TestBuildFactBody_SparseMetadata(t *testing.T) {
	b := newTestBead("b21", "simple finding", beads.TypeAdvisory, beads.PriorityMedium, "quality")
	b.Notes = "Just a note about something"

	body := BuildFactBody(b, "quality")

	if !strings.Contains(body, "Just a note about something") {
		t.Error("body missing notes")
	}
	if !strings.Contains(body, "Source: bead:quality/b21") {
		t.Error("body missing source tracking")
	}
	if strings.Contains(body, "File:") {
		t.Error("body should not contain File when not set")
	}
	if strings.Contains(body, "Severity:") {
		t.Error("body should not contain Severity when not set")
	}
}

func TestBuildFactBody_EmptyNotes(t *testing.T) {
	b := newTestBead("b22", "no notes bead", beads.TypeBug, beads.PriorityMedium, "scanner")
	b.Metadata["file"] = "main.go"
	b.Metadata["recommendation"] = "fix the bug"

	body := BuildFactBody(b, "scanner")

	if strings.HasPrefix(body, "\n\n") {
		t.Error("body should not start with empty lines when notes are empty")
	}
	if !strings.Contains(body, "File: main.go") {
		t.Error("body missing file metadata")
	}
	if !strings.Contains(body, "Recommendation: fix the bug") {
		t.Error("body missing recommendation")
	}
}

func TestBuildFactBody_SourceTracking(t *testing.T) {
	b := newTestBead("abc123", "test source", beads.TypeAdvisory, beads.PriorityMedium, "architect")

	body := BuildFactBody(b, "architect")

	if !strings.Contains(body, "Source: bead:architect/abc123") {
		t.Errorf("expected source 'bead:architect/abc123' in body: %s", body)
	}
}

func TestBuildFactBody_NonStringMetadata(t *testing.T) {
	b := newTestBead("b23", "non string metadata", beads.TypeBug, beads.PriorityMedium, "scanner")
	b.Metadata["file"] = 42
	b.Metadata["severity"] = true

	body := BuildFactBody(b, "scanner")

	if !strings.Contains(body, "File: 42") {
		t.Error("Meta() should stringify non-string values via fmt.Sprintf")
	}
	if !strings.Contains(body, "Severity: true") {
		t.Error("Meta() should stringify bool values")
	}
}

func TestBuildFactBody_Truncation(t *testing.T) {
	b := newTestBead("b24", "long body", beads.TypeBug, beads.PriorityMedium, "scanner")
	b.Notes = strings.Repeat("x", 600)

	body := BuildFactBody(b, "scanner")

	if len([]rune(body)) > beadSynthMaxBodyLen+3 {
		t.Errorf("body length = %d, want <= %d (truncated)", len([]rune(body)), beadSynthMaxBodyLen+3)
	}
	if !strings.HasSuffix(body, "...") {
		t.Error("truncated body should end with ...")
	}
}

// --- extractBeadTags Tests ---

func TestExtractBeadTags(t *testing.T) {
	b := newTestBead("b25", "kubernetes deployment issue", beads.TypeBug, beads.PriorityMedium, "scanner")
	b.Notes = "The docker container fails with a Go compilation error"

	tags := extractBeadTags(b)

	tagSet := make(map[string]bool)
	for _, tag := range tags {
		tagSet[tag] = true
	}
	if !tagSet["kubernetes"] {
		t.Error("expected 'kubernetes' tag")
	}
	if !tagSet["docker"] {
		t.Error("expected 'docker' tag")
	}
	if !tagSet["go"] {
		t.Error("expected 'go' tag")
	}
}

// --- Deduplication Tests ---

func TestHasMatchingFact_ExactMatch(t *testing.T) {
	existing := []Fact{{Title: "guard .join() against undefined"}}
	if !hasMatchingFact(existing, "guard .join() against undefined") {
		t.Error("should match exact title")
	}
}

func TestHasMatchingFact_CaseInsensitive(t *testing.T) {
	existing := []Fact{{Title: "Guard .Join() Against Undefined"}}
	if !hasMatchingFact(existing, "guard .join() against undefined") {
		t.Error("should match case-insensitively")
	}
}

func TestHasMatchingFact_Substring(t *testing.T) {
	existing := []Fact{{Title: "always guard .join() against undefined arrays"}}
	if !hasMatchingFact(existing, "guard .join() against undefined") {
		t.Error("should match substring")
	}
}

func TestHasMatchingFact_NoMatch(t *testing.T) {
	existing := []Fact{{Title: "completely different fact"}}
	if hasMatchingFact(existing, "guard .join() against undefined") {
		t.Error("should not match unrelated fact")
	}
}

func TestHasMatchingFact_EmptyExisting(t *testing.T) {
	if hasMatchingFact(nil, "some title") {
		t.Error("should not match against empty list")
	}
}

// --- ParseSynthSchedule Tests ---

func TestParseSynthSchedule(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"hourly", time.Hour},
		{"Hourly", time.Hour},
		{"HOURLY", time.Hour},
		{"daily", 24 * time.Hour},
		{"Daily", 24 * time.Hour},
		{"", time.Hour},
		{"invalid", time.Hour},
		{" hourly ", time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ParseSynthSchedule(tt.input)
			if got != tt.want {
				t.Errorf("ParseSynthSchedule(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// --- Integration Tests (with mock wiki server) ---

func newMockWikiServer(t *testing.T) (*httptest.Server, *[]ExtractedFact) {
	t.Helper()
	var ingested []ExtractedFact

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/ingest" && r.Method == http.MethodPost:
			var facts []ExtractedFact
			if err := json.NewDecoder(r.Body).Decode(&facts); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			ingested = append(ingested, facts...)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/search":
			json.NewEncoder(w).Encode(map[string]interface{}{"results": []interface{}{}, "total": 0})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{"results": []interface{}{}, "total": 0})
		}
	}))
	t.Cleanup(srv.Close)

	return srv, &ingested
}

func newTestStore(t *testing.T) *beads.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("failed to create bead store: %v", err)
	}
	return store
}

func newTestSynthesizer(t *testing.T, stores map[string]*beads.Store, wikiURL string) *BeadSynthesizer {
	t.Helper()
	layers := []LayerConfig{{Type: LayerProject, URL: wikiURL}}
	api := NewKnowledgeAPI(layers, KnowledgeConfig{
		Enabled: true,
		Engine:  "llm-wiki",
	}, synthTestLogger())

	return NewBeadSynthesizer(stores, api, BeadSynthesizerConfig{
		Schedule:         "hourly",
		MinConfidence:    0.5,
		TargetLayer:      "project",
		MaxFactsPerCycle: 20,
		VaultPath:        t.TempDir(),
	}, synthTestLogger(), nil)
}

func TestRunSynthesis_BasicFlow(t *testing.T) {
	srv, ingested := newMockWikiServer(t)
	store := newTestStore(t)

	b, err := store.Create("critical bug found in auth handler", beads.TypeBug, beads.PriorityHigh, "scanner", "gh-100")
	if err != nil {
		t.Fatal(err)
	}
	store.Update(b.ID, func(b *beads.Bead) { b.Notes = "Auth handler allows unauthenticated access to admin endpoints." })
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}

	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	count, err := synth.RunSynthesis(context.Background())
	if err != nil {
		t.Fatalf("RunSynthesis failed: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	if len(*ingested) != 1 {
		t.Errorf("ingested = %d, want 1", len(*ingested))
	}

	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	updatedBead, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedBead.Meta(beadSynthMetaKeySynthesizedAt) == "" {
		t.Error("bead should be marked with synthesized_at after synthesis")
	}
}

func TestRunSynthesis_AlreadySynthesized(t *testing.T) {
	srv, ingested := newMockWikiServer(t)
	store := newTestStore(t)

	b, _ := store.Create("already synthesized finding", beads.TypeBug, beads.PriorityMedium, "scanner", "")
	store.Close(b.ID)
	store.SetMetadata(b.ID, beadSynthMetaKeySynthesizedAt, time.Now().UTC().Format(time.RFC3339))

	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	count, err := synth.RunSynthesis(context.Background())
	if err != nil {
		t.Fatalf("RunSynthesis failed: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 (already synthesized)", count)
	}
	if len(*ingested) != 0 {
		t.Errorf("ingested = %d, want 0", len(*ingested))
	}
}

func TestRunSynthesis_MaxFactsPerCycle(t *testing.T) {
	srv, ingested := newMockWikiServer(t)
	store := newTestStore(t)

	for i := 0; i < 5; i++ {
		b, _ := store.Create("bug finding number something", beads.TypeBug, beads.PriorityMedium, "scanner", "")
		store.Close(b.ID)
	}

	stores := map[string]*beads.Store{"scanner": store}
	layers := []LayerConfig{{Type: LayerProject, URL: srv.URL}}
	api := NewKnowledgeAPI(layers, KnowledgeConfig{Enabled: true}, synthTestLogger())

	synth := NewBeadSynthesizer(stores, api, BeadSynthesizerConfig{
		MinConfidence:    0.5,
		TargetLayer:      "project",
		MaxFactsPerCycle: 2,
		VaultPath:        t.TempDir(),
	}, synthTestLogger(), nil)

	count, err := synth.RunSynthesis(context.Background())
	if err != nil {
		t.Fatalf("RunSynthesis failed: %v", err)
	}
	if count > 2 {
		t.Errorf("count = %d, want <= 2 (MaxFactsPerCycle)", count)
	}
	if len(*ingested) > 2 {
		t.Errorf("ingested = %d, want <= 2", len(*ingested))
	}
}

func TestRunSynthesis_MinConfidenceFilter(t *testing.T) {
	srv, ingested := newMockWikiServer(t)
	store := newTestStore(t)

	b, _ := store.Create("chore: update deps", beads.TypeChore, beads.PriorityMinor, "ci-maintainer", "")
	store.Close(b.ID)

	b2, _ := store.Create("low confidence task", beads.TypeTask, beads.PriorityMinor, "scanner", "")
	store.Close(b2.ID)

	stores := map[string]*beads.Store{"ci-maintainer": store}
	layers := []LayerConfig{{Type: LayerProject, URL: srv.URL}}
	api := NewKnowledgeAPI(layers, KnowledgeConfig{Enabled: true}, synthTestLogger())

	synth := NewBeadSynthesizer(stores, api, BeadSynthesizerConfig{
		MinConfidence:    0.6,
		TargetLayer:      "project",
		MaxFactsPerCycle: 20,
		VaultPath:        t.TempDir(),
	}, synthTestLogger(), nil)

	count, err := synth.RunSynthesis(context.Background())
	if err != nil {
		t.Fatalf("RunSynthesis failed: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 (chore skipped, task below threshold)", count)
	}
	if len(*ingested) != 0 {
		t.Errorf("ingested = %d, want 0", len(*ingested))
	}
}

func TestPREnricher_ParseRef_GhFormat(t *testing.T) {
	e := NewPREnricher(nil, "kubestellar", []string{"console", "docs"}, synthTestLogger())

	owner, repo, num := e.parseRef("gh-42")
	if owner != "kubestellar" || repo != "" || num != 42 {
		t.Errorf("parseRef(gh-42) = %q, %q, %d; want kubestellar, (empty), 42", owner, repo, num)
	}
}

func TestPREnricher_ParseRef_RepoHash(t *testing.T) {
	e := NewPREnricher(nil, "kubestellar", []string{"console"}, synthTestLogger())

	owner, repo, num := e.parseRef("docs#123")
	if owner != "kubestellar" || repo != "docs" || num != 123 {
		t.Errorf("parseRef(docs#123) = %q, %q, %d; want kubestellar, docs, 123", owner, repo, num)
	}
}

func TestPREnricher_ParseRef_OrgRepoHash(t *testing.T) {
	e := NewPREnricher(nil, "kubestellar", nil, synthTestLogger())

	owner, repo, num := e.parseRef("other-org/my-repo#99")
	if owner != "other-org" || repo != "my-repo" || num != 99 {
		t.Errorf("parseRef(other-org/my-repo#99) = %q, %q, %d; want other-org, my-repo, 99", owner, repo, num)
	}
}

func TestPREnricher_ParseRef_PrefixedRepoHash(t *testing.T) {
	e := NewPREnricher(nil, "kubestellar", []string{"console"}, synthTestLogger())

	owner, repo, num := e.parseRef("gh-docs#123")
	if owner != "kubestellar" || repo != "docs" || num != 123 {
		t.Errorf("parseRef(gh-docs#123) = %q, %q, %d; want kubestellar, docs, 123", owner, repo, num)
	}
}

func TestPREnricher_ParseRef_PrefixedOrgRepoHash(t *testing.T) {
	e := NewPREnricher(nil, "kubestellar", nil, synthTestLogger())

	owner, repo, num := e.parseRef("gh-other-org/my-repo#99")
	if owner != "other-org" || repo != "my-repo" || num != 99 {
		t.Errorf("parseRef(gh-other-org/my-repo#99) = %q, %q, %d; want other-org, my-repo, 99", owner, repo, num)
	}
}

func TestPREnricher_ParseRef_Invalid(t *testing.T) {
	e := NewPREnricher(nil, "kubestellar", nil, synthTestLogger())

	_, _, num := e.parseRef("not-a-ref")
	if num != 0 {
		t.Errorf("parseRef(not-a-ref) num = %d; want 0", num)
	}
}

func TestPREnricher_FallbackOnNilClient(t *testing.T) {
	e := NewPREnricher(nil, "kubestellar", []string{"console"}, synthTestLogger())

	b := &beads.Bead{ID: "abc", ExternalRef: "gh-1"}
	body := e.EnrichBody(context.Background(), b, "scanner")
	if body != "" {
		t.Errorf("EnrichBody with nil client should return empty, got %q", body)
	}
}

func TestIsLowQualityBead(t *testing.T) {
	tests := []struct {
		title string
		notes string
		want  bool
	}{
		{"Merged: console#100 — add feature", "", true},
		{"Merged: docs#5 — fix typo", "some notes", false},
		{"Found a bug in auth handler", "", true},
		{"XSS regression in sanitizeHtmlForMdx", "", true},
		{"CSP connect-src bare wss: wildcard", "important detail here", false},
	}

	for _, tt := range tests {
		b := &beads.Bead{Title: tt.title, Notes: tt.notes}
		got := isLowQualityBead(b)
		if got != tt.want {
			t.Errorf("isLowQualityBead(%q, %q) = %v; want %v", tt.title, tt.notes, got, tt.want)
		}
	}
}

func TestIsJunkFact(t *testing.T) {
	junk := "---\ntitle: Merged: console-marketplace#228\ntype: pattern\n---\n\n- External: gh-228\n- Source: bead:scanner/abc123\n"
	if !isJunkFact(junk) {
		t.Error("expected junk fact to be detected")
	}

	good := "---\ntitle: Auth handler pattern\ntype: pattern\n---\n\nAlways validate OAuth tokens before checking session cookies.\n- Source: bead:scanner/xyz\n"
	if isJunkFact(good) {
		t.Error("expected good fact not to be junk")
	}

	metadataOnly := "---\ntitle: Something else\ntype: gotcha\n---\n\n- External: gh-5\n- Source: bead:scanner/def\n"
	if !isJunkFact(metadataOnly) {
		t.Error("fact with only metadata lines and no substantive content should be junk")
	}
}

func TestCleanupVault_RemovesJunk(t *testing.T) {
	vaultDir := t.TempDir()

	mergeJunk := "---\ntitle: Merged: console#100 — updated README\ntype: pattern\nconfidence: 0.70\nsource: bead:scanner/abc\n---\n\n- External: gh-100\n- Source: bead:scanner/abc\n"
	metadataOnlyJunk := "---\ntitle: console: merged CSP wss wildcard removal PR#17301\ntype: pattern\nconfidence: 0.60\n---\n\n- External: gh-17301\n- Source: bead:scanner/ef3346c5-501\n"
	good := "---\ntitle: Always check OAuth scopes\ntype: gotcha\nconfidence: 0.80\n---\n\nWhen handling OAuth callbacks, always verify the returned scopes match what was requested.\n- Source: bead:guide/xyz\n"

	os.WriteFile(filepath.Join(vaultDir, "merged-console100.md"), []byte(mergeJunk), 0644)
	os.WriteFile(filepath.Join(vaultDir, "csp-wss-removal.md"), []byte(metadataOnlyJunk), 0644)
	os.WriteFile(filepath.Join(vaultDir, "oauth-scopes.md"), []byte(good), 0644)

	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, synthTestLogger())
	synth := NewBeadSynthesizer(nil, api, BeadSynthesizerConfig{
		VaultPath: vaultDir,
	}, synthTestLogger(), nil)

	removed, err := synth.CleanupVault()
	if err != nil {
		t.Fatalf("CleanupVault failed: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d; want 2", removed)
	}

	entries, _ := os.ReadDir(vaultDir)
	mdCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			mdCount++
		}
	}
	if mdCount != 1 {
		t.Errorf("remaining md files = %d; want 1", mdCount)
	}
}

func TestRetentionScore_Priority(t *testing.T) {
	policy := &RetentionPolicy{PreserveWithDeps: false}
	stores := map[string]*beads.Store{}
	lm := NewBeadLifecycleManager(stores, policy, synthTestLogger())

	now := time.Now().UTC()
	base := &beads.Bead{
		Type:     beads.TypeTask,
		Priority: beads.PriorityMinor,
		Status:   beads.StatusClosed,
	}

	critical := *base
	critical.Priority = beads.PriorityCritical

	scoreLow := lm.retentionScore(base, "scanner", now)
	scoreHigh := lm.retentionScore(&critical, "scanner", now)

	if scoreHigh <= scoreLow {
		t.Errorf("critical priority score (%f) should be higher than minor (%f)", scoreHigh, scoreLow)
	}
}

func TestRetentionScore_SynthDecay(t *testing.T) {
	policy := &RetentionPolicy{
		ArchiveAfterSynthDays: 7,
		PreserveWithDeps:      false,
	}
	stores := map[string]*beads.Store{}
	lm := NewBeadLifecycleManager(stores, policy, synthTestLogger())

	now := time.Now().UTC()
	eightDaysAgo := now.Add(-8 * 24 * time.Hour)

	unsynthed := &beads.Bead{
		Type:     beads.TypeBug,
		Priority: beads.PriorityMedium,
		Status:   beads.StatusClosed,
	}

	synthed := *unsynthed
	synthed.Metadata = map[string]interface{}{
		"synthesized_at": eightDaysAgo.Format(time.RFC3339),
	}

	scoreUnsynthed := lm.retentionScore(unsynthed, "scanner", now)
	scoreSynthed := lm.retentionScore(&synthed, "scanner", now)

	if scoreSynthed >= scoreUnsynthed {
		t.Errorf("synthesized bead score (%f) should be lower than unsynthesized (%f)", scoreSynthed, scoreUnsynthed)
	}
}

func TestBeadArchive(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b, err := store.Create("test bead", beads.TypeBug, beads.PriorityHigh, "scanner", "gh-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store.Close(b.ID)

	if store.Count() != 1 {
		t.Fatalf("count = %d before archive; want 1", store.Count())
	}

	if err := store.Archive(b.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if store.Count() != 0 {
		t.Errorf("count = %d after archive; want 0", store.Count())
	}

	archiveData, err := os.ReadFile(filepath.Join(dir, "archive.jsonl"))
	if err != nil {
		t.Fatalf("reading archive: %v", err)
	}
	if !strings.Contains(string(archiveData), "test bead") {
		t.Error("archive.jsonl should contain the bead title")
	}

	var entry beads.ArchivedBead
	if err := json.Unmarshal(archiveData[:len(archiveData)-1], &entry); err != nil {
		t.Fatalf("unmarshal archive entry: %v", err)
	}
	if entry.ID != b.ID {
		t.Errorf("archived ID = %q; want %q", entry.ID, b.ID)
	}
}

func TestRunSynthesis_MultipleAgents(t *testing.T) {
	srv, ingested := newMockWikiServer(t)
	store1 := newTestStore(t)
	store2 := newTestStore(t)

	b1, _ := store1.Create("scanner found auth bug", beads.TypeBug, beads.PriorityMedium, "scanner", "")
	store1.Update(b1.ID, func(b *beads.Bead) { b.Notes = "Auth handler allows unauthenticated access." })
	store1.Close(b1.ID)

	b2, _ := store2.Create("quality found test gap", beads.TypeAdvisory, beads.PriorityMedium, "quality", "")
	store2.Update(b2.ID, func(b *beads.Bead) { b.Notes = "Missing integration tests for auth flow." })
	store2.SetMetadata(b2.ID, "finding_type", "test")
	store2.Close(b2.ID)

	stores := map[string]*beads.Store{"scanner": store1, "quality": store2}
	synth := newTestSynthesizer(t, stores, srv.URL)

	count, err := synth.RunSynthesis(context.Background())
	if err != nil {
		t.Fatalf("RunSynthesis failed: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (one from each agent)", count)
	}
	if len(*ingested) != 2 {
		t.Errorf("ingested = %d, want 2", len(*ingested))
	}
}

func TestRunSynthesis_EmptyStores(t *testing.T) {
	srv, _ := newMockWikiServer(t)
	store := newTestStore(t)

	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	count, err := synth.RunSynthesis(context.Background())
	if err != nil {
		t.Fatalf("RunSynthesis failed: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 (empty stores)", count)
	}
}

func TestRunSynthesis_OpenBeadsIgnored(t *testing.T) {
	srv, ingested := newMockWikiServer(t)
	store := newTestStore(t)

	store.Create("open bug", beads.TypeBug, beads.PriorityMedium, "scanner", "")

	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	count, err := synth.RunSynthesis(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 (open beads should be ignored)", count)
	}
	if len(*ingested) != 0 {
		t.Errorf("ingested = %d, want 0", len(*ingested))
	}
}

func TestDedup_SameTitleWithinBatch(t *testing.T) {
	srv, ingested := newMockWikiServer(t)
	store := newTestStore(t)

	b1, _ := store.Create("duplicate finding title", beads.TypeBug, beads.PriorityMedium, "scanner", "")
	store.Update(b1.ID, func(b *beads.Bead) { b.Notes = "Found a duplicate issue in the scanner." })
	store.Close(b1.ID)
	b2, _ := store.Create("duplicate finding title", beads.TypeBug, beads.PriorityMedium, "scanner", "")
	store.Update(b2.ID, func(b *beads.Bead) { b.Notes = "Found a duplicate issue in the scanner." })
	store.Close(b2.ID)

	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	count, err := synth.RunSynthesis(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (dedup within batch)", count)
	}
	if len(*ingested) != 1 {
		t.Errorf("ingested = %d, want 1", len(*ingested))
	}
}

func TestDedup_CrossAgent(t *testing.T) {
	srv, ingested := newMockWikiServer(t)
	store1 := newTestStore(t)
	store2 := newTestStore(t)

	b1, _ := store1.Create("same finding from both agents", beads.TypeBug, beads.PriorityMedium, "scanner", "")
	store1.Update(b1.ID, func(b *beads.Bead) { b.Notes = "Cross-agent finding details." })
	store1.Close(b1.ID)
	b2, _ := store2.Create("same finding from both agents", beads.TypeBug, beads.PriorityMedium, "quality", "")
	store2.Update(b2.ID, func(b *beads.Bead) { b.Notes = "Cross-agent finding details." })
	store2.Close(b2.ID)

	stores := map[string]*beads.Store{"scanner": store1, "quality": store2}
	synth := newTestSynthesizer(t, stores, srv.URL)

	count, err := synth.RunSynthesis(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (cross-agent dedup)", count)
	}
	if len(*ingested) != 1 {
		t.Errorf("ingested = %d, want 1", len(*ingested))
	}
}

func TestDedup_ExistingWikiFact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"results": []map[string]interface{}{
					{
						"slug":       "auth-bypass-in-handler",
						"title":      "auth bypass in handler",
						"type":       "gotcha",
						"confidence": 0.8,
					},
				},
				"total": 1,
			})
		case "/api/ingest":
			w.WriteHeader(http.StatusOK)
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{"results": []interface{}{}, "total": 0})
		}
	}))
	defer srv.Close()

	store := newTestStore(t)
	b, _ := store.Create("auth bypass in handler", beads.TypeBug, beads.PriorityMedium, "scanner", "")
	store.Close(b.ID)

	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	count, err := synth.RunSynthesis(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 (should match existing wiki fact)", count)
	}
}

// --- Vault Fallback Tests ---

func TestIngestFact_FallbackToVaultFile(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, synthTestLogger())
	synth := &BeadSynthesizer{
		knowledgeAPI: api,
		config: BeadSynthesizerConfig{
			TargetLayer: "project",
		},
		logger: synthTestLogger(),
	}

	fact := ExtractedFact{
		Title:      "test vault fallback fact",
		Body:       "This is the body of the fact",
		Type:       FactGotcha,
		Confidence: 0.7,
		Tags:       []string{"go", "testing"},
		SourcePR:   "bead:scanner/b1",
	}

	err := synth.ingestFact(context.Background(), fact)
	if err == nil {
		return
	}

	if !strings.Contains(err.Error(), "no configured endpoint") && !strings.Contains(err.Error(), "creating vault dir") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWriteFactToVault_Roundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, synthTestLogger())
	synth := &BeadSynthesizer{
		knowledgeAPI: api,
		config:       BeadSynthesizerConfig{TargetLayer: "project"},
		logger:       synthTestLogger(),
		vaultBaseDir: tmpDir,
	}

	fact := ExtractedFact{
		Title:      "vault roundtrip test",
		Body:       "fact body for vault write",
		Type:       FactGotcha,
		Confidence: 0.75,
		Tags:       []string{"go", "testing"},
		SourcePR:   "bead:scanner/rt1",
	}

	if err := synth.writeFactToVault(fact); err != nil {
		t.Fatalf("writeFactToVault failed: %v", err)
	}

	slug := slugify(fact.Title)
	path := filepath.Join(tmpDir, slug+".md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read vault file: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, "title: vault roundtrip test") {
		t.Error("file missing title")
	}
	if !strings.Contains(s, "type: gotcha") {
		t.Error("file missing type")
	}
	if !strings.Contains(s, "layer: project") {
		t.Error("file missing layer")
	}
	if !strings.Contains(s, "tags: [go, testing]") {
		t.Error("file missing tags")
	}
	if !strings.Contains(s, "fact body for vault write") {
		t.Error("file missing body")
	}
}

func TestWriteFactToVault_NoTags(t *testing.T) {
	tmpDir := t.TempDir()
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, synthTestLogger())
	synth := &BeadSynthesizer{
		knowledgeAPI: api,
		config:       BeadSynthesizerConfig{TargetLayer: "project"},
		logger:       synthTestLogger(),
		vaultBaseDir: tmpDir,
	}

	fact := ExtractedFact{
		Title:      "no tags fact",
		Body:       "body",
		Type:       FactDecision,
		Confidence: 0.6,
		SourcePR:   "bead:arch/nt1",
	}

	if err := synth.writeFactToVault(fact); err != nil {
		t.Fatal(err)
	}

	slug := slugify(fact.Title)
	content, _ := os.ReadFile(filepath.Join(tmpDir, slug+".md"))
	if strings.Contains(string(content), "tags:") {
		t.Error("file should not contain tags line when tags are empty")
	}
}

func TestIngestFact_VaultFallbackOnNoEndpoint(t *testing.T) {
	tmpDir := t.TempDir()
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, synthTestLogger())
	synth := &BeadSynthesizer{
		knowledgeAPI: api,
		config:       BeadSynthesizerConfig{TargetLayer: "project"},
		logger:       synthTestLogger(),
		vaultBaseDir: tmpDir,
	}

	fact := ExtractedFact{
		Title:      "fallback test fact",
		Body:       "body",
		Type:       FactPattern,
		Confidence: 0.5,
		SourcePR:   "bead:scanner/fb1",
	}

	err := synth.ingestFact(context.Background(), fact)
	if err != nil {
		t.Fatalf("ingestFact should fall back to vault, got: %v", err)
	}

	slug := slugify(fact.Title)
	path := filepath.Join(tmpDir, slug+".md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("vault fallback file should exist")
	}
}

func TestRunOnce_LogsSuccess(t *testing.T) {
	srv, _ := newMockWikiServer(t)
	store := newTestStore(t)

	b, _ := store.Create("finding for runOnce test", beads.TypeBug, beads.PriorityMedium, "scanner", "")
	store.Update(b.ID, func(b *beads.Bead) { b.Notes = "runOnce test bead with notes." })
	store.Close(b.ID)

	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	synth.runOnce(context.Background())

	store.Reload()
	updated, _ := store.Get(b.ID)
	if updated.Meta(beadSynthMetaKeySynthesizedAt) == "" {
		t.Error("runOnce should have synthesized the bead")
	}
}

func TestRunOnce_EmptyIsNoOp(t *testing.T) {
	srv, _ := newMockWikiServer(t)
	store := newTestStore(t)
	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	synth.runOnce(context.Background())
}

func TestWriteFactToVault_FileFormat(t *testing.T) {
	tmpDir := t.TempDir()

	fact := ExtractedFact{
		Title:      "Test Fact Title",
		Body:       "This is the fact body.",
		Type:       FactGotcha,
		Confidence: 0.75,
		Tags:       []string{"go", "testing"},
		SourcePR:   "bead:scanner/abc123",
	}

	vaultDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(vaultDir, 0o755)

	slug := slugify(fact.Title)
	path := filepath.Join(vaultDir, slug+".md")

	var buf strings.Builder
	buf.WriteString("---\n")
	buf.WriteString("title: " + fact.Title + "\n")
	buf.WriteString("type: " + string(fact.Type) + "\n")
	buf.WriteString("layer: project\n")
	buf.WriteString("confidence: 0.75\n")
	buf.WriteString("tags: [go, testing]\n")
	buf.WriteString("source: bead:scanner/abc123\n")
	buf.WriteString("synthesized: " + time.Now().UTC().Format(time.RFC3339) + "\n")
	buf.WriteString("---\n\n")
	buf.WriteString(fact.Body)

	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	s := string(content)
	if !strings.Contains(s, "title: Test Fact Title") {
		t.Error("file missing title frontmatter")
	}
	if !strings.Contains(s, "type: gotcha") {
		t.Error("file missing type frontmatter")
	}
	if !strings.Contains(s, "layer: project") {
		t.Error("file missing layer frontmatter")
	}
	if !strings.Contains(s, "confidence: 0.75") {
		t.Error("file missing confidence")
	}
	if !strings.Contains(s, "tags: [go, testing]") {
		t.Error("file missing tags")
	}
	if !strings.Contains(s, "source: bead:scanner/abc123") {
		t.Error("file missing source")
	}
	if !strings.Contains(s, "This is the fact body.") {
		t.Error("file missing body")
	}
}

// --- Background Loop Tests ---

func TestStart_ContextCancellation(t *testing.T) {
	srv, _ := newMockWikiServer(t)
	store := newTestStore(t)
	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		synth.Start(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

// --- Unsynthesized (beads.Store) Tests ---

func TestUnsynthesized_FiltersDoneAndClosed(t *testing.T) {
	store := newTestStore(t)

	store.Create("open bug", beads.TypeBug, beads.PriorityMedium, "scanner", "")

	b2, _ := store.Create("done bug", beads.TypeBug, beads.PriorityMedium, "scanner", "")
	store.Close(b2.ID)

	b3, _ := store.Create("closed bug", beads.TypeBug, beads.PriorityMedium, "scanner", "")
	store.Close(b3.ID)

	result := store.Unsynthesized()
	if len(result) != 2 {
		t.Errorf("Unsynthesized() returned %d beads, want 2 (done + closed)", len(result))
	}
}

func TestUnsynthesized_SkipsSynthesized(t *testing.T) {
	store := newTestStore(t)

	b, _ := store.Create("synthesized bug", beads.TypeBug, beads.PriorityMedium, "scanner", "")
	store.Close(b.ID)
	store.SetMetadata(b.ID, beadSynthMetaKeySynthesizedAt, time.Now().UTC().Format(time.RFC3339))

	result := store.Unsynthesized()
	if len(result) != 0 {
		t.Errorf("Unsynthesized() returned %d beads, want 0 (synthesized should be skipped)", len(result))
	}
}

func TestUnsynthesized_EmptyStore(t *testing.T) {
	store := newTestStore(t)
	result := store.Unsynthesized()
	if result != nil {
		t.Errorf("Unsynthesized() = %v, want nil for empty store", result)
	}
}

// --- IsEnabled Tests ---

func TestBeadSynthesizerConfig_IsEnabled_Default(t *testing.T) {
	cfg := BeadSynthesizerConfig{}
	if !cfg.IsEnabled() {
		t.Error("IsEnabled() should default to true when Enabled is nil")
	}
}

func TestBeadSynthesizerConfig_IsEnabled_ExplicitTrue(t *testing.T) {
	v := true
	cfg := BeadSynthesizerConfig{Enabled: &v}
	if !cfg.IsEnabled() {
		t.Error("IsEnabled() should be true when explicitly set to true")
	}
}

func TestBeadSynthesizerConfig_IsEnabled_ExplicitFalse(t *testing.T) {
	v := false
	cfg := BeadSynthesizerConfig{Enabled: &v}
	if cfg.IsEnabled() {
		t.Error("IsEnabled() should be false when explicitly set to false")
	}
}

func TestResolveBeadRelations(t *testing.T) {
	store := newTestStore(t)
	parent, _ := store.Create("auth handler fix", beads.TypeBug, beads.PriorityHigh, "scanner", "")
	child, _ := store.Create("login validation", beads.TypeTask, beads.PriorityMedium, "scanner", "")
	store.Close(parent.ID)
	store.Close(child.ID)

	store.Update(parent.ID, func(b *beads.Bead) {
		b.DependsOn = []string{child.ID}
	})

	stores := map[string]*beads.Store{"scanner": store}
	srv, _ := newMockWikiServer(t)
	synth := newTestSynthesizer(t, stores, srv.URL)

	reloaded, _ := store.Get(parent.ID)
	related := synth.resolveBeadRelations(reloaded, "scanner")

	if len(related) != 1 {
		t.Fatalf("expected 1 related slug, got %d", len(related))
	}
	want := slugify("login validation")
	if related[0] != want {
		t.Errorf("related[0] = %q, want %q", related[0], want)
	}
}

func TestResolveBeadRelations_NoDeps(t *testing.T) {
	store := newTestStore(t)
	b, _ := store.Create("standalone finding", beads.TypeBug, beads.PriorityMedium, "scanner", "")
	store.Close(b.ID)

	stores := map[string]*beads.Store{"scanner": store}
	srv, _ := newMockWikiServer(t)
	synth := newTestSynthesizer(t, stores, srv.URL)

	got, _ := store.Get(b.ID)
	related := synth.resolveBeadRelations(got, "scanner")
	if related != nil {
		t.Errorf("expected nil for bead with no deps, got %v", related)
	}
}

func TestResolveBeadRelations_CrossStore(t *testing.T) {
	storeA := newTestStore(t)
	storeB := newTestStore(t)
	dep, _ := storeB.Create("shared utility pattern", beads.TypeTask, beads.PriorityLow, "helper", "")
	storeB.Close(dep.ID)

	parent, _ := storeA.Create("main finding", beads.TypeBug, beads.PriorityHigh, "scanner", "")
	storeA.Close(parent.ID)
	storeA.Update(parent.ID, func(b *beads.Bead) {
		b.DependsOn = []string{dep.ID}
	})

	stores := map[string]*beads.Store{"scanner": storeA, "helper": storeB}
	srv, _ := newMockWikiServer(t)
	synth := newTestSynthesizer(t, stores, srv.URL)

	reloaded, _ := storeA.Get(parent.ID)
	related := synth.resolveBeadRelations(reloaded, "scanner")

	if len(related) != 1 {
		t.Fatalf("expected 1 related slug from cross-store lookup, got %d", len(related))
	}
	want := slugify("shared utility pattern")
	if related[0] != want {
		t.Errorf("related[0] = %q, want %q", related[0], want)
	}
}

func TestWriteFactToVault_RelatedFrontmatter(t *testing.T) {
	srv, _ := newMockWikiServer(t)
	store := newTestStore(t)
	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	fact := ExtractedFact{
		Title:      "test fact with relations",
		Body:       "some body content",
		Type:       FactGotcha,
		Confidence: 0.75,
		Tags:       []string{"testing"},
		Related:    []string{"auth-handler-fix", "login-validation"},
		SourcePR:   "bead:scanner/abc123",
		SourceDate: time.Now(),
	}

	if err := synth.writeFactToVault(fact); err != nil {
		t.Fatalf("writeFactToVault failed: %v", err)
	}

	slug := slugify(fact.Title)
	content, err := os.ReadFile(filepath.Join(synth.vaultBaseDir, slug+".md"))
	if err != nil {
		t.Fatalf("failed to read vault file: %v", err)
	}

	s := string(content)
	if !strings.Contains(s, "related: [auth-handler-fix, login-validation]") {
		t.Errorf("vault file missing related frontmatter, got:\n%s", s)
	}
}

func TestWriteFactToVault_EmitsGraphTriples(t *testing.T) {
	srv, _ := newMockWikiServer(t)
	store := newTestStore(t)
	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	graphPath := filepath.Join(t.TempDir(), "test-graph.db")
	gs, err := NewGraphStore(graphPath, synthTestLogger())
	if err != nil {
		t.Fatalf("failed to create graph store: %v", err)
	}
	defer gs.Close()
	synth.SetGraphStore(gs)

	fact := ExtractedFact{
		Title:      "parent fact",
		Body:       "body",
		Type:       FactPattern,
		Confidence: 0.9,
		Related:    []string{"child-a", "child-b"},
		SourcePR:   "bead:scanner/xyz",
		SourceDate: time.Now(),
	}

	if err := synth.writeFactToVault(fact); err != nil {
		t.Fatalf("writeFactToVault failed: %v", err)
	}

	factSlug := slugify(fact.Title)
	outgoing := gs.Outgoing(factSlug)
	if len(outgoing) != 2 {
		t.Fatalf("expected 2 outgoing triples, got %d", len(outgoing))
	}

	objects := map[string]bool{}
	for _, tr := range outgoing {
		if tr.Predicate != "related_to" {
			t.Errorf("unexpected predicate %q", tr.Predicate)
		}
		objects[tr.Object] = true
	}
	if !objects["child-a"] || !objects["child-b"] {
		t.Errorf("expected child-a and child-b in triples, got %v", objects)
	}
}
