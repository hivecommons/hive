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

// The embedded cncf-techdocs-assessment versions. Curated files are never
// edited in place, so each version is its own file and the catalogue keeps
// both (hivecommons/hive#18139 step 4 added 2026.11.2).
const (
	mktTechDocsName       = "cncf-techdocs-assessment"
	mktTechDocsVersion1   = "curated-2026.11.1"
	mktTechDocsVersion2   = "curated-2026.11.2"
	mktTechDocsKey        = mktTechDocsName + "@" + mktTechDocsVersion1
	mktTechDocsKeyLatest  = mktTechDocsName + "@" + mktTechDocsVersion2
	mktTechDocsVersionCnt = 2
)

// TestMarketplaceCuratedSeedPublishesAndIsIdempotent: the embedded curated
// models land at tier curated on the first seed; a second seed publishes
// nothing and leaves exactly the embedded versions per model, the newest
// last in the index.
func TestMarketplaceCuratedSeedPublishesAndIsIdempotent(t *testing.T) {
	s := marketplaceTestHub(t, nil)
	st := s.marketplace()
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)

	published, errs := s.seedCuratedModels(st, now)
	if len(errs) != 0 {
		t.Fatalf("seed errors: %v", errs)
	}
	if len(published) != mktCuratedCount(t) {
		t.Fatalf("published = %v", published)
	}
	seeded := map[string]bool{}
	for _, k := range published {
		seeded[k] = true
	}
	if !seeded[mktTechDocsKey] || !seeded[mktTechDocsKeyLatest] {
		t.Fatalf("published = %v; want both %s and %s", published, mktTechDocsKey, mktTechDocsKeyLatest)
	}
	raw, err := st.Get(mktTechDocsName, mktTechDocsVersion1)
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
	if b.Model.Spec.Intake.DerivesReferenceRepos() {
		t.Fatalf("%s must not declare intake fields: %+v", mktTechDocsKey, b.Model.Spec.Intake)
	}
	raw, err = st.Get(mktTechDocsName, mktTechDocsVersion2)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := bundle.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Tier != bundle.TierCurated || !latest.Model.Spec.Intake.DerivesReferenceRepos() {
		t.Fatalf("%s: tier=%s intake=%+v", mktTechDocsKeyLatest, latest.Tier, latest.Model.Spec.Intake)
	}

	published, errs = s.seedCuratedModels(st, now.Add(time.Hour))
	if len(published) != 0 || len(errs) != 0 {
		t.Fatalf("second seed: published=%v errs=%v", published, errs)
	}
	vs, err := st.Versions(mktTechDocsName)
	if err != nil || len(vs) != mktTechDocsVersionCnt {
		t.Fatalf("versions: %v %d", err, len(vs))
	}
	// The listing exposes both versions, oldest first, so the last entry is
	// the one a fresh import picks up.
	if vs[0].Version != mktTechDocsVersion1 || vs[len(vs)-1].Version != mktTechDocsVersion2 {
		t.Fatalf("versions order: %+v", vs)
	}
	idx, _, _, err := st.Index()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range idx.Entries {
		if e.Name == mktTechDocsName && (e.Tier != bundle.TierCurated || e.NewerIngestAvailable) {
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
		vs, err := s.marketplace().Versions(mktTechDocsName)
		return err == nil && len(vs) == mktTechDocsVersionCnt
	}, "curated models not seeded at startup")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StartMarketplaceRefresh did not stop on cancel")
	}
}
