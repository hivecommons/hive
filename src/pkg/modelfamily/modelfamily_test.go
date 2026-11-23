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

// A provider or gateway path and a ":variant" suffix name how the model was
// reached, not who made it, so they must not change the family or bucket.
func TestFamilyIgnoresProviderPrefixAndVariant(t *testing.T) {
	cases := []struct{ in, family, bucket string }{
		{"claude-opus-5-5", "claude", BucketClaude},
		{"anthropic/claude-opus-5-5", "claude", BucketClaude},
		{"aws/claude-haiku-4-5", "claude", BucketClaude},
		{"anthropic/claude-sonnet-5:max", "claude", BucketClaude},
		{"gpt-6.1-sol", "gpt", BucketGPT},
		{"openai-codex/gpt-6.1-sol", "gpt", BucketGPT},
		{"openai-codex/gpt-6.1-terra:high", "gpt", BucketGPT},
		{"github-copilot/gpt-5.6-terra", "gpt", BucketGPT},
		{"gemini-3.8-flash", "gemini", BucketGemini},
		{"github-copilot/gemini-3.8-flash", "gemini", BucketGemini},
		{"google/gemini-3.8-flash-low", "gemini", BucketGemini},
		{"qwen-3", "qwen", BucketOther},
		{"lemonade/qwen-3", "qwen", BucketOther},
		{"rits/zai-org/glm-5-3", "glm", BucketOther},
		{"anthropic/", Unknown, Unknown},
		{"-", Unknown, Unknown},
	}
	for _, c := range cases {
		if got := Family(c.in); got != c.family {
			t.Errorf("Family(%q)=%q want %q", c.in, got, c.family)
		}
		if got := Bucket(c.in); got != c.bucket {
			t.Errorf("Bucket(%q)=%q want %q", c.in, got, c.bucket)
		}
	}
}

func TestSameFamilyAcrossProviderSpellings(t *testing.T) {
	pairs := []struct{ a, b string }{
		{"anthropic/claude-opus-5-5", "claude-sonnet-5"},
		{"openai-codex/gpt-6.1-terra:high", "gpt-5.6-sol"},
		{"github-copilot/gemini-3.8-flash", "gemini-4-argon"},
	}
	for _, p := range pairs {
		if same, known := SameFamily(p.a, p.b); !same || !known {
			t.Errorf("SameFamily(%q, %q): got same=%v known=%v, want same vendor", p.a, p.b, same, known)
		}
	}
	if same, known := SameFamily("anthropic/claude-opus-5-5", "openai-codex/gpt-6.1-sol"); same || !known {
		t.Errorf("prefixed cross family: got same=%v known=%v", same, known)
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

// CLI aliases read as the family of the models they resolve to, so a review
// of an aliased author's PR by the same vendor is still "same family".
// Canonical keeps the alias as its own id, and auto stays Unknown.
func TestCLIAliasFamilies(t *testing.T) {
	cases := []struct{ in, family, bucket, canonical string }{
		{"opus", "claude", BucketClaude, "opus"},
		{"Sonnet", "claude", BucketClaude, "sonnet"},
		{"haiku", "claude", BucketClaude, "haiku"},
		{"fable", "claude", BucketClaude, "fable"},
		{"pro", "gemini", BucketGemini, "pro"},
		{"flash", "gemini", BucketGemini, "flash"},
		{"flash-lite", "gemini", BucketGemini, "flash-lite"},
		{"opus:high", "claude", BucketClaude, "opus"},
		{"anthropic/opus", "claude", BucketClaude, "opus"},
		{"google/flash", "gemini", BucketGemini, "flash"},
		{"pi-claude-code-provider/sonnet", "claude", BucketClaude, "sonnet"},
		// Another vendor's model that happens to be named like an alias.
		{"acme/flash", "flash", BucketOther, "flash"},
		// Real ids that contain an alias word keep their own family.
		{"gemini-2.5-pro", "gemini", BucketGemini, "gemini-2.5-pro"},
		{"gpt-5-pro", "gpt", BucketGPT, "gpt-5-pro"},
		{"flash-2", "flash", BucketOther, "flash-2"},
		{"opus-x", "opus", BucketOther, "opus-x"},
		// auto is ambiguous across backends and stays Unknown.
		{"auto", Unknown, Unknown, Unknown},
	}
	for _, c := range cases {
		if got := Family(c.in); got != c.family {
			t.Errorf("Family(%q)=%q want %q", c.in, got, c.family)
		}
		if got := Bucket(c.in); got != c.bucket {
			t.Errorf("Bucket(%q)=%q want %q", c.in, got, c.bucket)
		}
		if got := Canonical(c.in); got != c.canonical {
			t.Errorf("Canonical(%q)=%q want %q (the stats bucket key must not change)", c.in, got, c.canonical)
		}
	}
	for _, p := range []struct{ alias, id string }{
		{"flash", "gemini-2.5-flash"},
		{"pro", "gemini-3-pro-preview"},
		{"flash-lite", "gemini-3.1-flash-lite"},
		{"opus", "claude-opus-5-5"},
		{"fable", "claude-fable-5"},
	} {
		if same, known := SameFamily(p.alias, p.id); !same || !known {
			t.Errorf("SameFamily(%q, %q): same=%v known=%v, want same family", p.alias, p.id, same, known)
		}
	}
	if same, known := SameFamily("flash", "claude-opus-5-5"); same || !known {
		t.Errorf("SameFamily(flash, claude-opus-5-5): same=%v known=%v, want cross family", same, known)
	}
	if _, known := SameFamily("auto", "gemini-2.5-flash"); known {
		t.Error("SameFamily(auto, ...) must stay unknown")
	}
}

func TestResolveCLIAliasVendor(t *testing.T) {
	cases := []struct {
		in   string
		want Resolved
	}{
		{"fable", Resolved{Canonical: "fable", Vendor: "anthropic"}},
		{"pro", Resolved{Canonical: "pro", Vendor: "google"}},
		{"flash", Resolved{Canonical: "flash", Vendor: "google"}},
		{"flash-lite", Resolved{Canonical: "flash-lite", Vendor: "google"}},
		{"flash:high", Resolved{Canonical: "flash", Vendor: "google", Effort: "high"}},
		{"acme/flash", Resolved{Canonical: "flash", Vendor: "acme"}},
		{"auto", Resolved{Canonical: Auto, Vendor: Auto}},
	}
	for _, c := range cases {
		if got := Resolve(c.in, ""); got != c.want {
			t.Errorf("Resolve(%q)=%+v want %+v", c.in, got, c.want)
		}
	}
}
