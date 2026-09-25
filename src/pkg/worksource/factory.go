package worksource

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/linearagent"
)

// FromConfig constructs the WorkSource for a governor from its WorkSourceConfig.
// When cfg.Type is "" or "github", returns a githubIssuesSource wrapping the
// existing ghClient — no config change needed for existing hives.
func FromConfig(cfg config.WorkSourceConfig, ghClient *github.Client, ghToken, ghOrg string, logger *slog.Logger) (WorkSource, error) {
	var primary WorkSource
	switch cfg.Type {
	case "", "github":
		primary = NewGitHubIssuesSource(ghClient)
	case "github_projects":
		c := cfg.GitHubProjects
		primary = NewGitHubProjectsSource(GitHubProjectsConfig{
			Token:          ghToken,
			Org:            coalesce(c.Org, ghOrg),
			ProjectNumber:  c.ProjectNumber,
			States:         c.States,
			PriorityField:  c.PriorityField,
			IterationField: c.IterationField,
			DefaultRepo:    c.DefaultRepo,
		})
	case "linear":
		c := cfg.Linear
		if c.APIKey == "" {
			return nil, fmt.Errorf("work_source.linear.api_key is required")
		}
		apiKey, err := resolveSecretRef("work_source.linear.api_key", c.APIKey)
		if err != nil {
			return nil, err
		}
		if len(c.Teams) == 0 {
			return nil, fmt.Errorf("work_source.linear.teams must contain at least one team")
		}
		teams := make([]LinearTeamConfig, len(c.Teams))
		for i, t := range c.Teams {
			if t.Key == "" {
				return nil, fmt.Errorf("work_source.linear.teams[%d].key is required", i)
			}
			if t.Repo == "" {
				return nil, fmt.Errorf("work_source.linear.teams[%d].repo is required", i)
			}
			if t.Cycles != "" && t.Cycles != "current" {
				return nil, fmt.Errorf("work_source.linear.teams[%d].cycles = %q (want empty or current)", i, t.Cycles)
			}
			projects := make([]LinearProjectConfig, len(t.Projects))
			for j, p := range t.Projects {
				if p.Name == "" {
					return nil, fmt.Errorf("work_source.linear.teams[%d].projects[%d].name is required", i, j)
				}
				projects[j] = LinearProjectConfig{Name: p.Name, Repo: p.Repo}
			}
			teams[i] = LinearTeamConfig{Key: t.Key, Repo: t.Repo, States: t.States, Projects: projects, Cycles: t.Cycles}
		}
		viewerID := ""
		if c.AssignedOnly {
			// Fail closed: assigned_only without a connected Linear agent
			// cannot mean "enumerate everything" — that would silently hand
			// agents the whole backlog the operator asked to narrow.
			viewerID = linearagent.StoredViewerID(linearagent.DefaultStorePath())
			if viewerID == "" {
				return nil, fmt.Errorf("work_source.linear.assigned_only requires the Linear agent to be connected (no install found at %s)", linearagent.DefaultStorePath())
			}
		}
		primary = NewLinearSource(LinearConfig{
			APIKey:     apiKey,
			Teams:      teams,
			HoldLabels: c.HoldLabels,
			ViewerID:   viewerID,
			Logger:     logger,
		}, nil)
	case "jira":
		c := cfg.Jira
		apiToken, err := resolveSecretRef("work_source.jira.api_token", c.APIToken)
		if err != nil {
			return nil, err
		}
		password, err := resolveSecretRef("work_source.jira.password", c.Password)
		if err != nil {
			return nil, err
		}
		var caBundle, clientCert, clientKey string
		if jiraConfigIsDataCenter(c.Deployment) {
			caBundle, err = resolveSecretRef("work_source.jira.ca_bundle", c.CABundle)
			if err != nil {
				return nil, err
			}
			clientCert, err = resolveSecretRef("work_source.jira.client_cert", c.ClientCert)
			if err != nil {
				return nil, err
			}
			clientKey, err = resolveSecretRef("work_source.jira.client_key", c.ClientKey)
			if err != nil {
				return nil, err
			}
		}
		jiraCfg := JiraConfig{
			Deployment:         c.Deployment,
			BaseURL:            c.BaseURL,
			Email:              c.Email,
			Username:           c.Username,
			APIToken:           apiToken,
			Password:           password,
			CABundle:           caBundle,
			InsecureSkipVerify: c.InsecureSkipVerify,
			ClientCert:         clientCert,
			ClientKey:          clientKey,
			ProjectKeys:        c.ProjectKeys,
			JQL:                c.JQL,
			Repo:               c.Repo,
			HoldLabels:         c.HoldLabels,
			Logger:             logger,
		}
		if err := ValidateJiraTLSConfig(jiraCfg); err != nil {
			return nil, err
		}
		primary = NewJiraSource(jiraCfg)
	default:
		return nil, fmt.Errorf("unknown work_source type %q (want github, github_projects, linear, or jira)", cfg.Type)
	}
	return AppendAdditive(primary, cfg)
}

// AdditiveBuilder constructs one additive (non-primary) work source from the
// governor's work-source config. Additive sources live in sub-packages that
// import this package, so they register here at init time instead of being
// imported by the factory directly.
type AdditiveBuilder func(cfg config.WorkSourceConfig, logger *slog.Logger) (WorkSource, error)

// AdditiveWavefront is the registry name of the Wavefront migration-graph
// source (pkg/worksource/wavefront). It is enabled by
// governor.work_source.wavefront.enabled.
const AdditiveWavefront = "wavefront"

var (
	additiveMu       sync.RWMutex
	additiveBuilders = map[string]AdditiveBuilder{}

	runStageAccessorMu sync.RWMutex
	runStageAccessor   RunStageLeaseAccessor
)

// RegisterAdditive registers the builder for a named additive source. A
// sub-package calls it from init(); the hive binary links the sub-package with
// a blank import. Registering the same name twice panics: two builders for one
// flag would make the composed output depend on link order.
func RegisterAdditive(name string, build AdditiveBuilder) {
	additiveMu.Lock()
	defer additiveMu.Unlock()
	if _, dup := additiveBuilders[name]; dup {
		panic(fmt.Sprintf("worksource: additive source %q registered twice", name))
	}
	additiveBuilders[name] = build
}

func lookupAdditive(name string) (AdditiveBuilder, bool) {
	additiveMu.RLock()
	defer additiveMu.RUnlock()
	b, ok := additiveBuilders[name]
	return b, ok
}

// SetRunStageAccessor injects the live dashboard lease registry used by the
// additive run-stage work source. A nil accessor is allowed and makes the
// source list nothing, matching the pre-wiring fail-closed behavior.
func SetRunStageAccessor(accessor RunStageLeaseAccessor) {
	runStageAccessorMu.Lock()
	defer runStageAccessorMu.Unlock()
	runStageAccessor = accessor
}

func currentRunStageAccessor() RunStageLeaseAccessor {
	runStageAccessorMu.RLock()
	defer runStageAccessorMu.RUnlock()
	return runStageAccessor
}

// AppendAdditive wraps primary with every additive source the config enables.
// With no flag set it returns primary itself, so ListIssues output is
// byte-identical to a hive that has never heard of run stages or Wavefront.
func AppendAdditive(primary WorkSource, cfg config.WorkSourceConfig) (WorkSource, error) {
	return appendAdditive(primary, cfg, slog.Default())
}

func appendAdditive(primary WorkSource, cfg config.WorkSourceConfig, logger *slog.Logger) (WorkSource, error) {
	var extras []WorkSource
	if cfg.RunStages {
		extras = append(extras, NewRunStageSource(currentRunStageAccessor()))
	}
	if cfg.Wavefront.Enabled {
		build, ok := lookupAdditive(AdditiveWavefront)
		if !ok {
			return nil, fmt.Errorf("work_source.wavefront.enabled is set but the wavefront source is not linked into this binary")
		}
		extra, err := build(cfg, logger)
		if err != nil {
			return nil, fmt.Errorf("work_source.wavefront: %w", err)
		}
		extras = append(extras, extra)
	}
	if len(extras) == 0 {
		return primary, nil
	}
	return NewComposite(primary, extras...), nil
}

// secretRefPattern matches a credential written as a whole-value environment
// reference: `${LINEAR_API_KEY}` or `$LINEAR_API_KEY`.
var secretRefPattern = regexp.MustCompile(`^\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))$`)

// resolveSecretRef resolves a work-source credential at the point of use.
//
// hive.yaml documents `api_key: ${LINEAR_API_KEY}`. Values loaded from the
// file are env-expanded by config.Load, but a value saved from the dashboard
// (PUT /api/config/governor/work-source) is stored verbatim, so the adapter
// used to send the literal string `${LINEAR_API_KEY}` as its Authorization
// header and got a 401. Resolving here — rather than at save time — keeps the
// secret out of the persisted config: the overlay only ever holds the
// reference. An unset or empty variable is a clear configuration error, never
// a literal header. Anything that is not a whole-value reference is returned
// unchanged.
func resolveSecretRef(field, raw string) (string, error) {
	m := secretRefPattern.FindStringSubmatch(raw)
	if m == nil {
		return raw, nil
	}
	name := m[1]
	if name == "" {
		name = m[2]
	}
	val, ok := os.LookupEnv(name)
	if !ok || val == "" {
		return "", fmt.Errorf("%s references environment variable %s, which is not set in the hive's environment", field, name)
	}
	return val, nil
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
