package mention

import (
	"regexp"
	"strings"
)

type Parsed struct {
	Mentioned bool
	Agent     string
	Text      string
}

type ActionMarker struct {
	Source     string
	RunID      string
	RunAttempt string
	Workflow   string
	Actor      string
	Ref        string
	Transport  string
}

var askSelectorRe = regexp.MustCompile(`(?is)^ask\s+([A-Za-z0-9_.-]+)\b\s*(.*)$`)
var actionMarkerRe = regexp.MustCompile(`(?s)<!--\s*hive:([^>]*)-->`)

func Parse(body, appBotLogin string) Parsed {
	body, _ = ExtractActionMarker(body)
	login := strings.TrimSpace(appBotLogin)
	if login == "" {
		return Parsed{}
	}
	lowerBody := strings.ToLower(body)
	idx, end := findMention(lowerBody, login)
	if idx < 0 || end < 0 {
		return Parsed{}
	}
	rest := strings.TrimSpace(body[end:])
	p := Parsed{Mentioned: true, Text: rest}
	if m := askSelectorRe.FindStringSubmatch(rest); len(m) == 3 {
		p.Agent = strings.TrimSpace(m[1])
		p.Text = strings.TrimSpace(m[2])
	}
	return p
}

func ExtractActionMarker(body string) (string, ActionMarker) {
	matches := actionMarkerRe.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return body, ActionMarker{}
	}
	cleaned := body
	var marker ActionMarker
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		if m[2] >= 0 && m[3] >= 0 {
			candidate := parseActionMarkerFields(body[m[2]:m[3]])
			if strings.EqualFold(candidate.Source, SourceAction) {
				marker = candidate
			}
		}
		cleaned = strings.TrimSpace(cleaned[:m[0]] + cleaned[m[1]:])
	}
	return cleaned, marker
}

func parseActionMarkerFields(s string) ActionMarker {
	return ActionMarker{
		Source:     markerField(s, "source"),
		RunID:      markerField(s, "run_id"),
		RunAttempt: markerField(s, "run_attempt"),
		Workflow:   markerField(s, "workflow"),
		Actor:      markerField(s, "actor"),
		Ref:        markerField(s, "ref"),
		Transport:  markerField(s, "transport"),
	}
}

func markerField(s, key string) string {
	s = strings.TrimSpace(s)
	needle := key + "="
	idx := strings.Index(s, needle)
	if idx < 0 {
		return ""
	}
	start := idx + len(needle)
	end := len(s)
	for _, next := range []string{"source=", "run_id=", "run_attempt=", "workflow=", "actor=", "ref=", "transport="} {
		if next == needle {
			continue
		}
		if pos := strings.Index(s[start:], " "+next); pos >= 0 && start+pos < end {
			end = start + pos
		}
	}
	return strings.TrimSpace(s[start:end])
}

func findMention(lowerBody, appBotLogin string) (int, int) {
	login := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(appBotLogin)), "@")
	base := strings.TrimSuffix(login, "[bot]")
	candidates := []string{"@" + login}
	if base != "" && base != login {
		candidates = append(candidates, "@"+base)
	}
	for _, needle := range candidates {
		searchAt := 0
		for {
			idx := strings.Index(lowerBody[searchAt:], needle)
			if idx < 0 {
				break
			}
			idx += searchAt
			end := idx + len(needle)
			if mentionBoundary(lowerBody, end) {
				return idx, end
			}
			searchAt = end
		}
	}
	return -1, -1
}

func mentionBoundary(s string, end int) bool {
	if end >= len(s) {
		return true
	}
	next := s[end]
	return !((next >= 'a' && next <= 'z') || (next >= '0' && next <= '9') || next == '-' || next == '_' || next == '[' || next == ']')
}
