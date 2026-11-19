package hub

// Per-hive customer match endpoint (#19162, phase 2 of
// src/docs/design/customers-hub-consent.md).
//
// A hive posts salted hashes of the GitHub logins of its confirmed customers
// and the hub answers only for people who consented to share fields with that
// hive. Unknown, unconsented and never-delivered people are the same silence,
// so the reply is not an oracle for who exists on the hub. A cleared entry is
// the only statement about a past consent, and only to a hive that already
// received that person's data.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	customerMatchPath = "/api/customers/match"
	// Domain-separation label for the per-hive match key. Never the heartbeat
	// label: the bearer authenticates the hive, the match key salts the hashes.
	infoCustomerMatchKey = "hive-customer-match-v1"

	maxCustomerMatchBatch    = 200
	maxCustomerMatchBody     = 32 << 10
	maxCustomerMatchResponse = 128 << 10
	customerMatchRateWindow  = time.Minute
	customerMatchRateLimit   = 6
	// How long the per-hive index of consented hashes is reused. Consent is
	// still re-read per answer, so a withdrawal is effective immediately; a
	// new grant also drops the index (invalidateCustomerMatchIndex).
	customerMatchIndexTTL = 30 * time.Second
	customerMatchHashLen  = sha256.Size * 2
	customerMatchRevLen   = 16
)

// customerMatchKey derives the per-hive match key from one master. It returns
// "" for an empty master or hive id, so an unconfigured hub matches nothing.
func customerMatchKey(master, hiveID string) string {
	return derivePerHiveKey(master, infoCustomerMatchKey, hiveID)
}

// customerMatchHash is HMAC-SHA256(match key, normalised GitHub login), hex.
func customerMatchHash(key, login string) string {
	login = strings.ToLower(strings.TrimSpace(login))
	if key == "" || login == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(login))
	return hex.EncodeToString(mac.Sum(nil))
}

// customerMatchKeys returns the match key under every master generation the
// hub still accepts, current first, mirroring verifyHeartbeatBearer so a spoke
// not yet reconciled after a rotation still matches.
func (s *HubServer) customerMatchKeys(hiveID string) []string {
	var keys []string
	if gs := s.currentGenerations(); gs != nil {
		for _, g := range gs.acceptableGenerations(time.Now()) {
			if k := customerMatchKey(g.Secret, hiveID); k != "" {
				keys = append(keys, k)
			}
		}
		return keys
	}
	if k := customerMatchKey(s.hubSecret, hiveID); k != "" {
		keys = append(keys, k)
	}
	return keys
}

// customerMatchLogin is the one identifier matched in v1: the primary github:
// identity. A LinkedGitHubLogin is not matched because no hub flow proves it
// (nothing writes it after a GitHub OAuth check), and the design requires that
// proof first. Email is never matched in v1.
func customerMatchLogin(u *SaaSUser) string {
	provider, subject, ok := parseCanonical(userCanonicalID(u))
	if !ok || provider != legacyProvider {
		return ""
	}
	return subject
}

type customerMatchIndex struct {
	built time.Time
	// match hash -> consent file path. Holds only people with a record for
	// this hive that is active or owes a cleared reply.
	paths map[string]string
}

type customerMatchRate struct {
	start time.Time
	count int
}

type customerMatchState struct {
	mu      sync.Mutex
	indexes map[string]customerMatchIndex
	rates   map[string]customerMatchRate
}

var customerMatchStore customerMatchState

// allow counts one request against the hive's fixed window.
func (st *customerMatchState) allow(hiveID string, now time.Time) (time.Duration, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.rates == nil {
		st.rates = make(map[string]customerMatchRate)
	}
	r := st.rates[hiveID]
	if now.Sub(r.start) >= customerMatchRateWindow || now.Before(r.start) {
		r = customerMatchRate{start: now}
	}
	if r.count >= customerMatchRateLimit {
		return r.start.Add(customerMatchRateWindow).Sub(now), false
	}
	r.count++
	st.rates[hiveID] = r
	return 0, true
}

func invalidateCustomerMatchIndex() {
	customerMatchStore.mu.Lock()
	customerMatchStore.indexes = nil
	customerMatchStore.mu.Unlock()
}

// index returns the hive's hash index, rebuilding it when stale. The caller
// holds customerConsentMu.
func (s *HubServer) customerMatchIndexFor(hiveID string, keys []string, now time.Time) map[string]string {
	st := &customerMatchStore
	st.mu.Lock()
	if idx, ok := st.indexes[hiveID]; ok && now.Sub(idx.built) < customerMatchIndexTTL && !now.Before(idx.built) {
		st.mu.Unlock()
		return idx.paths
	}
	st.mu.Unlock()

	paths := make(map[string]string)
	dir := customerConsentDir()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Warn("customer match: list consents failed", "error", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		records, err := readCustomerConsents(path)
		if err != nil {
			continue
		}
		c, ok := records[hiveID]
		if !ok || c.discardable(now) || c.WithdrawnAt != nil && c.DeliveredFields == nil {
			continue
		}
		user := loadSaaSUser(strings.TrimSuffix(entry.Name(), ".json"))
		if user == nil {
			continue
		}
		if p, err := customerConsentPath(user); err != nil || p != path {
			continue
		}
		login := customerMatchLogin(user)
		for _, key := range keys {
			if h := customerMatchHash(key, login); h != "" {
				paths[h] = path
			}
		}
	}

	st.mu.Lock()
	if st.indexes == nil {
		st.indexes = make(map[string]customerMatchIndex)
	}
	st.indexes[hiveID] = customerMatchIndex{built: now, paths: paths}
	st.mu.Unlock()
	return paths
}

type customerMatchRequest struct {
	HiveID string   `json:"hive_id"`
	Hashes []string `json:"hashes"`
}

type customerMatchEntry struct {
	MatchHash    string   `json:"match_hash"`
	Rev          string   `json:"rev,omitempty"`
	RealName     string   `json:"real_name,omitempty"`
	Organization string   `json:"organization,omitempty"`
	Country      string   `json:"country,omitempty"`
	Cleared      []string `json:"cleared,omitempty"`
}

type customerMatchResponse struct {
	Matches []customerMatchEntry `json:"matches"`
	// Truncated is set when the response size cap dropped entries; the hive
	// asks again with the remaining hashes.
	Truncated bool `json:"truncated,omitempty"`
}

// customerMatchEffective is the answer-time consent check: record present and
// not withdrawn, current disclosure text, grant still held, person not blocked
// or deleted. The hub switch is checked by the handler before anything else.
func customerMatchEffective(c customerConsent, user *SaaSUser, hiveID string) bool {
	return c.WithdrawnAt == nil && c.TextVersion == customerConsentTextVersion &&
		user != nil && !user.Blocked && user.Hives[hiveID] != ""
}

// customerMatchShareable returns the values that may cross now: consented AND
// user-entered AND non-empty. Values are read live, never from the record.
func customerMatchShareable(c customerConsent, user *SaaSUser) (customerMatchEntry, customerConsentFields) {
	var e customerMatchEntry
	var sent customerConsentFields
	if c.Fields.RealName && user.FullNameSource == countrySourceUser {
		if v := truncateRunes(strings.TrimSpace(user.FullName), maxContactNameLen); v != "" {
			e.RealName, sent.RealName = v, true
		}
	}
	if c.Fields.Organization && user.CompanySource == countrySourceUser {
		if v := truncateRunes(strings.TrimSpace(user.Company), maxContactCompanyLen); v != "" {
			e.Organization, sent.Organization = v, true
		}
	}
	if c.Fields.Country && effectiveCountrySource(user) == countrySourceUser {
		if v := normalizeCountryCode(user.Country); v != "" {
			e.Country, sent.Country = v, true
		}
	}
	return e, sent
}

func customerMatchRev(e customerMatchEntry) string {
	sum := sha256.Sum256([]byte("real_name\x00" + e.RealName + "\x00organization\x00" + e.Organization + "\x00country\x00" + e.Country))
	return hex.EncodeToString(sum[:])[:customerMatchRevLen]
}

func validCustomerMatchHash(h string) bool {
	if len(h) != customerMatchHashLen {
		return false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// pendingCustomerMatch is one answer entry plus the stamp it earns if sent.
type pendingCustomerMatch struct {
	entry customerMatchEntry
	path  string
	sent  customerConsentFields
}

// fitCustomerMatches keeps entries, in order, while the encoded response
// stays within limit bytes. It reports whether any entry was dropped.
func fitCustomerMatches(pending []pendingCustomerMatch, limit int) ([]pendingCustomerMatch, bool) {
	size := len(`{"matches":[],"truncated":true}`)
	for i, p := range pending {
		b, err := json.Marshal(p.entry)
		if err != nil {
			return pending[:i], true
		}
		size += len(b) + 1
		if size > limit {
			return pending[:i], true
		}
	}
	return pending, false
}

func (s *HubServer) handleCustomerMatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// The endpoint does not exist while the hub sharing switch is off.
	if !customersShareEnabled() {
		http.NotFound(w, r)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	presented := strings.TrimPrefix(auth, "Bearer ")

	var req customerMatchRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCustomerMatchBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid match JSON")
		return
	}
	if dec.Decode(new(json.RawMessage)) != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "invalid match JSON")
		return
	}
	hiveID := sanitizeHeartbeatField(req.HiveID)
	if hiveID == "" || !isValidName(hiveID) {
		writeJSONError(w, http.StatusBadRequest, "invalid hive_id")
		return
	}
	// The bearer is bound to the claimed hive: a hive can only ask as itself.
	if !s.verifyHeartbeatBearer(presented, hiveID) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if wait, ok := customerMatchStore.allow(hiveID, time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		writeJSONError(w, http.StatusTooManyRequests, "match rate limit exceeded")
		return
	}
	if len(req.Hashes) > maxCustomerMatchBatch {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "too many hashes in one batch")
		return
	}
	seen := make(map[string]bool, len(req.Hashes))
	hashes := make([]string, 0, len(req.Hashes))
	for _, h := range req.Hashes {
		if !validCustomerMatchHash(h) {
			writeJSONError(w, http.StatusBadRequest, "each hash must be lowercase hex SHA-256")
			return
		}
		if !seen[h] {
			seen[h] = true
			hashes = append(hashes, h)
		}
	}
	keys := s.customerMatchKeys(hiveID)
	if len(keys) == 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "matching unavailable")
		return
	}

	customerConsentMu.Lock()
	defer customerConsentMu.Unlock()
	now := time.Now().UTC()
	index := s.customerMatchIndexFor(hiveID, keys, now)

	// A non-matching hash is dropped here: never stored, never logged.
	var pending []pendingCustomerMatch
	for _, h := range hashes {
		path, ok := index[h]
		if !ok {
			continue
		}
		records, err := readCustomerConsents(path)
		if err != nil {
			continue
		}
		c, ok := records[hiveID]
		if !ok {
			continue
		}
		user := loadSaaSUser(strings.TrimSuffix(filepath.Base(path), ".json"))
		var entry customerMatchEntry
		var sent customerConsentFields
		if customerMatchEffective(c, user, hiveID) {
			entry, sent = customerMatchShareable(c, user)
		}
		// cleared: fields this hive was sent before that do not cross now.
		if d := c.DeliveredFields; d != nil {
			entry.Cleared = customerConsentFields{
				RealName:     d.RealName && !sent.RealName,
				Organization: d.Organization && !sent.Organization,
				Country:      d.Country && !sent.Country,
			}.names()
		}
		if sent == (customerConsentFields{}) && len(entry.Cleared) == 0 {
			continue // silent, like an unknown person
		}
		entry.MatchHash = h
		if sent != (customerConsentFields{}) {
			entry.Rev = customerMatchRev(entry)
		}
		pending = append(pending, pendingCustomerMatch{entry: entry, path: path, sent: sent})
	}
	pending, truncated := fitCustomerMatches(pending, maxCustomerMatchResponse)

	resp := customerMatchResponse{Matches: make([]customerMatchEntry, 0, len(pending)), Truncated: truncated}
	delivered, cleared := 0, 0
	for _, p := range pending {
		resp.Matches = append(resp.Matches, p.entry)
		if len(p.entry.Cleared) > 0 {
			cleared++
		}
		if p.sent == (customerConsentFields{}) {
			continue
		}
		delivered++
		if err := stampCustomerDelivery(p.path, hiveID, p.sent, p.entry.Rev, now); err != nil {
			s.logger.Warn("customer match: delivery stamp failed", "hive", hiveID, "error", err)
		}
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(resp); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not encode match reply")
		return
	}
	// Per request: hive id and counts only. Per delivery: the record stamp.
	s.logger.Info("audit: customer match", "hive", hiveID, "requested", len(hashes),
		"delivered", delivered, "cleared", cleared, "truncated", truncated)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf.Bytes())
}

// stampCustomerDelivery records a delivery on the consent record. The
// delivered field set is the union of every field ever sent to this hive, so
// a later withdrawal or lost provenance knows which fields a cleared reply
// must name, and the person's page can show what was sent. The caller holds
// customerConsentMu.
func stampCustomerDelivery(path, hiveID string, sent customerConsentFields, rev string, now time.Time) error {
	records, err := readCustomerConsents(path)
	if err != nil {
		return err
	}
	c, ok := records[hiveID]
	if !ok {
		return nil
	}
	if c.FirstDeliveredAt == nil {
		c.FirstDeliveredAt = &now
	}
	c.LastDeliveredAt = &now
	union := sent
	if d := c.DeliveredFields; d != nil {
		union.RealName = union.RealName || d.RealName
		union.Organization = union.Organization || d.Organization
		union.Country = union.Country || d.Country
	}
	c.DeliveredFields = &union
	last := sent
	c.LastDeliveredFields = &last
	c.DeliveredRev = rev
	records[hiveID] = c
	return saveCustomerConsents(path, records)
}
