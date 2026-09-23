package config

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	ContributeHelpLinksMaxCount     = 5
	ContributeHelpLinkLabelMaxRunes = 80
)

var DefaultContributeHelpLinks = []ContributeHelpLink{
	{Label: "Contributor docs", URL: "https://github.com/hivecommons/hive/blob/v5/src/docs/contributor-relay.md"},
	{Label: "Open a Hive issue", URL: "https://github.com/hivecommons/hive/issues"},
}

func NormalizeContributeHelpLinks(in []ContributeHelpLink) ([]ContributeHelpLink, error) {
	if len(in) > ContributeHelpLinksMaxCount {
		return nil, fmt.Errorf("contribute.help_links must contain at most %d links", ContributeHelpLinksMaxCount)
	}
	out := make([]ContributeHelpLink, 0, len(in))
	for i, link := range in {
		label := sanitizeContributeHelpLinkLabel(link.Label)
		if label == "" {
			return nil, fmt.Errorf("contribute.help_links[%d].label is required", i)
		}
		rawURL := strings.TrimSpace(link.URL)
		u, err := url.Parse(rawURL)
		if err != nil || u == nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("contribute.help_links[%d].url must be an absolute http or https URL", i)
		}
		u.Fragment = strings.TrimSpace(u.Fragment)
		out = append(out, ContributeHelpLink{Label: label, URL: u.String()})
	}
	return out, nil
}

func ContributeHelpLinksOrDefault(in []ContributeHelpLink) []ContributeHelpLink {
	links, err := NormalizeContributeHelpLinks(in)
	if err != nil || len(links) == 0 {
		links = DefaultContributeHelpLinks
	}
	out := make([]ContributeHelpLink, len(links))
	copy(out, links)
	return out
}

func sanitizeContributeHelpLinkLabel(s string) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range strings.TrimSpace(s) {
		if r < 0x20 || r == 0x7f || !utf8.ValidRune(r) {
			r = ' '
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if lastSpace {
				continue
			}
			lastSpace = true
			b.WriteRune(' ')
			continue
		}
		lastSpace = false
		b.WriteRune(r)
	}
	runes := []rune(strings.TrimSpace(b.String()))
	if len(runes) > ContributeHelpLinkLabelMaxRunes {
		runes = runes[:ContributeHelpLinkLabelMaxRunes]
	}
	return string(runes)
}
