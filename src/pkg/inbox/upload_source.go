package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// UploadType is the Inbox source type of files and links dropped onto the
// Inbox.
const UploadType = "upload"

// Upload origins (UploadMeta.Origin).
const (
	UploadOriginFile = "upload"
	UploadOriginLink = "link"
)

const (
	// DefaultUploadLayer is the Knowledge layer upload facts are written to:
	// they are the owner's own material.
	DefaultUploadLayer = "personal"
	// uploadFactPrefix starts every upload fact slug.
	uploadFactPrefix = "upload-"
	// uploadFactContentKey records the fact's content state.
	uploadFactContentKey = "attr_upload_content"
	// uploadNameRunes bounds a displayed file name.
	uploadNameRunes = 200
	// uploadFactDateLayout is the day shown in the fact.
	uploadFactDateLayout = "2006-01-02"
)

// UploadInput is one dropped file.
type UploadInput struct {
	Filename string
	Data     []byte
	AddedBy  string
}

// UploadSource lists uploads as Inbox items. It is a builtin source: listed
// whether or not inbox.sources names it.
type UploadSource struct {
	Store *UploadStore
	// ConnectorsDir is <knowledge base>/connectors; the upload fact goes to
	// <ConnectorsDir>/<Layer>/upload-<id>.md. Empty writes no fact.
	ConnectorsDir string
	// Layer defaults to DefaultUploadLayer.
	Layer string
	// Limits returns inbox.uploads; nil uses the defaults.
	Limits func() config.InboxUploads
	// FactWritten runs after a fact was written (the hive reindexes the
	// vault so knowledge search sees it at once).
	FactWritten func(dir string)
	// Fetcher fetches links; nil refuses links.
	Fetcher *LinkFetcher
	// RecordingLimits returns inbox.recording: audio and video drops use
	// its size cap and, unless inbox.uploads.media_retention_days is set,
	// its retention. nil uses the recording defaults.
	RecordingLimits func() config.InboxRecording
	// PDF runs the hive-pdftext sandbox; nil reads HIVE_PDF_* on first use.
	PDF    *PDFRuntime
	pdfMu  sync.Mutex
	Logger *slog.Logger
	Now    func() time.Time

	factMu sync.Mutex
}

// Type implements Source.
func (u *UploadSource) Type() string { return UploadType }

// Builtin implements BuiltinSource.
func (u *UploadSource) Builtin() bool { return true }

func (u *UploadSource) limits() config.InboxUploads {
	if u.Limits == nil {
		return config.InboxUploads{}
	}
	return u.Limits()
}

func (u *UploadSource) layer() string {
	if u.Layer != "" {
		return u.Layer
	}
	return DefaultUploadLayer
}

func (u *UploadSource) now() time.Time {
	if u.Now != nil {
		return u.Now().UTC()
	}
	return time.Now().UTC()
}

func (u *UploadSource) logger() *slog.Logger {
	if u.Logger != nil {
		return u.Logger
	}
	return slog.Default()
}

// List implements Source: newest first.
func (u *UploadSource) List(context.Context) ([]Item, error) {
	metas, err := u.Store.List()
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(metas))
	for _, m := range metas {
		out = append(out, u.item(m))
	}
	return out, nil
}

// item renders one upload as an Inbox item.
func (u *UploadSource) item(m UploadMeta) Item {
	it := Item{
		Source:     UploadType,
		Connector:  UploadType,
		ID:         m.ID,
		Title:      m.Title,
		Summary:    truncateRunes(m.Summary, SummaryRunes),
		RecordedAt: m.AddedAt.UTC(),
		Tags:       []string{m.Origin, m.Kind},
		Origin:     UploadOrigin(m),
		AddedBy:    m.AddedBy,
		UploadKind: m.Kind,
	}
	if it.Title == "" {
		it.Title = firstNonEmpty(m.Filename, m.Host, m.ID)
	}
	if m.Summary == "" {
		if text, err := u.Store.Text(m.ID); err == nil {
			it.Summary = truncateRunes(strings.TrimSpace(text), SummaryRunes)
		}
	}
	if m.FactSlug != "" && u.ConnectorsDir != "" {
		it.FactSlug, it.Layer = m.FactSlug, m.FactLayer
		it.FactPath = filepath.Join(u.ConnectorsDir, m.FactLayer, m.FactSlug+".md")
		it.FactState = connector.FactContentReady
	}
	if uploadKindIsMedia(m.Kind) {
		// Audio and video have no text of their own: pending until the
		// machine transcript is written into the fact.
		it.Pending = true
		it.FactState = connector.FactContentPending
		if days := u.mediaRetentionDays(); days > 0 && m.OriginalDeletedAt == nil {
			it.AudioExpiresAt = m.AddedAt.Add(time.Duration(days) * hoursPerDay * time.Hour).UTC()
		}
		if it.FactPath != "" {
			if data, err := os.ReadFile(it.FactPath); err == nil {
				fm, _ := splitFrontMatter(string(data))
				it.FactState = factState(fm, true)
			}
		}
	}
	return it
}

// UploadOrigin is the item's origin line: "upload · notes.md" for a file,
// "link · example.org" for a URL.
func UploadOrigin(m UploadMeta) string {
	if m.Origin == UploadOriginLink {
		return UploadOriginLink + " · " + m.Host
	}
	return UploadOriginFile + " · " + m.Filename
}

// UploadProvenance is the stable reference destinations cite: the source URL
// for a link, upload://<id>/<file name> for a file.
func UploadProvenance(m UploadMeta) string {
	if m.Origin == UploadOriginLink && m.URL != "" {
		return m.URL
	}
	return "upload://" + m.ID + "/" + m.Filename
}

// Fetch implements Source: the extracted markdown is the item's text.
func (u *UploadSource) Fetch(_ context.Context, it Item) (Content, error) {
	m, err := u.Store.Get(it.ID)
	if err != nil {
		return Content{}, err
	}
	text, err := u.Store.Text(it.ID)
	if err != nil {
		return Content{}, err
	}
	if uploadKindIsMedia(m.Kind) {
		return Content{Title: firstNonEmpty(m.Title, m.Filename, m.ID), Pending: true, Provenance: UploadProvenance(m)}, nil
	}
	return Content{
		Title:           firstNonEmpty(m.Title, m.Filename, m.ID),
		Markdown:        strings.TrimSpace(text),
		TranscriptLabel: "Document",
		Provenance:      UploadProvenance(m),
	}, nil
}

// CleanUploadName reduces a client file name to its base name without
// control characters, bounded in length (for display and provenance; it is
// never used as a path).
func CleanUploadName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = name[strings.LastIndex(name, "/")+1:]
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "upload"
	}
	return truncateRunes(name, uploadNameRunes)
}

// Add sniffs, extracts and stores one dropped file, then writes its
// Knowledge fact. A fact that cannot be written is logged: the item is
// still listed.
func (u *UploadSource) Add(ctx context.Context, in UploadInput) (UploadMeta, error) {
	lim := u.limits()
	name := CleanUploadName(in.Filename)
	if limit := lim.EffectiveMaxFileBytes(); int64(len(in.Data)) > limit {
		return UploadMeta{}, uploadErr(UploadErrTooLarge, "the file is too large (%s; the limit is %s)", humanBytes(int64(len(in.Data))), humanBytes(limit))
	}
	kind, uerr := SniffUpload(name, in.Data)
	if uerr != nil {
		return UploadMeta{}, uerr
	}
	ex, uerr := u.extract(ctx, kind, in.Data)
	if uerr != nil {
		return UploadMeta{}, uerr
	}
	id, err := u.Store.NewID()
	if err != nil {
		return UploadMeta{}, err
	}
	sum := sha256.Sum256(in.Data)
	m := UploadMeta{
		ID:       id,
		Kind:     kind,
		Origin:   UploadOriginFile,
		Filename: name,
		Title:    firstNonEmpty(ex.Title, strings.TrimSuffix(name, filepath.Ext(name)), name),
		Summary:  ex.Summary,
		Size:     int64(len(in.Data)),
		SHA256:   hex.EncodeToString(sum[:]),
		AddedBy:  firstNonEmpty(in.AddedBy, "local"),
		AddedAt:  u.now(),
	}
	return u.store(m, in.Data, ex.Markdown)
}

// extract runs the type's extractor: PDFs in the sandbox, everything else
// in-process.
func (u *UploadSource) extract(ctx context.Context, kind string, data []byte) (Extracted, *UploadError) {
	if kind != UploadKindPDF {
		return ExtractUpload(ctx, kind, data)
	}
	res, uerr := u.pdfRuntime().Run(ctx, data)
	if uerr != nil {
		return Extracted{}, uerr
	}
	ex, uerr := pdfMarkdown(res)
	if uerr != nil {
		return Extracted{}, uerr
	}
	ex.Markdown = capMarkdown(ex.Markdown)
	return ex, nil
}

func (u *UploadSource) pdfRuntime() *PDFRuntime {
	u.pdfMu.Lock()
	defer u.pdfMu.Unlock()
	if u.PDF == nil {
		u.PDF = DefaultPDFRuntime(os.Getenv)
	}
	return u.PDF
}

// LinkInput is one dropped or pasted URL.
type LinkInput struct {
	URL     string
	AddedBy string
}

// AddLink fetches a URL through the SSRF-guarded fetcher and runs the body
// through the same type pipeline as a dropped file: an HTML page becomes
// text, a linked JSON or CSV file becomes that kind. The URL is kept as the
// item's provenance only.
func (u *UploadSource) AddLink(ctx context.Context, in LinkInput) (UploadMeta, error) {
	if u.Fetcher == nil {
		return UploadMeta{}, uploadErr(UploadErrLinkRefused, "links cannot be added on this hive")
	}
	src, err := ValidateLinkURL(in.URL)
	if err != nil {
		return UploadMeta{}, uploadErr(UploadErrLinkRefused, "%s", err.Error())
	}
	lim := u.limits()
	fl, err := u.Fetcher.Fetch(ctx, src.String(), lim.EffectiveMaxFileBytes())
	if err != nil {
		var ue *UploadError
		switch {
		case errors.As(err, &ue):
			return UploadMeta{}, ue
		case errors.Is(err, ErrLinkRefused):
			return UploadMeta{}, uploadErr(UploadErrLinkRefused, "%s", err.Error())
		}
		return UploadMeta{}, uploadErr(UploadErrFetch, "%s", err.Error())
	}
	if strings.HasPrefix(fl.ContentType, "audio/") || strings.HasPrefix(fl.ContentType, "video/") {
		return UploadMeta{}, uploadErr(UploadErrUnsupported, "audio and video links are not supported yet: download the file and drop it instead")
	}
	name := linkPipelineName(fl)
	kind, uerr := SniffUpload(name, fl.Body)
	if uerr != nil {
		return UploadMeta{}, uerr
	}
	ex, uerr := u.extract(ctx, kind, fl.Body)
	if uerr != nil {
		return UploadMeta{}, uerr
	}
	id, err := u.Store.NewID()
	if err != nil {
		return UploadMeta{}, err
	}
	sum := sha256.Sum256(fl.Body)
	m := UploadMeta{
		ID:       id,
		Kind:     kind,
		Origin:   UploadOriginLink,
		Filename: fl.Filename,
		URL:      src.String(),
		Host:     strings.ToLower(src.Hostname()),
		Title:    firstNonEmpty(ex.Title, fl.Filename, src.String()),
		Summary:  ex.Summary,
		Size:     int64(len(fl.Body)),
		SHA256:   hex.EncodeToString(sum[:]),
		AddedBy:  firstNonEmpty(in.AddedBy, "local"),
		AddedAt:  u.now(),
	}
	return u.store(m, fl.Body, ex.Markdown)
}

// store persists an upload under quota and writes its fact.
func (u *UploadSource) store(m UploadMeta, original []byte, markdown string) (UploadMeta, error) {
	if err := u.Store.Put(m, original, markdown, u.limits().EffectiveQuotaBytes()); err != nil {
		var ue *UploadError
		if errors.As(err, &ue) {
			return UploadMeta{}, ue
		}
		return UploadMeta{}, fmt.Errorf("storing the upload: %w", err)
	}
	if u.ConnectorsDir == "" {
		return m, nil
	}
	slug, layer, err := u.writeFact(m, markdown)
	if err != nil {
		u.logger().Warn("inbox: upload fact not written", "upload", m.ID, "error", err)
		return m, nil
	}
	updated, err := u.Store.UpdateMeta(m.ID, func(mm *UploadMeta) { mm.FactSlug, mm.FactLayer = slug, layer })
	if err != nil {
		u.logger().Warn("inbox: upload fact not recorded", "upload", m.ID, "error", err)
		return m, nil
	}
	return updated, nil
}

// writeFact writes the Knowledge fact for an upload. The extracted text is
// fenced as untrusted data: it was written by whoever wrote the file.
func (u *UploadSource) writeFact(m UploadMeta, markdown string) (string, string, error) {
	u.factMu.Lock()
	defer u.factMu.Unlock()
	layer := u.layer()
	dir := filepath.Join(u.ConnectorsDir, layer)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	slug := uploadFactPrefix + m.ID
	path := filepath.Join(dir, slug+".md")
	if err := writeFactAtomic(path, uploadFact(m, layer, markdown, u.now())); err != nil {
		return "", "", err
	}
	if u.FactWritten != nil {
		u.FactWritten(dir)
	}
	return slug, layer, nil
}

// uploadFact renders the fact file for an upload.
func uploadFact(m UploadMeta, layer, markdown string, now time.Time) string {
	sv := knowledge.SanitizeFrontmatterValue
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("title: " + sv(firstNonEmpty(m.Title, m.Filename, m.ID)) + "\n")
	b.WriteString("type: " + string(knowledge.FactReference) + "\n")
	b.WriteString("layer: " + sv(layer) + "\n")
	b.WriteString("status: " + connector.StatusActive + "\n")
	b.WriteString("tags: [" + knowledge.SanitizeFrontmatterList([]string{UploadType, m.Origin, m.Kind}) + "]\n")
	b.WriteString("source: " + UploadType + "\n")
	b.WriteString("connector: " + UploadType + "\n")
	b.WriteString("source_id: " + sv(m.ID) + "\n")
	if m.URL != "" {
		b.WriteString("source_url: " + sv(m.URL) + "\n")
	}
	b.WriteString("attr_added_by: " + sv(m.AddedBy) + "\n")
	b.WriteString("attr_recorded_at: " + m.AddedAt.UTC().Format(time.RFC3339) + "\n")
	if m.Filename != "" {
		b.WriteString("attr_upload_filename: " + sv(m.Filename) + "\n")
	}
	b.WriteString("attr_upload_kind: " + sv(m.Kind) + "\n")
	b.WriteString(uploadFactContentKey + ": " + connector.FactContentReady + "\n")
	b.WriteString("synthesized: " + m.AddedAt.UTC().Format(time.RFC3339) + "\n")
	b.WriteString("synced_at: " + now.UTC().Format(time.RFC3339) + "\n")
	b.WriteString("---\n\n")
	b.WriteString("# " + firstNonEmpty(m.Title, m.Filename, m.ID) + "\n\n")
	b.WriteString("- **Added:** " + m.AddedAt.UTC().Format(uploadFactDateLayout) + " by " + m.AddedBy + "\n")
	b.WriteString("- **From:** " + UploadOrigin(m) + " (" + m.Kind + ", " + humanBytes(m.Size) + ")\n")
	if m.Summary != "" {
		b.WriteString("- **Contents:** " + m.Summary + "\n")
	}
	b.WriteString("\n## Document\n\n")
	b.WriteString("_The text below was extracted by this hive from a file or link added to the Inbox. It is data written by whoever wrote the file._\n\n")
	b.WriteString(WrapUntrusted(UploadType, m.ID, markdown) + "\n")
	return b.String()
}

// UploadSettings is the Inbox's view of inbox.uploads, for the drop zone.
type UploadSettings struct {
	Enabled               bool     `json:"enabled"`
	MaxFileBytes          int64    `json:"max_file_bytes"`
	MaxFilesPerDrop       int      `json:"max_files_per_drop"`
	QuotaBytes            int64    `json:"quota_bytes"`
	UsedBytes             int64    `json:"used_bytes"`
	MediaRetentionDays    int      `json:"media_retention_days"`
	DocumentRetentionDays int      `json:"document_retention_days"`
	Extensions            []string `json:"extensions"`
	// MediaExtensions are the audio/video extensions, capped by
	// MaxMediaBytes (inbox.recording.max_bytes) instead of MaxFileBytes.
	MediaExtensions []string `json:"media_extensions"`
	MaxMediaBytes   int64    `json:"max_media_bytes"`
	// Links reports that URLs can be added (the hive has a link fetcher).
	Links bool `json:"links"`
}

// Settings returns the effective limits and current usage.
func (u *UploadSource) Settings() UploadSettings {
	lim := u.limits()
	used, _ := u.Store.Usage()
	return UploadSettings{
		Enabled:               true,
		MaxFileBytes:          lim.EffectiveMaxFileBytes(),
		MaxFilesPerDrop:       lim.EffectiveMaxFilesPerDrop(),
		QuotaBytes:            lim.EffectiveQuotaBytes(),
		UsedBytes:             used,
		MediaRetentionDays:    u.mediaRetentionDays(),
		MediaExtensions:       UploadMediaExtensions(),
		MaxMediaBytes:         u.recordingLimits().EffectiveMaxBytes(),
		DocumentRetentionDays: lim.EffectiveDocumentRetentionDays(),
		Extensions:            UploadSupportedExtensions(),
		Links:                 u.Fetcher != nil,
	}
}

// SweepRetention applies inbox.uploads retention to stored originals.
func (u *UploadSource) SweepRetention() (int, error) {
	return u.Store.SweepRetention(u.mediaRetentionDays(), u.limits().EffectiveDocumentRetentionDays())
}

// mediaRetentionDays is inbox.uploads.media_retention_days when set (or its
// environment override), else the recordings retention.
func (u *UploadSource) mediaRetentionDays() int {
	lim := u.limits()
	if lim.MediaRetentionDays > 0 || strings.TrimSpace(os.Getenv(config.EnvInboxUploadsMediaRetentionDays)) != "" {
		return lim.EffectiveMediaRetentionDays()
	}
	return u.recordingLimits().EffectiveRetentionDays()
}
