// Package ioscan is a deterministic, dependency-free (stdlib only) security
// scanner for agent input and output text.
//
// It inspects two kinds of text:
//
//   - INPUT: issue/PR bodies, comments, and any external text an agent is about
//     to act on. The threat here is prompt injection — text crafted to override
//     the agent's instructions, abuse its tools, or smuggle hidden directives
//     (zero-width characters, base64 blobs that decode to instructions).
//   - OUTPUT: agent-produced text and PR bodies. The threat here is data
//     exfiltration — leaked tokens/keys/private-key material — and dangerous
//     directives (rm -rf, curl|sh, guardrail-disable phrasing) that the agent
//     might propagate.
//
// The scanner is pure: ScanInput/ScanOutput take a string and return a Verdict
// with no I/O, no clock, and no goroutines, so it is trivially testable and safe
// to call inline on the agent path.
//
// Secret detection reuses logscrub.TokenPattern (the single source of truth for
// GitHub/JWT token shapes) rather than duplicating those regexes.
package ioscan

import (
	"encoding/base64"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/logscrub"
)

// Severity ranks a finding's risk. Higher values are more severe.
type Severity int

const (
	// SeverityLow is informational: suspicious but low-confidence or low-impact.
	SeverityLow Severity = iota
	// SeverityMedium warrants attention but is not on its own conclusive.
	SeverityMedium
	// SeverityHigh is a strong signal of injection, a leaked secret, or a
	// dangerous directive.
	SeverityHigh
	// SeverityCritical is an unambiguous, high-impact hit (e.g. a live-shaped
	// credential or destructive command) that should always block.
	SeverityCritical
)

// String renders a Severity for logs and audit records.
func (s Severity) String() string {
	switch s {
	case SeverityLow:
		return "low"
	case SeverityMedium:
		return "medium"
	case SeverityHigh:
		return "high"
	case SeverityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Kind categorizes what a finding represents.
type Kind string

const (
	// KindInjection is a prompt-injection / instruction-override pattern.
	KindInjection Kind = "injection"
	// KindSecret is a leaked credential or key.
	KindSecret Kind = "secret"
	// KindDangerous is a destructive or guardrail-disabling directive.
	KindDangerous Kind = "dangerous"
)

// snippetMaxLen bounds how much matched text is retained in a Finding, so a
// verdict can be logged without echoing an entire payload (and never a full
// secret).
const snippetMaxLen = 80

// entropySampleMinLen is the shortest base64/hex-like run considered for
// high-entropy generic-secret detection. Below this, false positives (hashes in
// prose, short IDs) dominate.
const entropySampleMinLen = 32

// entropyThresholdBits is the Shannon-entropy floor (bits per char) at which a
// long alnum run is treated as a probable generic secret.
const entropyThresholdBits = 4.0

// base64MinDecodedLen is the minimum decoded length for a base64 blob to be
// re-scanned for hidden instructions; shorter decodes are noise.
const base64MinDecodedLen = 12

// Finding is one detected issue.
type Finding struct {
	// Kind categorizes the finding (injection/secret/dangerous).
	Kind Kind `json:"kind"`
	// Severity ranks its risk.
	Severity Severity `json:"severity"`
	// Snippet is a bounded, safe excerpt of the offending text. For secrets it
	// is masked so the raw credential is never surfaced.
	Snippet string `json:"snippet"`
	// Rule is the stable identifier of the rule that fired.
	Rule string `json:"rule"`
}

// Verdict is the structured result of a scan.
type Verdict struct {
	// Findings lists every rule hit, in scan order.
	Findings []Finding `json:"findings"`
	// Blocked is the policy decision: true means the caller should refuse to
	// act on (input) or emit (output) the text.
	Blocked bool `json:"blocked"`
}

// HasFindings reports whether any rule fired.
func (v Verdict) HasFindings() bool { return len(v.Findings) > 0 }

// HasCriticalInjection reports whether the verdict contains a Critical
// prompt-injection finding. Callers that opt into fail-closed semantics use
// this narrower predicate so ordinary High-confidence injection keeps the
// historical redact-and-continue behavior.
func (v Verdict) HasCriticalInjection() bool {
	for _, f := range v.Findings {
		if f.Kind == KindInjection && f.Severity >= SeverityCritical {
			return true
		}
	}
	return false
}

// rule is a single named regex rule with a fixed kind and severity.
type rule struct {
	name     string
	kind     Kind
	severity Severity
	re       *regexp.Regexp
}

// injectionRules detect prompt-injection / instruction-override attempts.
// Patterns are case-insensitive ((?i)) and match on intent phrasing rather than
// exact wording so paraphrases are still caught.
var injectionRules = []rule{
	{
		name:     "injection.ignore_previous",
		kind:     KindInjection,
		severity: SeverityHigh,
		re:       regexp.MustCompile(`(?i)ignore\s+(all\s+|any\s+)?(previous|prior|above|earlier|preceding)\s+(instructions?|prompts?|directions?|context)`),
	},
	{
		name:     "injection.disregard",
		kind:     KindInjection,
		severity: SeverityHigh,
		re:       regexp.MustCompile(`(?i)disregard\s+(all\s+|any\s+|the\s+)?(previous|prior|above|earlier|system)\s+(instructions?|prompts?|rules?|messages?)`),
	},
	{
		name:     "injection.you_are_now",
		kind:     KindInjection,
		severity: SeverityHigh,
		re:       regexp.MustCompile(`(?i)you\s+are\s+now\s+(a|an|the|no\s+longer)\b`),
	},
	{
		name:     "injection.role_override",
		kind:     KindInjection,
		severity: SeverityMedium,
		re:       regexp.MustCompile(`(?i)(new\s+(system\s+)?prompt|new\s+instructions?|new\s+rules?)\s*[:\-]`),
	},
	{
		name:     "injection.pretend_act_as",
		kind:     KindInjection,
		severity: SeverityMedium,
		re:       regexp.MustCompile(`(?i)(pretend\s+(you\s+are|to\s+be)|act\s+as\s+(if\s+you\s+are\s+)?(a|an|the)\b|from\s+now\s+on\s+you)`),
	},
	{
		name:     "injection.developer_system_mode",
		kind:     KindInjection,
		severity: SeverityMedium,
		re:       regexp.MustCompile(`(?i)(developer\s+mode|dan\s+mode|jailbreak|system\s+override|admin\s+override)`),
	},
	{
		name:     "injection.reveal_prompt",
		kind:     KindInjection,
		severity: SeverityMedium,
		re:       regexp.MustCompile(`(?i)(reveal|print|repeat|show|output|leak)\s+(your\s+|the\s+)?(system\s+prompt|initial\s+instructions?|hidden\s+(prompt|instructions?)|your\s+instructions?)`),
	},
	{
		name:     "injection.tool_abuse",
		kind:     KindInjection,
		severity: SeverityHigh,
		re:       regexp.MustCompile(`(?i)(use\s+(your\s+)?(tools?|shell|bash|terminal)\s+to\s+(run|execute|delete|exfiltrate|send)|call\s+the\s+\w+\s+tool\s+to\s+(delete|send|exfiltrate))`),
	},
}

// dangerousRules detect destructive or guardrail-disabling directives.
var dangerousRules = []rule{
	{
		name:     "dangerous.rm_rf",
		kind:     KindDangerous,
		severity: SeverityCritical,
		re:       regexp.MustCompile(`(?i)\brm\s+-[a-z]*r[a-z]*f[a-z]*\b|\brm\s+-[a-z]*f[a-z]*r[a-z]*\b`),
	},
	{
		name:     "dangerous.curl_pipe_sh",
		kind:     KindDangerous,
		severity: SeverityCritical,
		re:       regexp.MustCompile(`(?i)(curl|wget)\s+[^\n|]*\|\s*(sudo\s+)?(sh|bash|zsh)\b`),
	},
	{
		name:     "dangerous.disable_guardrails",
		kind:     KindDangerous,
		severity: SeverityHigh,
		re:       regexp.MustCompile(`(?i)(disable|turn\s+off|bypass|ignore|remove)\s+(the\s+|all\s+|your\s+)?(guardrails?|safety\s+(checks?|filters?)|content\s+filters?|restrictions?|protections?)`),
	},
	{
		name:     "dangerous.exfiltrate",
		kind:     KindDangerous,
		severity: SeverityHigh,
		re:       regexp.MustCompile(`(?i)(exfiltrate|send\s+(the\s+)?(secrets?|tokens?|credentials?|env(ironment)?\s+vars?|api\s+keys?)\s+to|upload\s+.*(secrets?|credentials?)\s+to|post\s+.*(token|secret)\s+to\s+https?://)`),
	},
	{
		name:     "dangerous.fork_bomb",
		kind:     KindDangerous,
		severity: SeverityCritical,
		re:       regexp.MustCompile(`:\(\)\s*\{\s*:\|:&\s*\}\s*;`),
	},
	{
		name:     "dangerous.chmod_777_root",
		kind:     KindDangerous,
		severity: SeverityMedium,
		re:       regexp.MustCompile(`(?i)chmod\s+-R\s+777\s+/`),
	},
	{
		name:     "dangerous.read_env_secrets",
		kind:     KindDangerous,
		severity: SeverityMedium,
		re:       regexp.MustCompile(`(?i)(cat|print|echo|dump)\s+.*(/\.env|~/\.aws/credentials|id_rsa|\.ssh/id_)`),
	},
}

// secretRules detect leaked credentials by provider-specific shape. The generic
// GitHub/JWT shapes are delegated to logscrub.TokenPattern (see scanSecrets) to
// avoid duplicating that regex; these cover additional providers.
var secretRules = []rule{
	{
		name:     "secret.aws_access_key_id",
		kind:     KindSecret,
		severity: SeverityCritical,
		re:       regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`),
	},
	{
		name:     "secret.private_key_header",
		kind:     KindSecret,
		severity: SeverityCritical,
		re:       regexp.MustCompile(`-----BEGIN\s+(RSA\s+|EC\s+|OPENSSH\s+|DSA\s+|PGP\s+)?PRIVATE\s+KEY-----`),
	},
	{
		name:     "secret.slack_token",
		kind:     KindSecret,
		severity: SeverityHigh,
		re:       regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}\b`),
	},
	{
		name:     "secret.google_api_key",
		kind:     KindSecret,
		severity: SeverityHigh,
		re:       regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	},
	{
		name:     "secret.openai_key",
		kind:     KindSecret,
		severity: SeverityHigh,
		re:       regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`),
	},
}

const (
	unicodeSteganographyRule    = "injection.unicode_steganography"
	unicodeSteganographySnippet = "<unicode steganography characters>"
	unicodeVariationBurstLimit  = 2
)

// base64BlobRe finds standalone base64-looking runs (long, alphabet-only) that
// may hide instructions once decoded.
var base64BlobRe = regexp.MustCompile(`[A-Za-z0-9+/]{20,}={0,2}`)

// highEntropyRunRe finds long alphanumeric runs that are candidate generic
// secrets subject to an entropy check.
var highEntropyRunRe = regexp.MustCompile(`[A-Za-z0-9_\-+/]{` + strconv.Itoa(entropySampleMinLen) + `,}`)

// Scanner runs the rule tables against text. It holds no mutable state and is
// safe for concurrent use.
type Scanner struct{}

// NewScanner returns a ready-to-use Scanner.
func NewScanner() *Scanner { return &Scanner{} }

// ScanInput scans external text an agent is about to act on. It runs the full
// rule set (injection is the primary concern on input, but secrets and
// dangerous directives are also flagged when present).
func (s *Scanner) ScanInput(text string) Verdict {
	findings := s.collect(text)
	return Verdict{Findings: findings, Blocked: blockedInput(findings)}
}

// ScanOutput scans agent-produced text before it is emitted. Secret leakage and
// dangerous directives are the primary concern on output, but injection
// echo-back is also flagged.
func (s *Scanner) ScanOutput(text string) Verdict {
	findings := s.collect(text)
	return Verdict{Findings: findings, Blocked: blockedOutput(findings)}
}

// ScanInput is a package-level convenience equivalent to
// NewScanner().ScanInput, suitable as a "fullsend scan"-style entry point from a
// CLI or the pipeline.
func ScanInput(text string) Verdict { return NewScanner().ScanInput(text) }

// ScanOutput is a package-level convenience equivalent to
// NewScanner().ScanOutput.
func ScanOutput(text string) Verdict { return NewScanner().ScanOutput(text) }

// collect runs every detector and returns findings in a stable order:
// injection, dangerous, secrets, then structural (unicode/base64/entropy).
func (s *Scanner) collect(text string) []Finding {
	normalized := normalizeUnicodeSteganography(text)
	scanText := normalized.scanText
	var findings []Finding
	findings = append(findings, matchRules(scanText, injectionRules)...)
	findings = append(findings, matchRules(scanText, dangerousRules)...)
	findings = append(findings, scanSecrets(scanText)...)
	findings = append(findings, normalized.findings...)
	findings = append(findings, scanBase64Instructions(scanText)...)
	findings = append(findings, scanGenericSecrets(scanText)...)
	return findings
}

// matchRules applies a rule table to text and returns one finding per rule that
// fires (deduplicated per rule; a rule reports at most once).
func matchRules(text string, rules []rule) []Finding {
	var out []Finding
	for _, r := range rules {
		if loc := r.re.FindString(text); loc != "" {
			out = append(out, Finding{
				Kind:     r.kind,
				Severity: r.severity,
				Snippet:  snippet(loc),
				Rule:     r.name,
			})
		}
	}
	return out
}

// scanSecrets checks provider-specific secret rules plus the shared
// logscrub.TokenPattern (GitHub tokens / JWTs). Matched secrets are masked in
// the snippet so the raw value is never surfaced.
func scanSecrets(text string) []Finding {
	var out []Finding
	for _, r := range secretRules {
		if loc := r.re.FindString(text); loc != "" {
			out = append(out, Finding{
				Kind:     r.kind,
				Severity: r.severity,
				Snippet:  mask(loc),
				Rule:     r.name,
			})
		}
	}
	if loc := logscrub.TokenPattern.FindString(text); loc != "" {
		out = append(out, Finding{
			Kind:     KindSecret,
			Severity: SeverityCritical,
			Snippet:  mask(loc),
			Rule:     "secret.github_or_jwt",
		})
	}
	return out
}

type unicodeNormalization struct {
	scanText string
	findings []Finding
}

// normalizeUnicodeSteganography removes hidden formatting controls from the
// text used by detectors and folds a small set of high-risk homoglyphs back to
// ASCII. This lets "igno\u200bre" and "\u0456gnore" trip the same rule as "ignore",
// while still emitting a stable finding for audit/fail-closed policy.
func normalizeUnicodeSteganography(text string) unicodeNormalization {
	if text == "" || utf8.RuneCountInString(text) == len(text) {
		return unicodeNormalization{scanText: text}
	}

	runes := []rune(text)
	hasASCII := false
	var variationSelectors int
	for _, r := range runes {
		if isASCIIAlphaNum(r) {
			hasASCII = true
		}
		if isVariationSelector(r) {
			variationSelectors++
		}
	}

	var b strings.Builder
	suspicious := false
	for i, r := range runes {
		if isInvisibleControl(r) || isTagCharacter(r) {
			suspicious = true
			continue
		}
		if isVariationSelector(r) && isSuspiciousVariationSelector(runes, i, variationSelectors) {
			suspicious = true
			continue
		}
		if folded, ok := confusableASCII(r); ok && hasASCII {
			suspicious = true
			b.WriteRune(folded)
			continue
		}
		b.WriteRune(r)
	}

	if !suspicious {
		return unicodeNormalization{scanText: text}
	}
	return unicodeNormalization{
		scanText: b.String(),
		findings: []Finding{{
			Kind:     KindInjection,
			Severity: SeverityCritical,
			Snippet:  unicodeSteganographySnippet,
			Rule:     unicodeSteganographyRule,
		}},
	}
}

func isInvisibleControl(r rune) bool {
	switch r {
	case '\u180e', '\u200b', '\u200c', '\u200d', '\u200e', '\u200f', '\ufeff':
		return true
	}
	return (r >= '\u202a' && r <= '\u202e') || (r >= '\u2060' && r <= '\u2064') || (r >= '\u2066' && r <= '\u2069')
}

func isTagCharacter(r rune) bool {
	return r >= '\U000e0000' && r <= '\U000e007f'
}

func isVariationSelector(r rune) bool {
	return (r >= '\ufe00' && r <= '\ufe0f') || (r >= '\U000e0100' && r <= '\U000e01ef')
}

func isSuspiciousVariationSelector(runes []rune, i, total int) bool {
	if total > unicodeVariationBurstLimit {
		return true
	}
	if i > 0 && isASCIIAlphaNum(runes[i-1]) {
		return true
	}
	return i+1 < len(runes) && isASCIIAlphaNum(runes[i+1])
}

func isASCIIAlphaNum(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

func confusableASCII(r rune) (rune, bool) {
	switch r {
	case '\u0391', '\u0410': // Greek/Cyrillic A
		return 'A', true
	case '\u0392', '\u0412': // Greek/Cyrillic B
		return 'B', true
	case '\u0395', '\u0415': // Greek/Cyrillic E
		return 'E', true
	case '\u0397', '\u041d': // Greek Eta / Cyrillic En
		return 'H', true
	case '\u0399', '\u0406': // Greek Iota / Cyrillic Byelorussian-Ukrainian I
		return 'I', true
	case '\u039a', '\u041a': // Greek/Cyrillic K
		return 'K', true
	case '\u039c', '\u041c': // Greek/Cyrillic M
		return 'M', true
	case '\u039d': // Greek Nu
		return 'N', true
	case '\u039f', '\u041e': // Greek/Cyrillic O
		return 'O', true
	case '\u03a1', '\u0420': // Greek Rho / Cyrillic Er
		return 'P', true
	case '\u03a4', '\u0422': // Greek/Cyrillic T
		return 'T', true
	case '\u03a7', '\u0425': // Greek/Cyrillic Ha
		return 'X', true
	case '\u03a5', '\u04ae': // Greek Upsilon / Cyrillic straight U
		return 'Y', true
	case '\u0430':
		return 'a', true
	case '\u0441':
		return 'c', true
	case '\u0435':
		return 'e', true
	case '\u0456':
		return 'i', true
	case '\u0458':
		return 'j', true
	case '\u043e', '\u03bf':
		return 'o', true
	case '\u0440':
		return 'p', true
	case '\u0455':
		return 's', true
	case '\u0445':
		return 'x', true
	case '\u0443':
		return 'y', true
	}
	return 0, false
}

// scanBase64Instructions decodes standalone base64 blobs and, if the decoded
// text itself contains injection or dangerous phrasing, reports a finding. This
// catches instructions smuggled past a naive text scan by encoding them.
func scanBase64Instructions(text string) []Finding {
	var out []Finding
	for _, blob := range base64BlobRe.FindAllString(text, -1) {
		decoded, ok := tryDecodeBase64(blob)
		if !ok || len(decoded) < base64MinDecodedLen {
			continue
		}
		nested := append(matchRules(decoded, injectionRules), matchRules(decoded, dangerousRules)...)
		if len(nested) > 0 {
			out = append(out, Finding{
				Kind:     KindInjection,
				Severity: SeverityHigh,
				Snippet:  snippet("base64->" + decoded),
				Rule:     "injection.base64_encoded",
			})
			// One report per blob is enough; the decoded content is the signal.
			break
		}
	}
	return out
}

// scanGenericSecrets flags long, high-entropy alnum runs that no named rule
// caught — a heuristic for opaque generic credentials. Reported at Medium since
// it is a heuristic and prone to some noise (hashes, IDs).
func scanGenericSecrets(text string) []Finding {
	var out []Finding
	for _, run := range highEntropyRunRe.FindAllString(text, -1) {
		// Skip runs already covered by a named secret rule to avoid double
		// reporting the same credential.
		if isKnownSecretShape(run) {
			continue
		}
		if shannonBits(run) >= entropyThresholdBits {
			out = append(out, Finding{
				Kind:     KindSecret,
				Severity: SeverityMedium,
				Snippet:  mask(run),
				Rule:     "secret.high_entropy",
			})
			// A single representative finding suffices; avoid flooding on a doc
			// full of hashes.
			break
		}
	}
	return out
}

// isKnownSecretShape reports whether a run is already matched by a named secret
// rule (or the shared token pattern), so scanGenericSecrets can skip it.
func isKnownSecretShape(run string) bool {
	for _, r := range secretRules {
		if r.re.MatchString(run) {
			return true
		}
	}
	return logscrub.TokenPattern.MatchString(run)
}

// blockedInput is the block policy for input: any Critical finding, or any
// injection at High or above (untrusted text trying to override the agent).
func blockedInput(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity >= SeverityCritical {
			return true
		}
		if f.Kind == KindInjection && f.Severity >= SeverityHigh {
			return true
		}
	}
	return false
}

// blockedOutput is the block policy for output: stricter than input. Any
// Critical finding, or any High finding of any kind (a High secret/dangerous/
// injection in agent output must not be emitted).
func blockedOutput(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity >= SeverityHigh {
			return true
		}
	}
	return false
}

// tryDecodeBase64 attempts std and raw base64 decoding and reports success only
// when the decoded bytes are printable-ish text (avoids treating random binary
// as decoded instructions).
func tryDecodeBase64(s string) (string, bool) {
	for _, dec := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
	} {
		if b, err := dec(s); err == nil && isMostlyPrintable(b) {
			return string(b), true
		}
	}
	return "", false
}

// isMostlyPrintable reports whether the bytes look like text (used to reject
// binary base64 payloads).
func isMostlyPrintable(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	printable := 0
	for _, c := range b {
		if c == '\n' || c == '\t' || c == '\r' || (c >= 0x20 && c < 0x7f) {
			printable++
		}
	}
	const printableFraction = 0.85
	return float64(printable)/float64(len(b)) >= printableFraction
}

// shannonBits returns the Shannon entropy of s in bits per character.
func shannonBits(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var bits float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		bits -= p * math.Log2(p)
	}
	return bits
}

// snippet returns a bounded, single-line excerpt of s for logging.
func snippet(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > snippetMaxLen {
		return s[:snippetMaxLen] + "..."
	}
	return s
}

// mask returns a redacted representation of a secret: it never echoes the raw
// value, only a shape hint (first 4 chars + length).
func mask(s string) string {
	prefix := s
	const keep = 4
	if len(prefix) > keep {
		prefix = prefix[:keep]
	}
	return prefix + "***(len=" + strconv.Itoa(len(s)) + ")"
}
