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
	// fillInRe is matched against decoded query values only; keys use the
	// narrower keyFillInRe, so bracketed parameter names (filter[key]=1) do
	// not count. The your/insert/
	// paste form needs a placeholder separator (YOUR_TOKEN, insert-here) so
	// ordinary words (yourself, insertion, pasted) do not match (#19452).
	fillInRe = regexp.MustCompile(`(?i)(\{\{?\s*[a-z_][a-z0-9_ .-]*\}\}?|<[a-z_ -]{2,}>|\[[a-z_ -]{2,}\]|%7[bB][^%]*%7[dD]|\b(?:your|insert|paste)[_-][a-z]\w*)`)
	// keyFillInRe is matched against decoded query keys. It keeps only the
	// forms that are unambiguous in a key: templates ({{secrets.TOKEN}}),
	// angle placeholders (<your_data>) and upper-case YOUR_/INSERT_/PASTE_
	// names. Bracket names (filter[key]) and lower-case words are left out
	// because ordinary APIs use them as keys.
	keyFillInRe = regexp.MustCompile(`(?i:\{\{?\s*[a-z_][a-z0-9_ .-]*\}\}?|<[a-z_ -]{2,}>|%7[bB][^%]*%7[dD])|\b(?:YOUR|INSERT|PASTE)[_-][A-Z][A-Z0-9_]*\b`)
	exfilKeys   = map[string]bool{"data": true, "d": true, "q": true, "payload": true, "secret": true, "token": true, "key": true, "content": true, "text": true, "msg": true, "message": true}
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
	// Report the most severe image only: High for plain data, Medium for a
	// signed provider URL whose only long values are its signing parameters.
	var worst *Finding
	for _, u := range imgs {
		sev, ok := carriesData(u)
		if !ok || (worst != nil && sev <= worst.Severity) {
			continue
		}
		worst = &Finding{Kind: KindDangerous, Severity: sev, Rule: ruleImageExfil, Snippet: snippet(u)}
	}
	if worst != nil {
		out = append(out, *worst)
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
// cannot hide a value from the checks. A part without "=" (?{{secrets}}) is
// treated as a value, so the value-only checks still see it.
func queryParams(q string) []queryParam {
	var out []queryParam
	for _, part := range strings.Split(q, "&") {
		if part == "" {
			continue
		}
		k, v, hasEq := strings.Cut(part, "=")
		if !hasEq {
			k, v = "", k
		}
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
		if fillInRe.MatchString(p.value) || keyFillInRe.MatchString(p.key) {
			return true
		}
	}
	return false
}

// signingScheme is a URL-signing scheme whose parameters carry long values by
// construction. shapes holds an exact value shape for every parameter that is
// recognised as signing data; any other parameter, and any value that does
// not fit its shape, is counted as usual. A scheme only applies on a host of
// the provider that issues it (hosts), and only when its signature parameter
// is present and itself well-formed. Each key is recognised at most once per
// URL, so repeating it cannot widen the channel. Parameters that are free data
// by design (x-amz-security-token, CloudFront Policy, Azure si) are never
// recognised, so a URL that carries one stays High. Custom domains in front of
// these providers stay High; that is the safe direction.
//
// Recognised signing values are not a free pass: anyone can create a bucket,
// distribution or storage account on these hosts and read the query strings
// of requests from its access logs, and a shape such as a CloudFront
// signature holds about 129 bytes of arbitrary base64url. So a long signing
// value lowers the image finding to Medium rather than dropping it. Output
// blocks only at High, so an agent can post a signed screenshot link, while
// untrusted input still blocks at the default Medium line.
type signingScheme struct {
	sigKey string
	// hosts lists exact hostnames, or suffixes when they start with ".".
	hosts  []string
	shapes map[string]*regexp.Regexp
}

var (
	isoDateRe      = regexp.MustCompile(`^\d{8}T\d{6}Z$`)
	smallIntRe     = regexp.MustCompile(`^\d{1,10}$`)
	signedHdrsRe   = regexp.MustCompile(`^[a-z0-9-]{1,32}(?:;[a-z0-9-]{1,32}){0,7}$`)
	azureDateRe    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(?:T\d{2}:\d{2}(?::\d{2}(?:\.\d{1,7})?)?Z)?$`)
	azureGUIDRe    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	signingSchemes = []signingScheme{
		{"x-amz-signature", []string{".amazonaws.com", ".amazonaws.com.cn"}, map[string]*regexp.Regexp{
			"x-amz-signature":     regexp.MustCompile(`^[0-9a-f]{64}$`),
			"x-amz-algorithm":     regexp.MustCompile(`^AWS4-HMAC-SHA256$`),
			"x-amz-credential":    regexp.MustCompile(`^[A-Z0-9]{16,32}/\d{8}/[a-z0-9-]{2,20}/[a-z0-9-]{2,20}/aws4_request$`),
			"x-amz-date":          isoDateRe,
			"x-amz-expires":       smallIntRe,
			"x-amz-signedheaders": signedHdrsRe,
		}},
		{"x-goog-signature", []string{"storage.googleapis.com", ".storage.googleapis.com"}, map[string]*regexp.Regexp{
			"x-goog-signature":     regexp.MustCompile(`^(?:[0-9a-f]{64}|[0-9a-f]{512})$`),
			"x-goog-algorithm":     regexp.MustCompile(`^GOOG4-(?:RSA|HMAC)-SHA256$`),
			"x-goog-credential":    regexp.MustCompile(`^[a-z0-9.@_-]{1,96}/\d{8}/[a-z0-9-]{2,30}/storage/goog4_request$`),
			"x-goog-date":          isoDateRe,
			"x-goog-expires":       smallIntRe,
			"x-goog-signedheaders": signedHdrsRe,
		}},
		{"signature", []string{".cloudfront.net"}, map[string]*regexp.Regexp{
			// CloudFront: URL-safe base64 of a 1024- or 2048-bit RSA signature.
			"signature":   regexp.MustCompile(`^(?:[A-Za-z0-9~_-]{172}|[A-Za-z0-9~_-]{344})$`),
			"key-pair-id": regexp.MustCompile(`^(?:K[A-Z0-9]{12,20}|APKA[A-Z0-9]{16})$`),
			"expires":     smallIntRe,
		}},
		{"sig", []string{".blob.core.windows.net", ".dfs.core.windows.net"}, map[string]*regexp.Regexp{
			// Azure SAS: base64 of an HMAC-SHA256.
			"sig":   regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`),
			"sv":    azureDateRe,
			"st":    azureDateRe,
			"se":    azureDateRe,
			"skt":   azureDateRe,
			"ske":   azureDateRe,
			"skv":   azureDateRe,
			"skoid": azureGUIDRe,
			"sktid": azureGUIDRe,
		}},
	}
	jwtValRe = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*$`)
)

// jwtHosts are hosts whose image URLs carry a JWT access token (GitHub's
// private-user-images). The jwt exemption is tied to the host, not the value
// shape, because any base64url data can be dressed as a JWT. Unlike the
// signing providers, no third party can serve or log requests on these hosts,
// so a request there exfiltrates nothing and the jwt is fully exempt.
var jwtHosts = map[string]bool{"private-user-images.githubusercontent.com": true}

// paramClass is how signingParam classifies one query parameter.
type paramClass int

const (
	paramData    paramClass = iota // counted by the length checks as usual
	paramSigning                   // a recognised signing value; lowers the finding to Medium
	paramExempt                    // a jwt on a GitHub-owned host; never counted
)

// signingParam returns a classifier for parameters whose long values come
// from a recognised URL-signing scheme (#19452). See signingScheme for the
// host and shape bounds and for why these values still count at Medium. The
// classifier is stateful: it recognises each key at most once, so call it
// once per parameter, in order.
func signingParam(host string, params []queryParam) func(queryParam) paramClass {
	host = strings.ToLower(host)
	var active []signingScheme
	for _, s := range signingSchemes {
		if !hostMatches(host, s.hosts) {
			continue
		}
		for _, p := range params {
			if strings.ToLower(p.key) == s.sigKey && s.shapes[s.sigKey].MatchString(p.value) {
				active = append(active, s)
				break
			}
		}
	}
	jwtHost := jwtHosts[host]
	seen := map[string]bool{}
	return func(p queryParam) paramClass {
		k := strings.ToLower(p.key)
		if seen[k] {
			return paramData
		}
		class := paramData
		if k == "jwt" {
			if jwtHost && jwtValRe.MatchString(p.value) {
				class = paramExempt
			}
		} else {
			for _, s := range active {
				if re, ok := s.shapes[k]; ok && re.MatchString(p.value) {
					class = paramSigning
					break
				}
			}
		}
		if class != paramData {
			seen[k] = true
		}
		return class
	}
}

// hostMatches reports whether host equals an entry of hosts, or ends with an
// entry that starts with ".". host must already be lower-case.
func hostMatches(host string, hosts []string) bool {
	for _, h := range hosts {
		if host == h || (strings.HasPrefix(h, ".") && strings.HasSuffix(host, h)) {
			return true
		}
	}
	return false
}

// carriesData reports whether an image URL's query string looks like it
// transports data (a long value, a well-known data key, or a fill-in marker)
// and at what severity. It is High unless the only long values are recognised
// URL-signing parameters, which give Medium; see signingScheme.
func carriesData(raw string) (Severity, bool) {
	params := queryParams(queryOf(raw))
	if len(params) == 0 {
		return 0, false
	}
	if hasFillIn(params) {
		return SeverityHigh, true
	}
	var host string
	if u, err := url.Parse(raw); err == nil {
		host = u.Hostname()
	}
	classify := signingParam(host, params)
	signed := false
	for _, p := range params {
		switch classify(p) {
		case paramExempt:
			continue
		case paramSigning:
			// A scheme is active only once its signature is present and
			// well-formed, and every signature shape is long, so a signing
			// value always lowers the finding to Medium.
			signed = true
			continue
		}
		if len(p.value) >= exfilValueMinLen || (exfilKeys[strings.ToLower(p.key)] && len(p.value) >= exfilKeyedMinLen) {
			return SeverityHigh, true
		}
	}
	if signed {
		return SeverityMedium, true
	}
	return 0, false
}
