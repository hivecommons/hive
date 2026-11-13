package perspsrc

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDocument(t *testing.T) {
	tests := []struct {
		name      string
		doc       string
		wantNames []string
		wantFocus map[string]string
		wantErr   string
	}{
		{
			name: "built-in only",
			doc: `kind: ReviewPerspectives
perspectives:
  - name: correctness
  - name: security
`,
			wantNames: []string{"correctness", "security"},
			wantFocus: map[string]string{},
		},
		{
			name: "custom with focus",
			doc: `kind: ReviewPerspectives
perspectives:
  - name: correctness
  - name: spyre-constraints
    focus: "Head size a multiple of 64; fp16 only."
`,
			wantNames: []string{"correctness", "spyre-constraints"},
			wantFocus: map[string]string{"spyre-constraints": "Head size a multiple of 64; fp16 only."},
		},
		{
			name: "built-in with focus override",
			doc: `kind: ReviewPerspectives
perspectives:
  - name: style
    focus: "Follow the repo's CONTRIBUTING.md conventions."
`,
			wantNames: []string{"style"},
			wantFocus: map[string]string{"style": "Follow the repo's CONTRIBUTING.md conventions."},
		},
		{
			name: "wrong kind",
			doc: `kind: AgentDefinition
perspectives:
  - name: correctness
`,
			wantErr: `expected kind "ReviewPerspectives"`,
		},
		{
			name: "missing kind",
			doc: `perspectives:
  - name: correctness
`,
			wantErr: `expected kind "ReviewPerspectives", got ""`,
		},
		{
			name:    "empty list",
			doc:     "kind: ReviewPerspectives\nperspectives: []\n",
			wantErr: "perspectives list is empty",
		},
		{
			name: "unknown name without focus",
			doc: `kind: ReviewPerspectives
perspectives:
  - name: sekurity
`,
			wantErr: "has no focus text",
		},
		{
			name: "duplicate name",
			doc: `kind: ReviewPerspectives
perspectives:
  - name: security
  - name: Security
`,
			wantErr: "duplicate perspective",
		},
		{
			name:    "oversize",
			doc:     "kind: ReviewPerspectives\n# " + strings.Repeat("x", maxDocumentBytes) + "\nperspectives:\n  - name: correctness\n",
			wantErr: "over the limit",
		},
		{
			name:    "invalid yaml",
			doc:     "kind: [unterminated",
			wantErr: "invalid perspectives YAML",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			names, focus, err := ParseDocument(tc.doc)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseDocument() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDocument() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(names, tc.wantNames) {
				t.Errorf("names = %v, want %v", names, tc.wantNames)
			}
			if !reflect.DeepEqual(focus, tc.wantFocus) {
				t.Errorf("focus = %v, want %v", focus, tc.wantFocus)
			}
		})
	}
}
