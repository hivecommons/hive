package config

// ScannerConfig tunes live pre-dispatch guards.
type ScannerConfig struct {
	SkipIssuesWithOpenPR *bool `yaml:"skip_issues_with_open_pr,omitempty" json:"skip_issues_with_open_pr,omitempty"`
}

func (c ScannerConfig) IsSkipIssuesWithOpenPR() bool {
	return c.SkipIssuesWithOpenPR == nil || *c.SkipIssuesWithOpenPR
}
