package ioscan

import (
	"net/url"
	"regexp"
	"strings"
)

// Structural rules for markup that hides text from human reviewers (GitHub
// does not render HTML comments or collapsed <details> content prominently,
// but agents read all of it) and for image/link URLs that smuggle data out in
// their query string (#19444). Existing rule ids are untouched; these add new
// ids and raise the severity of phrase rules that fire inside hidden regions.

const (
	ruleHiddenMarkup   = "injection.hidden_markup"
	ruleImageExfil     = "dangerous.exfil_image_url"
	ruleLinkFillIn     = "dangerous.link_fill_in"
	exfilValueMinLen   = 24
	exfilKeyedMinLen   = 8
	hiddenRegionMaxLen = 1 << 16
)

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--(.*?)-->`)
	detailsRe     = regexp.MustCompile(`(?is)<details\b[^>]*>(.*?)</details>`)
	hiddenStyleRe = regexp.MustCompile(`(?is)<(\w+)\b[^>]*(?:\bhidden\b|style\s*=\s*"[^"]*(?:display\s*:\s*none|visibility\s*:\s*hidden|font-size\s*:\s*0)[^"]*")[^>]*>(.*?)</\1\s*>`)
	mdImageRe     = regexp.MustCompile(`!\[[^\]]*\]\(\s*<?([^)\s>]+)`)
	imgTagRe      = regexp.MustCompile(`(?i)<img\b[^>]*\bsrc\s*=\s*["']?([^"'\s>]+)`)
	mdLinkRe      = regexp.MustCompile(`(?:^|[^!])\[[^\]]*\]\(\s*<?([^)\s>]+)`)
	fillInRe      = regexp.MustCompile(`(?i)(\{\{?[^}]*\}\}?|<[a-z_ -]{2,}>|\[[a-z_ -]{2,}\]|%7[bB][^%]*%7[dD]|\b(your|insert|paste)[_-]?\w+)`)
	exfilKeys     = map[string]bool{"data": true, "d": true, "q": true, "payload": true, "secret": true, "token": true, "key": true, "content": true, "text": true, "msg": true, "message": true}
)

// scanHiddenMarkup returns findings for hidden-markup content and
// data-carrying URLs in text.
func scanHiddenMarkup(text string) []Finding {
	var out []Finding
	out = append(out, scanHiddenRegions(text)...)
	out = append(out, scanExfilURLs(text)...)
	return out
}

func hiddenRegions(text string) []string {
	var regions []string
	for _, re := range []*regexp.Regexp{htmlCommentRe, detailsRe} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			regions = append(regions, m[1])
		}
	}
	for _, m := range hiddenStyleRe.FindAllStringSubmatch(text, -1) {
		regions = append(regions, m[2])
	}
	return regions
}

// scanHiddenRegions re-runs the phrase rules on hidden regions. Any hit is
// reported once as a High injection finding, which raises Medium phrase hits
// (e.g. address_agent) to a blocking severity because hiding them is itself
// evidence of intent.
func scanHiddenRegions(text string) []Finding {
	for _, region := range hiddenRegions(text) {
		if len(region) > hiddenRegionMaxLen {
			region = region[:hiddenRegionMaxLen]
		}
		var hits []Finding
		hits = append(hits, matchRules(region, injectionRules)...)
		hits = append(hits, matchRules(region, dangerousRules)...)
		for _, h := range hits {
			if h.Severity == SeverityLow {
				continue
			}
			return []Finding{{
				Kind:     KindInjection,
				Severity: SeverityHigh,
				Rule:     ruleHiddenMarkup,
				Snippet:  h.Snippet,
			}}
		}
	}
	return nil
}

func scanExfilURLs(text string) []Finding {
	var out []Finding
	var imgs []string
	for _, re := range []*regexp.Regexp{mdImageRe, imgTagRe} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			imgs = append(imgs, m[1])
		}
	}
	for _, u := range imgs {
		if carriesData(u) {
			out = append(out, Finding{Kind: KindDangerous, Severity: SeverityHigh, Rule: ruleImageExfil, Snippet: snippet(u)})
			break
		}
	}
	for _, m := range mdLinkRe.FindAllStringSubmatch(text, -1) {
		if q := queryOf(m[1]); q != "" && fillInRe.MatchString(q) {
			out = append(out, Finding{Kind: KindDangerous, Severity: SeverityMedium, Rule: ruleLinkFillIn, Snippet: snippet(m[1])})
			break
		}
	}
	return out
}

func queryOf(raw string) string {
	i := strings.IndexByte(raw, '?')
	if i < 0 {
		return ""
	}
	return raw[i+1:]
}

// carriesData reports whether an image URL's query string looks like it
// transports data: a long value, a well-known data key, or a fill-in marker.
func carriesData(raw string) bool {
	q := queryOf(raw)
	if q == "" {
		return false
	}
	if fillInRe.MatchString(q) {
		return true
	}
	vals, err := url.ParseQuery(q)
	if err != nil {
		return len(q) >= exfilValueMinLen
	}
	for k, vs := range vals {
		for _, v := range vs {
			if len(v) >= exfilValueMinLen || (exfilKeys[strings.ToLower(k)] && len(v) >= exfilKeyedMinLen) {
				return true
			}
		}
	}
	return false
}
