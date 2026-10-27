package logread

import (
	"regexp"
	"strings"

	"github.com/hivecommons/hive/crux/logscrub"
)

// PrivateKeyKind is the redaction kind for private-key material, matching
// the pkg/logscrub category so one key counts under one name.
const PrivateKeyKind = "private-key"

const privateKeyMarker = "<redacted:" + PrivateKeyKind + ">"

var (
	keyBeginRE = regexp.MustCompile(`-----BEGIN[A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	keyEndRE   = regexp.MustCompile(`-----END[A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
)

type span struct{ start, end int }

// keySpans marks private-key material in a region of lines, in file order. A
// key may span many records, and a page, scan or rotation boundary may cut it,
// so the region is scrubbed with overlap from both sides: when the region's
// first key marker is an END, every line before it is inside a key whose BEGIN
// lies outside the region; a BEGIN with no END is redacted to the end of the
// region.
func keySpans(lines []string) [][]span {
	out := make([][]span, len(lines))
	inside := startsInsideKey(lines)
	for i, text := range lines {
		if !inside && !strings.Contains(text, "PRIVATE KEY") {
			continue
		}
		out[i], inside = lineKeySpans(text, inside)
	}
	return out
}

func startsInsideKey(lines []string) bool {
	for _, text := range lines {
		if !strings.Contains(text, "PRIVATE KEY") {
			continue
		}
		begin := keyBeginRE.FindStringIndex(text)
		end := keyEndRE.FindStringIndex(text)
		if end != nil && (begin == nil || end[0] < begin[0]) {
			return true
		}
		if begin != nil {
			return false
		}
	}
	return false
}

func lineKeySpans(text string, inside bool) ([]span, bool) {
	var spans []span
	pos, start := 0, 0
	for {
		if inside {
			loc := keyEndRE.FindStringIndex(text[pos:])
			if loc == nil {
				return append(spans, span{start, len(text)}), true
			}
			pos += loc[1]
			spans = append(spans, span{start, pos})
			inside = false
			continue
		}
		loc := keyBeginRE.FindStringIndex(text[pos:])
		if loc == nil {
			return spans, false
		}
		start = pos + loc[0]
		pos += loc[1]
		inside = true
	}
}

func applySpans(text string, spans []span) string {
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		b.WriteString(text[last:sp.start])
		b.WriteString(privateKeyMarker)
		last = sp.end
	}
	b.WriteString(text[last:])
	return b.String()
}

// scrubLine scrubs one raw line on read: private-key spans, then this hive's
// own secret values, then every pkg/logscrub credential shape (Basic-auth
// headers included). It reports what it replaced by kind, and whether the raw
// line already carried redaction-marker text.
func scrubLine(raw string, spans []span, secrets *SecretScrubber) (string, map[string]int, bool) {
	pre := logscrub.ContainsRedactionMarker(raw)
	counts := map[string]int{}
	text := applySpans(raw, spans)
	if len(spans) > 0 {
		counts[PrivateKeyKind] += len(spans)
	}
	text, n := secrets.Replace(text)
	if n > 0 {
		counts[SecretValueKind] += n
	}
	text, shaped := logscrub.ScrubStringCounted(text, logscrub.WithMarkers())
	for k, v := range shaped {
		counts[k] += v
	}
	return text, counts, pre
}
