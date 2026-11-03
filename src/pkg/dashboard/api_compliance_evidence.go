package dashboard

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/compliance"
)

// Settings → Compliance evidence and attestations (hivecommons/hive#11081).

// complianceAttestationsPath persists owner attestations on the /data PVC.
// The audit log keeps a copy of each, but it rotates; this store keeps them
// for the attestation panel. A var so tests point it at a temp dir.
var complianceAttestationsPath = "/data/compliance-attestations.jsonl"

// complianceAuditLogPath is the audit log the audit-slice export reads; ""
// means the production path. A var so tests use a fixture.
var complianceAuditLogPath = ""

// complianceEvidenceNow is the export clock; tests pin it.
var complianceEvidenceNow = time.Now

// complianceDefaultExportWindow is the export range when since is omitted.
const complianceDefaultExportWindow = 30 * 24 * time.Hour

// complianceAttestationBodyLimit bounds a POST /api/compliance/attestations body.
const complianceAttestationBodyLimit = 16 * 1024

// complianceExportFormats lists the formats each export kind supports; the
// first is the default.
var complianceExportFormats = map[string][]string{
	"controls":     {"json", "md"},
	"posture":      {"json", "csv"},
	"audit":        {"json"},
	"config":       {"json"},
	"attestations": {"json", "csv"},
	"bundle":       {"json"},
}

// complianceExportKinds is the stable order kinds are listed in errors.
var complianceExportKinds = []string{"controls", "posture", "audit", "config", "attestations", "bundle"}

// attestationStore lazily opens the attestation store.
func (s *Server) attestationStore() *compliance.AttestationStore {
	s.attestOnce.Do(func() {
		st, err := compliance.NewAttestationStore(complianceAttestationsPath)
		if err != nil && s.logger != nil {
			s.logger.Warn("compliance attestations unreadable; starting empty", "path", complianceAttestationsPath, "error", err)
		}
		s.attestations = st
	})
	return s.attestations
}

// parseComplianceUntil accepts an RFC 3339 time or a YYYY-MM-DD date; a date
// means the end of that day (the bound is exclusive).
func parseComplianceUntil(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.Add(24 * time.Hour), nil
	}
	return time.Time{}, errors.New("until must be an RFC 3339 time or a YYYY-MM-DD date")
}

// complianceRange parses ?since= and ?until= into [since, until). since
// defaults to complianceDefaultExportWindow before until; until to now.
func complianceRange(r *http.Request, now time.Time) (time.Time, time.Time, error) {
	q := r.URL.Query()
	until := now
	if v := q.Get("until"); v != "" {
		t, err := parseComplianceUntil(v)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		until = t
	}
	since := until.Add(-complianceDefaultExportWindow)
	if v := q.Get("since"); v != "" {
		t, err := parsePostureSince(v, now)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		since = t
	}
	if !since.Before(until) {
		return time.Time{}, time.Time{}, errors.New("since must be before until")
	}
	return since.UTC(), until.UTC(), nil
}

// compliancePostureHistoryResponse is the GET /api/compliance/posture/history body.
type compliancePostureHistoryResponse struct {
	Disclaimer string                     `json:"disclaimer"`
	Since      time.Time                  `json:"since"`
	Until      time.Time                  `json:"until"`
	Runs       int                        `json:"runs"`
	Truncated  bool                       `json:"truncated,omitempty"`
	Checks     []compliance.PostureSeries `json:"checks"`
}

// handleCompliancePostureHistory serves GET /api/compliance/posture/history:
// the posture history in [since, until) as one time series per check (the
// Compliance tab's sparklines). Owner only.
func (s *Server) handleCompliancePostureHistory(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	since, until, err := complianceRange(r, complianceEvidenceNow())
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	runs, truncated := s.postureRunner().History().Between(since, until, compliancePostureHistoryLimit)
	jsonResponse(w, compliancePostureHistoryResponse{
		Disclaimer: compliance.Disclaimer,
		Since:      since,
		Until:      until,
		Runs:       len(runs),
		Truncated:  truncated,
		Checks:     compliance.BuildPostureSeries(runs),
	})
}

// complianceAttestationsResponse is the GET /api/compliance/attestations body.
type complianceAttestationsResponse struct {
	Disclaimer   string                   `json:"disclaimer"`
	Attestations []compliance.Attestation `json:"attestations"`
}

// handleComplianceAttestations serves GET /api/compliance/attestations
// (?framework= filters): every recorded attestation, newest first. Owner only.
func (s *Server) handleComplianceAttestations(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	jsonResponse(w, complianceAttestationsResponse{
		Disclaimer:   compliance.Disclaimer,
		Attestations: s.attestationStore().List(r.URL.Query().Get("framework"), time.Time{}, time.Time{}),
	})
}

// complianceAttestationRequest is the POST /api/compliance/attestations body.
type complianceAttestationRequest struct {
	Framework  string `json:"framework"`
	ReviewedOn string `json:"reviewed_on"`
	Note       string `json:"note"`
}

// handleComplianceAttestationCreate serves POST /api/compliance/attestations:
// the signed-in owner records "reviewed <framework> on <date>" with a note.
// The attestation is written to the audit log (compliance_attestation) and
// to the attestation store. Owner only.
func (s *Server) handleComplianceAttestationCreate(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var req complianceAttestationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, complianceAttestationBodyLimit)).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	a, err := compliance.NormalizeAttestation(compliance.Attestation{
		Framework:  req.Framework,
		ReviewedOn: req.ReviewedOn,
		By:         requestUser(r),
		Note:       req.Note,
	}, complianceEvidenceNow())
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditFromRequest(r, "compliance_attestation", auditDetail(
		"framework", a.Framework,
		"reviewed_on", a.ReviewedOn,
		"note", a.Note,
	), "")
	if err := s.attestationStore().Add(a); err != nil && s.logger != nil {
		s.logger.Warn("compliance attestation write failed", "error", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(a)
}

// complianceConfigSnapshot is the redacted effective config and the sha256
// of exactly the bytes embedded as "effective".
type complianceConfigSnapshot struct {
	SHA256    string          `json:"sha256"`
	Effective json.RawMessage `json:"effective"`
}

func (s *Server) complianceConfigSnapshot() (complianceConfigSnapshot, error) {
	v, err := configStructToExportValue(s.deps.Config)
	if err != nil {
		return complianceConfigSnapshot{}, fmt.Errorf("marshaling effective config: %w", err)
	}
	raw, err := json.Marshal(redactConfigValue(v, ""))
	if err != nil {
		return complianceConfigSnapshot{}, fmt.Errorf("encoding effective config: %w", err)
	}
	sum := sha256.Sum256(raw)
	return complianceConfigSnapshot{SHA256: hex.EncodeToString(sum[:]), Effective: raw}, nil
}

// complianceAuditSlice returns audit entries in [since, until), from the
// on-disk log (current file and rotated backups) or, with no log on disk,
// the in-memory ring.
func (s *Server) complianceAuditSlice(since, until time.Time) []AuditEntry {
	var entries []AuditEntry
	if s.audit.HasOnDiskLog(complianceAuditLogPath) {
		entries = s.audit.OutputActionsSince(since, nil, complianceAuditLogPath)
	} else {
		entries = s.audit.RecentWithPrefixSince(since, "")
	}
	out := []AuditEntry{}
	for _, e := range entries {
		if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil && t.Before(until) {
			out = append(out, e)
		}
	}
	return out
}

// complianceExportMeta records the provenance, coverage period and source
// retention needed to interpret every compliance export.
type complianceExportMeta struct {
	Kind         string                     `json:"kind"`
	Disclaimer   string                     `json:"disclaimer"`
	HiveID       string                     `json:"hive_id"`
	SourceSystem string                     `json:"source_system"`
	GeneratedBy  string                     `json:"generated_by"`
	GeneratedAt  time.Time                  `json:"generated_at"`
	Since        time.Time                  `json:"since"`
	Until        time.Time                  `json:"until"`
	Profiles     []complianceProfileVersion `json:"profiles"`
	Attesters    []string                   `json:"attesters"`
	Retention    []string                   `json:"retention"`
}

type complianceProfileVersion struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type complianceExportManifestEntry struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
	Bytes       int    `json:"bytes"`
}

type complianceExportManifest struct {
	Algorithm string                          `json:"algorithm"`
	Entries   []complianceExportManifestEntry `json:"entries"`
}

func newComplianceManifestEntry(name, contentType string, payload []byte) complianceExportManifestEntry {
	sum := sha256.Sum256(payload)
	return complianceExportManifestEntry{
		Name:        name,
		ContentType: contentType,
		SHA256:      hex.EncodeToString(sum[:]),
		Bytes:       len(payload),
	}
}

// complianceJSON encodes value without HTML escaping so a verifier's
// compact re-encoding (for example `jq -c`) reproduces the manifest digest
// even when a note or audit detail contains <, > or &. indent "" is compact.
func complianceJSON(value any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func complianceManifestForJSON(payload map[string]any) (complianceExportManifest, error) {
	manifest := complianceExportManifest{Algorithm: "sha256", Entries: []complianceExportManifestEntry{}}
	for _, name := range []string{"meta", "controls", "posture", "audit", "config", "attestations"} {
		value, ok := payload[name]
		if !ok {
			continue
		}
		raw, err := complianceJSON(value, "")
		if err != nil {
			return complianceExportManifest{}, fmt.Errorf("encoding %s manifest entry: %w", name, err)
		}
		manifest.Entries = append(manifest.Entries, newComplianceManifestEntry(name, "application/json", raw))
	}
	return manifest, nil
}

func (s *Server) newComplianceExportMeta(kind, generatedBy string, generatedAt, since, until time.Time, attestations []compliance.Attestation) complianceExportMeta {
	cfg := s.deps.Config
	profiles := []complianceProfileVersion{}
	for _, id := range cfg.Compliance.SelectedFrameworks() {
		if profile, ok := compliance.ProfileByID(id); ok {
			profiles = append(profiles, complianceProfileVersion{ID: id, Version: profile.Version})
		}
	}
	attesters := []string{}
	seenAttester := map[string]bool{}
	for _, attestation := range attestations {
		if attestation.By != "" && !seenAttester[attestation.By] {
			seenAttester[attestation.By] = true
			attesters = append(attesters, attestation.By)
		}
	}
	retention := []string{"Generated export files are not retained by Hive."}
	if kind == "controls" || kind == "config" || kind == "bundle" {
		retention = append(retention, "Controls and config are point-in-time snapshots.")
	}
	if kind == "posture" || kind == "bundle" {
		retention = append(retention, fmt.Sprintf(
			"Posture history is retained for %d days or %d runs, whichever limit is reached first.",
			int(cfg.Compliance.PostureHistoryRetentionOrDefault()/(24*time.Hour)), compliance.MaxPostureHistoryRuns,
		))
	}
	if kind == "audit" || kind == "bundle" {
		retention = append(retention, fmt.Sprintf(
			"Audit entries are retained for up to %d days with size-based rotation; audit.covered_from reports actual coverage.",
			compliance.BuiltinAuditRetentionDays,
		))
	}
	if kind == "attestations" || kind == "bundle" {
		retention = append(retention, fmt.Sprintf("The newest %d attestations are retained.", compliance.MaxAttestations))
	}
	return complianceExportMeta{
		Kind:         kind,
		Disclaimer:   compliance.Disclaimer,
		HiveID:       cfg.HiveID,
		SourceSystem: "Hive",
		GeneratedBy:  generatedBy,
		GeneratedAt:  generatedAt,
		Since:        since,
		Until:        until,
		Profiles:     profiles,
		Attesters:    attesters,
		Retention:    retention,
	}
}

func complianceExportCSV(kind string, meta complianceExportMeta, evidence []byte) ([]byte, error) {
	rows, err := csv.NewReader(bytes.NewReader(evidence)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("reading %s CSV: %w", kind, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s CSV has no header", kind)
	}
	// Hash the domain CSV as it will be delivered, not the input bytes:
	// csv.Reader normalises CRLF inside quoted cells to LF, so a verifier
	// reconstructing the domain rows from the envelope must see the same bytes.
	var domain bytes.Buffer
	if err := csv.NewWriter(&domain).WriteAll(rows); err != nil {
		return nil, fmt.Errorf("encoding %s CSV: %w", kind, err)
	}
	metaRaw, err := complianceJSON(meta, "")
	if err != nil {
		return nil, fmt.Errorf("encoding CSV metadata: %w", err)
	}
	manifest := complianceExportManifest{
		Algorithm: "sha256",
		Entries: []complianceExportManifestEntry{
			newComplianceManifestEntry("meta", "application/json", metaRaw),
			newComplianceManifestEntry(kind, "text/csv; line-endings=LF", domain.Bytes()),
		},
	}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encoding CSV manifest: %w", err)
	}
	profilesRaw, _ := json.Marshal(meta.Profiles)
	attestersRaw, _ := json.Marshal(meta.Attesters)
	retentionRaw, _ := json.Marshal(meta.Retention)

	const metadataColumns = 12
	out := make([][]string, 0, len(rows)+1)
	out = append(out, append([]string{
		"record_type", "hive_id", "source_system", "generated_by", "generated_at",
		"period_since", "period_until", "profile_versions", "attesters", "retention", "metadata", "manifest",
	}, rows[0]...))
	metadata := []string{
		"metadata", meta.HiveID, meta.SourceSystem, meta.GeneratedBy, meta.GeneratedAt.Format(time.RFC3339),
		meta.Since.Format(time.RFC3339), meta.Until.Format(time.RFC3339), string(profilesRaw),
		string(attestersRaw), string(retentionRaw), string(metaRaw), string(manifestRaw),
	}
	for i := 1; i < len(metadata); i++ {
		if metadata[i] != "" && strings.ContainsRune("=+-@\t\r", rune(metadata[i][0])) {
			metadata[i] = "'" + metadata[i]
		}
	}
	out = append(out, append(metadata, make([]string, len(rows[0]))...))
	for _, row := range rows[1:] {
		prefix := make([]string, metadataColumns)
		prefix[0] = "evidence"
		out = append(out, append(prefix, row...))
	}
	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	if err := writer.WriteAll(out); err != nil {
		return nil, fmt.Errorf("encoding %s CSV: %w", kind, err)
	}
	return buf.Bytes(), nil
}

func complianceExportMarkdown(meta complianceExportMeta, report []byte) ([]byte, error) {
	metaRaw, err := complianceJSON(meta, "")
	if err != nil {
		return nil, fmt.Errorf("encoding Markdown metadata: %w", err)
	}
	manifest := complianceExportManifest{
		Algorithm: "sha256",
		Entries: []complianceExportManifestEntry{
			newComplianceManifestEntry("meta", "application/json", metaRaw),
			newComplianceManifestEntry("controls", "text/markdown; charset=utf-8", report),
		},
	}
	envelope, err := complianceJSON(map[string]any{"manifest": manifest, "meta": meta}, "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding Markdown manifest: %w", err)
	}
	out := append([]byte(nil), report...)
	out = append(out, []byte("\n## Evidence export metadata\n\n```json\n")...)
	out = append(out, envelope...)
	out = append(out, []byte("\n```\n")...)
	return out, nil
}

// complianceAuditExport is the audit slice with the window it actually
// covers: rotation is size-triggered, so the oldest retained entry may be
// later than since.
type complianceAuditExport struct {
	CoveredFrom string       `json:"covered_from,omitempty"`
	Entries     []AuditEntry `json:"entries"`
}

func newComplianceAuditExport(entries []AuditEntry) complianceAuditExport {
	out := complianceAuditExport{Entries: entries}
	if len(entries) > 0 {
		out.CoveredFrom = entries[0].Timestamp
	}
	return out
}

// complianceExportFormat resolves ?format= for kind, or reports why not.
func complianceExportFormat(kind, format string) (string, error) {
	formats, ok := complianceExportFormats[kind]
	if !ok {
		return "", fmt.Errorf("kind must be one of: %s", strings.Join(complianceExportKinds, ", "))
	}
	if format == "" {
		return formats[0], nil
	}
	for _, f := range formats {
		if f == format {
			return f, nil
		}
	}
	return "", fmt.Errorf("format for kind %s must be one of: %s", kind, strings.Join(formats, ", "))
}

// handleComplianceExport serves GET /api/compliance/export?kind=&format=&since=&until=
// as a download: the control-mapping report (json|md), posture history
// (json|csv), audit log slice (json), redacted config snapshot with sha256
// (json), attestations (json|csv), or all of them as one bundle (json).
// Every export carries provenance metadata and a sha256 manifest, and is
// audited as compliance_export. Owner only.
func (s *Server) handleComplianceExport(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	kind := strings.ToLower(strings.TrimSpace(q.Get("kind")))
	format, err := complianceExportFormat(kind, strings.ToLower(strings.TrimSpace(q.Get("format"))))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := complianceEvidenceNow().UTC()
	since, until, err := complianceRange(r, now)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	attestations := s.attestationStore().List("", since, until)
	meta := s.newComplianceExportMeta(kind, requestUser(r), now, since, until, attestations)

	var body []byte
	var contentType string
	switch {
	case kind == "controls" && format == "md":
		report := []byte(compliance.ReportMarkdown(compliance.BuildReport(cfg, complianceGetenv), cfg.HiveID, now))
		body, err = complianceExportMarkdown(meta, report)
		contentType = "text/markdown; charset=utf-8"
	case kind == "posture" && format == "csv":
		runs, _ := s.postureRunner().History().Between(since, until, 0)
		body, err = complianceExportCSV(kind, meta, compliance.PostureHistoryCSV(runs))
		contentType = "text/csv; charset=utf-8"
	case kind == "attestations" && format == "csv":
		body, err = complianceExportCSV(kind, meta, compliance.AttestationsCSV(attestations))
		contentType = "text/csv; charset=utf-8"
	default:
		var payload map[string]any
		payload, err = s.complianceExportJSON(kind, meta, attestations)
		if err == nil {
			body, err = complianceJSON(payload, "  ")
			body, contentType = append(body, '\n'), "application/json"
		}
	}
	if err != nil {
		jsonError(w, "encoding export: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// The in-band manifest only detects accidental corruption: whoever edits
	// the file can recompute it. Recording the digest of the delivered bytes
	// here ties each download to its audit entry out-of-band.
	bodySum := sha256.Sum256(body)
	s.auditFromRequest(r, "compliance_export", auditDetail(
		"kind", kind,
		"format", format,
		"since", since.Format(time.RFC3339),
		"until", until.Format(time.RFC3339),
		"sha256", hex.EncodeToString(bodySum[:]),
	), "")
	hiveID := safeFilenamePart(cfg.HiveID)
	if hiveID == "" {
		hiveID = "hive"
	}
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="hive-compliance-%s-%s-%s.%s"`, disposition, kind, hiveID, now.Format("20060102-1504"), format))
	_, _ = w.Write(body)
}

// complianceExportJSON builds the JSON payload and its per-section manifest.
func (s *Server) complianceExportJSON(kind string, meta complianceExportMeta, attestations []compliance.Attestation) (map[string]any, error) {
	cfg := s.deps.Config
	out := map[string]any{"meta": meta}
	want := func(k string) bool { return kind == k || kind == "bundle" }
	if want("controls") {
		out["controls"] = compliance.BuildReport(cfg, complianceGetenv)
	}
	if want("posture") {
		runs, _ := s.postureRunner().History().Between(meta.Since, meta.Until, 0)
		if runs == nil {
			runs = []compliance.PostureRun{}
		}
		out["posture"] = map[string]any{"runs": runs, "checks": compliance.BuildPostureSeries(runs)}
	}
	if want("audit") {
		out["audit"] = newComplianceAuditExport(s.complianceAuditSlice(meta.Since, meta.Until))
	}
	if want("config") {
		snap, err := s.complianceConfigSnapshot()
		if err != nil {
			return nil, err
		}
		out["config"] = snap
	}
	if want("attestations") {
		out["attestations"] = attestations
	}
	manifest, err := complianceManifestForJSON(out)
	if err != nil {
		return nil, err
	}
	out["manifest"] = manifest
	return out, nil
}
