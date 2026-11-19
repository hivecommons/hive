package transcribe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WhisperModel is one ggml model the hive may download, pinned by size and
// SHA-256 (the Git LFS object id of the file in the upstream repository).
type WhisperModel struct {
	Name   string
	Size   int64
	SHA256 string
}

// File is the model's file name: ggml-<name>.bin.
func (m WhisperModel) File() string { return "ggml-" + m.Name + ".bin" }

const (
	// DefaultWhisperModel is base.en: about 148 MB, and on 2 to 4 vCPU it
	// transcribes a 5-minute recording in one to three minutes (small is
	// three times slower for a modest accuracy gain).
	DefaultWhisperModel = "base.en"
	// DefaultWhisperMultilingualModel is used for a non-English language
	// when no model is set.
	DefaultWhisperMultilingualModel = "base"
	// DefaultWhisperModelDir is where models live on the hive's data volume;
	// operators of air-gapped hives pre-seed the file here.
	DefaultWhisperModelDir = "/data/models/whisper"
	// WhisperModelHost is the official ggml model host (egress allowlists);
	// its files are served from the *.hf.co CDN.
	WhisperModelHost = "huggingface.co"
	// WhisperModelCDNHosts is the CDN the host redirects downloads to.
	WhisperModelCDNHosts = "*.hf.co"
	// whisperModelRevision pins the upstream repository commit the models
	// are fetched from.
	whisperModelRevision = "5359861c739e955e79d9a303bcbc70fb988958b1"
	// DefaultWhisperModelBaseURL is the pinned download root.
	DefaultWhisperModelBaseURL = "https://" + WhisperModelHost + "/ggerganov/whisper.cpp/resolve/" + whisperModelRevision
	// whisperModelDirMode / whisperModelFileMode: the model is not secret and
	// must be readable by the sandbox uid through the inherited descriptor.
	whisperModelDirMode  = 0o755
	whisperModelFileMode = 0o644
	// whisperModelDownloadTimeout bounds one model download.
	whisperModelDownloadTimeout = 30 * time.Minute
	// verifiedSuffix marks a model file whose checksum matched, recording
	// its size and modification time so it is not rehashed every run.
	verifiedSuffix = ".verified"
)

// WhisperModels are the models hive accepts (inbox.transcription.whisper_model).
var WhisperModels = map[string]WhisperModel{
	"tiny":     {"tiny", 77691713, "be07e048e1e599ad46341c8d2a135645097a538221678b7acdd1b1919c6e1b21"},
	"tiny.en":  {"tiny.en", 77704715, "921e4cf8686fdd993dcd081a5da5b6c365bfde1162e72b08d75ac75289920b1f"},
	"base":     {"base", 147951465, "60ed5bc3dd14eea856493d334349b405782ddcaf0028d4b5df4088345fba2efe"},
	"base.en":  {"base.en", 147964211, "a03779c86df3323075f5e796cb2ce5029f00ec8869eee3fdfb897afe36c6d002"},
	"small":    {"small", 487601967, "1be3a9b2063867b937e64e2ec7483364a79917e157fa98c5d94b5c1fffea987b"},
	"small.en": {"small.en", 487614201, "c6138d6d58ecc8322097e0f987c32f1be8bb0a18532a3f88f734d1bbf9c41e5d"},
}

// ErrWhisperModelMissing is returned when the model is not on disk and
// downloading is disabled.
var ErrWhisperModelMissing = errors.New("whisper model is not on this hive")

// WhisperModelStore finds, verifies and downloads ggml models under Dir.
type WhisperModelStore struct {
	Dir string
	// BaseURL is the download root ("" = DefaultWhisperModelBaseURL).
	BaseURL string
	// NoDownload refuses to fetch a missing model (air-gapped hives).
	NoDownload bool
	// Client defaults to http.DefaultClient.
	Client *http.Client
	// Models overrides WhisperModels (tests).
	Models map[string]WhisperModel

	mu sync.Mutex
}

func (s *WhisperModelStore) lookup(name string) (WhisperModel, error) {
	models := s.Models
	if models == nil {
		models = WhisperModels
	}
	m, ok := models[name]
	if !ok {
		return WhisperModel{}, fmt.Errorf("whisper model %q is not one of the pinned models", name)
	}
	return m, nil
}

// Path is where the model lives.
func (s *WhisperModelStore) Path(name string) string {
	return filepath.Join(s.Dir, "ggml-"+name+".bin")
}

// Present reports whether a file of the right size is on disk (the checksum
// is verified on first use).
func (s *WhisperModelStore) Present(name string) bool {
	m, err := s.lookup(name)
	if err != nil {
		return false
	}
	info, err := os.Stat(s.Path(name))
	return err == nil && info.Mode().IsRegular() && info.Size() == m.Size
}

// Ensure returns the path of a verified model, downloading it when missing.
// A file with the wrong size or checksum is replaced by a fresh download,
// or reported when downloading is disabled.
func (s *WhisperModelStore) Ensure(ctx context.Context, name string) (string, error) {
	m, err := s.lookup(name)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.Path(name)
	if info, err := os.Stat(path); err == nil && info.Size() == m.Size {
		if s.verified(path, info) {
			return path, nil
		}
		if sum, err := fileSHA256(path); err == nil && sum == m.SHA256 {
			s.markVerified(path, info)
			return path, nil
		}
	}
	if s.NoDownload {
		return "", fmt.Errorf("%w: place %s (%d bytes, sha256 %s) in %s", ErrWhisperModelMissing, m.File(), m.Size, m.SHA256, s.Dir)
	}
	if err := s.download(ctx, m, path); err != nil {
		return "", err
	}
	return path, nil
}

func (s *WhisperModelStore) verified(path string, info os.FileInfo) bool {
	data, err := os.ReadFile(path + verifiedSuffix)
	return err == nil && strings.TrimSpace(string(data)) == verifiedStamp(info)
}

func (s *WhisperModelStore) markVerified(path string, info os.FileInfo) {
	_ = os.WriteFile(path+verifiedSuffix, []byte(verifiedStamp(info)+"\n"), whisperModelFileMode)
}

func verifiedStamp(info os.FileInfo) string {
	return strconv.FormatInt(info.Size(), 10) + " " + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

// download fetches m over HTTPS into a temp file next to path, capped at
// its pinned size, checks the SHA-256, then renames it into place.
func (s *WhisperModelStore) download(ctx context.Context, m WhisperModel, path string) error {
	if err := os.MkdirAll(s.Dir, whisperModelDirMode); err != nil {
		return fmt.Errorf("whisper model dir: %w", err)
	}
	base := strings.TrimRight(s.BaseURL, "/")
	if base == "" {
		base = DefaultWhisperModelBaseURL
	}
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://127.0.0.1") && !strings.HasPrefix(base, "http://localhost") {
		return fmt.Errorf("whisper model URL must be https")
	}
	ctx, cancel := context.WithTimeout(ctx, whisperModelDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+m.File(), nil)
	if err != nil {
		return fmt.Errorf("whisper model request: %w", err)
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return &RetryableError{Err: fmt.Errorf("whisper model download from %s: %v", WhisperModelHost, unwrapURLError(err))}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("whisper model download: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > m.Size {
		return fmt.Errorf("whisper model download: %d bytes, expected %d", resp.ContentLength, m.Size)
	}
	tmp, err := os.CreateTemp(s.Dir, ".download-*")
	if err != nil {
		return fmt.Errorf("whisper model temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, m.Size+1))
	cerr := tmp.Close()
	switch {
	case err != nil:
		return &RetryableError{Err: fmt.Errorf("whisper model download: %v", err)}
	case cerr != nil:
		return fmt.Errorf("whisper model write: %w", cerr)
	case n != m.Size:
		return fmt.Errorf("whisper model download: %d bytes, expected %d", n, m.Size)
	case hex.EncodeToString(h.Sum(nil)) != m.SHA256:
		return fmt.Errorf("whisper model download: checksum mismatch for %s", m.File())
	}
	if err := os.Chmod(tmp.Name(), whisperModelFileMode); err != nil {
		return fmt.Errorf("whisper model mode: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("whisper model install: %w", err)
	}
	if info, err := os.Stat(path); err == nil {
		s.markVerified(path, info)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
