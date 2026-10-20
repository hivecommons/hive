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
