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
		"-":                             Unknown,
		"gemini-3.8-flash-low":          "gemini-3.8-flash-low",
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

func TestResolveFoldsRuntimeSpellings(t *testing.T) {
	cases := []struct {
		in, provider string
		want         Resolved
	}{
		{"claude-sonnet-5-5", "anthropic", Resolved{Canonical: "claude-sonnet-5.5", Vendor: "anthropic"}},
		{"claude-sonnet-5.5", "anthropic", Resolved{Canonical: "claude-sonnet-5.5", Vendor: "anthropic"}},
		{"openai-codex/gpt-6.1-sol", "openai-codex", Resolved{Canonical: "gpt-6.1-sol", Vendor: "openai"}},
		{"gpt-6.1-sol", "openai", Resolved{Canonical: "gpt-6.1-sol", Vendor: "openai"}},
		{"auto", "unknown", Resolved{Canonical: Auto, Vendor: Auto}},
		{"default", "", Resolved{Canonical: Auto, Vendor: Auto}},
		{"claude-opus-4-6", "", Resolved{Canonical: "claude-opus-4.6", Vendor: "anthropic"}},
		{"claude-opus-4.6", "", Resolved{Canonical: "claude-opus-4.6", Vendor: "anthropic"}},
		{"gemini-3.8-flash-low", "google", Resolved{Canonical: "gemini-3.8-flash", Vendor: "google", Effort: "low"}},
		{"anthropic/claude-sonnet-5:max", "anthropic", Resolved{Canonical: "claude-sonnet-5", Vendor: "anthropic", Effort: "max"}},
		{"openai-codex/gpt-5.6-sol:high", "openai-codex", Resolved{Canonical: "gpt-5.6-sol", Vendor: "openai", Effort: "high"}},
		{"aws/claude-haiku-4-5", "aws", Resolved{Canonical: "claude-haiku-4.5", Vendor: "anthropic"}},
		{"github-copilot/gemini-3.8-flash", "github-copilot", Resolved{Canonical: "gemini-3.8-flash", Vendor: "google"}},
		{"rits/zai-org/glm-5-3", "rits", Resolved{Canonical: "glm-5.3", Vendor: "z-ai"}},
		{"lemonade/ornith-1.5-35b-a3b-gguf-q6_k", "lemonade", Resolved{Canonical: "ornith-1.5-35b-a3b-gguf-q6-k", Vendor: "lemonade"}},
		{"pi-claude-code-provider/sonnet", "pi-claude-code-provider", Resolved{Canonical: "claude-sonnet", Vendor: "anthropic"}},
		{"GPT-5.6 Terra", "openai", Resolved{Canonical: "gpt-5.6-terra", Vendor: "openai"}},
		{"claude-sonnet-4-5-20250929", "", Resolved{Canonical: "claude-sonnet-4.5", Vendor: "anthropic"}},
		{"kimi-k3", "moonshot", Resolved{Canonical: "kimi-k3", Vendor: "moonshot"}},
		{"unknown", "unknown", Resolved{Canonical: Unknown, Vendor: Unknown}},
		{"", "", Resolved{Canonical: Unknown, Vendor: Unknown}},
		{"ornith-1.5", " Lemonade ", Resolved{Canonical: "ornith-1.5", Vendor: "lemonade"}},
		{"ornith-1.5", "aws", Resolved{Canonical: "ornith-1.5", Vendor: Unknown}},
		{"ornith-1.5", "unknown", Resolved{Canonical: "ornith-1.5", Vendor: Unknown}},
		{"ornith-1.5", "", Resolved{Canonical: "ornith-1.5", Vendor: Unknown}},
		{"-", "", Resolved{Canonical: Unknown, Vendor: Unknown}},
		{"-:high", "", Resolved{Canonical: Unknown, Vendor: Unknown, Effort: "high"}},
		{"default:high", "", Resolved{Canonical: Auto, Vendor: Auto, Effort: "high"}},
		{"claude--opus-high", "", Resolved{Canonical: "claude-opus", Vendor: "anthropic", Effort: "high"}},
		{"opus", "", Resolved{Canonical: "claude-opus", Vendor: "anthropic"}},
		{"haiku:low", "", Resolved{Canonical: "claude-haiku", Vendor: "anthropic", Effort: "low"}},
		{"gpt-5.6-sol:fast", "openai", Resolved{Canonical: "gpt-5.6-sol", Vendor: "openai"}},
		{"gemini-3.8-flash-low:high", "google", Resolved{Canonical: "gemini-3.8-flash", Vendor: "google", Effort: "high"}},
		{"`'Claude-Opus-4-6'`", "", Resolved{Canonical: "claude-opus-4.6", Vendor: "anthropic"}},
	}
	for _, c := range cases {
		if got := Resolve(c.in, c.provider); got != c.want {
			t.Errorf("Resolve(%q,%q)=%+v want %+v", c.in, c.provider, got, c.want)
		}
	}
}
