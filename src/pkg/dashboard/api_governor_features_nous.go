package dashboard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// nousPatch is the Settings → Features "Nous / Experiment Lab" write shape
// (hivecommons/hive#12026): one optional field per nous.* leaf in hive.yaml.
// A nil pointer leaves that leaf untouched.
type nousPatch struct {
	dispatcher                  *string
	gatePollSeconds             *int
	fetchAllowlist              *[]string
	artifactRepo                *string
	artifactBranch              *string
	artifactPath                *string
	artifactSyncIntervalSeconds *int
}

// patchNousConfig applies p to a copy of cur and validates the result with
// the config package's own rules, so a value the dialog accepts is exactly a
// value `hive.yaml` would load. Nothing is mutated on error.
func patchNousConfig(cur config.NousConfig, p nousPatch) (config.NousConfig, error) {
	next := cur
	if p.dispatcher != nil {
		canonical, ok := config.CanonicalNousDispatcher(*p.dispatcher)
		if !ok {
			return cur, fmt.Errorf("nous dispatcher must be one of %s (or empty for %s)", strings.Join(config.NousDispatcherValues(), ", "), config.DefaultNousDispatcher)
		}
		next.Dispatcher = canonical
	}
	if p.gatePollSeconds != nil {
		if *p.gatePollSeconds < 0 {
			return cur, fmt.Errorf("nous gate_poll_seconds must be 0 (default %ds) or greater", int(config.DefaultNousGatePollInterval.Seconds()))
		}
		next.GatePollSeconds = *p.gatePollSeconds
	}
	if p.fetchAllowlist != nil {
		// An explicitly empty list is a real value (deny every fetch), so it
		// is kept as a non-nil empty slice rather than collapsing to the
		// absent default.
		hosts := make([]string, 0, len(*p.fetchAllowlist))
		for _, h := range *p.fetchAllowlist {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" {
				continue
			}
			if err := config.ValidateFetchAllowlistHost(h); err != nil {
				return cur, fmt.Errorf("nous %w", err)
			}
			hosts = append(hosts, h)
		}
		next.FetchAllowlist = hosts
	}
	if p.artifactRepo != nil {
		next.ArtifactRepo.Repo = strings.TrimSpace(*p.artifactRepo)
	}
	if p.artifactBranch != nil {
		branch := strings.TrimSpace(*p.artifactBranch)
		// Validated unconditionally: NousArtifactRepoConfig.Validate skips
		// branch/path while repo is empty, but a bad value saved now would
		// surface as a load error the moment an operator names a repo.
		if err := config.ValidateNousArtifactBranch(branch); err != nil {
			return cur, fmt.Errorf("nous %w", err)
		}
		next.ArtifactRepo.Branch = branch
	}
	if p.artifactPath != nil {
		path := strings.TrimSpace(*p.artifactPath)
		if err := config.ValidateNousArtifactPath(path); err != nil {
			return cur, fmt.Errorf("nous %w", err)
		}
		next.ArtifactRepo.Path = path
	}
	if p.artifactSyncIntervalSeconds != nil {
		if *p.artifactSyncIntervalSeconds < 0 {
			return cur, fmt.Errorf("nous artifact_repo.sync_interval_seconds must be 0 (default %ds) or greater", int(config.DefaultNousArtifactSyncInterval.Seconds()))
		}
		next.ArtifactRepo.SyncIntervalSeconds = *p.artifactSyncIntervalSeconds
	}
	if err := next.Validate(); err != nil {
		return cur, err
	}
	return next, nil
}

// experimentEngineValues lists the engines agents.<name>.engine may name,
// sorted, for the agent dialog's select.
func experimentEngineValues() []string {
	keys := make([]string, 0, len(config.ValidExperimentEngines))
	for k := range config.ValidExperimentEngines {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
