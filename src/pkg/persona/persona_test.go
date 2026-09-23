package persona

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestPersonaSchemaSeparateFromAutonomyFields(t *testing.T) {
	personaFields := jsonFields(reflect.TypeOf(Record{}))
	autonomyFields := jsonFields(reflect.TypeOf(config.Config{}))
	for field := range jsonFields(reflect.TypeOf(config.AgentConfig{})) {
		autonomyFields[field] = struct{}{}
	}

	for field := range personaFields {
		if _, ok := autonomyFields[field]; ok {
			t.Fatalf("persona field %q overlaps ACMM or agent-mode configuration", field)
		}
	}
}

func TestRecordNormalizeSetAndEmpty(t *testing.T) {
	if !(Record{}).Empty() {
		t.Fatal("zero persona should be empty")
	}
	record := Record{Depth: "tech", SummaryLength: "long", Notes: "  include receipts  "}.Normalize()
	if record.Depth != DepthTechnical || record.SummaryLength != SummaryDetailed || record.Notes != "include receipts" {
		t.Fatalf("normalized record = %#v", record)
	}
	var err error
	record, err = record.Set("depth", "outcomes")
	if err != nil {
		t.Fatalf("set depth: %v", err)
	}
	record, err = record.Set("summary-length", "brief")
	if err != nil {
		t.Fatalf("set summary: %v", err)
	}
	record, err = record.Set("notes", "prefers bullets")
	if err != nil {
		t.Fatalf("set notes: %v", err)
	}
	if record.Depth != DepthOutcomes || record.SummaryLength != SummaryShort || record.Notes != "prefers bullets" {
		t.Fatalf("set record = %#v", record)
	}
	if _, err := record.Set("autonomy", "high"); err == nil {
		t.Fatal("unknown key should fail")
	}
	if NormalizeDepth("surprise") != DepthOutcomes || NormalizeSummaryLength("surprise") != SummaryStandard {
		t.Fatal("unknown values should normalize to safe presentation defaults")
	}
}

func jsonFields(t reflect.Type) map[string]struct{} {
	fields := map[string]struct{}{}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		name := strings.Split(sf.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			name = sf.Name
		}
		fields[name] = struct{}{}
	}
	return fields
}
