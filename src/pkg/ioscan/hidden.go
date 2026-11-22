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
	hiddenStyleRe = regexp.MustCompile(`(?is)<(\w+)\b[^>]*(?:\bhidden\b|style\s*=\s*"[^"]*(?:display\s*:\s*none|visibility\s*:\s*hidden|font-size\s*:\s*0)[^"]*")[^>]*>`)
	mdImageRe     = regexp.MustCompile(`!\[[^\]]*\]\(\s*<?([^)\s>]+)`)
	imgTagRe      = regexp.MustCompile(`(?i)<img\b[^>]*\bsrc\s*=\s*["']?([^"'\s>]+)`)
	mdLinkRe      = regexp.MustCompile(`(?:^|[^!])\[[^\]]*\]\(\s*<?([^)\s>]+)`)
	// fillInRe is matched against decoded query values only, never keys, so
	// bracketed parameter names (filter[key]=1) do not count. The your/insert/
	// paste form needs a placeholder separator (YOUR_TOKEN, insert-here) so
	// ordinary words (yourself, insertion, pasted) do not match (#19452).
	fillInRe   = regexp.MustCompile(`(?i)(\{\{?\s*[a-z_][a-z0-9_ .-]*\}\}?|<[a-z_ -]{2,}>|\[[a-z_ -]{2,}\]|%7[bB][^%]*%7[dD]|\b(?:your|insert|paste)[_-][a-z]\w*)`)
	tokenValRe = regexp.MustCompile(`^[A-Za-z0-9._~+/=:@-]*$`)
	jwtValRe   = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*$`)
	exfilKeys  = map[string]bool{"data": true, "d": true, "q": true, "payload": true, "secret": true, "token": true, "key": true, "content": true, "text": true, "msg": true, "message": true}
)

// scanHiddenMarkup returns findings for hidden-markup content and
// data-carrying URLs in text.
func scanHiddenMarkup(text string) []Finding {
	var out []Finding
	out = append(out, scanHiddenRegions(text)...)
	out = append(out, scanExfilURLs(text)...)
	return out
}

// hiddenRegion is a span of text a human reviewer does not see by default.
// promote is false for <details>: collapsed sections are an ordinary GitHub
// idiom for logs and long output and are one click away, so only hits that
// are already High count there.
type hiddenRegion struct {
	text    string
	promote bool
}

func hiddenRegions(text string) []hiddenRegion {
	var regions []hiddenRegion
	for _, m := range htmlCommentRe.FindAllStringSubmatch(text, -1) {
		regions = append(regions, hiddenRegion{m[1], true})
	}
	for _, m := range detailsRe.FindAllStringSubmatch(text, -1) {
		regions = append(regions, hiddenRegion{m[1], false})
	}
	// RE2 has no backreferences, so the matching close tag is found by hand.
	for _, loc := range hiddenStyleRe.FindAllStringSubmatchIndex(text, -1) {
		rest := text[loc[1]:]
		closeTag := "</" + strings.ToLower(text[loc[2]:loc[3]])
		if end := strings.Index(strings.ToLower(rest), closeTag); end >= 0 {
			regions = append(regions, hiddenRegion{rest[:end], true})
		}
	}
	return regions
}

// scanHiddenRegions re-runs the phrase rules on hidden regions. Any hit is
// reported once as a High injection finding, which raises Medium phrase hits
// (e.g. address_agent) in comments and CSS-hidden elements to a blocking
// severity because hiding them is itself evidence of intent. In <details>
// only hits that are already High count.
func scanHiddenRegions(text string) []Finding {
	for _, hr := range hiddenRegions(text) {
		region := hr.text
		if len(region) > hiddenRegionMaxLen {
			region = region[:hiddenRegionMaxLen]
		}
		var hits []Finding
		hits = append(hits, matchRules(region, injectionRules)...)
		hits = append(hits, matchRules(region, dangerousRules)...)
		for _, h := range hits {
			if h.Severity == SeverityLow || (!hr.promote && h.Severity < SeverityHigh) {
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
		if hasFillIn(queryParams(queryOf(m[1]))) {
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
	q := raw[i+1:]
	if j := strings.IndexByte(q, '#'); j >= 0 {
		q = q[:j]
	}
	return q
}

type queryParam struct{ key, value string }

// queryParams splits a raw query string into decoded key/value pairs. A part
// that fails to decode is kept raw rather than dropped, so a malformed escape
// cannot hide a value from the checks.
func queryParams(q string) []queryParam {
	var out []queryParam
	for _, part := range strings.Split(q, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		if dv, err := url.QueryUnescape(v); err == nil {
			v = dv
		}
		out = append(out, queryParam{k, v})
	}
	return out
}

func hasFillIn(params []queryParam) bool {
	for _, p := range params {
		if fillInRe.MatchString(p.value) {
			return true
		}
	}
	return false
}

var (
	azureSASKeys   = map[string]bool{"sig": true, "sv": true, "se": true, "st": true, "sp": true, "sr": true, "spr": true, "srt": true, "ss": true, "si": true, "sdd": true, "skoid": true, "sktid": true, "skt": true, "ske": true, "sks": true, "skv": true}
	cloudFrontKeys = map[string]bool{"signature": true, "key-pair-id": true, "policy": true, "expires": true}
)

// signingParam returns a predicate for parameters that belong to a
// recognised URL-signing scheme (AWS SigV4, GCS V4, CloudFront, Azure SAS, or
// a JWT access token such as GitHub's private-user-images). Their values are
// long by construction, so the length checks skip them. A scheme is only
// recognised when its signature parameter is present, and a value is only
// skipped when it is token-shaped, so prose smuggled under a signing key name
// is still caught.
func signingParam(params []queryParam) func(queryParam) bool {
	has := map[string]bool{}
	for _, p := range params {
		has[strings.ToLower(p.key)] = true
	}
	amz, goog := has["x-amz-signature"], has["x-goog-signature"]
	cf := has["signature"] && has["key-pair-id"]
	sas := has["sig"] && has["sv"]
	return func(p queryParam) bool {
		k := strings.ToLower(p.key)
		switch {
		case amz && strings.HasPrefix(k, "x-amz-"),
			goog && strings.HasPrefix(k, "x-goog-"),
			cf && cloudFrontKeys[k],
			sas && azureSASKeys[k]:
			return tokenValRe.MatchString(p.value)
		case k == "jwt":
			return jwtValRe.MatchString(p.value)
		}
		return false
	}
}

// carriesData reports whether an image URL's query string looks like it
// transports data: a long value, a well-known data key, or a fill-in marker.
// Values of recognised URL-signing parameters are not counted as long.
func carriesData(raw string) bool {
	params := queryParams(queryOf(raw))
	if len(params) == 0 {
		return false
	}
	if hasFillIn(params) {
		return true
	}
	signing := signingParam(params)
	for _, p := range params {
		if signing(p) {
			continue
		}
		if len(p.value) >= exfilValueMinLen || (exfilKeys[strings.ToLower(p.key)] && len(p.value) >= exfilKeyedMinLen) {
			return true
		}
	}
	return false
}
