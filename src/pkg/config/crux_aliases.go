package config

import cruxconfig "github.com/hivecommons/hive/crux/config"

type (
	AgentBobConfig             = cruxconfig.AgentBobConfig
	AgentConfig                = cruxconfig.AgentConfig
	AgentRuntimeConfig         = cruxconfig.AgentRuntimeConfig
	SessionRecycleConfig       = cruxconfig.SessionRecycleConfig
	AgentRuntimeGovernorConfig = cruxconfig.AgentRuntimeGovernorConfig
	AgentSandboxConfig         = cruxconfig.AgentSandboxConfig
	AgentSandboxOverride       = cruxconfig.AgentSandboxOverride
	BackendsConfig             = cruxconfig.BackendsConfig
	BobConfig                  = cruxconfig.BobConfig
	ChannelConfig              = cruxconfig.ChannelConfig
	ConnectionAuth             = cruxconfig.ConnectionAuth
	ConnectionConfig           = cruxconfig.ConnectionConfig
	DefinitionSourceConfig     = cruxconfig.DefinitionSourceConfig
	GatewayConfig              = cruxconfig.GatewayConfig
	InferenceAuthConfig        = cruxconfig.InferenceAuthConfig
	KickLimitsConfig           = cruxconfig.KickLimitsConfig
	LiteLLMConfig              = cruxconfig.LiteLLMConfig
	LiteLLMRouteSource         = cruxconfig.LiteLLMRouteSource
	PRFollowUpConfig           = cruxconfig.PRFollowUpConfig
	PromptSourceConfig         = cruxconfig.PromptSourceConfig
	ProviderBudgetConfig       = cruxconfig.ProviderBudgetConfig
	ProviderRotationConfig     = cruxconfig.ProviderRotationConfig
	ProviderStatusConfig       = cruxconfig.ProviderStatusConfig
	ProvidersConfig            = cruxconfig.ProvidersConfig
	ReentrantTurnConfig        = cruxconfig.ReentrantTurnConfig
	RepeatFailureConfig        = cruxconfig.RepeatFailureConfig
	ReviewHead                 = cruxconfig.ReviewHead
	ReviewModelPoolEntry       = cruxconfig.ReviewModelPoolEntry
	ReviewModelsConfig         = cruxconfig.ReviewModelsConfig
	RotationConfig             = cruxconfig.RotationConfig
	SandboxJobConfig           = cruxconfig.SandboxJobConfig
	SandboxJobResources        = cruxconfig.SandboxJobResources
	SandboxJobToleration       = cruxconfig.SandboxJobToleration
	SandboxJobVolume           = cruxconfig.SandboxJobVolume
	StatsDisplayEntry          = cruxconfig.StatsDisplayEntry
	ToolApprovalConfig         = cruxconfig.ToolApprovalConfig
	ToolApprovalRule           = cruxconfig.ToolApprovalRule
	ToolRule                   = cruxconfig.ToolRule
	ToolsConfig                = cruxconfig.ToolsConfig
	TrajectoryConfig           = cruxconfig.TrajectoryConfig
	TurnConfig                 = cruxconfig.TurnConfig
	VarDef                     = cruxconfig.VarDef
	WatchdogConfig             = cruxconfig.WatchdogConfig
	WatchdogLivenessConfig     = cruxconfig.WatchdogLivenessConfig
	WatchdogReadinessConfig    = cruxconfig.WatchdogReadinessConfig
	WatchdogRestartConfig      = cruxconfig.WatchdogRestartConfig
)

const (
	BobAPIKeyEnvVar                     = cruxconfig.BobAPIKeyEnvVar
	BobApprovalModeFlag                 = cruxconfig.BobApprovalModeFlag
	BobApprovalModeYolo                 = cruxconfig.BobApprovalModeYolo
	BobAuthMethodFlag                   = cruxconfig.BobAuthMethodFlag
	BobAuthTypeAPIKey                   = cruxconfig.BobAuthTypeAPIKey
	BobAuthTypeEnvVar                   = cruxconfig.BobAuthTypeEnvVar
	BobAutoApproveFlag                  = cruxconfig.BobAutoApproveFlag
	BobLegacyDisableAutoUpdateKey       = cruxconfig.BobLegacyDisableAutoUpdateKey
	BobLegacyDisableUpdateNagKey        = cruxconfig.BobLegacyDisableUpdateNagKey
	BobLegacyGeneralKey                 = cruxconfig.BobLegacyGeneralKey
	BobSettingsAuthKey                  = cruxconfig.BobSettingsAuthKey
	BobSettingsDirMode                  = cruxconfig.BobSettingsDirMode
	BobSettingsEnforcedTypeKey          = cruxconfig.BobSettingsEnforcedTypeKey
	BobSettingsFileMode                 = cruxconfig.BobSettingsFileMode
	BobSettingsRelPath                  = cruxconfig.BobSettingsRelPath
	BobSettingsSecurityKey              = cruxconfig.BobSettingsSecurityKey
	BobSettingsSelectedTypeKey          = cruxconfig.BobSettingsSelectedTypeKey
	BobStateDirName                     = cruxconfig.BobStateDirName
	BobTrustFlag                        = cruxconfig.BobTrustFlag
	BobV2APIKeyEnvVar                   = cruxconfig.BobV2APIKeyEnvVar
	BobV2AutoUpdateKey                  = cruxconfig.BobV2AutoUpdateKey
	BobV2ProviderHarness                = cruxconfig.BobV2ProviderHarness
	BobV2ProviderKey                    = cruxconfig.BobV2ProviderKey
	BobV2SettingsRelPath                = cruxconfig.BobV2SettingsRelPath
	BobV2ShellKey                       = cruxconfig.BobV2ShellKey
	CadenceScopeAggregate               = cruxconfig.CadenceScopeAggregate
	CadenceScopePerRepo                 = cruxconfig.CadenceScopePerRepo
	ChannelTypeKick                     = cruxconfig.ChannelTypeKick
	ClaudeBackend                       = cruxconfig.ClaudeBackend
	ClaudeEffortHigh                    = cruxconfig.ClaudeEffortHigh
	ClaudeEffortLow                     = cruxconfig.ClaudeEffortLow
	ClaudeEffortMax                     = cruxconfig.ClaudeEffortMax
	ClaudeEffortMedium                  = cruxconfig.ClaudeEffortMedium
	ClaudeEffortXHigh                   = cruxconfig.ClaudeEffortXHigh
	DefaultAgentsGitHubAPIHourlyCap     = cruxconfig.DefaultAgentsGitHubAPIHourlyCap
	DefaultBobAPIKeyEnv                 = cruxconfig.DefaultBobAPIKeyEnv
	DefaultBobAPIKeyFile                = cruxconfig.DefaultBobAPIKeyFile
	DefaultBusyNoActivityThreshold      = cruxconfig.DefaultBusyNoActivityThreshold
	DefaultContinuousBudgetPct          = cruxconfig.DefaultContinuousBudgetPct
	DefaultContinuousCooldown           = cruxconfig.DefaultContinuousCooldown
	DefaultContinuousTurnCeiling        = cruxconfig.DefaultContinuousTurnCeiling
	DefaultGitHubAgentReserveFloor      = cruxconfig.DefaultGitHubAgentReserveFloor
	DefaultLiteLLMAPIKeyEnv             = cruxconfig.DefaultLiteLLMAPIKeyEnv
	DefaultLiteLLMAPIKeyFile            = cruxconfig.DefaultLiteLLMAPIKeyFile
	DefaultMaxIssuesPerKick             = cruxconfig.DefaultMaxIssuesPerKick
	DefaultMaxPRsPerKick                = cruxconfig.DefaultMaxPRsPerKick
	DefaultProviderBudgetProbeIntervalS = cruxconfig.DefaultProviderBudgetProbeIntervalS
	DefaultRepeatFailureNoDataAfterS    = cruxconfig.DefaultRepeatFailureNoDataAfterS
	DefaultRepeatFailureThreshold       = cruxconfig.DefaultRepeatFailureThreshold
	DefaultRepeatFailureWindowS         = cruxconfig.DefaultRepeatFailureWindowS
	ExplainLinePrefix                   = cruxconfig.ExplainLinePrefix
	ExplainModeBrief                    = cruxconfig.ExplainModeBrief
	ExplainModeEnvVar                   = cruxconfig.ExplainModeEnvVar
	ExplainModeFull                     = cruxconfig.ExplainModeFull
	ExplainModeOff                      = cruxconfig.ExplainModeOff
	FieldOwnerOperator                  = cruxconfig.FieldOwnerOperator
	FieldOwnerPack                      = cruxconfig.FieldOwnerPack
	FieldOwnerSpec                      = cruxconfig.FieldOwnerSpec
	GatewayKindCustom                   = cruxconfig.GatewayKindCustom
	LegacyLiteLLMGatewayName            = cruxconfig.LegacyLiteLLMGatewayName
	GatewayKindGroq                     = cruxconfig.GatewayKindGroq
	GatewayKindVLLM                     = cruxconfig.GatewayKindVLLM
	GatewayKindLLMD                     = cruxconfig.GatewayKindLLMD
	GatewayKindLiteLLM                  = cruxconfig.GatewayKindLiteLLM
	GatewayKindOpenRouter               = cruxconfig.GatewayKindOpenRouter
	GatewayKindWatsonx                  = cruxconfig.GatewayKindWatsonx
	JevModeAssist                       = cruxconfig.JevModeAssist
	JevModeOff                          = cruxconfig.JevModeOff
	KickListUnlimited                   = cruxconfig.KickListUnlimited
	KickLogSourceAuto                   = cruxconfig.KickLogSourceAuto
	KickLogSourceEnvVar                 = cruxconfig.KickLogSourceEnvVar
	KickLogSourceScrollback             = cruxconfig.KickLogSourceScrollback
	KickLogSourceTranscript             = cruxconfig.KickLogSourceTranscript
	LiteLLMEndpointEnv                  = cruxconfig.LiteLLMEndpointEnv
	LiteLLMRouteNone                    = cruxconfig.LiteLLMRouteNone
	LiteLLMRouteGateway                 = cruxconfig.LiteLLMRouteGateway
	LiteLLMRouteLegacyEnv               = cruxconfig.LiteLLMRouteLegacyEnv
	LiteLLMRouteLegacyYAML              = cruxconfig.LiteLLMRouteLegacyYAML
	KickLogSourceOriginEnv              = cruxconfig.KickLogSourceOriginEnv
	KickLogSourceOriginConfig           = cruxconfig.KickLogSourceOriginConfig
	KickLogSourceOriginDefault          = cruxconfig.KickLogSourceOriginDefault
	LocalLiteLLMProxyPort               = cruxconfig.LocalLiteLLMProxyPort
	MinKickListCap                      = cruxconfig.MinKickListCap
	MaxKickListCap                      = cruxconfig.MaxKickListCap
	ProxyInjectGHAuthEnv                = cruxconfig.ProxyInjectGHAuthEnv
	ProxyInjectGHAuthOnValue            = cruxconfig.ProxyInjectGHAuthOnValue
	ReviewModelsFallbackPinned          = cruxconfig.ReviewModelsFallbackPinned
	ReviewModelsFallbackRequiresHuman   = cruxconfig.ReviewModelsFallbackRequiresHuman
	ReviewModelsFallbackSkip            = cruxconfig.ReviewModelsFallbackSkip
	RotationHeadroomSourceBuiltin       = cruxconfig.RotationHeadroomSourceBuiltin
	RotationHeadroomSourceCCLeft        = cruxconfig.RotationHeadroomSourceCCLeft
	SandboxRuntimeJob                   = cruxconfig.SandboxRuntimeJob
	SandboxRuntimePodman                = cruxconfig.SandboxRuntimePodman
	SelfMergeMinACMMLevel               = cruxconfig.SelfMergeMinACMMLevel
	WritableBobAPIKeyFile               = cruxconfig.WritableBobAPIKeyFile
	WritableLiteLLMAPIKeyFile           = cruxconfig.WritableLiteLLMAPIKeyFile
	WritableSecretsDir                  = cruxconfig.WritableSecretsDir
)

var (
	KickLogSources            = cruxconfig.KickLogSources
	ValidKickLogSource        = cruxconfig.ValidKickLogSource
	KickLogSourceEnvOverride  = cruxconfig.KickLogSourceEnvOverride
	CLIBackends               = cruxconfig.CLIBackends
	InferenceBackends         = cruxconfig.InferenceBackends
	PresetToMode              = cruxconfig.PresetToMode
	ReasoningEffortsByBackend = cruxconfig.ReasoningEffortsByBackend
	ToolPresets               = cruxconfig.ToolPresets
	ValidSandboxRuntimes      = cruxconfig.ValidSandboxRuntimes
)

var (
	APIKeySHA256              = cruxconfig.APIKeySHA256
	AllowSecretFileRoot       = cruxconfig.AllowSecretFileRoot
	IsCLIBackend              = cruxconfig.IsCLIBackend
	IsInferenceBackend        = cruxconfig.IsInferenceBackend
	LiteLLMChatCompletionsURL = cruxconfig.LiteLLMChatCompletionsURL
	LocalLiteLLMProxyURL      = cruxconfig.LocalLiteLLMProxyURL
	NormalizeKickLogSource    = cruxconfig.NormalizeKickLogSource
	ProxyInjectGHAuth         = cruxconfig.ProxyInjectGHAuth
	QualifyRepo               = cruxconfig.QualifyRepo
	ReviewHeadKey             = cruxconfig.ReviewHeadKey
	SecretFilePathAllowed     = cruxconfig.SecretFilePathAllowed
	SecretFileRoots           = cruxconfig.SecretFileRoots
	SetSecretFileRootsForTest = cruxconfig.SetSecretFileRootsForTest
	SupportedBackends         = cruxconfig.SupportedBackends
	ValidEffort               = cruxconfig.ValidEffort
	ValidateReasoningEffort   = cruxconfig.ValidateReasoningEffort
	WildcardMatch             = cruxconfig.WildcardMatch
)

// origin_labels.go (moved to the leaf with pkg/proxy's migration).
const (
	DefaultReviewBacklogLabel = cruxconfig.DefaultReviewBacklogLabel
	OriginLabelPrefix         = cruxconfig.OriginLabelPrefix
	GitHubLabelDescriptionMax = cruxconfig.GitHubLabelDescriptionMax
	OriginReviewerLabel       = cruxconfig.OriginReviewerLabel
	LegacyOriginReviewLabel   = cruxconfig.LegacyOriginReviewLabel
	LegacyReviewBacklogLabel  = cruxconfig.LegacyReviewBacklogLabel
	OriginSourceReviewer      = cruxconfig.OriginSourceReviewer
	OriginSourceSelfReport    = cruxconfig.OriginSourceSelfReport
	OriginSourceGovernor      = cruxconfig.OriginSourceGovernor
	OriginSourceDashboard     = cruxconfig.OriginSourceDashboard
	OriginSourceForge         = cruxconfig.OriginSourceForge
	OriginSourceConvergence   = cruxconfig.OriginSourceConvergence
)

var (
	KnownOriginLanes        = cruxconfig.KnownOriginLanes
	OriginLabelRenames      = cruxconfig.OriginLabelRenames
	CanonicalOriginLabel    = cruxconfig.CanonicalOriginLabel
	OriginLabel             = cruxconfig.OriginLabel
	IsOriginLabel           = cruxconfig.IsOriginLabel
	OriginSourceOf          = cruxconfig.OriginSourceOf
	OriginLaneFromAgent     = cruxconfig.OriginLaneFromAgent
	AgentLaneLabel          = cruxconfig.AgentLaneLabel
	CanonicalAgentLaneLabel = cruxconfig.CanonicalAgentLaneLabel
	OriginLabelForAgent     = cruxconfig.OriginLabelForAgent
	IsKnownOriginLane       = cruxconfig.IsKnownOriginLane
	IsAgentLaneLabel        = cruxconfig.IsAgentLaneLabel
	FirstOriginLabel        = cruxconfig.FirstOriginLabel
	WithOriginLabel         = cruxconfig.WithOriginLabel
	WithIssueOriginLabel    = cruxconfig.WithIssueOriginLabel
	WithAgentLaneLabel      = cruxconfig.WithAgentLaneLabel
	OriginLabelDescription  = cruxconfig.OriginLabelDescription
	KnownOriginSources      = cruxconfig.KnownOriginSources
)

// Per-agent blocks and helpers added on v6 alongside AgentConfig fields that
// live in the leaf: the advisor override, task-MCP options, the standby block,
// the experimenter role and the mention channel.
type (
	AdvisorOverride    = cruxconfig.AdvisorOverride
	AgentTaskMCPConfig = cruxconfig.AgentTaskMCPConfig
	StandbyConfig      = cruxconfig.StandbyConfig
)

const (
	ChannelTypeMention      = cruxconfig.ChannelTypeMention
	DefaultExperimentEngine = cruxconfig.DefaultExperimentEngine
	ExperimentEngineNous    = cruxconfig.ExperimentEngineNous
	RoleExperimenter        = cruxconfig.RoleExperimenter
	StandbyDefaultFloor     = cruxconfig.StandbyDefaultFloor
	StandbyTierT1           = cruxconfig.StandbyTierT1
	StandbyTierT2           = cruxconfig.StandbyTierT2
	StandbyTierT3           = cruxconfig.StandbyTierT3
	StandbyTierUnknown      = cruxconfig.StandbyTierUnknown
)

var NormalizeStandbyTier = cruxconfig.NormalizeStandbyTier

var DefaultRepeatFailureExclude = cruxconfig.DefaultRepeatFailureExclude
