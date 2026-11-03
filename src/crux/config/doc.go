// Package config is the stdlib-only home of the agent runtime's
// configuration types: AgentConfig and the AgentRuntimeConfig /
// AgentRuntimeGovernorConfig surfaces it belongs to, with the gateway,
// LiteLLM, sandbox, tool-approval, turn and watchdog types they embed and
// the defaults they apply (Crux Stage 2 prerequisite 1,
// hivecommons/hive#13536; src/docs/design/crux.md).
//
// It is named config so that a ring package's migration from pkg/config is
// an import-path swap. pkg/config imports it as cruxconfig and re-exports
// every symbol as an alias (pkg/config/crux_aliases.go), so hive.yaml
// loading, validation and every YAML key position are unchanged: Crux owns
// the types, Hive aliases them. Loading, overlays, Config itself and any
// validation that needs YAML or the ACMM packs stay in pkg/config.
//
// Methods are declared here because Go cannot attach a method to an alias
// of another package's type; the two that needed a host-only type were
// reshaped instead (ReviewModelsConfig.Select is config.SelectReviewModel,
// and ContinuousInMode takes the ContinuousCadence interface).
package config
