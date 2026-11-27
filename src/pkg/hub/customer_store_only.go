package hub

// HIVE_HUB_CUSTOMER_STORE=colony-only (#19972): the colony store is the only
// copy of the hub's customer data.
//
// A hub user's JSON record holds account data (hives, expiry, quota,
// tokens) and the CRM fields. In colony-only:
//
//   - the CRM fields (crmFields) and the person's consent records, delivery
//     bookkeeping included, are kept exactly in the person's "hub.crm"
//     attachment in /data/saas/colony.db, and colony's typed slots (facts,
//     identities, consents) are written beside it as in colony mode;
//   - a user's JSON is written with the CRM fields stripped and
//     crm_in_colony set, but only after the colony write succeeded; when it
//     fails, the full record is written as before, so nothing is lost;
//   - a read of a record with crm_in_colony puts the fields back from
//     colony; a record without it is read as it is (the JSON is then the
//     truth for that person, and the next save moves it);
//   - consent records are read, written, stamped and swept in colony only.
//     /data/saas/consents is renamed consents.migrated.
//
// The first colony-only boot runs the colony boot sync, which must report
// parity, copies the users directory to users.migrated, writes every
// person's attachment in one transaction, reads them all back and compares
// them with the JSON. Only then is the consents directory renamed and the
// colony-only document written; that document alone says the store is
// colony-only. A mismatch keeps the hub in colony mode, with every file as
// it was and the records it wrote cleared. A consents directory found beside
// a colony-only store is never read as the records: older than the
// migration it is set aside; newer (and different) it is merged into colony
// narrow-only and set aside, or refuses the boot when the merge cannot
// classify it (customer_store_only_merge.go).
//
// Rollback: a boot in colony, shadow or json mode that finds the
// colony-only document writes the CRM fields back into every stripped user
// record and the consent files back from colony first, then drops the
// document. hive colony export --format hive-hub-json writes the same files
// elsewhere, by hand.
//
// Nothing here changes who customer data may reach: the match and consent
// paths still gate every answer on HIVE_HUB_CUSTOMERS_TRUSTED_HIVES
// (customer_trust.go). Nothing here logs a value.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/colony/store"
)

const (
	// hubCustomerStoreColonyOnly is the colony-only mode.
	hubCustomerStoreColonyOnly = "colony-only"
	// crmAttachRecord is the per-person attachment namespace.
	crmAttachRecord = "hub.crm"
	// crmAttachDoc is the store-level marker that colony-only took over.
	crmAttachDoc = "hub.colony-only"
	// crmAttachPending notes a migration's renames before they run.
	crmAttachPending = "hub.colony-only-pending"
	// crmOnlyVersion versions both attachments.
	crmOnlyVersion = 1
	// hubMigratedSuffix names the copies the migration keeps.
	hubMigratedSuffix = ".migrated"
	// hubRestoredAsideSuffix names a consents directory set aside by a
	// rollback restore.
	hubRestoredAsideSuffix = ".pre-restore"
	// hubRestoringSuffix names the directory a restore writes the consent
	// files into before renaming it into place.
	hubRestoringSuffix = ".restoring"
	// hubIgnoredSuffix names a consents directory that reappeared beside a
	// colony-only store and was set aside.
	hubIgnoredSuffix = ".ignored"
	// hubMigratedStamp suffixes a name that is already taken.
	hubMigratedStamp = "20060102T150405Z"
	// hubUserFileMode and hubConsentFileMode match saveSaaSUser and
	// saveCustomerConsents.
	hubUserFileMode    os.FileMode = 0o644
	hubConsentFileMode os.FileMode = 0o600
	hubConsentDirMode  os.FileMode = 0o700
	hubUsersDirMode    os.FileMode = 0o755
)

// crmFields are the CRM fields of a SaaSUser: what colony-only keeps in
// colony instead of the user JSON. Everything else on the record is account
// data and stays in the JSON.
type crmFields struct {
	FullName              string `json:"full_name,omitempty"`
	FullNameSource        string `json:"full_name_source,omitempty"`
	SlackID               string `json:"slack_id,omitempty"`
	Notes                 string `json:"notes,omitempty"`
	Company               string `json:"company,omitempty"`
	CompanySource         string `json:"company_source,omitempty"`
	Country               string `json:"country,omitempty"`
	CountrySetByUser      bool   `json:"country_set_by_user,omitempty"`
	CountrySource         string `json:"country_source,omitempty"`
	Affiliation           string `json:"affiliation,omitempty"`
	AffiliationSource     string `json:"affiliation_source,omitempty"`
	AffiliationConfidence string `json:"affiliation_confidence,omitempty"`
	CNCFContributions     int    `json:"cncf_contributions,omitempty"`
	ProfileBio            string `json:"profile_bio,omitempty"`
	ProfileLocation       string `json:"profile_location,omitempty"`
	PublicActivity        string `json:"public_activity,omitempty"`
	TopRepo               string `json:"top_repo,omitempty"`
	TopRepoURL            string `json:"top_repo_url,omitempty"`
	TopRepoUpdatedAt      string `json:"top_repo_updated_at,omitempty"`
}

func crmFieldsOf(u *SaaSUser) crmFields {
	return crmFields{
		FullName: u.FullName, FullNameSource: u.FullNameSource, SlackID: u.SlackID, Notes: u.Notes,
		Company: u.Company, CompanySource: u.CompanySource, Country: u.Country, CountrySetByUser: u.CountrySetByUser,
		CountrySource: u.CountrySource, Affiliation: u.Affiliation, AffiliationSource: u.AffiliationSource,
		AffiliationConfidence: u.AffiliationConfidence, CNCFContributions: u.CNCFContributions, ProfileBio: u.ProfileBio,
		ProfileLocation: u.ProfileLocation, PublicActivity: u.PublicActivity, TopRepo: u.TopRepo, TopRepoURL: u.TopRepoURL,
		TopRepoUpdatedAt: u.TopRepoUpdatedAt,
	}
}

func (f crmFields) applyTo(u *SaaSUser) {
	u.FullName, u.FullNameSource, u.SlackID, u.Notes = f.FullName, f.FullNameSource, f.SlackID, f.Notes
	u.Company, u.CompanySource, u.Country, u.CountrySetByUser = f.Company, f.CompanySource, f.Country, f.CountrySetByUser
	u.CountrySource, u.Affiliation, u.AffiliationSource = f.CountrySource, f.Affiliation, f.AffiliationSource
	u.AffiliationConfidence, u.CNCFContributions, u.ProfileBio = f.AffiliationConfidence, f.CNCFContributions, f.ProfileBio
	u.ProfileLocation, u.PublicActivity, u.TopRepo, u.TopRepoURL = f.ProfileLocation, f.PublicActivity, f.TopRepo, f.TopRepoURL
	u.TopRepoUpdatedAt = f.TopRepoUpdatedAt
}

// hubCRMRecord is the hub.crm attachment: the person's CRM fields and
// consent records exactly as the JSON held them.
type hubCRMRecord struct {
	Version  int                        `json:"version"`
	Fields   crmFields                  `json:"fields"`
	Consents map[string]customerConsent `json:"consents,omitempty"`
	// Stripped records that a user JSON was written without these fields:
	// colony, not the JSON, then holds them. It is decided here, on the
	// colony side, so a JSON rewritten without crm_in_colony (by an image
	// that predates colony-only) is never read as an emptied record.
	Stripped bool `json:"stripped,omitempty"`
}

// fillFrom returns f with every empty field taken from held: what a stripped
// record keeps when its JSON was rewritten without crm_in_colony. A value
// the JSON holds wins.
func (f crmFields) fillFrom(held crmFields) crmFields {
	str := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}
	str(&f.FullName, held.FullName)
	str(&f.FullNameSource, held.FullNameSource)
	str(&f.SlackID, held.SlackID)
	str(&f.Notes, held.Notes)
	str(&f.Company, held.Company)
	str(&f.CompanySource, held.CompanySource)
	str(&f.Country, held.Country)
	if !f.CountrySetByUser {
		f.CountrySetByUser = held.CountrySetByUser
	}
	str(&f.CountrySource, held.CountrySource)
	str(&f.Affiliation, held.Affiliation)
	str(&f.AffiliationSource, held.AffiliationSource)
	str(&f.AffiliationConfidence, held.AffiliationConfidence)
	if f.CNCFContributions == 0 {
		f.CNCFContributions = held.CNCFContributions
	}
	str(&f.ProfileBio, held.ProfileBio)
	str(&f.ProfileLocation, held.ProfileLocation)
	str(&f.PublicActivity, held.PublicActivity)
	str(&f.TopRepo, held.TopRepo)
	str(&f.TopRepoURL, held.TopRepoURL)
	str(&f.TopRepoUpdatedAt, held.TopRepoUpdatedAt)
	return f
}

// crmFieldsFor is what u's CRM fields are, given colony's record: a record
// stripped in colony whose JSON lost crm_in_colony keeps colony's values for
// every field the JSON left empty.
func crmFieldsFor(u *SaaSUser, rec hubCRMRecord, have bool) crmFields {
	f := crmFieldsOf(u)
	if have && rec.Stripped && !u.CRMInColony {
		f = f.fillFrom(rec.Fields)
	}
	return f
}

// hubColonyOnlyDoc is the hub.colony-only store attachment.
type hubColonyOnlyDoc struct {
	Version    int       `json:"version"`
	MigratedAt time.Time `json:"migrated_at"`
	// Renamed names what the migration renamed or copied (base names).
	Renamed []string `json:"renamed,omitempty"`
}

// errCRMHeldInColony refuses a CRM change to a record whose fields colony
// holds while colony-only is not running.
var errCRMHeldInColony = errors.New("hub customer store: this record's customer fields are held in colony.db, which is not open in colony-only mode")

// crmStoreUnavailable reports a colony.db on disk that this boot did not
// open (a fallback to json): a delete must not skip forgetting the person.
func crmStoreUnavailable() bool {
	if st, _ := hubCRM.active(); st != nil {
		return false
	}
	_, err := os.Stat(hubCustomerStorePath())
	return err == nil
}

// crmColonyOnly returns the open store when the hub runs colony-only.
func crmColonyOnly() (*store.Store, bool) {
	st, mode := hubCRM.active()
	return st, st != nil && mode == hubCustomerStoreColonyOnly
}

func crmGetRecord(st *store.Store, id string) (hubCRMRecord, bool, error) {
	p, err := st.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return hubCRMRecord{}, false, nil
	}
	if err != nil {
		return hubCRMRecord{}, false, err
	}
	return crmRecordOfPerson(p)
}

func crmRecordOfPerson(p store.Person) (hubCRMRecord, bool, error) {
	raw, ok := p.Attachments[crmAttachRecord]
	if !ok {
		return hubCRMRecord{}, false, nil
	}
	var rec hubCRMRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return hubCRMRecord{}, false, fmt.Errorf("hub customer store: decoding %s: %w", crmAttachRecord, err)
	}
	if rec.Version > crmOnlyVersion {
		return hubCRMRecord{}, false, fmt.Errorf("hub customer store: %s version %d is newer than this release", crmAttachRecord, rec.Version)
	}
	return rec, true, nil
}

// crmPutRecord writes one person's attachment, creating the person with
// their sign-in identity when colony holds none yet.
func crmPutRecord(st *store.Store, u *SaaSUser, rec hubCRMRecord) error {
	id := crmPersonID(u)
	if id == "" {
		return errCRMSkipped
	}
	rec.Version = crmOnlyVersion
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := st.Get(id); errors.Is(err, store.ErrNotFound) {
		primary, ok := crmPrimaryIdentity(u)
		if !ok {
			return errCRMSkipped
		}
		if err := upsertCRMPrimary(st, id, primary); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return st.SetAttachment(id, crmAttachRecord, raw)
}

// crmOnlyOverlay is loadSaaSUser's colony-only read: a stripped record gets
// its CRM fields back from colony. It reports whether u now holds its CRM
// fields (a save may then write them).
func crmOnlyOverlay(st *store.Store, u *SaaSUser) bool {
	rec, ok, err := crmGetRecord(st, crmPersonID(u))
	if !u.CRMInColony {
		if err != nil {
			// The JSON holds them, but a stripped colony copy may hold
			// more: a save must not decide without it.
			slog.Warn("hub customer store: colony-only read failed; CRM fields read from json and left untouched by saves",
				"error", crmErrClass(err))
			return false
		}
		crmFieldsFor(u, rec, ok).applyTo(u)
		return true
	}
	if err != nil || !ok {
		slog.Warn("hub customer store: colony-only record missing; CRM fields read empty and left untouched by saves",
			"error", crmErrClass(err))
		return false
	}
	rec.Fields.applyTo(u)
	return true
}

// crmOnlySaveUser is saveSaaSUser in colony-only: colony first, then the
// JSON without the CRM fields; on a failed colony write the full record, as
// before colony-only. write is the atomic JSON write.
func crmOnlySaveUser(st *store.Store, u *SaaSUser, write func(*SaaSUser) error) error {
	hubCRM.mu.Lock()
	defer hubCRM.mu.Unlock()
	id := crmPersonID(u)
	out := *u
	if u.CRMInColony && !u.crmOverlaid {
		// The caller read the stripped file without the overlay (a sweep
		// that only changes account data): colony keeps the CRM fields as
		// they are, and the JSON stays stripped.
		return write(&out)
	}
	rec, have, err := crmGetRecord(st, id)
	if err == nil {
		rec.Fields = crmFieldsFor(u, rec, have)
		rec.Stripped = true
		err = crmPutRecord(st, u, rec)
	}
	if err != nil {
		hubCRM.markCRM(id, err)
		if !errors.Is(err, errCRMSkipped) {
			slog.Warn("hub customer store: colony-only write failed; the full record is kept in json", "error", crmErrClass(err))
		}
		out.CRMInColony = false
		return write(&out)
	}
	// The typed slots follow, as in colony mode; the attachment above is
	// what colony-only reads.
	full := *u
	rec.Fields.applyTo(&full)
	syncErr := syncPerson(st, &full)
	hubCRM.markCRM(id, syncErr)
	if syncErr != nil && !errors.Is(syncErr, errCRMSkipped) {
		slog.Warn("hub customer store: typed mirror write failed; the colony-only record is kept", "error", crmErrClass(syncErr))
	}
	crmFields{}.applyTo(&out)
	out.CRMInColony = true
	return write(&out)
}

// consentPathPersonID maps a consent file path to the user it belongs to.
func consentPathPersonID(path string) (*SaaSUser, bool) {
	stem := strings.TrimSuffix(filepath.Base(path), ".json")
	key := stem
	if id, ok := decodeUserFilename(stem); ok {
		key = id
	}
	for _, p := range saaSUserFilePaths(key) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		u, err := readHubUserForSync(p)
		if err != nil {
			return nil, false
		}
		if cp, err := customerConsentPath(u); err != nil || cp != path {
			return nil, false
		}
		return u, true
	}
	return nil, false
}

// crmOnlyReadConsents is readCustomerConsents in colony-only.
func crmOnlyReadConsents(st *store.Store, path string) (map[string]customerConsent, error) {
	records := make(map[string]customerConsent)
	u, ok := consentPathPersonID(path)
	if !ok {
		return records, nil
	}
	rec, _, err := crmGetRecord(st, crmPersonID(u))
	if err != nil {
		return nil, err
	}
	for k, v := range rec.Consents {
		records[k] = v
	}
	return records, nil
}

// crmOnlySaveConsents is saveCustomerConsents in colony-only. An empty map
// removes the records.
func crmOnlySaveConsents(st *store.Store, path string, records map[string]customerConsent) error {
	u, ok := consentPathPersonID(path)
	if !ok {
		return fmt.Errorf("hub customer store: no user for consent record: %w", fs.ErrNotExist)
	}
	hubCRM.mu.Lock()
	defer hubCRM.mu.Unlock()
	id := crmPersonID(u)
	rec, have, err := crmGetRecord(st, id)
	if err != nil {
		return err
	}
	if !have {
		if len(records) == 0 {
			// Nothing kept, so nothing to remove (a delete of a person colony
			// holds no record for must not fail).
			return nil
		}
		// A record not yet moved: its CRM fields are still in the JSON.
		full := *u
		if full.CRMInColony {
			return fmt.Errorf("hub customer store: colony-only record missing for a stripped user")
		}
		rec.Fields = crmFieldsOf(&full)
	}
	rec.Consents = nil
	if len(records) > 0 {
		rec.Consents = make(map[string]customerConsent, len(records))
		for k, v := range records {
			rec.Consents[k] = v
		}
	}
	if err := crmPutRecord(st, u, rec); err != nil {
		return err
	}
	if err := syncCRMConsents(st, id, records); err != nil {
		hubCRM.markCRM(id, err)
		slog.Warn("hub customer store: typed consent mirror failed; the colony-only record is kept", "error", crmErrClass(err))
	}
	return nil
}

// customerConsentPaths lists the consent records that exist: the files in
// /data/saas/consents, or in colony-only the users colony holds consent
// records for (at the path the file would have).
func customerConsentPaths() ([]string, error) {
	if st, ok := crmColonyOnly(); ok {
		files, err := listHubUserFiles()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var out []string
		seen := map[string]bool{}
		unreadable := 0
		for _, f := range files {
			u, err := readHubUserForSync(f.Path)
			if err == nil {
				var rec hubCRMRecord
				rec, _, err = crmGetRecord(st, crmPersonID(u))
				if err == nil && len(rec.Consents) == 0 {
					continue
				}
			}
			if err != nil {
				unreadable++
				continue
			}
			if p, err := customerConsentPath(u); err == nil && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
		if unreadable > 0 {
			slog.Warn("hub customer store: colony-only consent listing skipped unreadable records", "count", unreadable)
		}
		sort.Strings(out)
		return out, nil
	}
	dir := customerConsentDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// uniqueSibling returns path+suffix, or path+suffix-<time> when taken.
func uniqueSibling(path, suffix string, now time.Time) (string, error) {
	to := path + suffix
	if _, err := os.Stat(to); err == nil {
		to = path + suffix + "-" + now.UTC().Format(hubMigratedStamp)
		if _, err := os.Stat(to); err == nil {
			return "", fmt.Errorf("hub customer store: %s already exists", filepath.Base(to))
		}
	}
	return to, nil
}

// copyDirFiles copies the regular files of src into a new directory dst. It
// reports whether it created dst (then dst is its own to remove on error).
func copyDirFiles(src, dst string, mode os.FileMode) (bool, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return false, err
	}
	if err := os.Mkdir(dst, hubConsentDirMode); err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name())) // #nosec G304 -- entries of the hub's own data directory
		if err != nil {
			return true, err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, mode); err != nil { // #nosec G703 -- dst is the migration's own new directory beside the hub data dir, and the name is a directory entry of the hub's users directory
			return true, err
		}
	}
	return true, nil
}

// hubColonyOnlyMigrated reports whether colony-only took over this store:
// only the document written after the renames says so, and a restore drops
// it.
func hubColonyOnlyMigrated(st *store.Store) (bool, error) {
	_, ok, err := hubColonyOnlyDocOf(st)
	return ok, err
}

// hubColonyOnlyDocOf reads the colony-only document.
func hubColonyOnlyDocOf(st *store.Store) (hubColonyOnlyDoc, bool, error) {
	raw, ok, err := st.StoreAttachment(crmAttachDoc)
	if err != nil || !ok {
		return hubColonyOnlyDoc{}, false, err
	}
	var doc hubColonyOnlyDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return hubColonyOnlyDoc{}, true, fmt.Errorf("hub customer store: decoding %s: %w", crmAttachDoc, err)
	}
	if doc.Version > crmOnlyVersion {
		return hubColonyOnlyDoc{}, true, fmt.Errorf("hub customer store: %s version %d is newer than this release", crmAttachDoc, doc.Version)
	}
	return doc, true, nil
}

// hubColonyOnlyPending is the hub.colony-only-pending store attachment: the
// names a migration is about to copy and rename to, written before it
// touches a file, so a boot after a crash part-way through can put the
// consents directory back (recoverHubColonyOnlyPending).
type hubColonyOnlyPending struct {
	Version    int    `json:"version"`
	UsersTo    string `json:"users_to,omitempty"`
	ConsentsTo string `json:"consents_to,omitempty"`
}

// recoverHubColonyOnlyPending finishes what a crashed migration left: with
// the colony-only document written the migration is done and only the
// pending note goes; without it, a consents directory the migration renamed
// is renamed back, so no boot ever reads an empty consents directory as the
// records. The users copy is a copy and is kept. It runs on every boot that
// opens colony.db, before anything reads the files.
func recoverHubColonyOnlyPending(st *store.Store) error {
	raw, ok, err := st.StoreAttachment(crmAttachPending)
	if err != nil || !ok {
		return err
	}
	migrated, err := hubColonyOnlyMigrated(st)
	if err != nil {
		return err
	}
	if !migrated {
		var p hubColonyOnlyPending
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("hub customer store: decoding %s: %w", crmAttachPending, err)
		}
		if p.ConsentsTo != "" {
			from := filepath.Join(filepath.Dir(saasUsersDir), filepath.Base(p.ConsentsTo))
			_, fromErr := os.Stat(from)
			_, dirErr := os.Stat(customerConsentDir())
			switch {
			case fromErr == nil && errors.Is(dirErr, os.ErrNotExist):
				if err := os.Rename(from, customerConsentDir()); err != nil {
					return err
				}
				slog.Warn("hub customer store: an interrupted colony-only migration was undone; the consents directory is back in place")
			case fromErr == nil && dirErr == nil:
				// Both exist: neither can be chosen safely, so nothing is.
				return fmt.Errorf("hub customer store: an interrupted colony-only migration left both %s and %s; move one aside",
					filepath.Base(from), filepath.Base(customerConsentDir()))
			case fromErr != nil && !errors.Is(fromErr, os.ErrNotExist):
				return fromErr
			}
		}
	}
	return st.SetStoreAttachment(crmAttachPending, nil)
}

// hubMigrateRename renames the consents directory in a migration; a test
// replaces it to fail the step after the users copy.
var hubMigrateRename = os.Rename

// crmSnapshotRecords returns each person's hub.crm attachment as it is now
// (nil where there is none), so a migration that fails can put them back.
func crmSnapshotRecords(st *store.Store, ids []string) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(ids))
	for _, id := range ids {
		p, err := st.Get(id)
		if errors.Is(err, store.ErrNotFound) {
			out[id] = nil
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = p.Attachments[crmAttachRecord]
	}
	return out, nil
}

// crmPutBackRecords restores the attachments crmSnapshotRecords saw, in one
// transaction.
func crmPutBackRecords(st *store.Store, before map[string]json.RawMessage) error {
	return st.Batch(func(b *store.Batch) error {
		for id, raw := range before {
			_ = b.SetAttachment(id, crmAttachRecord, raw)
		}
		return nil
	})
}

// migrateHubColonyOnly is the first colony-only boot, after a colony boot
// sync with parity. It returns an error, and changes no file, unless every
// person's record reads back exactly.
func migrateHubColonyOnly(st *store.Store, now time.Time) error {
	files, err := listHubUserFiles()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	type want struct {
		u   *SaaSUser
		rec hubCRMRecord
	}
	var wants []want
	seen := map[string]bool{}
	for _, f := range files {
		u, err := readHubUserForSync(f.Path)
		if err != nil {
			return fmt.Errorf("hub customer store: unreadable user record: %w", err)
		}
		id := crmPersonID(u)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		rec := hubCRMRecord{Version: crmOnlyVersion, Fields: crmFieldsOf(u)}
		if path, err := customerConsentPath(u); err == nil {
			records, err := readCustomerConsents(path)
			if err != nil {
				return err
			}
			if len(records) > 0 {
				rec.Consents = records
			}
		}
		have, ok, err := crmGetRecord(st, id)
		if err != nil {
			return err
		}
		if u.CRMInColony {
			// Stripped by an earlier colony-only run that was rolled back
			// incompletely: colony holds the fields.
			if !ok {
				return fmt.Errorf("hub customer store: a stripped user record has no colony copy")
			}
			rec.Fields = have.Fields
		} else {
			rec.Fields = crmFieldsFor(u, have, ok)
		}
		wants = append(wants, want{u: u, rec: rec})
	}
	ids := make([]string, 0, len(wants))
	for _, w := range wants {
		ids = append(ids, crmPersonID(w.u))
	}
	before, err := crmSnapshotRecords(st, ids)
	if err != nil {
		return err
	}
	// undo clears what this migration wrote to colony: a migration that
	// does not finish leaves the store as it found it.
	undo := func(cause error) error {
		if err := crmPutBackRecords(st, before); err != nil {
			return fmt.Errorf("%w (and the records it wrote were not cleared: %v)", cause, crmErrClass(err))
		}
		return cause
	}
	// One transaction for every attachment.
	err = st.Batch(func(b *store.Batch) error {
		for _, w := range wants {
			raw, err := json.Marshal(w.rec)
			if err != nil {
				return err
			}
			if err := b.SetAttachment(crmPersonID(w.u), crmAttachRecord, raw); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	mismatches := 0
	for _, w := range wants {
		got, ok, err := crmGetRecord(st, crmPersonID(w.u))
		if err != nil || !ok || !crmSameJSON(got, w.rec) {
			mismatches++
		}
	}
	if mismatches > 0 {
		return undo(fmt.Errorf("hub customer store: %d of %d records did not read back", mismatches, len(wants)))
	}
	var pending hubColonyOnlyPending
	pending.Version = crmOnlyVersion
	_, usersErr := os.Stat(saasUsersDir)
	if usersErr == nil {
		if pending.UsersTo, err = uniqueSibling(saasUsersDir, hubMigratedSuffix, now); err != nil {
			return undo(err)
		}
	}
	_, consentsErr := os.Stat(customerConsentDir())
	if consentsErr == nil {
		if pending.ConsentsTo, err = uniqueSibling(customerConsentDir(), hubMigratedSuffix, now); err != nil {
			return undo(err)
		}
	}
	// Noted before any file moves, so a crash part-way is undone on the
	// next boot (recoverHubColonyOnlyPending).
	note, err := json.Marshal(pending)
	if err != nil {
		return undo(err)
	}
	if err := st.SetStoreAttachment(crmAttachPending, note); err != nil {
		return undo(err)
	}
	// fail undoes the file steps that ran and clears what was written.
	usersCopied, consentsRenamed := false, false
	fail := func(cause error) error {
		if consentsRenamed {
			if err := os.Rename(pending.ConsentsTo, customerConsentDir()); err != nil {
				// The pending note stays, so the next boot puts it back.
				return fmt.Errorf("%w (and the consents directory was not put back: %v)", cause, crmErrClass(err))
			}
		}
		if usersCopied {
			// The copy this migration just made, and nothing else.
			_ = os.RemoveAll(pending.UsersTo)
		}
		_ = st.SetStoreAttachment(crmAttachPending, nil)
		return undo(cause)
	}
	var renamed []string
	if pending.UsersTo != "" {
		created, err := copyDirFiles(saasUsersDir, pending.UsersTo, hubUserFileMode)
		usersCopied = created
		if err != nil {
			return fail(err)
		}
		renamed = append(renamed, filepath.Base(pending.UsersTo))
	}
	if pending.ConsentsTo != "" {
		if err := hubMigrateRename(customerConsentDir(), pending.ConsentsTo); err != nil {
			return fail(err)
		}
		consentsRenamed = true
		renamed = append(renamed, filepath.Base(pending.ConsentsTo))
	}
	doc, err := json.Marshal(hubColonyOnlyDoc{Version: crmOnlyVersion, MigratedAt: now.UTC(), Renamed: renamed})
	if err != nil {
		return fail(err)
	}
	// The document and the end of the pending note in one transaction:
	// this is the moment colony-only takes over.
	if err := st.Batch(func(b *store.Batch) error {
		_ = b.SetStoreAttachment(crmAttachDoc, doc)
		_ = b.SetStoreAttachment(crmAttachPending, nil)
		return nil
	}); err != nil {
		return fail(err)
	}
	// Retention for the kept copies runs from now (pruneHubKeptCopies).
	for _, kept := range []string{pending.UsersTo, pending.ConsentsTo} {
		if kept != "" {
			markHubKeptCopy(kept, now)
		}
	}
	slog.Info("hub customer store: colony-only took over; consent files renamed, CRM fields leave the user json on the next save",
		"people", len(wants), "renamed", len(renamed))
	return nil
}

func crmSameJSON(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// hubCustomerFiles renders what a colony-only store stands for, for each
// user record in usersDir: the full user JSON, and its consent file (nil
// when there is none). It never writes.
func hubCustomerFiles(st *store.Store, usersDir string) (map[string][]byte, map[string][]byte, error) {
	entries, err := os.ReadDir(usersDir)
	if err != nil {
		return nil, nil, err
	}
	users := map[string][]byte{}
	consents := map[string][]byte{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(usersDir, e.Name())) // #nosec G304 -- entries of the hub's own users directory
		if err != nil {
			return nil, nil, err
		}
		var u SaaSUser
		if err := json.Unmarshal(data, &u); err != nil {
			return nil, nil, fmt.Errorf("hub customer store: unreadable user record %d: %w", len(users), err)
		}
		rec, ok, err := crmGetRecord(st, crmPersonID(&u))
		if err != nil {
			return nil, nil, err
		}
		rewrite := true
		switch {
		case u.CRMInColony && !ok:
			// No colony copy (a delete whose last step failed, or a lost
			// record): nothing holds the fields, so the record is written
			// as what it is, an account without them. users.migrated may
			// still hold an older copy.
			slog.Warn("hub customer store: a stripped user record has no colony copy; restored without CRM fields (see users.migrated)")
			u.CRMInColony = false
		case u.CRMInColony:
			rec.Fields.applyTo(&u)
			u.CRMInColony = false
		case ok && rec.Stripped:
			// Rewritten without crm_in_colony: colony fills what it lacks.
			crmFieldsFor(&u, rec, ok).applyTo(&u)
		default:
			rewrite = false
		}
		users[e.Name()] = data
		if rewrite {
			out, err := json.MarshalIndent(&u, "", "  ")
			if err != nil {
				return nil, nil, err
			}
			users[e.Name()] = out
		}
		if ok && len(rec.Consents) > 0 {
			out, err := json.MarshalIndent(rec.Consents, "", "  ")
			if err != nil {
				return nil, nil, err
			}
			// Named as json mode reads it (customerConsentPath).
			name := e.Name()
			if p, err := customerConsentPath(&u); err == nil {
				name = filepath.Base(p)
			}
			consents[name] = out
		}
	}
	return users, consents, nil
}

// restoreHubFromColonyOnly is the automatic rollback, in every mode but
// colony-only: when the colony-only document is there, it writes the CRM
// fields back into every stripped user record and the consent files back
// from colony, then drops the document and the attachments of the records
// it wrote back, in one transaction. A consents directory that exists is
// handled first, before anything is written (checkReappearedConsents): one
// that matches colony is kept, an older one is set aside, never
// overwritten, and a newer one is merged into colony narrow-only first and
// then set aside, or refuses the restore when it cannot be. On error the
// document stays and the next boot tries again.
func restoreHubFromColonyOnly(st *store.Store, now time.Time) (bool, error) {
	doc, migrated, err := hubColonyOnlyDocOf(st)
	if err != nil || !migrated {
		return false, err
	}
	users, consents, err := hubCustomerFiles(st, saasUsersDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	dir := customerConsentDir()
	state, err := checkReappearedConsents(dir, consents, doc.MigratedAt)
	if err != nil {
		return false, err
	}
	if state == consentsNewer {
		// Merged narrow-only into colony first (errConsentsNewer when it
		// cannot be), then rendered again and set aside as an older one.
		if err := mergeNewerConsents(st, dir, consents); err != nil {
			return false, err
		}
		if users, consents, err = hubCustomerFiles(st, saasUsersDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		state = consentsOlder
	}
	// The consent files are written into consents.restoring and renamed
	// into place whole, so a crash part-way never leaves a partial
	// consents directory that a later boot would have to refuse. A
	// leftover consents.restoring is this restore's own unfinished copy of
	// what colony holds.
	staging := dir + hubRestoringSuffix
	if err := os.RemoveAll(staging); err != nil {
		return false, err
	}
	if err := writeHubCustomerFiles(saasUsersDir, staging, users, consents); err != nil {
		return false, err
	}
	if state != consentsAbsent {
		// Older, or the same as colony: set aside, never overwritten.
		aside, err := uniqueSibling(dir, hubRestoredAsideSuffix, now)
		if err != nil {
			return false, err
		}
		if err := os.Rename(dir, aside); err != nil {
			return false, err
		}
		markHubKeptCopy(aside, now)
	}
	if len(consents) > 0 {
		if err := os.Rename(staging, dir); err != nil {
			return false, err
		}
	}
	ids := make([]string, 0, len(users))
	for name, data := range users {
		var u SaaSUser
		if json.Unmarshal(data, &u) == nil {
			if id := crmPersonID(&u); id != "" {
				ids = append(ids, id)
			}
		} else {
			return false, fmt.Errorf("hub customer store: re-reading a restored record %s failed", name)
		}
	}
	if err := st.Batch(func(b *store.Batch) error {
		// The JSON holds these records again; the copies go with the
		// document, so no stale copy outlives the rollback.
		for _, id := range ids {
			_ = b.SetAttachment(id, crmAttachRecord, nil)
		}
		_ = b.SetStoreAttachment(crmAttachDoc, nil)
		return nil
	}); err != nil {
		return false, err
	}
	slog.Info("hub customer store: restored the user json and consent files from the colony-only store",
		"users", len(users), "consents", len(consents))
	return true, nil
}

// errConsentsNewer refuses a boot: a consents directory written after the
// colony-only migration (by an older binary, or a json-mode fallback) holds
// changes colony lacks that the narrow-only merge (mergeNewerConsents)
// cannot classify. Neither copy can be chosen safely, so neither is, and
// nothing is written.
var errConsentsNewer = errors.New("hub customer store: /data/saas/consents is newer than the colony-only migration and holds changes the narrow-only merge cannot classify; " +
	"move it aside to keep colony.db (apply any withdrawal or narrowed grant from it by hand), then boot again")

// Reappeared consents directory states.
const (
	consentsAbsent = iota
	consentsSame
	consentsOlder
	consentsNewer
)

// checkReappearedConsents compares a consents directory beside a
// colony-only store with what colony holds (want, as hubCustomerFiles
// renders it): absent, the same files and bytes, or different and last
// changed before or after the migration.
func checkReappearedConsents(dir string, want map[string][]byte, migratedAt time.Time) (int, error) {
	fi, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return consentsAbsent, nil
	}
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	newest := fi.ModTime()
	same := true
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return 0, err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") {
			same = false
			continue
		}
		w, ok := want[e.Name()]
		if !ok {
			same = false
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- entries of the hub's own consents directory
		if err != nil {
			return 0, err
		}
		if string(data) != string(w) {
			same = false
		}
	}
	switch {
	case same:
		// Every file it holds is colony's own, byte for byte (an export
		// copied into place, or part of one): it says nothing colony lacks.
		return consentsSame, nil
	case newest.After(migratedAt):
		return consentsNewer, nil
	default:
		return consentsOlder, nil
	}
}

// writeHubCustomerFiles writes rendered user and consent files atomically.
func writeHubCustomerFiles(usersDir, consentsDir string, users, consents map[string][]byte) error {
	if err := os.MkdirAll(usersDir, hubUsersDirMode); err != nil {
		return err
	}
	if len(consents) > 0 {
		if err := os.MkdirAll(consentsDir, hubConsentDirMode); err != nil {
			return err
		}
	}
	for _, set := range []struct {
		dir   string
		files map[string][]byte
		mode  os.FileMode
	}{{usersDir, users, hubUserFileMode}, {consentsDir, consents, hubConsentFileMode}} {
		names := make([]string, 0, len(set.files))
		for n := range set.files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if err := writeFileAtomicMode(filepath.Join(set.dir, n), set.files[n], set.mode); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeFileAtomicMode(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Chmod(mode)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
	}
	return werr
}

// HubCustomerExportReport counts what ExportHubCustomerStore wrote.
type HubCustomerExportReport struct {
	Users    int    `json:"users"`
	Consents int    `json:"consents"`
	UsersDir string `json:"users_dir"`
	Consent  string `json:"consents_dir"`
}

// ExportHubCustomerStore writes the hub's user records with their CRM fields
// and the consent files, in the shape the json mode reads, from the
// colony-only store in saasDir (/data/saas) into outDir/users and
// outDir/consents. It is the manual rollback (hive colony export --format
// hive-hub-json). The hub must be stopped: it holds colony.db's lock. A file
// that exists is never overwritten unless force is set.
func ExportHubCustomerStore(saasDir, outDir string, force bool) (HubCustomerExportReport, error) {
	var rep HubCustomerExportReport
	if absClean(saasDir) == absClean(outDir) {
		// Writing over the live files would leave the colony-only marker
		// behind them; the restore on the next boot does that in place.
		return rep, errors.New("hub customer store: export into a directory other than the hub data directory")
	}
	path := filepath.Join(saasDir, store.DefaultFileName)
	if _, err := os.Stat(path); err != nil {
		// Never create an empty colony.db by exporting from the wrong place.
		return rep, fmt.Errorf("hub customer store: colony.db: %w", err)
	}
	st, err := store.Open(path, store.Options{LockTimeout: hubCustomerStoreLockTimeout})
	if err != nil {
		return rep, fmt.Errorf("hub customer store: opening colony.db (stop the hub first): %w", err)
	}
	defer func() { _ = st.Close() }()
	migrated, err := hubColonyOnlyMigrated(st)
	if err != nil {
		return rep, err
	}
	if !migrated {
		return rep, errors.New("hub customer store: colony.db was not written in colony-only mode")
	}
	users, consents, err := hubCustomerFiles(st, filepath.Join(saasDir, filepath.Base(saasUsersDir)))
	if err != nil {
		return rep, err
	}
	rep.UsersDir = filepath.Join(outDir, filepath.Base(saasUsersDir))
	rep.Consent = filepath.Join(outDir, filepath.Base(customerConsentDir()))
	if !force {
		for n := range users {
			if _, err := os.Stat(filepath.Join(rep.UsersDir, n)); err == nil {
				return rep, fmt.Errorf("hub customer store: %s exists; move it aside or pass --force", filepath.Join(filepath.Base(rep.UsersDir), n))
			}
		}
		for n := range consents {
			if _, err := os.Stat(filepath.Join(rep.Consent, n)); err == nil {
				return rep, fmt.Errorf("hub customer store: %s exists; move it aside or pass --force", filepath.Join(filepath.Base(rep.Consent), n))
			}
		}
	}
	if err := writeHubCustomerFiles(rep.UsersDir, rep.Consent, users, consents); err != nil {
		return rep, err
	}
	rep.Users, rep.Consents = len(users), len(consents)
	return rep, nil
}

func absClean(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
}

// scrubHubMigrated removes a deleted user's copies from users.migrated*,
// consents.migrated*, consents.pre-restore* and consents.ignored*: a
// deletion must not leave them in a kept copy.
func scrubHubMigrated(u *SaaSUser) error {
	path, err := saveSaaSUserPath(u)
	if err != nil {
		return nil
	}
	bases := []string{filepath.Base(path)}
	for _, p := range saaSUserFilePaths(userCanonicalID(u)) {
		bases = append(bases, filepath.Base(p))
	}
	saasDir := filepath.Dir(saasUsersDir)
	var dirs []string
	for _, k := range hubKeptCopies() {
		matches, err := filepath.Glob(filepath.Join(saasDir, k.pattern))
		if err != nil {
			return err
		}
		dirs = append(dirs, matches...)
	}
	for _, d := range dirs {
		// A removal must not restart the copy's retention clock
		// (pruneHubKeptCopies reads it from the directory's mtime).
		fi, statErr := os.Stat(d)
		removed := false
		for _, base := range bases {
			err := os.Remove(filepath.Join(d, base))
			if err == nil {
				removed = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if removed && statErr == nil {
			_ = os.Chtimes(d, fi.ModTime(), fi.ModTime())
		}
	}
	return nil
}

// setAsideReappearedConsents handles a consents directory beside a
// colony-only store, which is never read as the records: one older than the
// migration (a stale copy put back) or matching colony is set aside, never
// deleted; a newer one that differs is merged into colony narrow-only and
// then set aside, or refuses colony-only (errConsentsNewer) with nothing
// moved or written. It reports whether one was set aside.
func setAsideReappearedConsents(st *store.Store, now time.Time) (bool, error) {
	dir := customerConsentDir()
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	doc, _, err := hubColonyOnlyDocOf(st)
	if err != nil {
		return false, err
	}
	_, consents, err := hubCustomerFiles(st, saasUsersDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	state, err := checkReappearedConsents(dir, consents, doc.MigratedAt)
	if err != nil {
		return false, err
	}
	if state == consentsNewer {
		// Merged narrow-only into colony, then set aside like any other.
		if err := mergeNewerConsents(st, dir, consents); err != nil {
			return false, err
		}
	}
	to, err := uniqueSibling(dir, hubIgnoredSuffix, now)
	if err != nil {
		return false, err
	}
	if err := os.Rename(dir, to); err != nil {
		return false, err
	}
	markHubKeptCopy(to, now)
	return true, nil
}
