package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEffectivePerspectivesPrecedence(t *testing.T) {
	source := &DefinitionSourceConfig{Owner: "acme", Repo: "widgets", Path: ".hive/review-perspectives.yaml"}
	linked := &LinkedPerspectives{
		Names:  []string{"correctness", "spyre-numerics"},
		Focus:  map[string]string{"spyre-numerics": "fp16 accumulation order"},
		Source: "acme/widgets/.hive/review-perspectives.yaml",
	}
	inlineNames := []string{"security"}
	inlineFocus := map[string]string{"security": "secrets in logs"}

	tests := []struct {
		name      string
		cfg       ReviewConfig
		wantNames []string
		wantFocus map[string]string
	}{
		{
			name:      "source set and resolved uses linked set",
			cfg:       ReviewConfig{Perspectives: inlineNames, PerspectivePrompts: inlineFocus, PerspectivesSource: source, LinkedPerspectives: linked},
			wantNames: linked.Names,
			wantFocus: linked.Focus,
		},
		{
			name:      "source set but unresolved falls back to inline",
			cfg:       ReviewConfig{Perspectives: inlineNames, PerspectivePrompts: inlineFocus, PerspectivesSource: source},
			wantNames: inlineNames,
			wantFocus: inlineFocus,
		},
		{
			name:      "partial source is unset",
			cfg:       ReviewConfig{Perspectives: inlineNames, PerspectivePrompts: inlineFocus, PerspectivesSource: &DefinitionSourceConfig{Owner: "acme", Repo: "widgets"}, LinkedPerspectives: linked},
			wantNames: inlineNames,
			wantFocus: inlineFocus,
		},
		{
			name:      "source unset uses inline",
			cfg:       ReviewConfig{Perspectives: inlineNames, PerspectivePrompts: inlineFocus},
			wantNames: inlineNames,
			wantFocus: inlineFocus,
		},
		{
			name: "nothing configured",
			cfg:  ReviewConfig{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			names, focus := tc.cfg.EffectivePerspectives()
			if !reflect.DeepEqual(names, tc.wantNames) {
				t.Errorf("names = %v, want %v", names, tc.wantNames)
			}
			if !reflect.DeepEqual(focus, tc.wantFocus) {
				t.Errorf("focus = %v, want %v", focus, tc.wantFocus)
			}
		})
	}
}

func TestLinkedPerspectivesNotPersisted(t *testing.T) {
	cfg := ReviewConfig{
		PerspectivesSource: &DefinitionSourceConfig{Owner: "acme", Repo: "widgets", Path: ".hive/review-perspectives.yaml"},
		LinkedPerspectives: &LinkedPerspectives{
			Names:   []string{"linkedonlyname"},
			Focus:   map[string]string{"linkedonlyname": "linkedonlyfocus"},
			Source:  "linkedonlyprovenance",
			Warning: "linkedonlywarning",
		},
	}
	y, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	j, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for format, out := range map[string]string{"yaml": string(y), "json": string(j)} {
		if !strings.Contains(out, "perspectives_source") {
			t.Errorf("%s output lost perspectives_source:\n%s", format, out)
		}
		for _, leaked := range []string{"linked", "Linked"} {
			if strings.Contains(out, leaked) {
				t.Errorf("%s output persists linked data (%q):\n%s", format, leaked, out)
			}
		}
	}

	var round ReviewConfig
	if err := yaml.Unmarshal(y, &round); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if round.LinkedPerspectives != nil {
		t.Errorf("LinkedPerspectives survived a yaml round-trip: %+v", round.LinkedPerspectives)
	}
	if !round.PerspectivesSource.IsSet() {
		t.Errorf("PerspectivesSource lost in yaml round-trip: %+v", round.PerspectivesSource)
	}
}
