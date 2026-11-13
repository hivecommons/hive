// Package modelfamily normalizes model identifiers and derives their vendor
// family. It has no dependencies so that pkg/config (review-model selection),
// pkg/review (swarm dispatch), pkg/github (attribution trailers) and
// pkg/evidence (accuracy bucketing) can all agree on one rule without an
// import cycle.
package modelfamily

import "strings"

// Unknown is the normalized value for an absent or placeholder model.
const Unknown = "unknown"

// Buckets the evidence accuracy card reports authors under. Any model whose
// family is not one of the named vendors lands in BucketOther; a PR with no
// model at all (a person) is BucketHuman.
const (
	BucketClaude = "claude"
	BucketGPT    = "gpt"
	BucketGemini = "gemini"
	BucketOther  = "other"
	BucketHuman  = "human"
)

// Normalize canonicalizes a model (or any attribution footer value):
// lowercase, strip quotes/backticks, drop bracketed suffixes such as
// "model[preview]". Empty and placeholder values become Unknown.
func Normalize(value string) string {
	v := strings.TrimSpace(strings.ToLower(value))
	v = strings.Trim(v, "`'\"“”‘’")
	if i := strings.Index(v, "["); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	v = strings.Trim(v, "`'\"“”‘’")
	switch v {
	case "", "auto", "<agent>", "<backend>", "<model>", "...", "…", "placeholder":
		return Unknown
	}
	return v
}

// Canonical folds the spellings one model arrives under into a single id so
// per-model statistics aggregate: Normalize, then drop a provider prefix
// ("openai-codex/gpt-6.1-sol" → "gpt-6.1-sol", "anthropic/claude-opus-5" →
// "claude-opus-5"), a ":variant" suffix ("gpt-5.6-sol:high" → "gpt-5.6-sol")
// and a dash between two digits ("claude-opus-5-5" → "claude-opus-5.5").
// Unknown stays Unknown. Reasoning-effort suffixes are kept; Resolve strips
// them.
func Canonical(model string) string {
	m := Normalize(model)
	if m == Unknown {
		return Unknown
	}
	m = canonicalID(m)
	if m == "" {
		return Unknown
	}
	return m
}

// Auto is the canonical id and vendor of a runtime's auto-selected model:
// the harness picked, the hive was not told which.
const Auto = "auto"

// Resolved is the canonical reading of a raw model id.
type Resolved struct {
	// Canonical is the id spellings fold to ("claude-sonnet-5.5"); Auto for
	// a runtime auto-select; Unknown when nothing was reported.
	Canonical string
	// Vendor is the model's vendor family ("anthropic", "openai", ...),
	// inferred from the model name when the reported prefix is a CLI or
	// gateway ("openai-codex/", "aws/"); Auto for Auto, Unknown otherwise.
	Vendor string
	// Effort is the reasoning-effort label stripped from the id
	// ("gemini-3.8-flash-low" → "low", "gpt-5.6-sol:high" → "high"); empty
	// when none was reported.
	Effort string
}

// effortLabels are the reasoning-effort suffixes harnesses glue onto ids.
var effortLabels = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// gateways are prefixes that name the CLI or routing layer, not the vendor,
// so they never become Resolved.Vendor.
var gateways = map[string]bool{
	"openai-codex": true, "github-copilot": true, "copilot": true, "openrouter": true, "aws": true, "bedrock": true,
	"vertex": true, "azure": true, "rits": true, "pi-claude-code-provider": true, "kilocode": true, "lab-worker": true,
	"stealth": true,
}

// vendorFamilies maps a canonical id's family (text before the first dash)
// to its vendor.
var vendorFamilies = map[string]string{
	"claude": "anthropic", "gpt": "openai", "o1": "openai", "o3": "openai", "o4": "openai", "codex": "openai",
	"gemini": "google", "gemma": "google", "granite": "ibm", "llama": "meta", "mistral": "mistral", "qwen": "alibaba",
	"deepseek": "deepseek", "grok": "xai", "kimi": "moonshot", "glm": "z-ai", "minimax": "minimax", "mimo": "xiaomi",
	"muse": "meta",
}

// canonicalOverrides are ids that do not follow the generic rules.
var canonicalOverrides = map[string]string{
	"sonnet": "claude-sonnet", "opus": "claude-opus", "haiku": "claude-haiku", "default": Auto,
}

// Resolve reads a raw model id into its canonical id, vendor and effort.
// reportedProvider is the vendor the harness claimed, if any; it is used
// only when the model name itself does not identify a vendor and it is not
// a CLI or gateway name.
func Resolve(model, reportedProvider string) Resolved {
	raw := strings.TrimSpace(strings.ToLower(model))
	raw = strings.Trim(raw, "`'\"“”‘’")
	if raw == Auto || raw == "default" {
		return Resolved{Canonical: Auto, Vendor: Auto}
	}
	m := Normalize(raw)
	if m == Unknown {
		return Resolved{Canonical: Unknown, Vendor: Unknown}
	}
	prefix := ""
	if i := strings.Index(m, "/"); i >= 0 {
		prefix = m[:i]
	}
	var effort string
	if i := strings.Index(m, ":"); i >= 0 {
		if effortLabels[m[i+1:]] {
			effort = m[i+1:]
		}
	}
	id := canonicalID(m)
	if parts := strings.Split(id, "-"); len(parts) > 1 && effortLabels[parts[len(parts)-1]] {
		if effort == "" {
			effort = parts[len(parts)-1]
		}
		id = strings.Join(parts[:len(parts)-1], "-")
	}
	if o, ok := canonicalOverrides[id]; ok {
		id = o
	}
	if id == "" {
		return Resolved{Canonical: Unknown, Vendor: Unknown, Effort: effort}
	}
	if id == Auto {
		return Resolved{Canonical: Auto, Vendor: Auto, Effort: effort}
	}
	vendor := Unknown
	family := id
	if i := strings.Index(id, "-"); i >= 0 {
		family = id[:i]
	}
	if v, ok := vendorFamilies[family]; ok {
		vendor = v
	} else if prefix != "" && !gateways[prefix] {
		vendor = prefix
	} else if p := strings.TrimSpace(strings.ToLower(reportedProvider)); p != "" && p != Unknown && !gateways[p] {
		vendor = p
	}
	return Resolved{Canonical: id, Vendor: vendor, Effort: effort}
}

// canonicalID applies the spelling folds to an already-normalized id: drop
// path prefixes and ":variant", spaces and underscores to dashes, join
// consecutive all-digit segments with dots ("sonnet-5-5" → "sonnet-5.5" but
// "ornith-1.5-35b" stays), drop an 8-digit date stamp ("-20250929").
func canonicalID(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.Index(m, ":"); i >= 0 {
		m = m[:i]
	}
	m = strings.NewReplacer(" ", "-", "_", "-").Replace(m)
	for strings.Contains(m, "--") {
		m = strings.ReplaceAll(m, "--", "-")
	}
	parts := strings.Split(strings.Trim(m, "-"), "-")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		if len(p) == 8 && allDigits(p) && len(out) > 0 {
			continue
		}
		if n := len(out); n > 0 && allDigits(p) && allDigits(lastSegment(out[n-1])) {
			out[n-1] += "." + p
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "-")
}

// lastSegment is the text after the final dot, so "5.5" reads as digits.
func lastSegment(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Family is the normalized vendor prefix before the first dash: claude, gpt,
// gemini, grok, ... Unknown stays Unknown.
func Family(model string) string {
	m := Normalize(model)
	if m == Unknown {
		return Unknown
	}
	if i := strings.Index(m, "-"); i >= 0 {
		return m[:i]
	}
	return m
}

// Bucket folds a model's family into the fixed set the accuracy card shows:
// the named vendors keep their family, everything else known is BucketOther,
// and an unknown model stays Unknown so an agent without a recorded model is
// never mistaken for a person.
func Bucket(model string) string {
	switch f := Family(model); f {
	case Unknown:
		return Unknown
	case BucketClaude, BucketGPT, BucketGemini:
		return f
	default:
		return BucketOther
	}
}

// SameFamily reports whether both models are known and share a family. The
// second result is false when either side is Unknown, so callers can tell
// "cross-family" from "cannot say".
func SameFamily(a, b string) (same, known bool) {
	fa, fb := Family(a), Family(b)
	if fa == Unknown || fb == Unknown {
		return false, false
	}
	return fa == fb, true
}
