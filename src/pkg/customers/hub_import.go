package customers

// Hub import (phase 3 of src/docs/design/customers-hub-consent.md, #19163).
//
// A person who entered their real name, organization or country on the hub,
// and consented to share them with this hive, can have them fill their
// profile here. The hive posts salted hashes of its confirmed customers'
// GitHub logins and the hub answers only for people who consented. The
// network call is the caller's HubMatcher; this file only decides what is
// asked and what an answer may change.
//
// Rules, each pinned by hub_import_test.go:
//   - Off unless Enabled says on: off makes no request at all.
//   - Only confirmed customers are asked about, never a suggestion, and only
//     by a confirmed GitHub login.
//   - Import never creates a person and touches nothing but the three fields.
//   - manual is never overwritten. Precedence (#19306): manual > entered by
//     the person on the hub > public forge profile (auto) > hub derived or
//     admin-set. A derived or admin-set hub value only fills an empty field
//     or replaces an earlier hub value; it never replaces an auto value.
//   - Each hub value keeps the label the hub sent (Rule ProfileSourceHub*),
//     which the dashboard shows in the pill tooltip and profile details.
//   - A cleared entry, or a hub value not refreshed within HubImportMaxAge,
//     removes only hub-provenance fields and hands them back to automatic.
//   - A forgotten person is gone with a tombstone; import cannot rebuild them.
//   - Reply values are untrusted: control and format characters stripped,
//     length-capped, a country must be a known ISO code.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// ProfileSourceHub is the Rule of a hub value the person entered and
	// consented to share (also every value from a hub that sends no labels).
	ProfileSourceHub = "hub_consent"
	// ProfileSourceHubPerson: entered by the person on the hub, passed as
	// public information without per-person consent (#19306).
	ProfileSourceHubPerson = "hub_person"
	// ProfileSourceHubAdmin: set by a hub admin on the person's behalf.
	ProfileSourceHubAdmin = "hub_admin"
	// ProfileSourceHubEmailDomain: derived on the hub from an organization
	// email domain the hub operator listed. The email never crosses.
	ProfileSourceHubEmailDomain = "hub_email_domain"
	// ProfileSourceHubBrowserLanguage: a country the hub inferred from the
	// browser language at sign-in.
	ProfileSourceHubBrowserLanguage = "hub_browser_language"
	// ProfileSourceHubUnrecorded: a hub value saved before the hub recorded
	// who supplied it.
	ProfileSourceHubUnrecorded = "hub_unrecorded"
	// The hub's own affiliation lookup, by how it found the organization: the
	// person's public profile company, a public organization membership, the
	// sign-in identity provider, or a GitHub Enterprise account. Only a
	// project hive is sent these (pkg/hub customer_match_project.go).
	ProfileSourceHubProfileCompany   = "hub_profile_company"
	ProfileSourceHubOrgMembership    = "hub_org_membership"
	ProfileSourceHubIdentityProvider = "hub_identity_provider"
	ProfileSourceHubGHEAccount       = "hub_ghe_account"
	// HubImportInterval is how often the bounded import pass runs.
	HubImportInterval = time.Hour
	// HubImportMaxAge is how long a hub value is kept without being
	// refreshed. Past it the value is cleared as if the hub had said so, so a
	// hive that loses touch with the hub does not keep a withdrawn value.
	HubImportMaxAge = 72 * time.Hour
	// HubImportBatch is the most hashes one request carries; it matches the
	// hub's maxCustomerMatchBatch.
	HubImportBatch = 200
	// HubImportMaxRequests bounds the requests of one pass, below the hub's
	// per-hive rate limit of six a minute.
	HubImportMaxRequests = 4
	// HubImportMaxReply is the most reply bytes read; it matches the hub's
	// response cap plus headroom.
	HubImportMaxReply = 160 << 10
	// HubMatchLabelsHeader asks the hub for labeled public fields (pkg/hub
	// customerMatchLabelsHeader); HubMatchLabelsV1 is its value. A hub that
	// predates labels ignores it and answers consent-only.
	HubMatchLabelsHeader = "X-Hive-Customer-Match-Labels"
	HubMatchLabelsV1     = "1"
)

// Skip reasons of a hub import pass.
const (
	HubSkipDisabled = "disabled"
	HubSkipNoKey    = "no_match_key"
	HubSkipNoPeople = "no_candidates"
	HubSkipFailed   = "request_failed"
)

// Audit actions a hub import reports.
const (
	HubAuditImport = "customer_hub_import"
	HubAuditClear  = "customer_hub_clear"
)

// HubFields are the only profile fields a hub import reads or writes.
var HubFields = []string{FieldRealName, FieldOrganization, FieldCountry}

// hubLabelRules maps the hub's wire source labels (pkg/hub
// customerMatchSource*) to the Rule a hub value carries here.
var hubLabelRules = map[string]string{
	"consent":           ProfileSourceHub,
	"person":            ProfileSourceHubPerson,
	"admin":             ProfileSourceHubAdmin,
	"email_domain":      ProfileSourceHubEmailDomain,
	"browser_language":  ProfileSourceHubBrowserLanguage,
	"unrecorded":        ProfileSourceHubUnrecorded,
	"profile_company":   ProfileSourceHubProfileCompany,
	"org_membership":    ProfileSourceHubOrgMembership,
	"identity_provider": ProfileSourceHubIdentityProvider,
	"ghe_account":       ProfileSourceHubGHEAccount,
}

// hubPublicFields are the fields the hub may send under a public label;
// a real name crosses only with consent.
var hubPublicFields = []string{FieldOrganization, FieldCountry}

// HubRuleEnteredByPerson reports whether a hub Rule is a value the person
// entered on the hub, which outranks a public forge profile value.
func HubRuleEnteredByPerson(rule string) bool {
	return rule == ProfileSourceHub || rule == ProfileSourceHubPerson
}

// HubValueYieldsToForge reports whether f is a hub value that a public forge
// profile value (or the country derived from the forge location) may
// replace: a hub-derived, admin-set or unrecorded value. The profile fill
// uses it so forge public > hub derived/admin holds whichever pass ran first.
func HubValueYieldsToForge(f ProfileField) bool {
	return f.Source == SourceHub && f.Rule != "" && !HubRuleEnteredByPerson(f.Rule)
}

// HubLabel is the provenance text a hub Rule shows, for tooltips.
func HubLabel(rule string) string {
	switch rule {
	case ProfileSourceHubPerson:
		return "from hub: entered by the person"
	case ProfileSourceHubAdmin:
		return "from hub: set by a hub admin"
	case ProfileSourceHubEmailDomain:
		return "from hub: derived from an organization email domain"
	case ProfileSourceHubBrowserLanguage:
		return "from hub: inferred from browser language"
	case ProfileSourceHubUnrecorded:
		return "from hub: source not recorded"
	case ProfileSourceHubProfileCompany:
		return "from hub: public profile company"
	case ProfileSourceHubOrgMembership:
		return "from hub: public organization membership"
	case ProfileSourceHubIdentityProvider:
		return "from hub: sign-in identity provider"
	case ProfileSourceHubGHEAccount:
		return "from hub: GitHub Enterprise account"
	}
	return "from hub: entered by the person, shared with consent"
}

// HubMatchEntry is one answer entry, already reduced to what import reads.
type HubMatchEntry struct {
	MatchHash    string
	RealName     string
	Organization string
	Country      string
	Cleared      []string
	// Rules is the Rule each value carries, from the hub's labels. A field
	// without a label is ProfileSourceHub (consented, or an unlabeled hub).
	Rules map[string]string
}

// rule is the Rule the named value is stored with.
func (e HubMatchEntry) rule(field string) string {
	if r := e.Rules[field]; r != "" {
		return r
	}
	return ProfileSourceHub
}

// HubMatchReply is a decoded hub answer.
type HubMatchReply struct {
	Matches   []HubMatchEntry
	Truncated bool
}

// HubMatcher posts one batch of match hashes to the hub.
type HubMatcher interface {
	Match(ctx context.Context, hashes []string) (HubMatchReply, error)
}

// HubMatchHash is HMAC-SHA256(match key, lower-cased GitHub login) in hex,
// the hash the hub's match endpoint compares (pkg/hub customerMatchHash).
func HubMatchHash(key, login string) string {
	login = strings.ToLower(strings.TrimSpace(login))
	if key == "" || login == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(login))
	return hex.EncodeToString(mac.Sum(nil))
}

// DecodeHubMatchReply reads a hub answer. Only match_hash, the three field
// values and cleared are read; every other key is ignored, and each value is
// sanitised (SanitizeHubValue). Cleared names outside HubFields are dropped.
func DecodeHubMatchReply(r io.Reader) (HubMatchReply, error) {
	var wire struct {
		Matches []struct {
			MatchHash    string            `json:"match_hash"`
			RealName     string            `json:"real_name"`
			Organization string            `json:"organization"`
			Country      string            `json:"country"`
			Cleared      []string          `json:"cleared"`
			Sources      map[string]string `json:"sources"`
		} `json:"matches"`
		Truncated bool `json:"truncated"`
	}
	if err := json.NewDecoder(io.LimitReader(r, HubImportMaxReply)).Decode(&wire); err != nil {
		return HubMatchReply{}, err
	}
	out := HubMatchReply{Truncated: wire.Truncated}
	for _, m := range wire.Matches {
		e := HubMatchEntry{
			MatchHash:    strings.TrimSpace(m.MatchHash),
			RealName:     SanitizeHubValue(m.RealName),
			Organization: SanitizeHubValue(m.Organization),
			Country:      hubCountry(m.Country),
		}
		for _, c := range m.Cleared {
			if contains(HubFields, c) && !contains(e.Cleared, c) {
				e.Cleared = append(e.Cleared, c)
			}
		}
		e.Rules, e.RealName = hubLabels(m.Sources, e.RealName)
		out.Matches = append(out.Matches, e)
	}
	return out, nil
}

// hubLabels turns the hub's source labels into Rules. An unknown label or
// field is dropped (the value is then treated as consented, as before labels
// existed). A real name labeled as anything but consent is dropped with its
// value: the hub never sends one, and a hive must not store one.
func hubLabels(sources map[string]string, realName string) (map[string]string, string) {
	if len(sources) == 0 {
		return nil, realName
	}
	if src, ok := sources[FieldRealName]; ok && hubLabelRules[src] != ProfileSourceHub {
		realName = ""
	}
	rules := make(map[string]string, len(hubPublicFields))
	for _, name := range hubPublicFields {
		if r := hubLabelRules[sources[name]]; r != "" {
			rules[name] = r
		}
	}
	if len(rules) == 0 {
		return nil, realName
	}
	return rules, realName
}

// SanitizeHubValue makes a hub value safe to store: control and format
// characters (including bidi overrides and zero-width marks) are removed,
// whitespace runs collapse to one space, and the result is cut on a rune
// boundary to MaxProfileFieldLen bytes.
func SanitizeHubValue(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			if unicode.IsSpace(r) {
				return ' '
			}
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= MaxProfileFieldLen {
		return s
	}
	cut := MaxProfileFieldLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}

// hubCountry accepts only a known ISO 3166 alpha-2 code.
func hubCountry(s string) string {
	s = strings.ToUpper(SanitizeHubValue(s))
	if len(s) != isoCodeLen || !ValidCountry(s) {
		return ""
	}
	return s
}

func (e HubMatchEntry) value(field string) string {
	switch field {
	case FieldRealName:
		return e.RealName
	case FieldOrganization:
		return e.Organization
	case FieldCountry:
		return e.Country
	}
	return ""
}

var errHubUnchanged = errors.New("customers: hub import changed nothing")

// ApplyHubMatch applies one hub answer entry to an existing customer who
// still holds login as a confirmed GitHub identity. It never creates a
// person and touches only HubFields: a manual field is left alone, a cleared
// field that is hub provenance is emptied and handed back to automatic, a
// value the person entered on the hub is written over an auto or empty
// field, and a derived or admin-set hub value only over an empty or hub
// field. A value the field already holds from the hub with the same label
// only refreshes its time. It returns the names it set and the names it
// cleared.
func (s *Store) ApplyHubMatch(id, login string, e HubMatchEntry) (set, cleared []string, err error) {
	key := IdentityKey(KindGitHub, login)
	_, err = s.changePerson(id, func(p *Person) error {
		if p.Status != StatusCustomer || !p.HasIdentity(key) || s.tombs[s.tombstoneHash(key)] {
			return ErrNotFound
		}
		now := s.now().UTC()
		touched := false
		for _, name := range HubFields {
			f := p.Profile.Field(name)
			if f.Source == SourceManual {
				continue
			}
			if contains(e.Cleared, name) {
				if f.Source == SourceHub {
					*f = ProfileField{}
					cleared = append(cleared, name)
					touched = true
				}
				continue
			}
			v := e.value(name)
			if name == FieldCountry {
				v = hubCountry(v)
			} else {
				v = SanitizeHubValue(v)
			}
			if v == "" {
				continue
			}
			rule := e.rule(name)
			if f.Source == SourceHub && f.Value == v && f.Rule == rule {
				f.At = now
				touched = true
				continue
			}
			if !HubRuleEnteredByPerson(rule) && f.Source != SourceHub && f.Value != "" {
				continue // a public forge profile value outranks hub derived/admin
			}
			*f = ProfileField{Value: v, Source: SourceHub, Rule: rule, At: now}
			set = append(set, name)
			touched = true
		}
		if contains(cleared, FieldCountry) {
			deriveAutoCountry(p, now)
		}
		if !touched {
			return errHubUnchanged
		}
		return nil
	})
	if errors.Is(err, errHubUnchanged) {
		err = nil
	}
	return set, cleared, err
}

// HubExpiry is one person's hub fields cleared for staleness.
type HubExpiry struct {
	Person string
	Fields []string
}

// ExpireHubFields clears every hub-provenance field not refreshed within
// maxAge, the same way a cleared reply does, and reports what it cleared.
func (s *Store) ExpireHubFields(maxAge time.Duration) ([]HubExpiry, error) {
	var out []HubExpiry
	err := s.change(func() error {
		now := s.now().UTC()
		for id, p := range s.people {
			var fields []string
			for _, name := range HubFields {
				f := p.Profile.Field(name)
				if f.Source == SourceHub && now.Sub(f.At) > maxAge {
					*f = ProfileField{}
					fields = append(fields, name)
				}
			}
			if len(fields) == 0 {
				continue
			}
			if contains(fields, FieldCountry) {
				deriveAutoCountry(&p, now)
			}
			p.UpdatedAt = now
			s.people[id] = p
			out = append(out, HubExpiry{Person: id, Fields: fields})
		}
		if len(out) == 0 {
			return errHubUnchanged
		}
		return nil
	})
	if errors.Is(err, errHubUnchanged) {
		err = nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Person < out[j].Person })
	return out, err
}

// HubImport runs the import. Every func is read at call time.
type HubImport struct {
	Store   *Store
	Matcher HubMatcher
	// Enabled is the spoke switch AND a hub registration. Off: no request.
	Enabled func() bool
	// Key is the per-hive match key; "" makes no request.
	Key func() string
	// Audit receives one call per import or clear: a person id, an action
	// (HubAuditImport or HubAuditClear) and field names, never values.
	Audit func(person, action string, fields []string)
	// MaxAge overrides HubImportMaxAge (tests).
	MaxAge time.Duration
}

// HubImportResult counts what one pass did: counts only.
type HubImportResult struct {
	Requests int    `json:"requests"`
	Asked    int    `json:"asked"`
	Imported int    `json:"imported"`
	Cleared  int    `json:"cleared"`
	Expired  int    `json:"expired"`
	Skipped  string `json:"skipped,omitempty"`
}

type hubCandidate struct{ id, login string }

// Run is one import pass. With only set, it asks about that one customer
// (the accept-time pull); otherwise about every confirmed customer with a
// GitHub login. Staleness expiry runs first and on every pass, switch on or
// off, because it makes no request and it is what removes a value the hive
// can no longer confirm.
func (h *HubImport) Run(ctx context.Context, only string) HubImportResult {
	var res HubImportResult
	if h == nil || h.Store == nil {
		res.Skipped = HubSkipDisabled
		return res
	}
	maxAge := h.MaxAge
	if maxAge <= 0 {
		maxAge = HubImportMaxAge
	}
	if exp, err := h.Store.ExpireHubFields(maxAge); err == nil {
		for _, x := range exp {
			res.Expired++
			h.audit(x.Person, HubAuditClear, x.Fields)
		}
	}
	if h.Matcher == nil || h.Enabled == nil || !h.Enabled() {
		res.Skipped = HubSkipDisabled
		return res
	}
	key := ""
	if h.Key != nil {
		key = h.Key()
	}
	if key == "" {
		res.Skipped = HubSkipNoKey
		return res
	}
	byHash := h.candidates(key, only)
	if len(byHash) == 0 {
		res.Skipped = HubSkipNoPeople
		return res
	}
	hashes := make([]string, 0, len(byHash))
	for hash := range byHash {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	res.Asked = len(hashes)
	for len(hashes) > 0 && res.Requests < HubImportMaxRequests {
		batch := hashes
		if len(batch) > HubImportBatch {
			batch = batch[:HubImportBatch]
		}
		res.Requests++
		reply, err := h.Matcher.Match(ctx, batch)
		if err != nil {
			res.Skipped = HubSkipFailed
			return res
		}
		last := -1
		pos := make(map[string]int, len(batch))
		for i, hash := range batch {
			pos[hash] = i
		}
		for _, e := range reply.Matches {
			i, asked := pos[e.MatchHash]
			if !asked {
				continue // never asked about: ignored
			}
			if i > last {
				last = i
			}
			c := byHash[e.MatchHash]
			set, cleared, err := h.Store.ApplyHubMatch(c.id, c.login, e)
			if err != nil {
				continue
			}
			if len(set) > 0 {
				res.Imported++
				h.audit(c.id, HubAuditImport, set)
			}
			if len(cleared) > 0 {
				res.Cleared++
				h.audit(c.id, HubAuditClear, cleared)
			}
		}
		next := len(batch)
		if reply.Truncated && last >= 0 && last+1 < len(batch) {
			// The hub dropped entries after the last one it sent; ask
			// again from there.
			next = last + 1
		}
		hashes = hashes[next:]
	}
	return res
}

// candidates maps each confirmed customer's GitHub login hash to them. A
// suggestion, a claimed identity, a bot and a forgotten login are never
// hashed.
func (h *HubImport) candidates(key, only string) map[string]hubCandidate {
	out := make(map[string]hubCandidate)
	for _, p := range h.Store.People() {
		if p.Status != StatusCustomer || only != "" && p.ID != only {
			continue
		}
		for _, i := range p.Identities {
			if i.Kind != KindGitHub || i.Claimed || IsBotLogin(i.Value) || h.Store.Forgotten(KindGitHub, i.Value) {
				continue
			}
			login := NormalizeIdentity(KindGitHub, i.Value)
			if hash := HubMatchHash(key, login); hash != "" {
				out[hash] = hubCandidate{id: p.ID, login: login}
			}
		}
	}
	return out
}

func (h *HubImport) audit(person, action string, fields []string) {
	if h.Audit != nil && len(fields) > 0 {
		h.Audit(person, action, append([]string(nil), fields...))
	}
}
