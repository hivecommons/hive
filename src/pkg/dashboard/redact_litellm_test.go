package dashboard

import (
	"strings"
	"testing"
)

// TestRedactLiteLLMKeyMaterial scrubs key-identifying material a gateway echoes
// in an auth-failure body — the SHA-256 key hash, the masked "Received API Key"
// hint, and any bare sk- token — while keeping the human-useful error text.
func TestRedactLiteLLMKeyMaterial(t *testing.T) {
	in := `{"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = sk-...GTNw, Key Hash (Token) =262b781f7b1e8d33076fba20db1ade92afdcb1f88867648849cdd25a49581fb2. Unable to find token in cache or ` + "`LiteLLM_VerificationTokenTable`" + `","type":"token_not_found_in_db","param":"key","code":"401"}}`

	out := redactLiteLLMKeyMaterial(in)

	// The specific leaked material must be gone.
	if strings.Contains(out, "262b781f7b1e8d33076fba20db1ade92afdcb1f88867648849cdd25a49581fb2") {
		t.Errorf("key hash not redacted:\n%s", out)
	}
	if strings.Contains(out, "sk-...GTNw") {
		t.Errorf("masked api key hint not redacted:\n%s", out)
	}
	// The useful diagnostic text must survive.
	for _, want := range []string{"Authentication Error", "token_not_found_in_db", "401", "[redacted]"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q to survive redaction:\n%s", want, out)
		}
	}
}

// TestRedactLiteLLMKeyMaterial_BareToken redacts a bare sk- token anywhere.
func TestRedactLiteLLMKeyMaterial_BareToken(t *testing.T) {
	out := redactLiteLLMKeyMaterial("failed with key sk-abcdef1234567890 at gateway")
	if strings.Contains(out, "sk-abcdef1234567890") {
		t.Errorf("bare token not redacted: %s", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Errorf("expected [redacted]: %s", out)
	}
}

// TestRedactLiteLLMKeyMaterial_NoFalsePositiveOnPlainText leaves ordinary error
// text (no key material) untouched.
func TestRedactLiteLLMKeyMaterial_NoFalsePositiveOnPlainText(t *testing.T) {
	in := "gateway returned 503: service temporarily unavailable"
	if out := redactLiteLLMKeyMaterial(in); out != in {
		t.Errorf("plain text should be unchanged:\n got  %q\n want %q", out, in)
	}
}
