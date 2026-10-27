package main

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/ioscan"
)

// ioscanClassifierSink is the part of the scheduler boot installs the
// semantic classifier on.
type ioscanClassifierSink interface {
	SetClassifier(ioscan.Classifier, ioscan.Thresholds)
	SetClassifierBudgetPerEval(int)
}

// ioscanClassifierAlerter is the part of the dashboard boot raises the
// blocked-kick alert on (dashboard.Server satisfies it). nil means no
// dashboard.
type ioscanClassifierAlerter interface {
	AddSystemAlert(id, severity, message string)
	ClearSystemAlert(id string)
}

// ioscanClassifierBlockedAlertID identifies the persistent dashboard alert
// raised when the classifier is required at the current ACMM level but
// cannot run, so every kick carrying untrusted text is being withheld.
const ioscanClassifierBlockedAlertID = "ioscan-classifier-blocked"

// installIoscanClassifier builds the semantic injection classifier when the
// ACMM level (or an explicit setting) enables it. An empty
// ioscan.classifier.backend uses the OpenAI-compatible HTTP endpoint when
// one is resolved; with no endpoint it falls back to the subscription CLI the
// hive's own agents run on, when that CLI can run tool-less (#14499). A named
// backend runs the classifier through that CLI, tool-less, with the low
// per-pass budget and the daily quota. A backend that cannot run tool-less
// is refused with one log line and handled like any classifier that cannot
// be built: withheld text at fail-closed, no classifier at fail-open. When
// the classifier is fail-closed and cannot run, a persistent dashboard alert
// says that kicks are being blocked and how to fix it; the alert is cleared
// on a boot where the classifier runs or is not required.
func installIoscanClassifier(sched ioscanClassifierSink, alerts ioscanClassifierAlerter, cfg *config.Config, logger *slog.Logger, now func() time.Time) {
	acmmLevel := cfg.ACMMLevelOrZero()
	if !cfg.Ioscan.ClassifierEnabledAtLevel(acmmLevel) {
		clearIoscanClassifierAlert(alerts)
		return
	}
	classifierFailClosed := cfg.Ioscan.ClassifierFailClosedAtLevel(acmmLevel)
	cl := cfg.Ioscan.Classifier
	thresholds := ioscan.Thresholds{Warn: cl.WarnThreshold, Block: cl.BlockThreshold}
	accepted := strings.Join(ioscan.ToollessBackends(), ", ")

	backend := cl.CLIBackend()
	endpoint, apiKey, model := cfg.Governor.ResolveReviewer()
	fallback := false
	if backend == "" && endpoint == "" {
		if backend = ioscanClassifierFallbackBackend(cfg); backend != "" {
			fallback = true
		}
	}

	if backend != "" {
		// NewCLIClassifier refuses exactly the backends SupportsToolless rejects.
		classifier, cerr := ioscan.NewCLIClassifier(backend, cl.Model, cl.CLIDailyQuotaOrDefault(), now)
		if cerr != nil {
			if classifierFailClosed {
				sched.SetClassifier(ioscan.UnavailableClassifier{Err: cerr}, ioscan.Thresholds{})
				raiseIoscanClassifierAlert(alerts, acmmLevel, cerr)
			} else {
				clearIoscanClassifierAlert(alerts)
			}
			logger.Error("ioscan classifier backend refused: it cannot run with no tools, no MCP, no GitHub token and an empty working directory",
				"backend", backend, "accepted", accepted, "fail_closed", classifierFailClosed, "acmm_level", acmmLevel,
				"fix", "set ioscan.classifier.backend to one of "+accepted+", or leave it empty to use the model endpoint (Settings → Security → Input Defense)")
			return
		}
		sched.SetClassifier(ioscan.NewCachedClassifier(classifier, ioscan.DefaultClassifierCacheEntries), thresholds)
		sched.SetClassifierBudgetPerEval(cl.CLIBudgetPerEvalOrDefault())
		clearIoscanClassifierAlert(alerts)
		logger.Info("ioscan semantic classifier enabled", "backend", backend, "backend_from_agents", fallback, "model", cl.Model,
			"budget_per_eval", cl.CLIBudgetPerEvalOrDefault(), "daily_quota", cl.CLIDailyQuotaOrDefault(), "fail_closed", classifierFailClosed)
		return
	}

	if cl.Model != "" {
		model = cl.Model
	}
	if model == "" {
		model = ioscan.DefaultClassifierModel
	}
	classifier, cerr := ioscan.NewLLMClassifier(ioscan.LLMClassifierConfig{
		Endpoint: endpoint,
		APIKey:   apiKey,
		Model:    model,
	})
	switch {
	case cerr != nil && classifierFailClosed:
		// The level requires the classifier and it cannot run: withhold
		// untrusted text rather than silently scanning without it.
		sched.SetClassifier(ioscan.UnavailableClassifier{Err: cerr}, ioscan.Thresholds{})
		raiseIoscanClassifierAlert(alerts, acmmLevel, cerr)
		logger.Error("ioscan classifier required at this ACMM level but not running; untrusted text is withheld and kicks carrying it are blocked",
			"reason", cerr.Error(), "acmm_level", acmmLevel,
			"fix", "configure governor.trajectory/litellm, set ioscan.classifier.backend to one of "+accepted+", or set ioscan.classifier.enabled: false or ioscan.classifier.fail_mode: open")
	case cerr != nil:
		clearIoscanClassifierAlert(alerts)
		logger.Warn("ioscan classifier enabled but not running", "reason", cerr.Error())
	default:
		sched.SetClassifier(ioscan.NewCachedClassifier(classifier, ioscan.DefaultClassifierCacheEntries), thresholds)
		clearIoscanClassifierAlert(alerts)
		logger.Info("ioscan semantic classifier enabled", "model", model, "fail_closed", classifierFailClosed)
	}
}

// ioscanClassifierFallbackBackend picks the subscription CLI the classifier
// runs through when no backend is configured and no model endpoint is
// resolved: the tool-less-capable backend most of the hive's agents run on,
// ties broken by name so the choice is stable across boots. Empty when no
// agent runs on a backend ioscan.SupportsToolless accepts.
func ioscanClassifierFallbackBackend(cfg *config.Config) string {
	counts := map[string]int{}
	for _, agent := range cfg.Agents {
		b := strings.ToLower(strings.TrimSpace(agent.Backend))
		if ioscan.SupportsToolless(b) {
			counts[b]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	backends := make([]string, 0, len(counts))
	for b := range counts {
		backends = append(backends, b)
	}
	sort.Slice(backends, func(i, j int) bool {
		if counts[backends[i]] != counts[backends[j]] {
			return counts[backends[i]] > counts[backends[j]]
		}
		return backends[i] < backends[j]
	})
	return backends[0]
}

// ioscanClassifierBlockedAlertMessage is the dashboard text for a required
// classifier that cannot run.
func ioscanClassifierBlockedAlertMessage(acmmLevel int, cause error) string {
	return fmt.Sprintf("ioscan semantic classifier is required at ACMM L%d but is not running (%v). "+
		"Every kick carrying untrusted text (issues, comments, PR bodies, reviews) is being withheld until this is fixed. "+
		"Fix: configure a model endpoint (governor.trajectory or governor.litellm), set ioscan.classifier.backend to one of %s "+
		"(Settings → Security → Input Defense), or accept scanning without the classifier with ioscan.classifier.fail_mode: open. "+
		"Changes take effect after a restart.", acmmLevel, cause, strings.Join(ioscan.ToollessBackends(), ", "))
}

func raiseIoscanClassifierAlert(alerts ioscanClassifierAlerter, acmmLevel int, cause error) {
	if alerts == nil {
		return
	}
	alerts.AddSystemAlert(ioscanClassifierBlockedAlertID, "error", ioscanClassifierBlockedAlertMessage(acmmLevel, cause))
}

func clearIoscanClassifierAlert(alerts ioscanClassifierAlerter) {
	if alerts == nil {
		return
	}
	alerts.ClearSystemAlert(ioscanClassifierBlockedAlertID)
}
