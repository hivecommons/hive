package dashboard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/stageengine"
)

// stageEngineMetricHelp is the HELP text per ADR-0025 central stage engine
// series; the names are the stageengine.Metric* constants, exported with the
// hive_ prefix like every other series on /metrics.
var stageEngineMetricHelp = map[string]string{
	stageengine.MetricDerivations:     "Stage derivations the central stage engine ran, by item kind and derived stage.",
	stageengine.MetricShadowMismatch:  "Shadow-mode disagreements between the sweep's legacy derivation and the engine, by kind and both stages.",
	stageengine.MetricAnomaly:         "Stage transitions the engine's admission table rejected, by kind and from/to stage.",
	stageengine.MetricWriterWrites:    "Label writes the single StageWriter performed, by kind and operation.",
	stageengine.MetricDuplicateHealed: "Items on which the writer removed duplicate stage pills, by kind.",
}

// writeStageEngineMetrics renders the process-wide stageengine counters.
// Every series is emitted (zero when idle) so dashboards see the engine
// exists even before its first derivation.
func writeStageEngineMetrics(b *strings.Builder, hiveID string, writeHeader func(name, help, typ string), c *stageengine.Counters) {
	if c == nil {
		return
	}
	samples := c.Snapshot()
	byName := make(map[string][]stageengine.Sample, len(stageEngineMetricHelp))
	for _, s := range samples {
		byName[s.Name] = append(byName[s.Name], s)
	}
	names := make([]string, 0, len(stageEngineMetricHelp))
	for name := range stageEngineMetricHelp {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		full := "hive_" + name
		writeHeader(full, stageEngineMetricHelp[name], "counter")
		rows := byName[name]
		if len(rows) == 0 {
			fmt.Fprintf(b, "%s{hive_id=%s} 0\n", full, promQuote(hiveID))
			continue
		}
		for _, s := range rows {
			keys := make([]string, 0, len(s.Labels))
			for k := range s.Labels {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var lbl strings.Builder
			lbl.WriteString("hive_id=" + promQuote(hiveID))
			for _, k := range keys {
				lbl.WriteString("," + k + "=" + promQuote(s.Labels[k]))
			}
			fmt.Fprintf(b, "%s{%s} %d\n", full, lbl.String(), s.Value)
		}
	}
}
