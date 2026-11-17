package inbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// Audio and video drops. The original is streamed to the upload store (it
// is never held in memory), capped by the recording limits
// (inbox.recording.max_bytes), played back from this hive's disk through the
// same-origin stream, and transcribed like a dashboard recording. Its
// original is deleted after the recordings retention (5 days by default);
// the transcript, the item and its fact stay. Video keeps its audio track
// for transcription only: there is no video playback.
const (
	// MediaSniffBytes is how much of a file the media sniffer reads.
	MediaSniffBytes = 64
	// uploadMediaPendingNote is the fact's note while it has no transcript.
	uploadMediaPendingNote = "_Added to the Inbox as audio; no transcript yet._"
)

// mediaFormat is one accepted audio or video container.
type mediaFormat struct {
	kind string
	mime string
}

// uploadMediaExtensions maps the accepted media extensions to their kind
// and the Content-Type playback uses. The content must match one of the
// signatures in sniffMedia as well.
var uploadMediaExtensions = map[string]mediaFormat{
	".mp3":  {UploadKindAudio, "audio/mpeg"},
	".m4a":  {UploadKindAudio, "audio/mp4"},
	".aac":  {UploadKindAudio, "audio/aac"},
	".wav":  {UploadKindAudio, "audio/wav"},
	".ogg":  {UploadKindAudio, "audio/ogg"},
	".opus": {UploadKindAudio, "audio/ogg"},
	".webm": {UploadKindAudio, "audio/webm"},
	".mp4":  {UploadKindVideo, "audio/mp4"},
	".mov":  {UploadKindVideo, "audio/mp4"},
}

// UploadMediaExtensions lists the accepted media extensions.
func UploadMediaExtensions() []string {
	out := make([]string, 0, len(uploadMediaExtensions))
	for ext := range uploadMediaExtensions {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// sniffMedia names the container family of head ("" when it is not one of
// the accepted audio/video signatures).
func sniffMedia(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("ID3")):
		return "mp3"
	case len(head) >= 2 && head[0] == 0xff && head[1]&0xf6 == 0xf0:
		return "aac" // ADTS
	case len(head) >= 2 && head[0] == 0xff && head[1]&0xe0 == 0xe0:
		return "mp3" // MPEG audio frame sync
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WAVE")):
		return "wav"
	case bytes.HasPrefix(head, []byte("OggS")):
		return "ogg"
	case bytes.HasPrefix(head, webmMagic):
		return "webm"
	case len(head) >= 8 && bytes.Equal(head[4:8], []byte("ftyp")):
		return "mp4"
	}
	return ""
}

// mediaFamilies are the signatures each extension may carry.
var mediaFamilies = map[string][]string{
	".mp3":  {"mp3"},
	".m4a":  {"mp4"},
	".aac":  {"aac", "mp4"},
	".wav":  {"wav"},
	".ogg":  {"ogg"},
	".opus": {"ogg"},
	".webm": {"webm"},
	".mp4":  {"mp4"},
	".mov":  {"mp4"},
}

// SniffMedia reports whether a dropped file is accepted audio or video, and
// as what. ok is false for anything else (the text pipeline decides then).
// A media extension whose content does not match is refused.
func SniffMedia(name string, head []byte) (mediaFormat, bool, *UploadError) {
	ext := strings.ToLower(filepath.Ext(name))
	family := sniffMedia(head)
	f, known := uploadMediaExtensions[ext]
	if !known {
		if family != "" && ext == "" {
			return mediaFormat{}, false, uploadErr(UploadErrUnsupported, "audio or video files need their extension (.mp3, .m4a, .wav, .ogg, .webm, .mp4, ...)")
		}
		return mediaFormat{}, false, nil
	}
	for _, want := range mediaFamilies[ext] {
		if family == want {
			return f, true, nil
		}
	}
	return mediaFormat{}, false, uploadErr(UploadErrUnsupported, "the file is named %s but its content is not %s audio or video", ext, strings.TrimPrefix(ext, "."))
}

// MediaInput is one dropped audio or video file, streamed.
type MediaInput struct {
	Filename string
	// Head is the first bytes already read for sniffing; Body is the rest.
	Head    []byte
	Body    io.Reader
	AddedBy string
}

func (u *UploadSource) recordingLimits() config.InboxRecording {
	if u.RecordingLimits == nil {
		return config.InboxRecording{}
	}
	return u.RecordingLimits()
}

// AddMedia streams an audio or video file into the store under the
// recording size cap and the uploads quota, and writes a pending fact that
// the machine transcript later fills.
func (u *UploadSource) AddMedia(ctx context.Context, in MediaInput) (UploadMeta, error) {
	name := CleanUploadName(in.Filename)
	f, ok, uerr := SniffMedia(name, in.Head)
	if uerr != nil {
		return UploadMeta{}, uerr
	}
	if !ok {
		return UploadMeta{}, uploadErr(UploadErrUnsupported, "not an audio or video file")
	}
	limit := u.recordingLimits().EffectiveMaxBytes()
	id, err := u.Store.NewID()
	if err != nil {
		return UploadMeta{}, err
	}
	m := UploadMeta{
		ID:       id,
		Kind:     f.kind,
		MIME:     f.mime,
		Origin:   UploadOriginFile,
		Filename: name,
		Title:    firstNonEmpty(strings.TrimSuffix(name, filepath.Ext(name)), name),
		AddedBy:  firstNonEmpty(in.AddedBy, "local"),
		AddedAt:  u.now(),
	}
	h := sha256.New()
	src := io.TeeReader(io.MultiReader(bytes.NewReader(in.Head), in.Body), h)
	size, err := u.Store.PutStream(m, &ctxReader{ctx: ctx, r: src}, limit, u.limits().EffectiveQuotaBytes())
	if err != nil {
		var ue *UploadError
		if errors.As(err, &ue) {
			return UploadMeta{}, ue
		}
		return UploadMeta{}, fmt.Errorf("storing the upload: %w", err)
	}
	m.Size = size
	m.SHA256 = hex.EncodeToString(h.Sum(nil))
	if _, err := u.Store.UpdateMeta(m.ID, func(mm *UploadMeta) { mm.Size, mm.SHA256 = m.Size, m.SHA256 }); err != nil {
		return UploadMeta{}, err
	}
	if u.ConnectorsDir == "" {
		return m, nil
	}
	slug, layer, err := u.writeMediaFact(m)
	if err != nil {
		u.logger().Warn("inbox: upload fact not written", "upload", m.ID, "error", err)
		return m, nil
	}
	return u.Store.UpdateMeta(m.ID, func(mm *UploadMeta) { mm.FactSlug, mm.FactLayer = slug, layer })
}

// ctxReader stops a stream once its context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// writeMediaFact writes the pending fact of an audio or video upload.
func (u *UploadSource) writeMediaFact(m UploadMeta) (string, string, error) {
	u.factMu.Lock()
	defer u.factMu.Unlock()
	layer := u.layer()
	dir := filepath.Join(u.ConnectorsDir, layer)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	slug := uploadFactPrefix + m.ID
	sv := knowledge.SanitizeFrontmatterValue
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("title: " + sv(m.Title) + "\n")
	b.WriteString("type: " + string(knowledge.FactReference) + "\n")
	b.WriteString("layer: " + sv(layer) + "\n")
	b.WriteString("status: " + connector.StatusActive + "\n")
	b.WriteString("tags: [" + knowledge.SanitizeFrontmatterList([]string{UploadType, m.Origin, m.Kind}) + "]\n")
	b.WriteString("source: " + UploadType + "\n")
	b.WriteString("connector: " + UploadType + "\n")
	b.WriteString("source_id: " + sv(m.ID) + "\n")
	b.WriteString("attr_added_by: " + sv(m.AddedBy) + "\n")
	b.WriteString("attr_recorded_at: " + m.AddedAt.UTC().Format(time.RFC3339) + "\n")
	b.WriteString("attr_upload_filename: " + sv(m.Filename) + "\n")
	b.WriteString("attr_upload_kind: " + sv(m.Kind) + "\n")
	b.WriteString(uploadFactContentKey + ": " + connector.FactContentPending + "\n")
	b.WriteString("synthesized: " + m.AddedAt.UTC().Format(time.RFC3339) + "\n")
	b.WriteString("synced_at: " + u.now().Format(time.RFC3339) + "\n")
	b.WriteString("---\n\n")
	b.WriteString("# " + m.Title + "\n\n")
	b.WriteString("- **Added:** " + m.AddedAt.UTC().Format(uploadFactDateLayout) + " by " + m.AddedBy + "\n")
	b.WriteString("- **From:** " + UploadOrigin(m) + " (" + m.Kind + ", " + humanBytes(m.Size) + ")\n\n")
	b.WriteString(uploadMediaPendingNote + "\n")
	if err := writeFactAtomic(filepath.Join(dir, slug+".md"), b.String()); err != nil {
		return "", "", err
	}
	if u.FactWritten != nil {
		u.FactWritten(dir)
	}
	return slug, layer, nil
}

// uploadAudioPathPrefix is the same-origin stream of an upload's audio.
const uploadAudioPathPrefix = "/api/inbox/" + UploadType + "/"

// AudioLink implements AudioSource: the hive's own same-origin stream (the
// generic stream handler serves the file from disk through OpenAudio).
func (u *UploadSource) AudioLink(_ context.Context, it Item) (AudioLink, error) {
	m, err := u.Store.Get(it.ID)
	if err != nil || !uploadKindIsMedia(m.Kind) {
		return AudioLink{}, ErrNoAudio
	}
	if m.OriginalDeletedAt != nil {
		return AudioLink{}, fmt.Errorf("%w: the audio was deleted after the retention period", ErrNoAudio)
	}
	return AudioLink{URL: uploadAudioPathPrefix + m.ID + "/audio/stream", ExpiresAt: u.now().Add(recordingAudioLinkTTL)}, nil
}

// OpenAudio implements AudioOpener: playback and transcription read the
// original from this hive's disk.
func (u *UploadSource) OpenAudio(_ context.Context, it Item) (LocalAudio, error) {
	m, err := u.Store.Get(it.ID)
	if err != nil || !uploadKindIsMedia(m.Kind) {
		return LocalAudio{}, ErrNoAudio
	}
	f, err := u.Store.OpenOriginal(it.ID)
	if err != nil {
		return LocalAudio{}, fmt.Errorf("%w: the audio was deleted after the retention period", ErrNoAudio)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return LocalAudio{}, err
	}
	return LocalAudio{File: f, Name: m.Filename, MIME: firstNonEmpty(m.MIME, "application/octet-stream"), Size: st.Size(), ModTime: st.ModTime()}, nil
}
