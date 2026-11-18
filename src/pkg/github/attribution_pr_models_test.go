package github

import "testing"

func TestParseAttributionTrailerNormalizesFinalFooter(t *testing.T) {
	body := "quoted example\n-- hive: agent=<agent> backend=<backend> model=<model>\n\nreal footer\n-- hive: agent=`Scanner` backend=\"Claude\" model='Claude-Fable-5[latest]'"
	meta, ok := ParseAttributionTrailer(body)
	if !ok {
		t.Fatal("expected trailing attribution footer")
	}
	if meta.Agent != "scanner" || meta.Backend != "claude" || meta.Model != "claude-fable-5" {
		t.Fatalf("meta = %+v, want scanner/claude/claude-fable-5", meta)
	}
}

func TestParseAttributionTrailerBucketsPlaceholdersAndAutoAsUnknown(t *testing.T) {
	for _, body := range []string{
		"-- hive: agent=<agent> backend=<backend> model=<model>",
		"summary\n\n-- hive: agent=scanner backend=bob model=auto",
	} {
		meta, ok := ParseAttributionTrailer(body)
		if !ok {
			t.Fatalf("expected footer in %q", body)
		}
		if meta.Model != "unknown" {
			t.Fatalf("model = %q, want unknown for %q", meta.Model, body)
		}
	}
}

func TestParseAttributionTrailerRequiresHiveLine(t *testing.T) {
	if _, ok := ParseAttributionTrailer("agent=scanner backend=claude model=sonnet"); ok {
		t.Fatal("plain key/value text must not count as an attribution footer")
	}
	if _, ok := ParseAttributionTrailer("-- hive: agent=<agent> backend=<backend> model=<model>\n\nmore prose after the template"); ok {
		t.Fatal("non-trailing hive template mention must not count as an attribution footer")
	}
}

func TestParseAttributionTrailerReadsEffort(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"present", "Done\n\n-- hive: agent=scanner backend=codex model=gpt-6-astra effort=high", "high"},
		{"absent", "Done\n\n-- hive: agent=scanner backend=codex model=gpt-6-astra", ""},
		{"upper-case", "-- hive: agent=scanner backend=codex model=gpt-6-astra EFFORT=Medium", "medium"},
		{"quoted", "-- hive: agent=scanner backend=codex model=gpt-6-astra effort=\"Low\"", "low"},
		{"empty", "-- hive: agent=scanner backend=codex model=gpt-6-astra effort=", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta, ok := ParseAttributionTrailer(tc.body)
			if !ok {
				t.Fatal("expected attribution trailer")
			}
			if meta.Effort != tc.want {
				t.Fatalf("effort = %q, want %q", meta.Effort, tc.want)
			}
			if meta.Agent != "scanner" || meta.Backend != "codex" || meta.Model != "gpt-6-astra" {
				t.Fatalf("meta = %+v, want scanner/codex/gpt-6-astra unchanged", meta)
			}
		})
	}
}
