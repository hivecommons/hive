package customers

import (
	"strings"
	"unicode"
)

// DeriveCountry turns a free-text location into an ISO 3166 alpha-2 country,
// deterministically and offline, from the bundled table in country_data.go.
// It is a best guess about a text the person typed, not a fact: callers store
// it as an automatic field a person can overwrite.

// CountryRuleLocation is the rule name recorded on an automatic country.
const CountryRuleLocation = "location"

// Limits on how much of a location is examined, so one long string cannot
// make a lookup expensive.
const (
	maxLocationSegments = 6
	maxSuffixWords      = 3
	maxAbbrevLen        = 4
	isoCodeLen          = 2
	flagBase            = 0x1F1E6
	flagLast            = 0x1F1FF
)

// ambiguousStates are US state names that are also a country; a city of that
// other country before them wins ("Tbilisi, Georgia").
var ambiguousStates = map[string]bool{"georgia": true}

var (
	countryNames   = map[string]string{} // folded name or alias -> ISO
	countryByCode  = map[string]string{} // ISO -> English name (folded)
	subdivisions   = map[string]string{} // folded state, province -> ISO
	stateAbbrevMap = map[string]string{} // upper-case abbreviation -> ISO
	cityCountries  = map[string]string{} // folded city -> ISO
)

func init() {
	for _, line := range strings.Split(countryTable, "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			continue
		}
		code := parts[0]
		countryByCode[code] = parts[1]
		countryNames[parts[1]] = code
		for _, a := range strings.Split(parts[2], ";") {
			if a != "" {
				countryNames[a] = code
			}
		}
	}
	loadPlaces(subdivisionTable, subdivisions)
	loadPlaces(cityTable, cityCountries)
	for _, line := range strings.Split(stateAbbrevs, "\n") {
		code, list, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		for _, ab := range strings.Fields(list) {
			stateAbbrevMap[ab] = code
		}
	}
}

func loadPlaces(table string, into map[string]string) {
	for _, line := range strings.Split(table, "\n") {
		code, list, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		for _, n := range strings.Split(list, ";") {
			if n != "" {
				into[n] = code
			}
		}
	}
}

var placeFolder = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ã", "a", "ä", "a", "å", "a", "ā", "a", "ă", "a",
	"ç", "c", "ć", "c", "č", "c",
	"é", "e", "è", "e", "ê", "e", "ë", "e", "ě", "e", "ę", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i", "ı", "i",
	"ł", "l", "ñ", "n", "ń", "n", "ň", "n",
	"ó", "o", "ò", "o", "ô", "o", "õ", "o", "ö", "o", "ø", "o", "ő", "o",
	"ř", "r", "š", "s", "ş", "s", "ś", "s", "ß", "ss",
	"ú", "u", "ù", "u", "û", "u", "ü", "u", "ű", "u", "ů", "u",
	"ý", "y", "ž", "z", "ź", "z", "ż", "z", "ğ", "g",
)

// foldPlace lower-cases s, folds common Latin accents, turns punctuation into
// spaces and collapses whitespace: "São Paulo" and "sao paulo" compare equal.
func foldPlace(s string) string {
	s = placeFolder.Replace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	f := strings.Join(strings.Fields(b.String()), " ")
	switch f {
	case "u s a", "u s":
		return "usa"
	case "u k":
		return "uk"
	}
	return strings.TrimPrefix(f, "the ")
}

// ValidCountry reports whether code is a known ISO 3166 alpha-2 code.
func ValidCountry(code string) bool {
	_, ok := countryByCode[strings.ToUpper(strings.TrimSpace(code))]
	return ok
}

// CountryFromInput reads a country a person typed or picked: an ISO code
// ("de") or a country name or alias ("Germany", "UK"). It returns "" when the
// input is neither.
func CountryFromInput(s string) string {
	t := strings.TrimSpace(s)
	if len(t) == isoCodeLen && ValidCountry(t) {
		return strings.ToUpper(t)
	}
	return countryNames[foldPlace(t)]
}

// flagCountry returns the country of a regional-indicator flag emoji in s.
func flagCountry(s string) string {
	rs := []rune(s)
	for i := 0; i+1 < len(rs); i++ {
		a, b := rs[i], rs[i+1]
		if a >= flagBase && a <= flagLast && b >= flagBase && b <= flagLast {
			code := string([]rune{'A' + (a - flagBase), 'A' + (b - flagBase)})
			if ValidCountry(code) {
				return code
			}
		}
	}
	return ""
}

// segmentCountry resolves one comma-separated piece. afterComma is true for
// the final piece of a multi-piece location, where an upper-case two-letter
// code reads as a US state first ("Austin, TX"). ambiguous reports a hit a
// city of another country before it may overrule ("Berlin, DE").
func segmentCountry(raw string, afterComma bool) (code string, ambiguous bool) {
	raw = strings.TrimSpace(raw)
	if t := strings.ReplaceAll(raw, ".", ""); t != "" && len(t) <= maxAbbrevLen && t == strings.ToUpper(t) {
		if c, ok := stateAbbrevMap[t]; ok && afterComma {
			return c, ValidCountry(t)
		}
		if len(t) == isoCodeLen && ValidCountry(t) {
			return t, false
		}
	}
	words := strings.Fields(foldPlace(raw))
	// Longest word-suffix first, so "kansas city" beats "kansas" and
	// "munich germany" resolves through "germany".
	for n := min(len(words), maxSuffixWords); n >= 1; n-- {
		cand := strings.Join(words[len(words)-n:], " ")
		if cand == "us" && len(words) > 1 {
			continue
		}
		if c, ok := countryNames[cand]; ok {
			return c, false
		}
		if c, ok := subdivisions[cand]; ok {
			return c, ambiguousStates[cand]
		}
		if c, ok := cityCountries[cand]; ok {
			return c, false
		}
	}
	return "", false
}

// DeriveCountry returns the ISO alpha-2 country a free-text location names,
// or "" when it names none ("remote", "Earth", empty). The last
// comma-separated piece decides; earlier pieces are tried only when it names
// nothing, except that a known city before an ambiguous state name or code
// wins.
func DeriveCountry(location string) string {
	loc := strings.TrimSpace(location)
	if loc == "" {
		return ""
	}
	if c := flagCountry(loc); c != "" {
		return c
	}
	segs := strings.FieldsFunc(loc, func(r rune) bool {
		return r == ',' || r == ';' || r == '/' || r == '|' || r == '(' || r == ')' || r == '\n'
	})
	if len(segs) > maxLocationSegments {
		segs = segs[len(segs)-maxLocationSegments:]
	}
	for i := len(segs) - 1; i >= 0; i-- {
		c, amb := segmentCountry(segs[i], i == len(segs)-1 && i > 0)
		if c == "" {
			continue
		}
		if amb {
			for j := i - 1; j >= 0; j-- {
				if other := cityCountries[foldPlace(segs[j])]; other != "" {
					return other
				}
			}
		}
		return c
	}
	return ""
}
