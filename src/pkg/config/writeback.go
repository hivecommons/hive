package config

import (
	"strings"

	"github.com/hivecommons/hive/pkg/writeback"
)

// WritebackOptInLabels returns the labels that count as a maintainer opting
// a PR or issue in to unsolicited hive write-back on repo:
// review.require_labels when set, else writeback.DefaultOptInLabel
// (`hive-review`). The gate consults this per decision.
func (c *Config) WritebackOptInLabels(_ string) []string {
	if ls := c.Review.NormalizedRequireLabels(); len(ls) > 0 {
		return ls
	}
	return []string{writeback.DefaultOptInLabel}
}

// WritebackRepoListed reports whether repo is one of project.repos (any
// case, bare or owner-qualified). The gate refuses unsolicited writes on
// repos the operator never listed.
func (c *Config) WritebackRepoListed(repo string) bool {
	want := strings.ToLower(strings.TrimSpace(repo))
	if want == "" {
		return false
	}
	if i := strings.LastIndexByte(want, '/'); i >= 0 {
		want = want[i+1:]
	}
	for _, r := range c.Project.Repos {
		have := strings.ToLower(strings.TrimSpace(r))
		if i := strings.LastIndexByte(have, '/'); i >= 0 {
			have = have[i+1:]
		}
		if have == want {
			return true
		}
	}
	return false
}

// WritebackGateDeps builds the gate's lookups over this config.
func (c *Config) WritebackGateDeps() writeback.Deps {
	return writeback.Deps{
		LevelFor:    c.EffectiveACMMLevelForRepo,
		OptInLabels: c.WritebackOptInLabels,
		RepoListed:  c.WritebackRepoListed,
	}
}
