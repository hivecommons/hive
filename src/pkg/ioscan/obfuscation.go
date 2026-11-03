package ioscan

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Obfuscation layers. An attacker who knows the rule table hides the trigger
// phrase behind a cheap encoding (hex escapes, HTML entities, percent
// encoding, rot13, slug casing) or quotes it as "just a mention". This file
// peels each encoding back to plain text and runs the phrase rules on the
// result, and it recognises the quoted-mention shape so a bug report that
// talks ABOUT an injection string is not treated as one.

const (
	// obfuscationMinDecodedLen is the shortest decoded layer worth scanning;
	// anything shorter cannot hold an instruction.
	obfuscationMinDecodedLen = 6
	// obfuscationMinHexPairs is how many consecutive byte pairs a spaced-hex
	// run needs before it is decoded. Shorter runs are hashes and offsets.
	obfuscationMinHexPairs = 8
	// base64MaxNesting bounds how many times a blob is re-decoded when the
	// decoded text is itself base64.
	base64MaxNesting = 3
	// quotedMentionMaxSpan is the longest inline quoted span treated as a
	// mention. Longer quotes are payloads, not examples.
	quotedMentionMaxSpan = 160
	// quotedMentionContextLen is how far back from the opening quote the
	// framing word ("containing", "the phrase", ...) may sit.
	quotedMentionContextLen = 60
	// obfuscatedRule is the rule name for a phrase rule that fired only after
	// an encoding layer was peeled. The snippet names the layer.
	obfuscatedRule = "injection.obfuscated"
	// QuotedRuleSuffix marks a finding whose match sat inside an inline quote
	// framed as a mention. It is reported at Low so it is visible in audit
	// but does not block.
	QuotedRuleSuffix = "@quoted"
)

var (
	hexEscapeRunRe   = regexp.MustCompile(`(?:\\x[0-9A-Fa-f]{2}){3,}`)
	htmlEntityRe     = regexp.MustCompile(`&(?:#\d{2,3}|#x[0-9A-Fa-f]{2}|[a-z]{2,6});`)
	percentEscapeRe  = regexp.MustCompile(`(?:%[0-9A-Fa-f]{2}){3,}`)
	spacedHexRunRe   = regexp.MustCompile(`\b(?:[0-9A-Fa-f]{2}[ :,]){` + strconv.Itoa(obfuscationMinHexPairs-1) + `,}[0-9A-Fa-f]{2}\b`)
	slugRunRe        = regexp.MustCompile(`[A-Za-z]+(?:[-_][A-Za-z]+){2,}`)
	quotedMentionCue = regexp.MustCompile(`(?i)\b(contain(s|ing|ed)?|string|phrase|text|words?|message|like|such\s+as|e\.g\.|literal|pattern|payload|example|report(s|ed)?\s+that|saying|says|wrote|paste[sd]?)\W*$`)
)

// decodedLayer is one obfuscation layer peeled from the input.
type decodedLayer struct {
	name string
	text string
}

// decodeObfuscations returns every plain-text layer hidden in text. Each
// layer is scanned with the phrase rules by scanObfuscated; a layer that
// decodes to nothing useful is simply empty.
func decodeObfuscations(text string) []decodedLayer {
	var layers []decodedLayer
	add := func(name, decoded string) {
		if len(decoded) >= obfuscationMinDecodedLen && decoded != text {
			layers = append(layers, decodedLayer{name: name, text: decoded})
		}
	}
	if hexEscapeRunRe.MatchString(text) {
		add("hex", hexEscapeRunRe.ReplaceAllStringFunc(text, decodeHexEscapes))
	}
	if htmlEntityRe.MatchString(text) {
		add("entity", html.UnescapeString(text))
	}
	if percentEscapeRe.MatchString(text) {
		add("percent", percentEscapeRe.ReplaceAllStringFunc(text, func(s string) string {
			if d, err := url.PathUnescape(s); err == nil && isMostlyPrintable([]byte(d)) {
				return d
			}
			return s
		}))
	}
	if spacedHexRunRe.MatchString(text) {
		add("spaced-hex", spacedHexRunRe.ReplaceAllStringFunc(text, decodeSpacedHex))
	}
	if slugRunRe.MatchString(text) {
		add("slug", slugRunRe.ReplaceAllStringFunc(text, func(s string) string {
			return strings.NewReplacer("-", " ", "_", " ").Replace(s)
		}))
	}
	add("rot13", rot13(text))
	return layers
}

// scanObfuscated runs the phrase rules on each decoded layer and reports one
// High injection finding naming the first layer that carried a hit. The
// plain text has already been scanned, so only the hidden layers are new.
func scanObfuscated(text string) []Finding {
	for _, layer := range decodeObfuscations(text) {
		nested := append(matchRules(layer.text, injectionRules), matchRules(layer.text, dangerousRules)...)
		for _, f := range nested {
			if strings.HasSuffix(f.Rule, QuotedRuleSuffix) {
				continue
			}
			return []Finding{{
				Kind:     KindInjection,
				Severity: SeverityHigh,
				Snippet:  snippet(layer.name + "->" + f.Rule + ": " + f.Snippet),
				Rule:     obfuscatedRule,
			}}
		}
	}
	return nil
}

func decodeHexEscapes(run string) string {
	var b strings.Builder
	for i := 0; i+4 <= len(run); i += 4 {
		v, err := strconv.ParseUint(run[i+2:i+4], 16, 8)
		if err != nil {
			return run
		}
		b.WriteByte(byte(v))
	}
	if !isMostlyPrintable([]byte(b.String())) {
		return run
	}
	return b.String()
}

func decodeSpacedHex(run string) string {
	fields := strings.FieldsFunc(run, func(r rune) bool { return r == ' ' || r == ':' || r == ',' })
	var b strings.Builder
	for _, f := range fields {
		v, err := strconv.ParseUint(f, 16, 8)
		if err != nil {
			return run
		}
		b.WriteByte(byte(v))
	}
	if !isMostlyPrintable([]byte(b.String())) {
		return run
	}
	return b.String()
}

func rot13(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return 'a' + (r-'a'+13)%26
		case r >= 'A' && r <= 'Z':
			return 'A' + (r-'A'+13)%26
		}
		return r
	}, s)
}

// decodeBase64Nested decodes a blob and keeps decoding while the result is
// itself a bare base64 blob, up to base64MaxNesting layers. It returns the
// innermost text and how many layers were peeled.
func decodeBase64Nested(blob string) (string, int) {
	decoded, ok := tryDecodeBase64(blob)
	if !ok {
		return "", 0
	}
	depth := 1
	for depth < base64MaxNesting {
		inner := strings.TrimSpace(decoded)
		if !base64BlobRe.MatchString(inner) || base64BlobRe.FindString(inner) != inner {
			break
		}
		next, ok := tryDecodeBase64(inner)
		if !ok {
			break
		}
		decoded = next
		depth++
	}
	return decoded, depth
}

// quotedMention reports whether the match at [start,end) sits inside a short
// inline quote that the surrounding text frames as something being talked
// about ("issues containing 'ignore all previous instructions'"). Only
// single-line quotes count; a fenced block is still a payload.
func quotedMention(text string, start, end int) bool {
	open := -1
	var quote byte
	for i := start - 1; i >= 0 && start-i <= quotedMentionMaxSpan; i-- {
		c := text[i]
		if c == '\n' {
			return false
		}
		if c == '\'' || c == '"' || c == '`' {
			open, quote = i, c
			break
		}
	}
	if open < 0 {
		return false
	}
	closeAt := strings.IndexByte(text[end:], quote)
	if closeAt < 0 || closeAt+end-open > quotedMentionMaxSpan {
		return false
	}
	if strings.Contains(text[end:end+closeAt], "\n") {
		return false
	}
	from := open - quotedMentionContextLen
	if from < 0 {
		from = 0
	}
	return quotedMentionCue.MatchString(text[from:open])
}
