package fleetreport

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/hivecommons/hive/pkg/logscrub"
)

// Hive identity rows (hivecommons/hive#13211). A runtime-error report names
// the reporting hive by its hub id; these two evidence rows add the hive's
// human display name and a clickable link to its dashboard, so a maintainer
// reading the issue can tell WHICH hive without going through the hub's
// hives listing. Both are optional: a spoke that predates them, or one that
// is not hub-managed, leaves them "—" and the affected block unchanged.

const (
	// HiveNameField / SpokeField are the evidence-table row labels.
	HiveNameField = "hive name"
	SpokeField    = "spoke"
	// affectedOpenLabel is the link text of the per-hive open link in the
	// affected block.
	affectedOpenLabel = "open"
	// maxHiveNameLen mirrors the hub's maxHiveDisplayNameLen.
	maxHiveNameLen = 100
	// maxSpokeURLLen bounds a dashboard URL rendered into an issue body.
	maxSpokeURLLen = 256
)

// NormalizeHiveName reduces a display name to something safe inside a
// markdown table cell and an affected-block bullet: scrubbed, single-line,
// without the characters the renderers use as structure ("|", "—", "[",
// "]"), collapsed whitespace, at most maxHiveNameLen runes. "" when nothing
// printable remains.
func NormalizeHiveName(name string) string {
	name = logscrub.ScrubString(name)
	var b strings.Builder
	space := false
	for _, r := range name {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case r == '|' || r == '—' || r == '[' || r == ']' || r == '`' || r == '<' || r == '>' || unicode.IsControl(r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if n := []rune(out); len(n) > maxHiveNameLen {
		out = strings.TrimSpace(string(n[:maxHiveNameLen]))
	}
	return out
}

// NormalizeSpokeURL accepts an absolute http(s) URL with a host and returns
// it without userinfo, query, fragment or trailing slash; "" for anything
// else so a bad value never becomes a link in a public issue.
func NormalizeSpokeURL(raw string) string {
	raw = strings.TrimSpace(logscrub.ScrubString(raw))
	if raw == "" || len(raw) > maxSpokeURLLen || strings.ContainsAny(raw, " \t\r\n<>()[]\"'`|") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return ""
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = nil, "", "", "", false
	u.Host = strings.ToLower(u.Host)
	return strings.TrimRight(u.String(), "/")
}

// SpokeLink renders the spoke cell: "[<name or hive>](<url>)", "—" when the
// URL is unknown. Inputs are expected to be normalised already.
func SpokeLink(name, hive, spokeURL string) string {
	if spokeURL == "" {
		return "—"
	}
	label := name
	if label == "" {
		label = hive
	}
	if label == "" {
		label = affectedOpenLabel
	}
	return fmt.Sprintf("[%s](%s)", label, spokeURL)
}

// UpsertHiveIdentity sets the "hive name" and "spoke" rows of the evidence
// table in body to name / url, inserting them after the "| hive |" row when
// the body (an older spoke's report) has none. Empty inputs never blank a
// value the body already carries. changed is false when the body has no
// evidence table or nothing would differ, so the hub never rewrites a body
// for nothing.
func UpsertHiveIdentity(body, name, spokeURL string) (string, bool) {
	name, spokeURL = NormalizeHiveName(name), NormalizeSpokeURL(spokeURL)
	if name == "" && spokeURL == "" {
		return body, false
	}
	lines := strings.Split(body, "\n")
	hiveAt, nameAt, spokeAt := -1, -1, -1
	hive := ""
	for i, line := range lines {
		label, value, ok := tableRow(line)
		if !ok {
			continue
		}
		switch label {
		case "hive":
			if hiveAt < 0 {
				hiveAt, hive = i, value
			}
		case HiveNameField:
			if nameAt < 0 {
				nameAt = i
			}
		case SpokeField:
			if spokeAt < 0 {
				spokeAt = i
			}
		}
	}
	if hiveAt < 0 {
		return body, false
	}
	if nameAt >= 0 {
		if _, have, _ := tableRow(lines[nameAt]); name == "" && have != "—" {
			name = NormalizeHiveName(have)
		}
	}
	if spokeAt >= 0 {
		if _, have, _ := tableRow(lines[spokeAt]); spokeURL == "" {
			spokeURL = linkTarget(have)
		}
	}
	nameRow := fmt.Sprintf("| %s | %s |", HiveNameField, emptyDash(name))
	spokeRow := fmt.Sprintf("| %s | %s |", SpokeField, SpokeLink(name, hive, spokeURL))
	changed := false
	set := func(at int, row string) {
		if lines[at] != row {
			lines[at] = row
			changed = true
		}
	}
	switch {
	case nameAt >= 0 && spokeAt >= 0:
		set(nameAt, nameRow)
		set(spokeAt, spokeRow)
	case nameAt < 0 && spokeAt < 0:
		lines = append(lines[:hiveAt+1], append([]string{nameRow, spokeRow}, lines[hiveAt+1:]...)...)
		changed = true
	case nameAt >= 0:
		set(nameAt, nameRow)
		lines = append(lines[:nameAt+1], append([]string{spokeRow}, lines[nameAt+1:]...)...)
		changed = true
	default:
		set(spokeAt, spokeRow)
		lines = append(lines[:spokeAt], append([]string{nameRow}, lines[spokeAt:]...)...)
		changed = true
	}
	if !changed {
		return body, false
	}
	return strings.Join(lines, "\n"), true
}

// tableRow splits "| label | value |" into its two cells.
func tableRow(line string) (label, value string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return "", "", false
	}
	cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|"), "|")
	if len(cells) != 2 {
		return "", "", false
	}
	return strings.TrimSpace(cells[0]), strings.TrimSpace(cells[1]), true
}

// linkTarget returns the URL of a "[label](url)" cell, "" otherwise.
func linkTarget(cell string) string {
	cell = strings.TrimSpace(cell)
	i := strings.LastIndex(cell, "](")
	if !strings.HasPrefix(cell, "[") || i < 0 || !strings.HasSuffix(cell, ")") {
		return ""
	}
	return NormalizeSpokeURL(cell[i+2 : len(cell)-1])
}
