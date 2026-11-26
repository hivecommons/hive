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
// colony-only document written. A mismatch keeps the hub in colony mode,
// with every file as it was.
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
	// crmOnlyVersion versions both attachments.
	crmOnlyVersion = 1
	// hubMigratedSuffix names the copies the migration keeps.
	hubMigratedSuffix = ".migrated"
	// hubRestoredAsideSuffix names a consents directory set aside by a
	// rollback restore.
	hubRestoredAsideSuffix = ".pre-restore"
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
}

// hubColonyOnlyDoc is the hub.colony-only store attachment.
type hubColonyOnlyDoc struct {
	Version    int       `json:"version"`
	MigratedAt time.Time `json:"migrated_at"`
	// Renamed names what the migration renamed or copied (base names).
	Renamed []string `json:"renamed,omitempty"`
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
	if !u.CRMInColony {
		return true // the JSON still holds them
	}
	rec, ok, err := crmGetRecord(st, crmPersonID(u))
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
	rec, _, err := crmGetRecord(st, id)
	if err == nil {
		rec.Fields = crmFieldsOf(u)
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
	syncErr := syncPerson(st, u)
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
		for _, f := range files {
			u, err := readHubUserForSync(f.Path)
			if err != nil {
				continue
			}
			rec, _, err := crmGetRecord(st, crmPersonID(u))
			if err != nil || len(rec.Consents) == 0 {
				continue
			}
			if p, err := customerConsentPath(u); err == nil && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
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

// copyDirFiles copies the regular files of src into a new directory dst.
func copyDirFiles(src, dst string, mode os.FileMode) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dst, hubConsentDirMode); err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name())) // #nosec G304 -- entries of the hub's own data directory
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, mode); err != nil {
			return err
		}
	}
	return nil
}

// hubColonyOnlyMigrated reports whether colony-only took over this store.
func hubColonyOnlyMigrated(st *store.Store) (bool, error) {
	_, ok, err := st.StoreAttachment(crmAttachDoc)
	return ok, err
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
		if u.CRMInColony {
			// Stripped by an earlier colony-only run that was rolled back
			// incompletely: colony holds the fields.
			have, ok, err := crmGetRecord(st, id)
			if err != nil || !ok {
				return fmt.Errorf("hub customer store: a stripped user record has no colony copy")
			}
			rec.Fields = have.Fields
		}
		wants = append(wants, want{u: u, rec: rec})
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
		return fmt.Errorf("hub customer store: %d of %d records did not read back", mismatches, len(wants))
	}
	saasDir := filepath.Dir(saasUsersDir)
	var renamed []string
	if _, err := os.Stat(saasUsersDir); err == nil {
		to, err := uniqueSibling(saasUsersDir, hubMigratedSuffix, now)
		if err != nil {
			return err
		}
		if err := copyDirFiles(saasUsersDir, to, hubUserFileMode); err != nil {
			return err
		}
		renamed = append(renamed, filepath.Base(to))
	}
	if _, err := os.Stat(customerConsentDir()); err == nil {
		to, err := uniqueSibling(customerConsentDir(), hubMigratedSuffix, now)
		if err != nil {
			return err
		}
		if err := os.Rename(customerConsentDir(), to); err != nil {
			return err
		}
		renamed = append(renamed, filepath.Base(to))
	}
	doc, err := json.Marshal(hubColonyOnlyDoc{Version: crmOnlyVersion, MigratedAt: now.UTC(), Renamed: renamed})
	if err != nil {
		return err
	}
	if err := st.SetStoreAttachment(crmAttachDoc, doc); err != nil {
		// The consents directory is renamed already: put it back, so a
		// colony-mode hub reads the files again.
		for _, name := range renamed {
			if strings.HasPrefix(name, filepath.Base(customerConsentDir())) {
				_ = os.Rename(filepath.Join(saasDir, name), customerConsentDir())
			}
		}
		return err
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
		if u.CRMInColony {
			if !ok {
				return nil, nil, errors.New("hub customer store: a stripped user record has no colony copy")
			}
			rec.Fields.applyTo(&u)
			u.CRMInColony = false
			out, err := json.MarshalIndent(&u, "", "  ")
			if err != nil {
				return nil, nil, err
			}
			data = out
		}
		users[e.Name()] = data
		if ok && len(rec.Consents) > 0 {
			out, err := json.MarshalIndent(rec.Consents, "", "  ")
			if err != nil {
				return nil, nil, err
			}
			consents[e.Name()] = out
		}
	}
	return users, consents, nil
}

// restoreHubFromColonyOnly is the automatic rollback, in every mode but
// colony-only: when the colony-only document is there, it writes the CRM
// fields back into every stripped user record and the consent files back
// from colony, then drops the document. A consents directory that exists is
// set aside first, never overwritten. On error nothing is dropped and the
// next boot tries again.
func restoreHubFromColonyOnly(st *store.Store, now time.Time) (bool, error) {
	migrated, err := hubColonyOnlyMigrated(st)
	if err != nil || !migrated {
		return false, err
	}
	users, consents, err := hubCustomerFiles(st, saasUsersDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	dir := customerConsentDir()
	if _, err := os.Stat(dir); err == nil {
		aside, err := uniqueSibling(dir, hubRestoredAsideSuffix, now)
		if err != nil {
			return false, err
		}
		if err := os.Rename(dir, aside); err != nil {
			return false, err
		}
	}
	if err := writeHubCustomerFiles(saasUsersDir, dir, users, consents); err != nil {
		return false, err
	}
	if err := st.SetStoreAttachment(crmAttachDoc, nil); err != nil {
		return false, err
	}
	slog.Info("hub customer store: restored the user json and consent files from the colony-only store",
		"users", len(users), "consents", len(consents))
	return true, nil
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
	names := make([]string, 0, len(users))
	for n := range users {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := writeFileAtomicMode(filepath.Join(usersDir, n), users[n], hubUserFileMode); err != nil {
			return err
		}
		if c, ok := consents[n]; ok {
			if err := writeFileAtomicMode(filepath.Join(consentsDir, n), c, hubConsentFileMode); err != nil {
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

// scrubHubMigrated removes a deleted user's copies from users.migrated* and
// consents.migrated*: a deletion must not leave them in a kept copy.
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
	for _, stem := range []string{filepath.Base(saasUsersDir), filepath.Base(customerConsentDir())} {
		matches, err := filepath.Glob(filepath.Join(saasDir, stem+hubMigratedSuffix+"*"))
		if err != nil {
			return err
		}
		dirs = append(dirs, matches...)
	}
	for _, d := range dirs {
		for _, base := range bases {
			if err := os.Remove(filepath.Join(d, base)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
