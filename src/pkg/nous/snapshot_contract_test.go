package nous

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivestate"
)

// Keep catalog source names tied to the actual persisted snapshot schema.
func TestCatalogSnapshotContract(t *testing.T) {
	snapshot := reflect.TypeOf(hivestate.Snapshot{})
	fields := map[string]string{
		"queue_issues":   "QueueIssues",
		"queue_prs":      "QueuePRs",
		"queue_hold":     "QueueHold",
		"sla_violations": "SLAViolations",
	}
	for _, definition := range MetricCatalog() {
		for _, source := range definition.SourceFields {
			field, ok := snapshot.FieldByName(fields[source])
			if !ok {
				t.Errorf("%s: source %q has no snapshot field", definition.ID, source)
				continue
			}
			if tag := strings.Split(field.Tag.Get("json"), ",")[0]; tag != source {
				t.Errorf("%s: source %q differs from snapshot JSON tag %q", definition.ID, source, tag)
			}
			if field.Type.Kind() != reflect.Int {
				t.Errorf("%s: source %q is not an integer count", definition.ID, source)
			}
		}
	}
}
