package wavefront

import (
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/worksource"
)

func init() {
	worksource.RegisterAdditive(worksource.AdditiveWavefront, FromConfig)
}

// FromConfig builds the source from governor.work_source.wavefront. The
// factory only calls it when Enabled is true.
func FromConfig(cfg config.WorkSourceConfig, _ *slog.Logger) (worksource.WorkSource, error) {
	w := cfg.Wavefront
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return New(Options{Repo: w.Repo, Path: w.Path, URL: w.URL, ReceiptsDir: w.ReceiptsDir})
}
