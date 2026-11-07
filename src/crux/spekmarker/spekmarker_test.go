package spekmarker

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		want   Marker
		wantOK bool
	}{
		{
			name:   "rfc marker with required fields",
			text:   "## Overview\n\n…\n\n<!-- spek:publish name=auth-refresh artifact_id=spk_01HX target=github -->",
			want:   Marker{Name: "auth-refresh", ArtifactID: "spk_01HX", Target: "github"},
			wantOK: true,
		},
		{
			name:   "child marker with task and rfc",
			text:   "<!-- spek:publish name=auth-refresh artifact_id=spk_01HX target=linear task=T3 rfc=https://github.com/o/r/issues/7 -->",
			want:   Marker{Name: "auth-refresh", ArtifactID: "spk_01HX", Target: "linear", Task: "T3", RFC: "https://github.com/o/r/issues/7"},
			wantOK: true,
		},
		{
			name:   "quoted value with spaces and unknown key tolerated",
			text:   `<!-- spek:publish name="my spec" artifact_id=a1 target=Jira future=x -->`,
			want:   Marker{Name: "my spec", ArtifactID: "a1", Target: "jira"},
			wantOK: true,
		},
		{
			name:   "loose whitespace around the comment delimiters",
			text:   "<!--  spek:publish   name=n artifact_id=i target=github   -->",
			want:   Marker{Name: "n", ArtifactID: "i", Target: "github"},
			wantOK: true,
		},
		{name: "no marker", text: "## Goal\n\nJust a body.", wantOK: false},
		{name: "missing artifact_id", text: "<!-- spek:publish name=n target=github -->", wantOK: false},
		{name: "missing target", text: "<!-- spek:publish name=n artifact_id=i -->", wantOK: false},
		{name: "empty value", text: "<!-- spek:publish name= artifact_id=i target=github -->", wantOK: false},
		{
			name:   "template still unsubstituted is not a marker",
			text:   "<!-- spek:publish name=<name> artifact_id=<id> target=<t> -->",
			wantOK: false,
		},
		{name: "bare word without key", text: "<!-- spek:publish name=n artifact_id=i target=github stray -->", wantOK: false},
		{name: "unterminated quote", text: `<!-- spek:publish name="n artifact_id=i target=github -->`, wantOK: false},
		{name: "other hive marker is not confused", text: "<!-- hive:clarity-gate -->", wantOK: false},
		{
			name:   "first valid marker wins over an earlier malformed one",
			text:   "<!-- spek:publish name=n -->\n<!-- spek:publish name=n artifact_id=i target=github -->",
			want:   Marker{Name: "n", ArtifactID: "i", Target: "github"},
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Parse(tt.text)
			if ok != tt.wantOK {
				t.Fatalf("Parse ok = %v, want %v (got %+v)", ok, tt.wantOK, got)
			}
			if ok && got != tt.want {
				t.Fatalf("Parse = %+v, want %+v", got, tt.want)
			}
			if Has(tt.text) != tt.wantOK {
				t.Fatalf("Has = %v, want %v", !tt.wantOK, tt.wantOK)
			}
		})
	}
}

func TestMarkerIsChild(t *testing.T) {
	rfc, _ := Parse("<!-- spek:publish name=n artifact_id=i target=github -->")
	child, _ := Parse("<!-- spek:publish name=n artifact_id=i target=github task=T1 -->")
	if rfc.IsChild() {
		t.Error("RFC marker without task reported as child")
	}
	if !child.IsChild() {
		t.Error("marker with task not reported as child")
	}
}
