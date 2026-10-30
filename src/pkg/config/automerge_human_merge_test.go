package config

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

func TestAutoMergeHumanMergeRuleDefaultsAndOverrides(t *testing.T) {
	var unset AutoMergeConfig
	if got := unset.HumanMergeRule(); !reflect.DeepEqual(got.TitlePrefixes, prstage.DefaultHumanMergeTitlePrefixes) || !reflect.DeepEqual(got.Labels, prstage.DefaultHumanMergeLabels) {
		t.Fatalf("unset keys should yield the defaults, got %+v", got)
	}
	if ok, _ := unset.HumanMergeRule().RequiresHumanMerge("\u26a0\ufe0f break", nil); !ok {
		t.Fatal("default rule must catch a ⚠️ title")
	}

	off := AutoMergeConfig{HumanMergeTitlePrefixes: []string{}, HumanMergeLabels: []string{}}
	if got := off.HumanMergeRule(); len(got.TitlePrefixes) != 0 || got.TitlePrefixes == nil || len(got.Labels) != 0 || got.Labels == nil {
		t.Fatalf("explicit empty lists must stay empty (non-nil), got %+v", got)
	}
	if ok, _ := off.HumanMergeRule().RequiresHumanMerge("\u26a0\ufe0f break", []string{"needs-owner-merge"}); ok {
		t.Fatal("empty lists must switch the rule off")
	}

	custom := AutoMergeConfig{HumanMergeTitlePrefixes: []string{"BREAKING:"}, HumanMergeLabels: []string{"ship-by-owner"}}
	prefixes := custom.HumanMergeTitlePrefixesEffective()
	prefixes[0] = "mutated"
	if custom.HumanMergeTitlePrefixes[0] != "BREAKING:" {
		t.Fatal("HumanMergeTitlePrefixesEffective must return a copy")
	}
	labels := custom.HumanMergeLabelsEffective()
	labels[0] = "mutated"
	if custom.HumanMergeLabels[0] != "ship-by-owner" {
		t.Fatal("HumanMergeLabelsEffective must return a copy")
	}
	if ok, reason := custom.HumanMergeRule().RequiresHumanMerge("x", []string{"Ship-By-Owner"}); !ok || reason != "label:ship-by-owner" {
		t.Fatalf("custom label rule = %v %q", ok, reason)
	}
}
