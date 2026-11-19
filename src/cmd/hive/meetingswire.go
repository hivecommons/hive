package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/keystore"
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
// the optional transcription and TTS service tokens from the keystore. A secret that cannot be read is logged and left unset:
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
	ttsToken, err := m.TTSToken(ctx)
	if err != nil && logger != nil {
		logger.Warn("[meetings] TTS service token unreadable; Discord replies are not spoken", "error", err)
	}
	return quorumproc.Options{
		Binary:          m.QuorumBinary(),
		Listen:          m.QuorumListen(),
		VexaURL:         m.VexaURL(),
		VexaAPIKey:      vexaKey,
		DiscordToken:    discordToken,
		TranscribeURL:   m.TranscribeURLOrEmpty(),
		TranscribeToken: transcribeToken,
		TTSURL:          m.TTSURLOrEmpty(),
		TTSToken:        ttsToken,
		// Discord speaks in the persona's voice by default.
		TTSVoice: m.Persona.Resolve().Voice,
		Logger:   logger,
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
func wireMeetingsManager(cfg *config.Config, sup quorumSupervisor, audit func(user, action, detail, agent string), logger *slog.Logger) (*meetings.Manager, *meetings.Store) {
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
		// meeting.join's voice_id: the persona's voice on every platform.
		VoiceID:      cfg.Meetings.Persona.Resolve().Voice,
		LobbyTimeout: time.Duration(cfg.Meetings.LobbyTimeoutSOrDefault()) * time.Second,
		Logger:       logger,
		// Discord `/hive join` requests from a person the owner chose to
		// always approve (matched by Discord user id) join at once; every
		// such approval is audited.
		Trust: meetings.NewTrustStore(cfg.Meetings.DataDirOrDefault()),
		OnAutoApproved: func(m meetings.Meeting, who meetings.Person) {
			if audit != nil {
				audit("hive", "meeting_request_auto_approve",
					"meeting="+m.ID+", platform="+m.Platform+", requester_id="+who.ID+", requester_name="+who.Name+", guild="+m.GuildID+", channel="+m.ChannelID, "")
			}
		},
		// Vexa fixes speaking ability at join time, so with voice-on-address
		// allowed every bot starts able to speak; the meeting's voice toggle
		// still decides whether hive does.
		VoiceOnAddress: cfg.Meetings.VoiceOnAddressEnabled(),
		// The persona's fixed spoken lines (short intro, acknowledgements,
		// goodbye) are pre-rendered when a meeting that can speak goes live,
		// so her first words do not wait for speech synthesis.
		FixedSpokenLines: func(meetings.Meeting) []string {
			return betty.FixedSpokenLines(cfg.Meetings.Persona.Resolve())
		},
	})
	if err != nil {
		if logger != nil {
			logger.Error("[meetings] meeting manager not created", "error", err)
		}
		return nil, nil
	}
	// Discord `/hive join` requests arrive on the all-meetings stream. Each
	// waits for the owner in the dashboard unless its requester's Discord
	// user id is on the trusted list (the owner's "Always approve this
	// person" on an earlier request).
	mgr.StartRequests()
	if n := mgr.Resume(); n > 0 && logger != nil {
		logger.Info("[meetings] re-attached to meetings", "count", n)
	}
	return mgr, store
}

// meetingSpekProbeArgs checks once per installed binary that spektacular
// has the live `meeting` command (hivecommons/spektacular#126); a release
// without it answers "unknown command" and live mode stays off without a
// child restart loop (hivecommons/hive#18630).
var meetingSpekProbeArgs = []string{"meeting", "--help"}

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
	beadStores func() map[string]*beads.Store, research live.ResearchDispatcher, logger *slog.Logger,
	hiveStatus hiveStatusSource) *live.Runner {
	if cfg == nil || mgr == nil || store == nil {
		return nil
	}
	m := cfg.Meetings
	binary := cfg.Runs.Spektacular.BinaryOrDefault()
	llm := cfg.ResolveMeetingsLLM(context.Background())
	hiveBin, err := os.Executable()
	if err != nil && (llm.Backend == config.MeetingsLLMModeOpenAI || llm.Backend == config.MeetingsLLMModeAgentBackend) {
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
			return spektacular.StreamExec(binary, meetingSpekArgs(mt, filepath.Join(store.Dir(), mt.ID)), spektacular.StreamOptions{
				Logger: logger, Env: env, Probe: meetingSpekProbeArgs,
			})
		},
		SpekMode: llm.Label(),
		UsageFile: func(id string) string {
			// `hive meeting-llm` (OpenAI-compatible) and the persona's own
			// calls (either backend) write the meeting's usage ledger.
			if llm.Backend != config.MeetingsLLMModeOpenAI && llm.Backend != config.MeetingsLLMModeAnthropic &&
				llm.Backend != config.MeetingsLLMModeAgentBackend {
				return ""
			}
			return meetingUsageFile(store, id)
		},
		Research: research,
		Notes:    notes,
		Policy: live.Policy{
			ChatPostsPerMinute:   m.ChatPostsPerMinuteOrDefault(),
			ChatAnswerBurst:      m.ChatAnswerBurstOrDefault(),
			ResearchPerMeeting:   m.ResearchPerMeetingOrDefault(),
			VoiceOnAddress:       m.VoiceOnAddressEnabled(),
			MaxTokensPerMeeting:  m.Budget.MaxTokensPerMeetingOrDefault(),
			MaxIntentsPerMeeting: m.Budget.MaxIntentsPerMeetingOrDefault(),
		},
		Ledger:      live.NewLedger(m.Budget.MaxTokensPerHourOrDefault(), m.Budget.MaxTokensPerDayOrDefault()),
		Logger:      logger,
		Persona:     meetingPersonaFactory(cfg, persona, llm, store, kapi, beadStores, slotsDir, maxTokens, research != nil, logger, hiveStatus),
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
// addressed question she cannot answer well become a deep dive. hiveStatus,
// when set, is the dashboard's live hive state (open issues and PRs,
// actionable, held and blocked counts, agents): questions about the hive
// itself are answered from it in-process, never sent to research
// (hivecommons/hive#18590 finding 9).
func meetingPersonaFactory(cfg *config.Config, persona config.ResolvedMeetingsPersona, llm config.ResolvedMeetingsLLM,
	store *meetings.Store, kapi *knowledge.KnowledgeAPI, beadStores func() map[string]*beads.Store,
	slotsDir string, maxTokens int, canResearch bool, logger *slog.Logger,
	hiveStatus hiveStatusSource) func(meetings.Meeting) *betty.Engine {
	if !persona.Enabled {
		return nil
	}
	var retrievers []betty.Retriever
	if hiveStatus != nil {
		retrievers = append(retrievers, betty.HiveStatusRetriever{Brief: hiveStatus.HiveStatusBrief, Item: hiveStatus.HiveItemBrief})
	}
	if kapi != nil {
		// Vaults and ready git sources: the hive's own docs are a git
		// source, which FileStores alone does not list.
		stores := betty.KnowledgeStores(kapi)
		retrievers = append(retrievers, betty.KnowledgeRetriever{Stores: stores})
		if cfg.Knowledge.CodeMaps.IsEnabled() {
			retrievers = append(retrievers, betty.CodeMapRetriever{Stores: stores})
		}
	}
	if beadStores != nil {
		retrievers = append(retrievers, betty.BeadRetriever{Stores: beadStores})
	}
	// Customers the question names (#18742): read at call time, so it is
	// empty while customers are off or not yet wired.
	retrievers = append(retrievers, betty.PeopleRetriever{Directory: meetingsPeopleDirectory})
	client := &http.Client{}
	// Web answers (opt-in through searxng_url): SearXNG and the keyless
	// direct sources, behind a guard that keeps questions about this hive,
	// its org and its repos off the public web. The client carries no
	// credentials.
	private := append([]string{cfg.Project.Org, cfg.Project.Name, cfg.Project.PrimaryRepo}, cfg.Project.Repos...)
	retrievers = append(retrievers, betty.PublicWebRetrievers(betty.PublicWeb{
		SearxngURL: persona.SearxngURL, Client: client, Logger: logger,
		Guard: betty.NewWebGuard(persona.Name, persona.Aliases, private),
	})...)
	// Product questions are answered from the docs brief (betty/product.go).
	retrievers = betty.WithProductBrief(retrievers)
	// A model gateway with a private CA (its ca_bundle) gets its own client;
	// one that cannot be built leaves the persona model-less, never
	// unverified.
	modelClient, clientErr := meetllm.HTTPClient(llm.CABundle)
	if clientErr != nil && logger != nil && llm.Backend == config.MeetingsLLMModeOpenAI {
		logger.Error("[meetings] persona model client not built; the persona answers without a model", "backend", llm.AgentBackend, "error", clientErr)
	}
	return func(mt meetings.Meeting) *betty.Engine {
		var model betty.LLM
		mc := meetllm.Config{
			// The engine bounds each call itself (2.5 s unprompted, 4 s
			// addressed, 800 ms restyle); this is the outer cap.
			Timeout:   betty.DefaultAddressedTimeout,
			UsageFile: meetingUsageFile(store, mt.ID), MaxTokens: maxTokens,
			SlotsDir: slotsDir, MaxInFlight: llm.MaxInFlight,
			// Several requests are answered at once, each call reserving
			// budget against the others in flight (#18590).
			Gate: meetllm.NewGate(llm.PerMeetingInFlight),
		}
		switch llm.Backend {
		case config.MeetingsLLMModeOpenAI:
			mc.Endpoint, mc.Model, mc.APIKey = llm.Endpoint, llm.Model, llm.APIKey
			if clientErr == nil {
				model = betty.MeetLLM{Client: modelClient, Config: mc}
			}
		case config.MeetingsLLMModeAnthropic:
			mc.Provider, mc.Model, mc.APIKey = meetllm.ProviderAnthropic, llm.Model, llm.AnthropicKey
			model = betty.MeetLLM{Client: client, Config: mc}
		}
		settings := betty.SettingsFrom(persona)
		settings.ListeningTone = cfg.Meetings.ListeningToneEnabled()
		if llm.Backend == config.MeetingsLLMModeAgentBackend {
			if m, ok := meetingCLIModel(llm, mc, mt.ID, logger); ok {
				model = m
				applyMeetingCLITimeouts(&settings, llm.CLI)
			}
		}
		return betty.New(settings, betty.Deps{LLM: model, Retrievers: retrievers, Logger: logger, CanResearch: canResearch})
	}
}

// meetingCLIModel is the persona's model on an agent backend
// (hivecommons/hive#18517, #18549), in the backend's no-tools completion
// profile with the agents' sign-in: a pool of warm CLIs for the meeting
// (one per concurrent answer, #18590), the first started now so the first
// question finds it ready, on a backend with a warm mode; one CLI per call
// otherwise. It reports false when the backend has no
// profile.
func meetingCLIModel(llm config.ResolvedMeetingsLLM, mc meetllm.Config, meetingID string, logger *slog.Logger) (betty.MeetLLM, bool) {
	mc.Provider, mc.Backend, mc.Model = meetllm.ProviderAgentBackend, llm.AgentBackend, llm.Model
	mc.CLIBinary, mc.CLIHome, mc.RecycleTurns = llm.CLI.Binary, llm.CLI.Home, llm.CLI.RecycleTurns
	mc.Timeout = time.Duration(llm.CLI.AddressedTimeoutMS) * time.Millisecond
	mc.Getenv = meetingCLIGetenv(os.Getenv)
	if !meetllm.SupportsWarm(llm.AgentBackend) {
		if !meetllm.HasCompletionProfile(llm.AgentBackend) {
			if logger != nil {
				logger.Error("[meetings] persona model not started; the backend has no completion profile", "meeting", meetingID, "backend", llm.AgentBackend)
			}
			return betty.MeetLLM{}, false
		}
		return betty.MeetLLM{Config: mc}, true
	}
	pool, err := meetllm.NewCLIPool(mc, meetingCLIPoolSize(llm, mc))
	if err != nil {
		if logger != nil {
			logger.Error("[meetings] persona model not started", "meeting", meetingID, "backend", llm.AgentBackend, "error", err)
		}
		return betty.MeetLLM{}, false
	}
	if err := pool.Warm(); err != nil && logger != nil {
		// The first call retries the start.
		logger.Warn("[meetings] persona CLI not warmed", "meeting", meetingID, "backend", llm.AgentBackend, "error", err)
	}
	mc.Pool = pool
	return betty.MeetLLM{Config: mc}, true
}

// meetingCLIPoolSize is how many warm CLIs one meeting may run: its
// concurrent answers (the gate's size), else the resolved per-meeting cap.
func meetingCLIPoolSize(llm config.ResolvedMeetingsLLM, mc meetllm.Config) int {
	if mc.Gate != nil {
		return mc.Gate.Size()
	}
	return llm.PerMeetingInFlight
}

// applyMeetingCLITimeouts widens the persona's per-call timeouts to the CLI
// mode's: a warm turn is slower than an API call.
func applyMeetingCLITimeouts(s *betty.Settings, cli config.ResolvedMeetingsCLI) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	if cli.AddressedTimeoutMS > 0 {
		s.AddressedTimeout = ms(cli.AddressedTimeoutMS)
	}
	if cli.UnpromptedTimeoutMS > 0 {
		s.UnpromptedTimeout = ms(cli.UnpromptedTimeoutMS)
	}
	if cli.RestyleTimeoutMS > 0 {
		s.RestyleTimeout = ms(cli.RestyleTimeoutMS)
	}
}

// meetingClaudeOperatorToken is the operator's long-lived Claude token
// (HIVE_CLAUDE_OAUTH_TOKEN), the seam tests replace.
var meetingClaudeOperatorToken = claude.OperatorToken

// meetingCopilotTokenFile is the dashboard Copilot sign-in the agents fall
// back to (a var so tests can point it elsewhere).
var meetingCopilotTokenFile = crux.CopilotUserTokenPath

// meetingCopilotToken is the Copilot sign-in the agents get:
// COPILOT_GITHUB_TOKEN, else the dashboard device-flow token read through
// the keystore.
func meetingCopilotToken(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv(meetllm.EnvCopilotToken)); v != "" {
		return v
	}
	data, err := keystore.ReadFile(context.Background(), meetingCopilotTokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// meetingCLIGetenv is the environment a meetings CLI reads its sign-in
// from: base, with the sign-in variables set the way the agent launcher
// sets them for agents. CLAUDE_CODE_OAUTH_TOKEN is the operator's
// long-lived token only (a hive signed in with the credentials file needs
// none), and COPILOT_GITHUB_TOKEN is the agents' Copilot token. The values
// go to the CLI's environment only: hive never logs them or writes them to
// disk.
func meetingCLIGetenv(base func(string) string) func(string) string {
	return func(k string) string {
		switch k {
		case meetllm.EnvClaudeOAuthToken:
			return meetingClaudeOperatorToken()
		case meetllm.EnvCopilotToken:
			return meetingCopilotToken(base)
		}
		return base(k)
	}
}

// meetingCLISignInEnv are, per backend, the sign-in variables `hive
// meeting-llm` needs in the Spek child's environment to hand to the CLI.
var meetingCLISignInEnv = map[string][]string{
	meetllm.BackendClaude:  {"CLAUDE_CONFIG_DIR", meetllm.EnvClaudeOAuthToken},
	meetllm.BackendCopilot: {meetllm.EnvCopilotToken, "COPILOT_GH_HOST", "GH_HOST"},
	meetllm.BackendOMP:     {"OMP_AUTH_BROKER_URL", "OMP_AUTH_BROKER_TOKEN"},
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
		if llm.CABundle != "" {
			env = append(env, meetllm.EnvCABundle+"="+llm.CABundle)
		}
	case config.MeetingsLLMModeAgentBackend:
		// One CLI per prompt through `hive meeting-llm`, in the backend's
		// completion profile, with the cold-start timeout (#18517).
		env = append(env,
			"SPEK_MEETING_LLM=command",
			"SPEK_MEETING_LLM_CMD="+hiveBin+" "+meetllm.Subcommand,
			"SPEK_MEETING_LLM_TIMEOUT_MS="+strconv.Itoa(llm.CLI.OneShotTimeoutMS),
			meetllm.EnvBackend+"="+llm.AgentBackend,
			meetllm.EnvModel+"="+llm.Model,
			meetllm.EnvTimeoutMS+"="+strconv.Itoa(llm.CLI.OneShotTimeoutMS),
			meetllm.EnvUsageFile+"="+usageFile,
			meetllm.EnvMaxTokens+"="+strconv.Itoa(maxTokens),
			meetllm.EnvSlotsDir+"="+slotsDir,
			meetllm.EnvMaxInFlight+"="+strconv.Itoa(llm.MaxInFlight),
		)
		if llm.CLI.Binary != "" {
			env = append(env, meetllm.EnvCLIBinary+"="+llm.CLI.Binary)
		}
		if llm.CLI.Home != "" {
			env = append(env, meetllm.EnvCLIHome+"="+llm.CLI.Home)
		}
		// The CLI's sign-in, as the agents get it: a config-dir path, or a
		// token, kept off argv like the API keys above.
		signIn := meetingCLIGetenv(getenv)
		for _, k := range meetingCLISignInEnv[llm.AgentBackend] {
			if v := signIn(k); v != "" {
				env = append(env, k+"="+v)
			}
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

// hiveStatusSource is the dashboard's live hive state for the meeting
// attendee (*dashboard.Server): the status brief and one-item lookups.
type hiveStatusSource interface {
	HiveStatusBrief(ctx context.Context) (string, error)
	HiveItemBrief(ctx context.Context, number int) (string, bool)
}
