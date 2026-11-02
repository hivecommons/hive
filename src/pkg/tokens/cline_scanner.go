package tokens

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"

	"github.com/hivecommons/hive/crux/cline"
)

// SetClineHomesRoot adds normalized Cline usage in shared and per-agent homes.
// Configure before Start, like the other session-directory setters.
func (c *Collector) SetClineHomesRoot(root string) { c.clineHomesRoot = root }

func (c *Collector) mergeClineUsage(agg *AggregateSummary) {
	if c.clineHomesRoot == "" {
		return
	}
	paths, err := filepath.Glob(filepath.Join(c.clineHomesRoot, "agents", "*", cline.UsageRelativeDir))
	if err != nil {
		c.logger.Warn("Cline usage discovery failed", "error", err)
		return
	}
	paths = append(paths, filepath.Join(c.clineHomesRoot, cline.UsageRelativeDir))
	for _, path := range paths {
		recorded, err := CollectFromDir(path, c.detector)
		if err != nil {
			c.logger.Warn("Cline usage scan failed", "error", err)
			continue
		}
		for i := range recorded.Sessions {
			// Turn directory names are local to a home; namespace their IDs
			// before the shared collector deduplicates sessions.
			recorded.Sessions[i].SessionID = fmt.Sprintf("cline-%x-%s", sha256.Sum256([]byte(path)), recorded.Sessions[i].SessionID)
		}
		if recorded.SessionCount > 0 {
			MergeAggregates(agg, recorded)
		}
	}
}
