package dashboard

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// featuresOpsBody is the part of the Features PUT that covers the operational
// hive.yaml blocks issues.*, jev.*, duplicate_sweep.*, policies.* and the
// turn.pr_follow_up / turn.reentrant knobs. Every field is a pointer so an
// absent key leaves the config untouched. Durations travel as Go duration
// strings ("10m", "24h"); an empty string means the built-in default.
type featuresOpsBody struct {
	IssuesCloseOnMerge                 *bool   `json:"issuesCloseOnMerge"`
	IssuesCloseOnMergeInterval         *string `json:"issuesCloseOnMergeInterval"`
	IssuesCloseOnMergeBackfillInterval *string `json:"issuesCloseOnMergeBackfillInterval"`
	IssuesCloseOnMergeRecheck          *string `json:"issuesCloseOnMergeRecheck"`
	IssuesReporterConfirmation         *bool   `json:"issuesReporterConfirmation"`

	JevProvider  *string `json:"jevProvider"`
	JevModel     *string `json:"jevModel"`
	JevEndpoint  *string `json:"jevEndpoint"`
	JevAPIKeyEnv *string `json:"jevApiKeyEnv"`
	JevTimeout   *string `json:"jevTimeout"`

	DuplicateSweepEnabled       *bool `json:"duplicateSweepEnabled"`
	DuplicateSweepPostComments  *bool `json:"duplicateSweepPostComments"`
	DuplicateSweepMaxComments   *int  `json:"duplicateSweepMaxComments"`
	DuplicateSweepMaxPRsPerRepo *int  `json:"duplicateSweepMaxPRsPerRepo"`

	PoliciesRepo         *string `json:"policiesRepo"`
	PoliciesBranch       *string `json:"policiesBranch"`
	PoliciesPath         *string `json:"policiesPath"`
	PoliciesLocalDir     *string `json:"policiesLocalDir"`
	PoliciesPollInterval *string `json:"policiesPollInterval"`

	PRFollowUpMaxAge         *string `json:"prFollowUpMaxAge"`
	PRFollowUpResumeIDMaxAge *string `json:"prFollowUpResumeIdMaxAge"`
	PRFollowUpRetention      *string `json:"prFollowUpRetention"`

	ReentrantTurnEnabled                *bool `json:"reentrantTurnEnabled"`
	ReentrantTurnBackgroundFleetEnabled *bool `json:"reentrantTurnBackgroundFleetEnabled"`

	GoodFirstIssueEnabled     *bool   `json:"goodFirstIssueEnabled"`
	GoodFirstIssueInterval    *string `json:"goodFirstIssueInterval"`
	GoodFirstIssueMaxPerRepo  *int    `json:"goodFirstIssueMaxPerRepo"`
	GoodFirstIssueMaxPerSweep *int    `json:"goodFirstIssueMaxPerSweep"`
	GoodFirstIssueMinAge      *string `json:"goodFirstIssueMinAge"`
	// The scorer tuning knobs: 0 or empty means the gfi package default.
	GoodFirstIssueMinBodyChars     *int    `json:"goodFirstIssueMinBodyChars"`
	GoodFirstIssueMaxBodyChars     *int    `json:"goodFirstIssueMaxBodyChars"`
	GoodFirstIssueMaxPackages      *int    `json:"goodFirstIssueMaxPackages"`
	GoodFirstIssueMinScore         *int    `json:"goodFirstIssueMinScore"`
	GoodFirstIssueReselectCooldown *string `json:"goodFirstIssueReselectCooldown"`
	GoodFirstIssueDocsURL          *string `json:"goodFirstIssueDocsUrl"`
	GoodFirstIssueCommentTemplate  *string `json:"goodFirstIssueCommentTemplate"`
}

// envVarNamePattern is the shape of a POSIX environment variable name.
// jev.api_key_env names the variable holding the key; it is never the key.
var envVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseSettingsDuration parses an optional Go duration. Empty means zero
// (the built-in default); negative durations are rejected.
func parseSettingsDuration(name, raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be empty (default) or a non-negative duration such as 10m or 24h", name)
	}
	return d, nil
}

// settingsDurationString renders a duration for the dialog; zero is empty so
// the control shows the default as its placeholder.
func settingsDurationString(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}

// prepare validates every field the request carried against cur and returns
// the mutation to apply. Nothing is changed when an error is returned.
func (b featuresOpsBody) prepare(cur *config.Config) (func(*config.Config), error) {
	var steps []func(*config.Config)

	for _, f := range []struct {
		raw  *string
		name string
		dst  func(*config.Config) *time.Duration
	}{
		{b.IssuesCloseOnMergeInterval, "issues close_on_merge_interval", func(c *config.Config) *time.Duration { return &c.Issues.CloseOnMergeInterval }},
		{b.IssuesCloseOnMergeBackfillInterval, "issues close_on_merge_backfill_interval", func(c *config.Config) *time.Duration { return &c.Issues.CloseOnMergeBackfillInterval }},
		{b.IssuesCloseOnMergeRecheck, "issues close_on_merge_recheck", func(c *config.Config) *time.Duration { return &c.Issues.CloseOnMergeRecheck }},
		{b.PoliciesPollInterval, "policies poll_interval", func(c *config.Config) *time.Duration { return &c.Policies.PollInterval }},
	} {
		if f.raw == nil {
			continue
		}
		d, err := parseSettingsDuration(f.name, *f.raw)
		if err != nil {
			return nil, err
		}
		dst := f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = d })
	}
	if b.IssuesCloseOnMerge != nil {
		v := *b.IssuesCloseOnMerge
		steps = append(steps, func(c *config.Config) { c.Issues.CloseOnMerge = &v })
	}
	if b.IssuesReporterConfirmation != nil {
		v := *b.IssuesReporterConfirmation
		steps = append(steps, func(c *config.Config) { c.Issues.ReporterConfirmation = &v })
	}

	if b.JevProvider != nil || b.JevModel != nil || b.JevEndpoint != nil || b.JevAPIKeyEnv != nil || b.JevTimeout != nil {
		jev := cur.Jev
		if b.JevProvider != nil {
			jev.Provider = strings.ToLower(strings.TrimSpace(*b.JevProvider))
		}
		if b.JevModel != nil {
			jev.Model = strings.TrimSpace(*b.JevModel)
		}
		if b.JevEndpoint != nil {
			jev.Endpoint = strings.TrimSpace(*b.JevEndpoint)
		}
		if b.JevAPIKeyEnv != nil {
			name := strings.TrimSpace(*b.JevAPIKeyEnv)
			if name != "" && !envVarNamePattern.MatchString(name) {
				return nil, fmt.Errorf("jev api_key_env must be an environment variable name (letters, digits, underscore), not a key value")
			}
			jev.APIKeyEnv = name
		}
		if b.JevTimeout != nil {
			d, err := parseSettingsDuration("jev timeout", *b.JevTimeout)
			if err != nil {
				return nil, err
			}
			jev.Timeout = d
		}
		if err := jev.Validate(); err != nil {
			return nil, err
		}
		steps = append(steps, func(c *config.Config) { c.Jev = jev })
	}

	for _, f := range []struct {
		v    *int
		name string
	}{
		{b.DuplicateSweepMaxComments, "max_comments"},
		{b.DuplicateSweepMaxPRsPerRepo, "max_prs_per_repo"},
	} {
		if f.v != nil && *f.v < 0 {
			return nil, fmt.Errorf("duplicate_sweep %s must be 0 (default) or greater", f.name)
		}
	}
	if b.DuplicateSweepEnabled != nil {
		v := *b.DuplicateSweepEnabled
		steps = append(steps, func(c *config.Config) { c.DuplicateSweep.Enabled = v })
	}
	if b.DuplicateSweepPostComments != nil {
		v := *b.DuplicateSweepPostComments
		steps = append(steps, func(c *config.Config) { c.DuplicateSweep.PostComments = v })
	}
	if b.DuplicateSweepMaxComments != nil {
		v := *b.DuplicateSweepMaxComments
		steps = append(steps, func(c *config.Config) { c.DuplicateSweep.MaxComments = v })
	}
	if b.DuplicateSweepMaxPRsPerRepo != nil {
		v := *b.DuplicateSweepMaxPRsPerRepo
		steps = append(steps, func(c *config.Config) { c.DuplicateSweep.MaxPRsPerRepo = v })
	}

	if b.PoliciesLocalDir != nil {
		dir := strings.TrimSpace(*b.PoliciesLocalDir)
		if dir != "" && (!filepath.IsAbs(dir) || filepath.Clean(dir) != dir) {
			return nil, fmt.Errorf("policies local_dir must be empty or a clean absolute path")
		}
		steps = append(steps, func(c *config.Config) { c.Policies.LocalDir = dir })
	}
	if b.PoliciesPath != nil {
		p := strings.TrimSpace(*b.PoliciesPath)
		if strings.HasPrefix(p, "/") || containsDotDotSegment(p) {
			return nil, fmt.Errorf("policies path must be a relative path inside the policy repo")
		}
		steps = append(steps, func(c *config.Config) { c.Policies.Path = p })
	}
	if b.PoliciesRepo != nil {
		v := strings.TrimSpace(*b.PoliciesRepo)
		if strings.ContainsAny(v, " \t\r\n") {
			return nil, fmt.Errorf("policies repo must not contain whitespace")
		}
		steps = append(steps, func(c *config.Config) { c.Policies.Repo = v })
	}
	if b.PoliciesBranch != nil {
		v := strings.TrimSpace(*b.PoliciesBranch)
		if strings.ContainsAny(v, " \t\r\n") || strings.HasPrefix(v, "-") {
			return nil, fmt.Errorf("policies branch must be a branch name")
		}
		steps = append(steps, func(c *config.Config) { c.Policies.Branch = v })
	}

	for _, f := range []struct {
		raw  *string
		name string
		dst  func(*config.Config) *string
	}{
		{b.PRFollowUpMaxAge, "turn pr_follow_up max_age", func(c *config.Config) *string { return &c.Turn.PRFollowUp.MaxAge }},
		{b.PRFollowUpResumeIDMaxAge, "turn pr_follow_up resume_id_max_age", func(c *config.Config) *string { return &c.Turn.PRFollowUp.ResumeIDMaxAge }},
		{b.PRFollowUpRetention, "turn pr_follow_up retention", func(c *config.Config) *string { return &c.Turn.PRFollowUp.Retention }},
	} {
		if f.raw == nil {
			continue
		}
		v := strings.TrimSpace(*f.raw)
		if v != "" {
			if d, err := time.ParseDuration(v); err != nil || d <= 0 {
				return nil, fmt.Errorf("%s must be empty (default) or a positive duration such as 24h", f.name)
			}
		}
		dst := f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = v })
	}
	if b.ReentrantTurnEnabled != nil {
		v := *b.ReentrantTurnEnabled
		steps = append(steps, func(c *config.Config) { c.Turn.Reentrant.Enabled = v })
	}
	if b.ReentrantTurnBackgroundFleetEnabled != nil {
		v := *b.ReentrantTurnBackgroundFleetEnabled
		steps = append(steps, func(c *config.Config) { c.Turn.Reentrant.BackgroundFleetEnabled = v })
	}

	if b.GoodFirstIssueEnabled != nil || b.GoodFirstIssueInterval != nil || b.GoodFirstIssueMaxPerRepo != nil ||
		b.GoodFirstIssueMaxPerSweep != nil || b.GoodFirstIssueMinAge != nil || b.GoodFirstIssueMinBodyChars != nil ||
		b.GoodFirstIssueMaxBodyChars != nil || b.GoodFirstIssueMaxPackages != nil || b.GoodFirstIssueMinScore != nil ||
		b.GoodFirstIssueReselectCooldown != nil || b.GoodFirstIssueDocsURL != nil || b.GoodFirstIssueCommentTemplate != nil {
		g := cur.GoodFirstIssue
		if b.GoodFirstIssueEnabled != nil {
			g.Enabled = *b.GoodFirstIssueEnabled
		}
		if b.GoodFirstIssueInterval != nil {
			d, err := parseSettingsDuration("good_first_issue interval", *b.GoodFirstIssueInterval)
			if err != nil {
				return nil, err
			}
			g.Interval = d
		}
		if b.GoodFirstIssueMinAge != nil {
			d, err := parseSettingsDuration("good_first_issue min_age", *b.GoodFirstIssueMinAge)
			if err != nil {
				return nil, err
			}
			g.MinAge = d
		}
		if b.GoodFirstIssueMaxPerRepo != nil {
			g.MaxPerRepo = *b.GoodFirstIssueMaxPerRepo
		}
		if b.GoodFirstIssueMaxPerSweep != nil {
			g.MaxPerSweep = *b.GoodFirstIssueMaxPerSweep
		}
		if b.GoodFirstIssueMinBodyChars != nil {
			g.MinBodyChars = *b.GoodFirstIssueMinBodyChars
		}
		if b.GoodFirstIssueMaxBodyChars != nil {
			g.MaxBodyChars = *b.GoodFirstIssueMaxBodyChars
		}
		if b.GoodFirstIssueMaxPackages != nil {
			g.MaxPackages = *b.GoodFirstIssueMaxPackages
		}
		if b.GoodFirstIssueMinScore != nil {
			g.MinScore = *b.GoodFirstIssueMinScore
		}
		if b.GoodFirstIssueReselectCooldown != nil {
			d, err := parseSettingsDuration("good_first_issue reselect_cooldown", *b.GoodFirstIssueReselectCooldown)
			if err != nil {
				return nil, err
			}
			g.ReselectCooldown = d
		}
		if b.GoodFirstIssueDocsURL != nil {
			v := strings.TrimSpace(*b.GoodFirstIssueDocsURL)
			if v != "" {
				u, err := url.Parse(v)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					return nil, fmt.Errorf("good_first_issue docs_url must be empty (default) or an absolute http(s) URL")
				}
			}
			g.DocsURL = v
		}
		if b.GoodFirstIssueCommentTemplate != nil {
			// Whitespace-only means the built-in template; otherwise the text is
			// kept verbatim because Markdown is whitespace-sensitive.
			v := *b.GoodFirstIssueCommentTemplate
			if strings.TrimSpace(v) == "" {
				v = ""
			}
			g.CommentTemplate = v
		}
		if err := config.ValidateGoodFirstIssue(g); err != nil {
			return nil, err
		}
		steps = append(steps, func(c *config.Config) { c.GoodFirstIssue = g })
	}

	return func(c *config.Config) {
		for _, step := range steps {
			step(c)
		}
	}, nil
}

func containsDotDotSegment(p string) bool {
	for _, seg := range strings.Split(strings.ReplaceAll(p, "\\", "/"), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// featuresOpsResponse is the GET side of featuresOpsBody. Booleans with a
// nil-means-default config field are reported as their effective value, and
// each *Defaults map carries what an empty or zero field falls back to so
// the dialog can show it as a placeholder. Only the Jev key's variable NAME
// is returned, never its value.
func featuresOpsResponse(cfg *config.Config) map[string]interface{} {
	jevDefaults := config.JevConfig{Provider: cfg.Jev.Provider}
	return map[string]interface{}{
		"issuesCloseOnMerge":                 cfg.Issues.CloseOnMergeEnabled(),
		"issuesCloseOnMergeInterval":         settingsDurationString(cfg.Issues.CloseOnMergeInterval),
		"issuesCloseOnMergeBackfillInterval": settingsDurationString(cfg.Issues.CloseOnMergeBackfillInterval),
		"issuesCloseOnMergeRecheck":          settingsDurationString(cfg.Issues.CloseOnMergeRecheck),
		"issuesReporterConfirmation":         cfg.Issues.ReporterConfirmationEnabled(),
		"issuesDefaults": map[string]string{
			"issuesCloseOnMergeInterval":         config.DefaultCloseOnMergeInterval.String(),
			"issuesCloseOnMergeBackfillInterval": config.DefaultCloseOnMergeBackfillInterval.String(),
			"issuesCloseOnMergeRecheck":          config.DefaultCloseOnMergeRecheck.String(),
		},

		"jevProvider":  cfg.Jev.Provider,
		"jevModel":     cfg.Jev.Model,
		"jevEndpoint":  cfg.Jev.Endpoint,
		"jevApiKeyEnv": cfg.Jev.APIKeyEnv,
		"jevTimeout":   settingsDurationString(cfg.Jev.Timeout),
		"jevProviders": []string{config.JevProviderOpenRouter, config.JevProviderTypeSafe},
		"jevDefaults": map[string]string{
			"jevProvider":  jevDefaults.EffectiveProvider(),
			"jevModel":     jevDefaults.EffectiveModel(),
			"jevEndpoint":  jevDefaults.EffectiveEndpoint(),
			"jevApiKeyEnv": jevDefaults.EffectiveAPIKeyEnv(),
			"jevTimeout":   jevDefaults.EffectiveTimeout().String(),
		},

		"duplicateSweepEnabled":       cfg.DuplicateSweep.Enabled,
		"duplicateSweepPostComments":  cfg.DuplicateSweep.PostComments,
		"duplicateSweepMaxComments":   cfg.DuplicateSweep.MaxComments,
		"duplicateSweepMaxPRsPerRepo": cfg.DuplicateSweep.MaxPRsPerRepo,
		"duplicateSweepDefaults": map[string]int{
			"duplicateSweepMaxComments":   github.DefaultDuplicateSweepMaxComments,
			"duplicateSweepMaxPRsPerRepo": github.DefaultDuplicateSweepMaxPRsPerRepo,
		},

		"policiesRepo":         cfg.Policies.Repo,
		"policiesBranch":       cfg.Policies.Branch,
		"policiesPath":         cfg.Policies.Path,
		"policiesLocalDir":     cfg.Policies.LocalDir,
		"policiesPollInterval": settingsDurationString(cfg.Policies.PollInterval),

		"prFollowUpMaxAge":         cfg.Turn.PRFollowUp.MaxAge,
		"prFollowUpResumeIdMaxAge": cfg.Turn.PRFollowUp.ResumeIDMaxAge,
		"prFollowUpRetention":      cfg.Turn.PRFollowUp.Retention,
		"prFollowUpDefaults": map[string]string{
			"prFollowUpMaxAge":         config.DefaultPRFollowUpMaxAge.String(),
			"prFollowUpResumeIdMaxAge": config.DefaultPRFollowUpResumeIDMaxAge.String(),
			"prFollowUpRetention":      config.DefaultPRFollowUpRetention.String(),
		},

		"reentrantTurnEnabled":                cfg.Turn.Reentrant.Enabled,
		"reentrantTurnBackgroundFleetEnabled": cfg.Turn.Reentrant.BackgroundFleetEnabled,

		"goodFirstIssueEnabled":     cfg.GoodFirstIssue.Enabled,
		"goodFirstIssueInterval":    settingsDurationString(cfg.GoodFirstIssue.Interval),
		"goodFirstIssueMaxPerRepo":  cfg.GoodFirstIssue.MaxPerRepo,
		"goodFirstIssueMaxPerSweep": cfg.GoodFirstIssue.MaxPerSweep,
		"goodFirstIssueMinAge":      settingsDurationString(cfg.GoodFirstIssue.MinAge),

		"goodFirstIssueMinBodyChars":     cfg.GoodFirstIssue.MinBodyChars,
		"goodFirstIssueMaxBodyChars":     cfg.GoodFirstIssue.MaxBodyChars,
		"goodFirstIssueMaxPackages":      cfg.GoodFirstIssue.MaxPackages,
		"goodFirstIssueMinScore":         cfg.GoodFirstIssue.MinScore,
		"goodFirstIssueReselectCooldown": settingsDurationString(cfg.GoodFirstIssue.ReselectCooldown),
		"goodFirstIssueDocsUrl":          cfg.GoodFirstIssue.DocsURL,
		"goodFirstIssueCommentTemplate":  cfg.GoodFirstIssue.CommentTemplate,
		"goodFirstIssueDefaults": map[string]string{
			"goodFirstIssueInterval":         config.DefaultGoodFirstIssueInterval.String(),
			"goodFirstIssueMaxPerRepo":       strconv.Itoa(config.DefaultGoodFirstIssueMaxPerRepo),
			"goodFirstIssueMaxPerSweep":      strconv.Itoa(config.DefaultGoodFirstIssueMaxPerSweep),
			"goodFirstIssueMinAge":           config.DefaultGoodFirstIssueMinAge.String(),
			"goodFirstIssueMinBodyChars":     strconv.Itoa(config.DefaultGoodFirstIssueMinBodyChars),
			"goodFirstIssueMaxBodyChars":     strconv.Itoa(config.DefaultGoodFirstIssueMaxBodyChars),
			"goodFirstIssueMaxPackages":      strconv.Itoa(config.DefaultGoodFirstIssueMaxPackages),
			"goodFirstIssueMinScore":         strconv.Itoa(config.DefaultGoodFirstIssueMinScore),
			"goodFirstIssueReselectCooldown": config.DefaultGoodFirstIssueReselectCooldown.String(),
			"goodFirstIssueDocsUrl":          config.DefaultGoodFirstIssueDocsBase,
		},
	}
}
