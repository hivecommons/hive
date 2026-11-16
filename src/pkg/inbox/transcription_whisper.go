package inbox

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
	"github.com/hivecommons/hive/pkg/transcribe/whisperbox"
)

// Environment overrides for the built-in whisper provider.
const (
	EnvWhisperSandbox       = "HIVE_WHISPER_SANDBOX"
	EnvWhisperEngineDir     = "HIVE_WHISPER_ENGINE_DIR"
	EnvWhisperRunAs         = "HIVE_WHISPER_RUN_AS"
	EnvWhisperModelDir      = "HIVE_WHISPER_MODEL_DIR"
	EnvWhisperModelBaseURL  = "HIVE_WHISPER_MODEL_BASE_URL"
	EnvWhisperModelDownload = "HIVE_WHISPER_MODEL_DOWNLOAD"
	EnvWhisperMemoryMB      = "HIVE_WHISPER_MEMORY_MB"
)

const (
	// whisperJobMargin is added to the whisper wall-clock limit for the
	// whole job (download of the recording, and of the model the first time).
	whisperJobMargin = 15 * time.Minute
	// bytesPerMBf renders the model size in the readiness line.
	bytesPerMBf = float64(1 << 20)
)

// WhisperRuntime locates the built-in whisper engine and its models. The
// zero value is filled from the environment by whisperRuntime.
type WhisperRuntime struct {
	Sandbox   string
	EngineDir string
	RunAs     string
	SuExec    string
	MemoryMB  int
	CPUs      int
	Models    *transcribe.WhisperModelStore
}

// DefaultWhisperRuntime reads the HIVE_WHISPER_* overrides.
func DefaultWhisperRuntime(getenv func(string) string) *WhisperRuntime {
	env := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	mem, _ := strconv.Atoi(env(EnvWhisperMemoryMB, "0"))
	download := strings.ToLower(env(EnvWhisperModelDownload, "true"))
	return &WhisperRuntime{
		Sandbox:   env(EnvWhisperSandbox, transcribe.DefaultWhisperSandbox),
		EngineDir: env(EnvWhisperEngineDir, whisperbox.DefaultEngineDir),
		RunAs:     env(EnvWhisperRunAs, transcribe.DefaultWhisperRunAs),
		MemoryMB:  max(mem, 0),
		CPUs:      runtime.GOMAXPROCS(0),
		Models: &transcribe.WhisperModelStore{
			Dir:        env(EnvWhisperModelDir, transcribe.DefaultWhisperModelDir),
			BaseURL:    env(EnvWhisperModelBaseURL, ""),
			NoDownload: download == "false" || download == "0" || download == "no",
		},
	}
}

func (s *Service) whisperRuntime() *WhisperRuntime {
	s.whisperMu.Lock()
	defer s.whisperMu.Unlock()
	if s.Whisper == nil {
		s.Whisper = DefaultWhisperRuntime(s.getenv)
	}
	return s.Whisper
}

// whisperStatus reports whether the engine, the sandbox user and the model
// (or permission to download it) are in place.
func (s *Service) whisperStatus(t config.InboxTranscription) ProviderStatus {
	rt := s.whisperRuntime()
	model := t.EffectiveWhisperModel()
	st := ProviderStatus{Name: config.InboxTranscriptionProviderWhisper, AudioGoesTo: AudioStaysOnHive, Model: model, Status: ProviderNotInstalled}
	if !executable(rt.Sandbox) || !executable(filepath.Join(rt.EngineDir, "whisper-cli")) || !executable(filepath.Join(rt.EngineDir, "ffmpeg")) {
		st.Detail = "the whisper engine is not in this hive image"
		return st
	}
	if rt.RunAs != transcribe.WhisperRunAsSelf {
		if _, err := user.Lookup(rt.RunAs); err != nil {
			st.Detail = "the sandbox user " + rt.RunAs + " does not exist in this image"
			return st
		}
		if rt.SuExec == "" {
			if _, err := exec.LookPath("su-exec"); err != nil {
				st.Detail = "su-exec is missing, so the sandbox cannot drop to " + rt.RunAs
				return st
			}
		}
	}
	m := transcribe.WhisperModels[model]
	switch {
	case rt.Models.Present(model):
		st.Available, st.Status, st.Detail = true, ProviderReady, "model "+model+" is on this hive"
	case rt.Models.NoDownload:
		st.Status = ProviderNeedsKey
		st.Detail = fmt.Sprintf("model %s is missing and downloads are off: place %s in %s", model, m.File(), rt.Models.Dir)
	default:
		st.Available, st.Status = true, ProviderReady
		st.Detail = fmt.Sprintf("model %s (%.0f MB) downloads on first use from %s", model, float64(m.Size)/bytesPerMBf, transcribe.WhisperModelHost)
		st.Egress = []string{transcribe.WhisperModelHost, transcribe.WhisperModelCDNHosts}
	}
	return st
}

// whisperTranscriber builds the whisper provider for one run.
func (s *Service) whisperTranscriber(t config.InboxTranscription) (transcribe.Transcriber, error) {
	rt := s.whisperRuntime()
	if st := s.whisperStatus(t); !st.Available {
		return nil, fmt.Errorf("whisper is not ready on this hive: %s", st.Detail)
	}
	w := &transcribe.Whisper{
		Models: rt.Models, Model: t.EffectiveWhisperModel(), Threads: t.EffectiveWhisperThreads(rt.CPUs),
		Language: t.EffectiveLanguage(), MaxBytes: t.EffectiveMaxBytes(), MemoryMB: rt.MemoryMB,
		Sandbox: rt.Sandbox, RunAs: rt.RunAs, SuExec: rt.SuExec,
	}
	if rt.EngineDir != whisperbox.DefaultEngineDir {
		w.ExtraArgs = append(w.ExtraArgs, "-engine-dir", rt.EngineDir)
	}
	return w, nil
}

// jobTimeout bounds one whole run: whisper scales with the recording.
func jobTimeout(provider string, durationMS int64) time.Duration {
	if provider != config.InboxTranscriptionProviderWhisper {
		return transcribeJobTimeout
	}
	return max(transcribeJobTimeout, transcribe.WhisperTimeout(time.Duration(durationMS)*time.Millisecond)+whisperJobMargin)
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
