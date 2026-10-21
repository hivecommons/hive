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
