package main

import (
	"fmt"

	"github.com/kubestellar/hive/pkg/integrated"
)

type runtimeOwnerIntent string

const (
	runtimeOwnerNormalHive      runtimeOwnerIntent = "normal_hive"
	runtimeOwnerLegacyScheduler runtimeOwnerIntent = "legacy_scheduler"
	runtimeOwnerHosted          runtimeOwnerIntent = "hosted_controller"
)

// configuredRuntimeOwnerIntent derives ownership only from the current
// authoritative integrated config. Runtime lease records are observations and
// must never keep an obsolete ownership mode logically configured.
func configuredRuntimeOwnerIntent(config integrated.Config) runtimeOwnerIntent {
	if config.ExecutionMode == integrated.ExecutionHosted {
		return runtimeOwnerHosted
	}
	if integrated.UsesNormalHiveRuntime(config) {
		return runtimeOwnerNormalHive
	}
	return runtimeOwnerLegacyScheduler
}

type runtimeOwnerObservation struct {
	NormalLeaseRecordPresent bool
	NormalOwnerLive          bool
	LegacyOwnerLive          bool
	LegacyServiceRegistered  bool
	ObservedOwner            string
	Conflict                 bool
	Message                  string
}

func inspectRuntimeOwner(stateDir string, config integrated.Config, daemon integratedDaemonStatus) runtimeOwnerObservation {
	normalOwner, recordPresent, normalLive := normalVisualDaemonObserved(stateDir)
	legacyLive := daemon.RuntimeRunning
	legacyRegistered := daemon.Service != nil && daemon.Service.Managed
	intent := configuredRuntimeOwnerIntent(config)
	observation := runtimeOwnerObservation{
		NormalLeaseRecordPresent: recordPresent,
		NormalOwnerLive:          normalLive,
		LegacyOwnerLive:          legacyLive,
		LegacyServiceRegistered:  legacyRegistered,
	}
	switch {
	case normalLive && legacyLive:
		observation.ObservedOwner = "conflict"
	case normalLive:
		observation.ObservedOwner = string(runtimeOwnerNormalHive)
	case legacyLive:
		observation.ObservedOwner = string(runtimeOwnerLegacyScheduler)
	case legacyRegistered:
		observation.ObservedOwner = "legacy_scheduler_registered"
	case recordPresent:
		observation.ObservedOwner = "stale_normal_hive_record"
	default:
		observation.ObservedOwner = "none"
	}
	observation.Conflict = normalLive && intent != runtimeOwnerNormalHive ||
		(legacyLive || legacyRegistered) && intent != runtimeOwnerLegacyScheduler

	switch {
	case intent == runtimeOwnerHosted:
		observation.Message = "the repository-hosted controller is the configured runtime owner"
	case normalLive && legacyLive:
		observation.Message = "both ordinary Hive and the legacy scheduler appear live; stop the legacy scheduler and restart ordinary Hive/dashboard"
	case intent == runtimeOwnerNormalHive && legacyLive:
		observation.Message = "the legacy scheduler is live but ordinary Hive/dashboard is configured to own Visual repair; run hive stop, then restart ordinary Hive/dashboard"
	case intent == runtimeOwnerNormalHive && legacyRegistered:
		observation.Message = "a legacy scheduler registration remains but ordinary Hive/dashboard is configured to own Visual repair; run hive stop, then restart ordinary Hive/dashboard"
	case intent == runtimeOwnerNormalHive && normalLive:
		observation.Message = fmt.Sprintf("ordinary Hive/dashboard is the configured and live runtime owner in pid %d", normalOwner.PID)
	case intent == runtimeOwnerNormalHive:
		observation.Message = "ordinary Hive/dashboard is the configured runtime owner but is not live; restart ordinary Hive/dashboard (do not run hive start)"
	case normalLive:
		observation.Message = fmt.Sprintf("ordinary Hive/dashboard pid %d still owns the runtime after configuration changed to the legacy scheduler; restart ordinary Hive/dashboard, then run hive start", normalOwner.PID)
	case legacyLive:
		observation.Message = "the legacy scheduler is the configured and live runtime owner"
	case legacyRegistered:
		observation.Message = "the legacy scheduler is configured and registered but its runtime is not live; run hive start to repair or restart it"
	case recordPresent:
		observation.Message = "an inactive ordinary-Hive owner record was observed; the current config requires the legacy scheduler and hive start may replace the stale record"
	default:
		observation.Message = "the legacy scheduler is configured but no runtime owner is live; run hive start"
	}
	return observation
}

func runtimeOwnerStatusFields(intent runtimeOwnerIntent, observation runtimeOwnerObservation) map[string]any {
	return map[string]any{
		"runtime_owner_intent":        string(intent),
		"runtime_owner_observed":      observation.ObservedOwner,
		"runtime_owner_conflict":      observation.Conflict,
		"runtime_owner_message":       observation.Message,
		"normal_lease_record_present": observation.NormalLeaseRecordPresent,
		"normal_owner_live":           observation.NormalOwnerLive,
		"legacy_owner_live":           observation.LegacyOwnerLive,
		"legacy_service_registered":   observation.LegacyServiceRegistered,
	}
}

func runtimeOwnerDoctorCheck(intent runtimeOwnerIntent, observation runtimeOwnerObservation) doctorCheck {
	aligned := !observation.Conflict
	if intent == runtimeOwnerNormalHive {
		aligned = aligned && observation.NormalOwnerLive
	}
	return doctorCheck{Name: "runtime_owner", OK: aligned, Message: fmt.Sprintf("configured=%s observed=%s; %s", intent, observation.ObservedOwner, observation.Message)}
}

// preflightLegacySchedulerStart is deliberately read-only. Every path that
// can register or launch the legacy scheduler calls it before service mutation.
func preflightLegacySchedulerStart(stateDir string, config integrated.Config) error {
	switch configuredRuntimeOwnerIntent(config) {
	case runtimeOwnerHosted:
		return fmt.Errorf("hosted installations are owned by the repository controller and cannot start a local scheduler")
	case runtimeOwnerNormalHive:
		legacyRuntime := readIntegratedDaemonRuntimeStatus(stateDir)
		_, serviceExists, serviceErr := readDaemonServiceSpec(stateDir)
		if legacyRuntime.Running || serviceExists || serviceErr != nil {
			return fmt.Errorf("ordinary Hive/dashboard is the configured runtime owner for local Visual %s automation, but a legacy scheduler runtime or registration remains; run hive stop, then restart ordinary Hive/dashboard (do not run hive start)", config.Automation)
		}
		return fmt.Errorf("ordinary Hive/dashboard is the configured runtime owner for local Visual %s automation; restart ordinary Hive/dashboard (do not run hive start)", config.Automation)
	}
	owner, _, live := normalVisualDaemonObserved(stateDir)
	if live {
		return fmt.Errorf("ordinary Hive/dashboard pid %d still owns this runtime after configuration changed to the legacy scheduler; restart ordinary Hive/dashboard, then retry hive start", owner.PID)
	}
	return nil
}
