package hub

// The narrow-only merge of a consents directory written after the
// colony-only migration (#20115). Such a directory (an older binary, or a
// json-mode fallback, wrote it) may hold a withdrawal or a narrowed grant
// colony lacks. It is folded into colony only where that can never widen what
// colony holds:
//
//   - a withdrawal on either side wins;
//   - a grant both hold keeps only the fields both grant;
//   - a grant only the directory holds is not added (it would widen);
//   - delivery bookkeeping is kept from both (it records what reached a
//     hive, never what may).
//
// Anything else refuses with errConsentsNewer and nothing written: a file
// that is not a consent document of a known user, a record that does not
// match its key, a grant colony holds that the directory lacks (a withdrawal
// of an undelivered grant removes the record, so the absence cannot be told
// from a directory that never held it), or a delivered grant colony has no
// record of.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/colony/store"
)

// mergeNewerConsents merges the consents directory dir narrow-only into
// colony, whose records want holds as hubCustomerFiles renders them. Every
// file is classified before anything is written; the records it changes are
// written in one transaction. The caller sets dir aside only when it
// returns nil.
func mergeNewerConsents(st *store.Store, dir string, want map[string][]byte) error {
	type change struct {
		id       string
		rec      hubCRMRecord
		consents map[string]customerConsent
	}
	var changes []change
	seen := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			return errConsentsNewer
		}
		path := filepath.Join(dir, name)
		u, ok := consentPathPersonID(path)
		if !ok {
			return errConsentsNewer
		}
		id := crmPersonID(u)
		if id == "" {
			return errConsentsNewer
		}
		data, err := os.ReadFile(path) // #nosec G304 -- entries of the hub's own consents directory
		if err != nil {
			return err
		}
		found, ok := decodeConsentDocument(data)
		if !ok {
			return errConsentsNewer
		}
		rec, _, err := crmGetRecord(st, id)
		if err != nil {
			return err
		}
		merged, ok := mergeConsentsNarrow(rec.Consents, found)
		if !ok {
			return errConsentsNewer
		}
		seen[name] = true
		// The merge never adds a hive, so a person colony holds no
		// records for is never written.
		if len(rec.Consents) > 0 && !crmSameJSON(merged, rec.Consents) {
			changes = append(changes, change{id: id, rec: rec, consents: merged})
		}
	}
	// A person colony holds a grant for and the directory holds no file
	// for: the file may have been removed by a withdrawal.
	for name, data := range want {
		if seen[name] {
			continue
		}
		held, ok := decodeConsentDocument(data)
		if !ok {
			return errConsentsNewer
		}
		for _, c := range held {
			if c.WithdrawnAt == nil {
				return errConsentsNewer
			}
		}
	}
	if len(changes) == 0 {
		return nil
	}
	if err := st.Batch(func(b *store.Batch) error {
		for _, c := range changes {
			rec := c.rec
			rec.Version = crmOnlyVersion
			rec.Consents = c.consents
			raw, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if err := b.SetAttachment(c.id, crmAttachRecord, raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, c := range changes {
		if err := syncCRMConsents(st, c.id, c.consents); err != nil {
			slog.Warn("hub customer store: typed consent mirror failed after the narrow-only merge; the colony-only record is kept",
				"error", crmErrClass(err))
		}
	}
	slog.Info("hub customer store: merged a newer consents directory into colony narrow-only", "people", len(changes))
	return nil
}

// decodeConsentDocument reads one consent file strictly: a field this
// release does not know is something the merge cannot classify.
func decodeConsentDocument(data []byte) (map[string]customerConsent, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var records map[string]customerConsent
	if err := dec.Decode(&records); err != nil || records == nil || dec.More() {
		return nil, false
	}
	return records, true
}

// mergeConsentsNarrow merges one person's records from the directory (found)
// into colony's (held). It reports false for anything it cannot classify.
func mergeConsentsNarrow(held, found map[string]customerConsent) (map[string]customerConsent, bool) {
	out := make(map[string]customerConsent, len(held))
	for hive, c := range held {
		out[hive] = c
	}
	for hive, d := range found {
		if d.HiveID != hive {
			return nil, false
		}
		c, ok := held[hive]
		if !ok {
			if consentDelivered(d) {
				return nil, false
			}
			// Not added: a grant would widen, a withdrawal grants nothing.
			continue
		}
		if c.Person != d.Person || c.HiveID != d.HiveID {
			return nil, false
		}
		out[hive] = mergeConsentNarrow(c, d)
	}
	for hive, c := range held {
		if _, ok := found[hive]; !ok && c.WithdrawnAt == nil {
			return nil, false
		}
	}
	return out, true
}

func consentDelivered(c customerConsent) bool {
	return c.FirstDeliveredAt != nil || c.LastDeliveredAt != nil || c.DeliveredFields != nil ||
		c.LastDeliveredFields != nil || c.DeliveredRev != ""
}

// mergeConsentNarrow is colony's record c narrowed by the directory's d: a
// withdrawal in either wins, a grant keeps only the fields both grant, and
// colony's text version and grant time stay.
func mergeConsentNarrow(c, d customerConsent) customerConsent {
	out := c
	switch {
	case c.WithdrawnAt != nil:
		// Colony's withdrawal wins.
	case d.WithdrawnAt != nil:
		w := *d.WithdrawnAt
		out.Fields = customerConsentFields{}
		out.WithdrawnAt = &w
		if d.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = d.UpdatedAt
		}
	default:
		out.Fields = customerConsentFields{
			RealName:     c.Fields.RealName && d.Fields.RealName,
			Organization: c.Fields.Organization && d.Fields.Organization,
			Country:      c.Fields.Country && d.Fields.Country,
		}
		if out.Fields != c.Fields && d.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = d.UpdatedAt
		}
	}
	// What reached a hive is kept from both sides.
	if d.FirstDeliveredAt != nil && (out.FirstDeliveredAt == nil || d.FirstDeliveredAt.Before(*out.FirstDeliveredAt)) {
		t := *d.FirstDeliveredAt
		out.FirstDeliveredAt = &t
	}
	if d.LastDeliveredAt != nil && (out.LastDeliveredAt == nil || d.LastDeliveredAt.After(*out.LastDeliveredAt)) {
		t := *d.LastDeliveredAt
		out.LastDeliveredAt = &t
		out.LastDeliveredFields = d.LastDeliveredFields
		out.DeliveredRev = d.DeliveredRev
	}
	if d.DeliveredFields != nil {
		u := *d.DeliveredFields
		if out.DeliveredFields != nil {
			u.RealName = u.RealName || out.DeliveredFields.RealName
			u.Organization = u.Organization || out.DeliveredFields.Organization
			u.Country = u.Country || out.DeliveredFields.Country
		}
		out.DeliveredFields = &u
	}
	return out
}
