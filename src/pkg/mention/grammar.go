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

var askSelectorRe = regexp.MustCompile(`(?is)^ask\s+([A-Za-z0-9_.-]+)\b\s*(.*)$`)

func Parse(body, appBotLogin string) Parsed {
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
