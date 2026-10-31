package rfcnotify

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/discord"
)

// Embed is an alias for discord.Embed.
type Embed = discord.Embed

// Colors for RFC status cards in Discord.
const (
	ColorHeld          = 0x95a5a6 // grey
	ColorNeedsDecision = 0xf1c40f // amber
	ColorReady         = 0x2ecc71 // green
	ColorCompleted     = 0x3498db // blue
	ColorNotPlanned    = 0x2c2f33 // dark
)

// Discord embed text limits.
const (
	MaxTitleRunes       = 256
	MaxDescriptionRunes = 4096
	MaxFieldValueRunes  = 1024
	MaxAuthorNameRunes  = 256
	MaxSummaryRunes     = 300
)

// RFCMeta holds structured fields parsed from an RFC body.
type RFCMeta struct {
	Target          string
	Status          string
	Summary         string
	OpenDecisions   int
	RelatedRFCLinks []string
}

// Empty reports whether neither an RFC field table nor a summary was found.
func (m RFCMeta) Empty() bool {
	return m.Target == "" && m.Status == "" && m.Summary == ""
}

var (
	headingSummaryRE   = regexp.MustCompile(`(?i)^#{1,6}\s+(?:executive\s+)?summary\b`)
	headingDecisionsRE = regexp.MustCompile(`(?i)^#{1,6}\s+decisions\s*(?:for\s+maintainers)?\b`)
	genericHeadingRE   = regexp.MustCompile(`^#{1,6}\s+`)
	numberedItemRE     = regexp.MustCompile(`^(?:\d+[\.\)]|\(\d+\))\s+\S+`)
	rfcLinkRE          = regexp.MustCompile(`(?i)(?:[\w.-]+/[\w.-]+)?#\d+|https?://github\.com/[\w.-]+/[\w.-]+/(?:issues|pull)/\d+`)
)

// ParseRFC extracts Target and Status from the RFC field table, the first
// paragraph under ## Summary (capped to 300 runes and scrubbed), the count
// of open items under ## Decisions for maintainers, and related RFC links.
// Parsing never fails; missing fields stay empty.
func ParseRFC(body string) RFCMeta {
	var meta RFCMeta
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")

	inSummary := false
	inDecisions := false
	var summaryLines []string

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])

		if genericHeadingRE.MatchString(line) {
			if headingSummaryRE.MatchString(line) {
				inSummary = true
				inDecisions = false
				continue
			} else if headingDecisionsRE.MatchString(line) {
				inDecisions = true
				inSummary = false
				continue
			} else {
				inSummary = false
				inDecisions = false
			}
		}

		if inSummary {
			if len(summaryLines) == 0 && line == "" {
				continue
			}
			if line == "" {
				inSummary = false
				continue
			}
			if strings.HasPrefix(line, "|") || strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
				if len(summaryLines) > 0 {
					inSummary = false
					continue
				}
			}
			summaryLines = append(summaryLines, line)
			continue
		}

		if inDecisions {
			if line == "" {
				continue
			}
			if strings.Contains(line, "~~") {
				continue
			}
			if isCheckedCheckbox(line) {
				continue
			}
			if isUncheckedCheckbox(line) {
				meta.OpenDecisions++
				continue
			}
			if isNumberedItem(line) {
				meta.OpenDecisions++
				continue
			}
		}

		if strings.Contains(line, "|") {
			cols := splitTableCols(line)
			if len(cols) >= 2 {
				key := strings.ToLower(cols[0])
				val := cols[1]
				if !isTableHeaderOrSeparator(key, val) {
					switch {
					case key == "target":
						if meta.Target == "" {
							meta.Target = val
						}
					case key == "status":
						if meta.Status == "" {
							meta.Status = val
						}
					case isRelatedKey(key):
						links := extractRFCLinks(val)
						if len(links) > 0 {
							for _, l := range links {
								if !containsString(meta.RelatedRFCLinks, l) {
									meta.RelatedRFCLinks = append(meta.RelatedRFCLinks, l)
								}
							}
						} else if val != "" && !containsString(meta.RelatedRFCLinks, val) {
							meta.RelatedRFCLinks = append(meta.RelatedRFCLinks, val)
						}
					}
				}
			}
		}
	}

	if len(summaryLines) > 0 {
		rawSummary := strings.Join(strings.Fields(strings.Join(summaryLines, " ")), " ")
		meta.Summary = truncateRunes(logscrub.ScrubString(rawSummary), MaxSummaryRunes)
	}

	return meta
}

func isTableHeaderOrSeparator(key, val string) bool {
	if key == "field" || key == "key" || strings.HasPrefix(key, "---") || strings.HasPrefix(val, "---") {
		return true
	}
	return false
}

func isRelatedKey(k string) bool {
	return k == "related" || k == "related rfcs" || k == "related rfc" ||
		k == "builds on" || k == "supersedes" || strings.HasPrefix(k, "related")
}

func splitTableCols(line string) []string {
	parts := strings.Split(line, "|")
	var cols []string
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			cols = append(cols, t)
		}
	}
	return cols
}

func extractRFCLinks(val string) []string {
	matches := rfcLinkRE.FindAllString(val, -1)
	var out []string
	seen := make(map[string]bool)
	for _, m := range matches {
		m = strings.TrimSpace(m)
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func isUncheckedCheckbox(line string) bool {
	return strings.HasPrefix(line, "- [ ]") || strings.HasPrefix(line, "* [ ]") || strings.HasPrefix(line, "+ [ ]")
}

func isCheckedCheckbox(line string) bool {
	return strings.HasPrefix(line, "- [x]") || strings.HasPrefix(line, "- [X]") ||
		strings.HasPrefix(line, "* [x]") || strings.HasPrefix(line, "* [X]") ||
		strings.HasPrefix(line, "+ [x]") || strings.HasPrefix(line, "+ [X]")
}

func isNumberedItem(line string) bool {
	return numberedItemRE.MatchString(line)
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

func isNeedsDecision(labels []string) bool {
	for _, l := range labels {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "stage/045-needs-decision" || l == "needs-decision" || l == "stage/085-needs-decision" {
			return true
		}
	}
	return false
}

// CardColor returns the Discord embed color for an event: held grey,
// needs-decision amber, ready green, closed completed blue, closed
// not-planned dark.
func CardColor(e Event) int {
	if !e.Issue.Open() || e.Kind == KindClosed {
		if ClosedReason(e.Issue.StateReason) == closedNotPlanned {
			return ColorNotPlanned
		}
		return ColorCompleted
	}
	if e.Kind == KindDocAccepted {
		return ColorCompleted
	}
	if e.Kind == KindReady {
		return ColorReady
	}
	if isNeedsDecision(e.Issue.Labels) {
		return ColorNeedsDecision
	}
	if Held(e.Issue.Labels) {
		return ColorHeld
	}
	if Actionable(e.Issue.Labels) {
		return ColorReady
	}
	return ColorHeld
}

// Card builds a Discord summary card (embed) for an RFC lifecycle event.
func Card(e Event, meta RFCMeta, includeRepo bool) Embed {
	ref := fmt.Sprintf("#%d", e.Issue.Number)
	if includeRepo && e.Issue.Repo != "" {
		ref = e.Issue.Repo + ref
	}
	title := strings.TrimSpace(e.Issue.Title)
	if d := strings.TrimSpace(e.Issue.DocTitle); d != "" {
		title = d
	}
	cardTitle := fmt.Sprintf("%s %s", ref, title)
	cardTitle = truncateRunes(logscrub.ScrubString(cardTitle), MaxTitleRunes)

	var author *discord.EmbedAuthor
	if a := strings.TrimSpace(e.Issue.Author); a != "" {
		author = &discord.EmbedAuthor{
			Name:    truncateRunes(a, MaxAuthorNameRunes),
			IconURL: fmt.Sprintf("https://github.com/%s.png", a),
		}
	}

	var fields []discord.EmbedField
	if meta.Target != "" {
		fields = append(fields, discord.EmbedField{
			Name:   "Target",
			Value:  truncateRunes(meta.Target, MaxFieldValueRunes),
			Inline: true,
		})
	}
	if meta.Status != "" {
		fields = append(fields, discord.EmbedField{
			Name:   "Status",
			Value:  truncateRunes(meta.Status, MaxFieldValueRunes),
			Inline: true,
		})
	}
	if meta.OpenDecisions > 0 {
		fields = append(fields, discord.EmbedField{
			Name:   "Decisions",
			Value:  fmt.Sprintf("%d open", meta.OpenDecisions),
			Inline: true,
		})
	}
	if len(meta.RelatedRFCLinks) > 0 {
		fields = append(fields, discord.EmbedField{
			Name:   "Related",
			Value:  truncateRunes(strings.Join(meta.RelatedRFCLinks, ", "), MaxFieldValueRunes),
			Inline: false,
		})
	}

	return Embed{
		Title:       cardTitle,
		URL:         strings.TrimSpace(e.Issue.URL),
		Author:      author,
		Description: truncateRunes(meta.Summary, MaxDescriptionRunes),
		Color:       CardColor(e),
		Fields:      fields,
	}
}
