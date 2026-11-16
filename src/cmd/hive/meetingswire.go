package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
	"github.com/hivecommons/hive/pkg/meetings/live"
	"github.com/hivecommons/hive/pkg/meetings/meetllm"
	"github.com/hivecommons/hive/pkg/meetings/quorumproc"
	"github.com/hivecommons/hive/pkg/spektacular"
)

// quorumSupervisor is the slice of *quorumproc.Supervisor boot uses.
type quorumSupervisor interface {
	Start(ctx context.Context) error
	Stop()
	BaseURL() string
	Token() string
}

// newQuorumSupervisor is the constructor seam; tests replace it.
var newQuorumSupervisor = func(opts quorumproc.Options) (quorumSupervisor, error) {
	return quorumproc.New(opts)
}

// meetingsQuorumOptions builds the Quorum child options from the meetings
// config, resolving the Vexa tenant key, the optional Discord bot token and
// the optional transcription service token from the keystore. A secret that cannot be read is logged and left unset:
// Quorum then answers platform_unavailable for that platform instead of the
// hive failing to boot.
func meetingsQuorumOptions(ctx context.Context, m config.MeetingsConfig, logger *slog.Logger) quorumproc.Options {
	vexaKey, err := m.VexaAPIKey(ctx)
	if err != nil && logger != nil {
		logger.Warn("[meetings] Vexa tenant key unreadable; meeting platforms other than Discord are unavailable", "error", err)
	}
	discordToken, err := m.DiscordToken(ctx)
	if err != nil && logger != nil {
		logger.Warn("[meetings] Discord bot token unreadable; Discord is unavailable", "error", err)
	}
	transcribeToken, err := m.TranscribeToken(ctx)
	if err != nil && logger != nil {
		logger.Warn("[meetings] transcription service token unreadable; Discord audio is not transcribed", "error", err)
	}
	return quorumproc.Options{
		Binary:          m.QuorumBinary(),
		Listen:          m.QuorumListen(),
		VexaURL:         m.VexaURL(),
		VexaAPIKey:      vexaKey,
		DiscordToken:    discordToken,
		TranscribeURL:   m.TranscribeURLOrEmpty(),
		TranscribeToken: transcribeToken,
		Logger:          logger,
	}
}

// wireMeetings starts the Quorum child supervisor when meetings are enabled
// (hivecommons/hive#18289). With the feature off (the default) nothing is
// constructed and no process starts. It returns the running supervisor, or
// nil; the caller registers Stop for shutdown.
func wireMeetings(ctx context.Context, cfg *config.Config, logger *slog.Logger) quorumSupervisor {
	if cfg == nil || !cfg.Meetings.IsEnabled() {
		return nil
	}
	if bin := cfg.Meetings.QuorumBinary(); filepath.IsAbs(bin) {
		if _, err := os.Stat(bin); err != nil {
			if logger != nil {
				logger.Error("[meetings] quorum bundle missing; meetings unavailable (this image was built without QUORUM_VERSION, or meetings.quorum.binary is wrong)",
					"binary", bin, "error", err)
			}
			return nil
		}
	}
	sup, err := newQuorumSupervisor(meetingsQuorumOptions(ctx, cfg.Meetings, logger))
	if err != nil {
		if logger != nil {
			logger.Error("[meetings] quorum supervisor not created; meetings unavailable", "error", err)
		}
		return nil
	}
	if err := sup.Start(ctx); err != nil {
		if logger != nil {
			logger.Error("[meetings] quorum supervisor did not start; meetings unavailable", "error", err)
		}
		sup.Stop()
		return nil
	}
	if logger != nil {
		logger.Info("[meetings] quorum supervisor started",
			"binary", cfg.Meetings.QuorumBinary(),
			"listen", cfg.Meetings.QuorumListen())
	}
	return sup
}

// wireMeetingsManager builds the meeting manager over a running Quorum
// supervisor (hivecommons/hive#18290) and re-attaches to meetings that were
// live before a restart. It returns nil (logged) when the store cannot be
// opened.
func wireMeetingsManager(cfg *config.Config, sup quorumSupervisor, logger *slog.Logger) (*meetings.Manager, *meetings.Store) {
	if cfg == nil || sup == nil {
		return nil, nil
	}
	store, err := meetings.NewStore(cfg.Meetings.DataDirOrDefault())
	if err != nil {
		if logger != nil {
			logger.Error("[meetings] meeting store unavailable; meetings unavailable", "error", err)
		}
		return nil, nil
	}
	mgr, err := meetings.NewManager(meetings.Options{
		Store:        store,
		Quorum:       meetings.NewClient(sup.BaseURL(), sup.Token(), nil),
		BotName:      cfg.Meetings.BotNameOrDefault(),
		ConsentText:  cfg.Meetings.ConsentTextOrDefault(),
		LeavePhrases: cfg.Meetings.Persona.Resolve().LeavePhrases,
		LobbyTimeout: time.Duration(cfg.Meetings.LobbyTimeoutSOrDefault()) * time.Second,
		Logger:       logger,
		// Vexa fixes speaking ability at join time, so with voice-on-address
		// allowed every bot starts able to speak; the meeting's voice toggle
		// still decides whether hive does.
		VoiceOnAddress: cfg.Meetings.VoiceOnAddressEnabled(),
	})
	if err != nil {
		if logger != nil {
			logger.Error("[meetings] meeting manager not created", "error", err)
		}
		return nil, nil
	}
	// Discord `/hive join` requests arrive on the all-meetings stream. No
	// Discord identity is mapped to the hive owner yet, so none is approved
	// automatically: each waits for the owner in the dashboard.
	mgr.StartRequests()
	if n := mgr.Resume(); n > 0 && logger != nil {
		logger.Info("[meetings] re-attached to meetings", "count", n)
	}
	return mgr, store
}

// meetingSpekArgs is the contract's live-mode command line
// (quorum.contract/v1 section 2).
func meetingSpekArgs(m meetings.Meeting, contextDir string) []string {
	title := live.NotesTitle(m)
	return []string{"meeting", "--from", "stdin", "--format", "jsonl", "--title", title, "--context", contextDir}
}

// wireMeetingsLive starts the Spektacular live loop over the meeting
// manager (hivecommons/hive#18291): one `spektacular meeting` child per live
// meeting, its intents applied through the meetings policy. research may be
// nil until the research pool is wired; research intents then fail visibly
// in the panel instead of silently. beadStores feeds the persona's work-item
// retrieval and may be nil.
func wireMeetingsLive(cfg *config.Config, mgr *meetings.Manager, store *meetings.Store, kapi *knowledge.KnowledgeAPI,
	beadStores func() map[string]*beads.Store, research live.ResearchDispatcher, logger *slog.Logger) *live.Runner {
	if cfg == nil || mgr == nil || store == nil {
		return nil
	}
	m := cfg.Meetings
	binary := cfg.Runs.Spektacular.BinaryOrDefault()
	llm := cfg.ResolveMeetingsLLM(context.Background())
	hiveBin, err := os.Executable()
	if err != nil && llm.Backend == config.MeetingsLLMModeOpenAI {
		llm = config.ResolvedMeetingsLLM{Backend: config.MeetingsLLMModeNone, Reason: "rules only, the hive binary path is unknown"}
	}
	slotsDir := filepath.Join(store.Dir(), meetingLLMSlotsDir)
	maxTokens := m.Budget.MaxTokensPerMeetingOrDefault()
	if logger != nil {
		logger.Info("[meetings] spektacular live mode model", "model", llm.Label())
	}
	var notes live.NotesSink
	if kapi != nil {
		notes = live.KnowledgeNotes{API: kapi}
	}
	persona := m.Persona.Resolve()
	personaName := "Hive"
	if persona.Enabled {
		personaName = persona.Name
	}
	r, err := live.NewRunner(live.Options{
		Meetings: mgr,
		Store:    store,
		NewSpek: func(mt meetings.Meeting) live.Spek {
			env := meetingSpekEnv(os.Getenv, llm, hiveBin, meetingUsageFile(store, mt.ID), slotsDir, maxTokens)
			return spektacular.StreamExec(binary, meetingSpekArgs(mt, filepath.Join(store.Dir(), mt.ID)), spektacular.StreamOptions{Logger: logger, Env: env})
		},
		SpekMode: llm.Label(),
		UsageFile: func(id string) string {
			// `hive meeting-llm` (OpenAI-compatible) and the persona's own
			// calls (either backend) write the meeting's usage ledger.
			if llm.Backend != config.MeetingsLLMModeOpenAI && llm.Backend != config.MeetingsLLMModeAnthropic {
				return ""
			}
			return meetingUsageFile(store, id)
		},
		Research: research,
		Notes:    notes,
		Policy: live.Policy{
			ChatPostsPerMinute:   m.ChatPostsPerMinuteOrDefault(),
			ResearchPerMeeting:   m.ResearchPerMeetingOrDefault(),
			VoiceOnAddress:       m.VoiceOnAddressEnabled(),
			MaxTokensPerMeeting:  m.Budget.MaxTokensPerMeetingOrDefault(),
			MaxIntentsPerMeeting: m.Budget.MaxIntentsPerMeetingOrDefault(),
		},
		Ledger:      live.NewLedger(m.Budget.MaxTokensPerHourOrDefault(), m.Budget.MaxTokensPerDayOrDefault()),
		Logger:      logger,
		Persona:     meetingPersonaFactory(cfg, persona, llm, store, kapi, beadStores, slotsDir, maxTokens, research != nil, logger),
		PersonaName: personaName,
		IsLeave:     meetings.NewLeaveMatcher(persona.LeavePhrases).Match,
	})
	if err != nil {
		if logger != nil {
			logger.Error("[meetings] live loop not created", "error", err)
		}
		return nil
	}
	r.Start()
	return r
}

// meetingPersonaFactory builds the meeting attendee persona (Betty,
// hivecommons/hive#18392, #18393) for each live meeting, or returns nil when
// the persona is off. She retrieves from the in-memory knowledge vaults,
// code maps (when enabled), bead stores and, with
// meetings.persona.searxng_url set, the web through SearXNG. She answers
// through the meetings model: the OpenAI-compatible endpoint, or the
// Anthropic Messages API on a hive with only an Anthropic key, both charged
// to the meeting's usage file and budget. With no model she still
// introduces herself but does not answer questions. canResearch lets an
// addressed question she cannot answer well become a deep dive.
func meetingPersonaFactory(cfg *config.Config, persona config.ResolvedMeetingsPersona, llm config.ResolvedMeetingsLLM,
	store *meetings.Store, kapi *knowledge.KnowledgeAPI, beadStores func() map[string]*beads.Store,
	slotsDir string, maxTokens int, canResearch bool, logger *slog.Logger) func(meetings.Meeting) *betty.Engine {
	if !persona.Enabled {
		return nil
	}
	var retrievers []betty.Retriever
	if kapi != nil {
		stores := betty.FileStores(kapi.FileStores)
		retrievers = append(retrievers, betty.KnowledgeRetriever{Stores: stores})
		if cfg.Knowledge.CodeMaps.IsEnabled() {
			retrievers = append(retrievers, betty.CodeMapRetriever{Stores: stores})
		}
	}
	if beadStores != nil {
		retrievers = append(retrievers, betty.BeadRetriever{Stores: beadStores})
	}
	client := &http.Client{}
	if persona.SearxngURL != "" {
		retrievers = append(retrievers, betty.WebRetriever{BaseURL: persona.SearxngURL, Client: client})
	}
	return func(mt meetings.Meeting) *betty.Engine {
		var model betty.LLM
		mc := meetllm.Config{
			// The engine bounds each call itself (2.5 s unprompted, 4 s
			// addressed, 800 ms restyle); this is the outer cap.
			Timeout:   betty.DefaultAddressedTimeout,
			UsageFile: meetingUsageFile(store, mt.ID), MaxTokens: maxTokens,
			SlotsDir: slotsDir, MaxInFlight: llm.MaxInFlight,
		}
		switch llm.Backend {
		case config.MeetingsLLMModeOpenAI:
			mc.Endpoint, mc.Model, mc.APIKey = llm.Endpoint, llm.Model, llm.APIKey
			model = betty.MeetLLM{Client: client, Config: mc}
		case config.MeetingsLLMModeAnthropic:
			mc.Provider, mc.Model, mc.APIKey = meetllm.ProviderAnthropic, llm.Model, llm.AnthropicKey
			model = betty.MeetLLM{Client: client, Config: mc}
		}
		return betty.New(betty.SettingsFrom(persona), betty.Deps{LLM: model, Retrievers: retrievers, Logger: logger, CanResearch: canResearch})
	}
}

// meetingLLMSlotsDir holds the in-flight slot locks `hive meeting-llm`
// shares across meetings; the leading dot keeps it out of the meeting list.
const meetingLLMSlotsDir = ".llm-slots"

// meetingUsageFile is a meeting's model usage ledger (meetllm).
func meetingUsageFile(store *meetings.Store, id string) string {
	return filepath.Join(store.Dir(), id, "llm-usage.jsonl")
}

// meetingSpekPassthrough are the hive environment variables the Spek child
// inherits; hive's own secrets never reach it.
var meetingSpekPassthrough = []string{
	"PATH", "HOME", "TMPDIR", "TZ", "LANG", "LC_ALL",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR",
}

// meetingSpekEnv is the Spek child's environment. Hive owns the model
// credential and budget: with an OpenAI-compatible model Spek runs
// SPEK_MEETING_LLM=command against `hive meeting-llm`, which reads the
// endpoint and key from this environment (never argv) and charges the
// meeting's usage file; with an Anthropic key Spek calls Anthropic itself;
// with neither it runs on rules only.
func meetingSpekEnv(getenv func(string) string, llm config.ResolvedMeetingsLLM, hiveBin, usageFile, slotsDir string, maxTokens int) []string {
	var env []string
	for _, k := range meetingSpekPassthrough {
		if v := getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	switch llm.Backend {
	case config.MeetingsLLMModeOpenAI:
		env = append(env,
			"SPEK_MEETING_LLM=command",
			"SPEK_MEETING_LLM_CMD="+hiveBin+" "+meetllm.Subcommand,
			"SPEK_MEETING_LLM_TIMEOUT_MS="+strconv.Itoa(llm.TimeoutMS),
			meetllm.EnvEndpoint+"="+llm.Endpoint,
			meetllm.EnvModel+"="+llm.Model,
			meetllm.EnvTimeoutMS+"="+strconv.Itoa(llm.TimeoutMS),
			meetllm.EnvUsageFile+"="+usageFile,
			meetllm.EnvMaxTokens+"="+strconv.Itoa(maxTokens),
			meetllm.EnvSlotsDir+"="+slotsDir,
			meetllm.EnvMaxInFlight+"="+strconv.Itoa(llm.MaxInFlight),
		)
		if llm.APIKey != "" {
			env = append(env, meetllm.EnvAPIKey+"="+llm.APIKey)
		}
	case config.MeetingsLLMModeAnthropic:
		env = append(env, "SPEK_MEETING_LLM=anthropic", "ANTHROPIC_API_KEY="+llm.AnthropicKey,
			"SPEK_MEETING_LLM_TIMEOUT_MS="+strconv.Itoa(llm.TimeoutMS))
		if llm.AnthropicModel != "" {
			env = append(env, "SPEK_MEETING_MODEL="+llm.AnthropicModel)
		}
	default:
		env = append(env, "SPEK_MEETING_LLM=none")
	}
	return env
}
