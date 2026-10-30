package modelfamily

import "testing"

func TestNormalizeAndFamily(t *testing.T) {
	cases := []struct{ in, norm, family, bucket string }{
		{"Claude-Fable-5.1", "claude-fable-5.1", "claude", BucketClaude},
		{"`gpt-6-astra[preview]`", "gpt-6-astra", "gpt", BucketGPT},
		{"gemini-4-argon", "gemini-4-argon", "gemini", BucketGemini},
		{"grok-4.7", "grok-4.7", "grok", BucketOther},
		{"", Unknown, Unknown, Unknown},
		{"auto", Unknown, Unknown, Unknown},
		{"<model>", Unknown, Unknown, Unknown},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.norm {
			t.Errorf("Normalize(%q)=%q want %q", c.in, got, c.norm)
		}
		if got := Family(c.in); got != c.family {
			t.Errorf("Family(%q)=%q want %q", c.in, got, c.family)
		}
		if got := Bucket(c.in); got != c.bucket {
			t.Errorf("Bucket(%q)=%q want %q", c.in, got, c.bucket)
		}
	}
}

func TestCanonicalFoldsSpellings(t *testing.T) {
	cases := map[string]string{
		"gpt-6.1-sol":                   "gpt-6.1-sol",
		"openai-codex/gpt-6.1-sol":      "gpt-6.1-sol",
		"openai-codex/gpt-5.6-sol:high": "gpt-5.6-sol",
		"claude-opus-5.5":               "claude-opus-5.5",
		"claude-opus-5-5":               "claude-opus-5.5",
		"anthropic/claude-opus-5":       "claude-opus-5",
		"Claude-Sonnet-5-5":             "claude-sonnet-5.5",
		"gpt-6-astra":                   "gpt-6-astra",
		"lemonade/qwen-3":               "qwen-3",
		"":                              Unknown,
		"auto":                          Unknown,
	}
	for in, want := range cases {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSameFamily(t *testing.T) {
	if same, known := SameFamily("claude-fable-5", "claude-opus-4-6"); !same || !known {
		t.Fatalf("same family: got same=%v known=%v", same, known)
	}
	if same, known := SameFamily("claude-fable-5", "gpt-6-astra"); same || !known {
		t.Fatalf("cross family: got same=%v known=%v", same, known)
	}
	if same, known := SameFamily("", "gpt-6-astra"); same || known {
		t.Fatalf("unknown author: got same=%v known=%v", same, known)
	}
}
