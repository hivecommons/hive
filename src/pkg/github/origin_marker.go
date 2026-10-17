package github

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
)

// Origin marker: which hive filed or touched an item.
//
// The `hive/<hive-id>` label used to carry this (hivecommons/hive#12539). It
// was a label nobody read as a signal but every agent write re-applied, so
// it crowded the label bar on every issue and PR a hive touched. The
// information now travels in the body: bin/gh-wrapper.sh appends
// OriginMarker next to its identity footer on every body it injects, and
// readers call OriginHive, which prefers the marker and falls back to the
// legacy label while open items still carry one.
const (
	// OriginMarkerName is the HTML-comment marker name, in the same
	// `hive:<kind>` family as the claim markers (pkg/claims).
	OriginMarkerName = "hive:origin"
	// LegacyHiveIDLabelPrefix is the namespace the retired provenance label
	// lived in. The prefix is shared with the hive/* signal labels, so only
	// legacyHiveIDLabel decides whether a given label is the retired one.
	LegacyHiveIDLabelPrefix = "hive/"
)

// OriginSource names where OriginHive found the hive id.
type OriginSource string

const (
	OriginSourceMarker OriginSource = "marker"
	OriginSourceLabel  OriginSource = "legacy-label"
	OriginSourceNone   OriginSource = ""
)

var originMarkerPattern = regexp.MustCompile(`<!--\s*` + regexp.QuoteMeta(OriginMarkerName) + `\s+([^>]*?)\s*-->`)

// OriginMarker renders `<!-- hive:origin hive=<id> at=<RFC3339> -->`. The
// result is "" for a blank hive id so callers can append it unconditionally.
func OriginMarker(hiveID string, at time.Time) string {
	hiveID = strings.TrimSpace(hiveID)
	if hiveID == "" {
		return ""
	}
	hiveID = strings.Join(strings.Fields(hiveID), "-")
	if at.IsZero() {
		return fmt.Sprintf("<!-- %s hive=%s -->", OriginMarkerName, hiveID)
	}
	return fmt.Sprintf("<!-- %s hive=%s at=%s -->", OriginMarkerName, hiveID, at.UTC().Format(time.RFC3339))
}

// ParseOriginMarker returns the hive id (and, when present, the at= instant)
// from the first origin marker in body. ok is false when there is none.
func ParseOriginMarker(body string) (hive string, at time.Time, ok bool) {
	m := originMarkerPattern.FindStringSubmatch(body)
	if m == nil {
		return "", time.Time{}, false
	}
	for _, field := range strings.Fields(m[1]) {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		switch key {
		case "hive":
			hive = value
		case "at":
			if t, err := time.Parse(time.RFC3339, value); err == nil {
				at = t
			}
		}
	}
	if hive == "" {
		return "", time.Time{}, false
	}
	return hive, at, true
}

// OriginHive reports which hive an item came from: the origin marker in its
// body when present, otherwise (migration fallback) the retired
// `hive/<hive-id>` label when the item still carries one. The source tells
// the caller which of the two answered, so a sweep can tell "still needs
// the label stripped" from "already migrated".
func OriginHive(body string, labels []string) (hive string, source OriginSource) {
	if h, _, ok := ParseOriginMarker(body); ok {
		return h, OriginSourceMarker
	}
	for _, l := range labels {
		if id, ok := legacyHiveIDLabel(l); ok {
			return id, OriginSourceLabel
		}
	}
	return "", OriginSourceNone
}

// legacyHiveIDLabel returns the hive id a retired `hive/<hive-id>` label
// names. Every hive/* label that is still read as a signal (selfreport,
// advisory, claimed-by-*, the PR-claim and verification state labels) is
// excluded by name so the fallback never mistakes one for a hive id.
func legacyHiveIDLabel(label string) (string, bool) {
	name := strings.TrimSpace(label)
	rest, ok := strings.CutPrefix(strings.ToLower(name), LegacyHiveIDLabelPrefix)
	if !ok || rest == "" {
		return "", false
	}
	if isHiveSignalLabel(strings.ToLower(name)) {
		return "", false
	}
	return name[len(LegacyHiveIDLabelPrefix):], true
}

// hiveSignalLabels are the hive/* labels that stay: each is read back as a
// signal somewhere in this code base or by the hub.
var hiveSignalLabels = []string{
	"hive/selfreport",
	"hive/advisory",
	"hive/sentinel",
	"hive/fleet",
	"hive/handoff",
	"hive/superseded",
	"hive/keep-open",
	"hive/in-progress",
	"hive/already-done",
	"hive/covered-by-pr",
	"hive/likely-done",
	"hive/partially-addressed",
	"hive/verified-open",
}

// hiveSignalLabelPrefixes are hive/* families with a variable suffix that
// are still read as signals.
var hiveSignalLabelPrefixes = []string{
	claims.ClaimedByPrefix, // hive/claimed-by-<who>
}

func isHiveSignalLabel(lower string) bool {
	for _, s := range hiveSignalLabels {
		if lower == s {
			return true
		}
	}
	for _, p := range hiveSignalLabelPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}
