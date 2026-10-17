package dashboard

import (
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
	"github.com/hivecommons/hive/pkg/evidence"
)

// EvidenceACMMChange is an observation, not the time a setting was changed.
type EvidenceACMMChange struct {
	Repo  string `json:"repo"`
	Month string `json:"month"`
	Level int    `json:"level"`
}

// evidenceACMMChanges uses all bundles, not the latest bundle per PR or the
// selected author/window: policy maturity is a repo property. The chart clips
// markers to its monthly snapshot range. Missing/invalid levels are unknown.
func evidenceACMMChanges(bundles []*evidence.Bundle, repo string) []EvidenceACMMChange {
	type key struct {
		repo  string
		level int
	}
	first := map[key]time.Time{}
	for _, b := range bundles {
		if b == nil || b.CreatedAt.IsZero() || b.Policy.ACMMLevel < 1 || b.Policy.ACMMLevel > acmmadvisor.MaxLevel {
			continue
		}
		if repo != "" && !strings.EqualFold(repo, b.Repo) {
			continue
		}
		k := key{strings.ToLower(b.Repo), b.Policy.ACMMLevel}
		if at, ok := first[k]; !ok || b.CreatedAt.Before(at) {
			first[k] = b.CreatedAt
		}
	}
	var out []EvidenceACMMChange
	for k, at := range first {
		out = append(out, EvidenceACMMChange{Repo: k.repo, Month: at.UTC().Format("2006-01"), Level: k.level})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		if out[i].Month != out[j].Month {
			return out[i].Month < out[j].Month
		}
		return out[i].Level < out[j].Level
	})
	return out
}
