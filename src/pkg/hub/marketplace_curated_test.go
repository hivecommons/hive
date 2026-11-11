package hub

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/hub/marketplace/curated"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/bundle"
)

const mktTechDocsKey = "cncf-techdocs-assessment@curated-2026.11.1"

// TestMarketplaceCuratedSeedPublishesAndIsIdempotent: the embedded curated
// models land at tier curated on the first seed; a second seed publishes
// nothing and leaves exactly one version per model.
func TestMarketplaceCuratedSeedPublishesAndIsIdempotent(t *testing.T) {
	s := marketplaceTestHub(t, nil)
	st := s.marketplace()
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)

	published, errs := s.seedCuratedModels(st, now)
	if len(errs) != 0 {
		t.Fatalf("seed errors: %v", errs)
	}
	if len(published) != mktCuratedCount(t) || published[0] != mktTechDocsKey {
		t.Fatalf("published = %v", published)
	}
	raw, err := st.Get("cncf-techdocs-assessment", "curated-2026.11.1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := bundle.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.Tier != bundle.TierCurated || b.CreatedBy != marketplaceCuratedBy || b.Model.Spec.Meta.Provenance != workflowmodel.ProvenanceCurated {
		t.Fatalf("bundle: tier=%s by=%s prov=%s", b.Tier, b.CreatedBy, b.Model.Spec.Meta.Provenance)
	}
	if b.Model.Spec.Scope.Repository != "cncf/techdocs" {
		t.Fatalf("scope: %+v", b.Model.Spec.Scope)
	}

	published, errs = s.seedCuratedModels(st, now.Add(time.Hour))
	if len(published) != 0 || len(errs) != 0 {
		t.Fatalf("second seed: published=%v errs=%v", published, errs)
	}
	vs, err := st.Versions("cncf-techdocs-assessment")
	if err != nil || len(vs) != 1 {
		t.Fatalf("versions: %v %d", err, len(vs))
	}
	idx, _, _, err := st.Index()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range idx.Entries {
		if e.Name == "cncf-techdocs-assessment" && (e.Tier != bundle.TierCurated || e.NewerIngestAvailable) {
			t.Fatalf("index entry: %+v", e)
		}
	}
}

// TestMarketplaceCuratedSeedRefusesBadModels: a loader error and a model
// that fails to compile are logged and skipped; the good model still seeds
// and nothing panics.
func TestMarketplaceCuratedSeedRefusesBadModels(t *testing.T) {
	s := marketplaceTestHub(t, nil)
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	good, errs := curated.Load()
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	broken := *good[0].Model
	broken.Metadata.Name = "broken-curated"
	broken.Spec.Columns = nil // compile refuses: states without columns
	old := marketplaceCuratedLoad
	t.Cleanup(func() { marketplaceCuratedLoad = old })
	marketplaceCuratedLoad = func() ([]curated.Entry, []error) {
		return append([]curated.Entry{{Path: "broken.yaml", Model: &broken}}, good...),
			[]error{errors.New("bad.yaml: curated: yaml: oops")}
	}

	published, seedErrs := s.seedCuratedModels(s.marketplace(), time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC))
	if len(published) != len(good) || len(seedErrs) != 2 {
		t.Fatalf("published=%v errs=%v", published, seedErrs)
	}
	logs := buf.String()
	for _, want := range []string{"curated model refused", "bad.yaml", "does not compile", "broken.yaml", "curated model published"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %q:\n%s", want, logs)
		}
	}
	if vs, err := s.marketplace().Versions("broken-curated"); err == nil && len(vs) != 0 {
		t.Fatalf("broken model must not be published: %v", vs)
	}

	// A refresh run surfaces the same errors without aborting ingest.
	res, err := s.runMarketplaceRefresh(context.Background(), []string{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Curated) != 0 || len(res.Errors) != 2 || !strings.HasPrefix(res.Errors[0], "curated: ") {
		t.Fatalf("refresh: %+v", res)
	}
}

// TestMarketplaceCuratedSeedConflictIsImmutable: editing a curated file
// without bumping its version is refused as a conflict; the stored bundle
// stays as first published.
func TestMarketplaceCuratedSeedConflictIsImmutable(t *testing.T) {
	s := marketplaceTestHub(t, nil)
	st := s.marketplace()
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	if _, errs := s.seedCuratedModels(st, now); len(errs) != 0 {
		t.Fatal(errs)
	}
	good, _ := curated.Load()
	edited := *good[0].Model
	edited.Spec.Meta.Description = "edited in place without a version bump"
	old := marketplaceCuratedLoad
	t.Cleanup(func() { marketplaceCuratedLoad = old })
	marketplaceCuratedLoad = func() ([]curated.Entry, []error) {
		return []curated.Entry{{Path: good[0].Path, Model: &edited}}, nil
	}
	published, errs := s.seedCuratedModels(st, now)
	if len(published) != 0 || len(errs) != 1 || !errors.Is(errs[0], errMarketplaceConflict) {
		t.Fatalf("published=%v errs=%v", published, errs)
	}
}

// TestMarketplaceStartRefreshSeedsCurated: startup seeds curated models
// before any ingest, with no GitHub token configured.
func TestMarketplaceStartRefreshSeedsCurated(t *testing.T) {
	s := marketplaceTestHub(t, nil)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("HIVE_HUB_GITHUB_TOKEN", "")
	t.Setenv(hubMarketplaceScheduleEnv, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.StartMarketplaceRefresh(ctx); close(done) }()
	testutil.Eventually(t, 5*time.Second, func() bool {
		vs, err := s.marketplace().Versions("cncf-techdocs-assessment")
		return err == nil && len(vs) == 1
	}, "curated model not seeded at startup")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StartMarketplaceRefresh did not stop on cancel")
	}
}
