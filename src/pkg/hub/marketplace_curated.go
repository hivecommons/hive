package hub

import (
	"time"

	"github.com/hivecommons/hive/pkg/hub/marketplace/curated"
	"github.com/hivecommons/hive/pkg/workflowmodel/bundle"
)

// RFC #17620 phase 7: curated seeding.
//
// The hub ships reviewed models under pkg/hub/marketplace/curated/*.yaml.
// seedCuratedModels compiles each, wraps it in a bundle at tier curated and
// publishes it when that name@version is not yet in the catalogue. Entries
// are immutable, so a changed file with an unchanged version is reported as
// a conflict rather than overwriting; a new version is a new file. Ingest
// never touches curated versions; marketplaceNormaliseIndex flags them
// newerIngestAvailable when a later ingested-unreviewed version exists.

// marketplaceCuratedBy is the publisher recorded on seeded bundles.
const marketplaceCuratedBy = "curated-seed"

// marketplaceCuratedLoad is swapped in tests to inject bad or extra models.
var marketplaceCuratedLoad = curated.Load

// seedCuratedModels publishes every embedded curated model that is absent
// from the catalogue. It returns the keys it published and the errors it
// met; nothing here fails the caller — a bad curated file is logged and
// skipped so the rest of the catalogue still seeds.
func (s *HubServer) seedCuratedModels(st *marketplaceStore, now time.Time) (published []string, errs []error) {
	entries, loadErrs := marketplaceCuratedLoad()
	for _, err := range loadErrs {
		s.logger.Error("marketplace: curated model refused", "error", err)
		errs = append(errs, err)
	}
	for _, e := range entries {
		b, err := bundle.Export(e.Model, marketplaceCuratedBy, now)
		if err != nil {
			s.logger.Error("marketplace: curated model does not compile", "path", e.Path, "error", err)
			errs = append(errs, err)
			continue
		}
		b.Tier = bundle.TierCurated
		entry, created, err := st.Publish(b, marketplaceCuratedBy, now, "")
		if err != nil {
			s.logger.Error("marketplace: curated model not published", "path", e.Path, "key", b.Key(), "error", err)
			errs = append(errs, err)
			continue
		}
		if created {
			s.logger.Info("marketplace: curated model published", "path", e.Path, "key", entry.Key())
			published = append(published, entry.Key())
		}
	}
	return published, errs
}
