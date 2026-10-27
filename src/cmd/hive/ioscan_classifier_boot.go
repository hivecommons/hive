package main

import (
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

const ioscanClassifierUnavailableAlertID = "ioscan-classifier-unavailable"

type ioscanAlertSink interface {
	AddSystemAlert(string, string, string)
	ClearSystemAlert(string)
}

// installIoscanClassifier builds the semantic injection classifier when the
// ACMM level (or an explicit setting) enables it. Explicit CLI configuration
// wins; otherwise prefer the HTTP endpoint, then a tool-less configured agent CLI.
func installIoscanClassifier(sched ioscanClassifierSink, cfg *config.Config, logger *slog.Logger, now func() time.Time, alerts ...ioscanAlertSink) {
	for _, sink := range alerts {
		sink.ClearSystemAlert(ioscanClassifierUnavailableAlertID)
	}
	alertUnavailable := func() {
		if classifierClosed := cfg.Ioscan.ClassifierFailClosedAtLevel(cfg.ACMMLevelOrZero()); classifierClosed {
			for _, sink := range alerts {
				sink.AddSystemAlert(ioscanClassifierUnavailableAlertID, "error", "Input defense classifier is required but unavailable: kicks are being blocked. Configure governor.trajectory/litellm, set ioscan.classifier.backend to claude or copilot, or set ioscan.classifier.fail_mode: open.")
			}
		}
	}
	acmmLevel := cfg.ACMMLevelOrZero()
	if !cfg.Ioscan.ClassifierEnabledAtLevel(acmmLevel) {
		return
	}
	classifierFailClosed := cfg.Ioscan.ClassifierFailClosedAtLevel(acmmLevel)
	cl := cfg.Ioscan.Classifier
	thresholds := ioscan.Thresholds{Warn: cl.WarnThreshold, Block: cl.BlockThreshold}

	endpoint, apiKey, model := cfg.Governor.ResolveReviewer()
	backend := cl.CLIBackend()
	if backend == "" && strings.TrimSpace(endpoint) == "" {
		names := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			candidate := strings.TrimSpace(cfg.Agents[name].Backend)
			if ioscan.SupportsToolless(candidate) {
				backend = candidate
				break
			}
		}
	}
	if backend != "" {
		// NewCLIClassifier refuses exactly the backends SupportsToolless rejects.
		classifier, cerr := ioscan.NewCLIClassifier(backend, cl.Model, cl.CLIDailyQuotaOrDefault(), now)
		if cerr != nil {
			alertUnavailable()
			if classifierFailClosed {
				sched.SetClassifier(ioscan.UnavailableClassifier{Err: cerr}, ioscan.Thresholds{})
			}
			logger.Error("ioscan classifier backend refused: it cannot run with no tools, no MCP, no GitHub token and an empty working directory",
				"backend", backend, "accepted", "claude,copilot", "fail_closed", classifierFailClosed, "acmm_level", acmmLevel,
				"fix", "set ioscan.classifier.backend to claude or copilot, or leave it empty to use the model endpoint (Settings → Security → Input Defense)")
			return
		}
		sched.SetClassifier(ioscan.NewCachedClassifier(classifier, ioscan.DefaultClassifierCacheEntries), thresholds)
		sched.SetClassifierBudgetPerEval(cl.CLIBudgetPerEvalOrDefault())
		logger.Info("ioscan semantic classifier enabled", "backend", backend, "model", cl.Model,
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
		alertUnavailable()
		// The level requires the classifier and it cannot run: withhold
		// untrusted text rather than silently scanning without it.
		sched.SetClassifier(ioscan.UnavailableClassifier{Err: cerr}, ioscan.Thresholds{})
		logger.Error("ioscan classifier required at this ACMM level but not running; untrusted text is withheld",
			"reason", cerr.Error(), "acmm_level", acmmLevel,
			"fix", "configure governor.trajectory/litellm, set ioscan.classifier.backend to claude or copilot, or set ioscan.classifier.fail_mode: open")
	case cerr != nil:
		logger.Warn("ioscan classifier enabled but not running", "reason", cerr.Error())
	default:
		sched.SetClassifier(ioscan.NewCachedClassifier(classifier, ioscan.DefaultClassifierCacheEntries), thresholds)
		logger.Info("ioscan semantic classifier enabled", "model", model, "fail_closed", classifierFailClosed)
	}
}
