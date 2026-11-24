package config

import (
	"strings"

	"github.com/hivecommons/hive/pkg/writeback"
)

// WritebackPolicy shares Save's lock. The policy's maps are replaced on
// updates so readers can keep this snapshot while a new mode is saved.
func (c *Config) WritebackPolicy() writeback.Config {
	saveMu.Lock()
	defer saveMu.Unlock()
	return c.Writeback.WithRepoOwner(c.Project.Org)
}

// SaveWritebackEdit publishes the policy only after its persistence succeeds.
// edit must replace any maps or pointers it changes.
func (c *Config) SaveWritebackEdit(edit func(*writeback.Config, string)) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	previous := c.Writeback
	edit(&c.Writeback, c.Project.Org)
	if c.SourcePath != "" {
		if err := c.saveLocked(); err != nil {
			c.Writeback = previous
			return err
		}
	}
	return nil
}

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
		Config:      c.WritebackPolicy,
		LevelFor:    c.EffectiveACMMLevelForRepo,
		OptInLabels: c.WritebackOptInLabels,
		RepoListed:  c.WritebackRepoListed,
		RepoPause:   c.WritebackRepoPause,
	}
}

// WritebackRepoPause adapts RepoPauseFor to the gate's pause lookup, so a
// pause taken in the dashboard refuses every gated write on the very next
// decision (#6203).
func (c *Config) WritebackRepoPause(repo string) (writeback.RepoPauseInfo, bool) {
	rp, paused := c.RepoPauseFor(repo)
	if !paused {
		return writeback.RepoPauseInfo{}, false
	}
	return writeback.RepoPauseInfo{By: rp.By, Reason: rp.Reason}, true
}
