package main

import (
	"log/slog"
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

// installIoscanClassifier builds the semantic injection classifier when the
// ACMM level (or an explicit setting) enables it. An empty
// ioscan.classifier.backend uses the OpenAI-compatible HTTP endpoint; a named
// backend runs the classifier through that subscription CLI, tool-less, with
// the low per-pass budget and the daily quota. A backend that cannot run
// tool-less is refused with one log line and handled like any classifier
// that cannot be built: withheld text at fail-closed, no classifier at
// fail-open.
func installIoscanClassifier(sched ioscanClassifierSink, cfg *config.Config, logger *slog.Logger, now func() time.Time) {
	acmmLevel := cfg.ACMMLevelOrZero()
	if !cfg.Ioscan.ClassifierEnabledAtLevel(acmmLevel) {
		return
	}
	classifierFailClosed := cfg.Ioscan.ClassifierFailClosedAtLevel(acmmLevel)
	cl := cfg.Ioscan.Classifier
	thresholds := ioscan.Thresholds{Warn: cl.WarnThreshold, Block: cl.BlockThreshold}

	if backend := cl.CLIBackend(); backend != "" {
		// NewCLIClassifier refuses exactly the backends SupportsToolless rejects.
		classifier, cerr := ioscan.NewCLIClassifier(backend, cl.Model, cl.CLIDailyQuotaOrDefault(), now)
		if cerr != nil {
			if classifierFailClosed {
				sched.SetClassifier(ioscan.UnavailableClassifier{Err: cerr}, ioscan.Thresholds{})
			}
			logger.Error("ioscan classifier backend refused: it cannot run with no tools, no MCP, no GitHub token and an empty working directory",
				"backend", backend, "accepted", "claude", "fail_closed", classifierFailClosed, "acmm_level", acmmLevel,
				"fix", "set ioscan.classifier.backend to claude, or leave it empty to use the model endpoint (Settings → Security → Input Defense)")
			return
		}
		sched.SetClassifier(ioscan.NewCachedClassifier(classifier, ioscan.DefaultClassifierCacheEntries), thresholds)
		sched.SetClassifierBudgetPerEval(cl.CLIBudgetPerEvalOrDefault())
		logger.Info("ioscan semantic classifier enabled", "backend", backend, "model", cl.Model,
			"budget_per_eval", cl.CLIBudgetPerEvalOrDefault(), "daily_quota", cl.CLIDailyQuotaOrDefault(), "fail_closed", classifierFailClosed)
		return
	}

	endpoint, apiKey, model := cfg.Governor.ResolveReviewer()
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
		logger.Error("ioscan classifier required at this ACMM level but not running; untrusted text is withheld",
			"reason", cerr.Error(), "acmm_level", acmmLevel,
			"fix", "configure governor.trajectory/litellm, set ioscan.classifier.backend: claude, or set ioscan.classifier.enabled: false or ioscan.classifier.fail_mode: open (Settings → Security → Input Defense)")
	case cerr != nil:
		logger.Warn("ioscan classifier enabled but not running", "reason", cerr.Error())
	default:
		sched.SetClassifier(ioscan.NewCachedClassifier(classifier, ioscan.DefaultClassifierCacheEntries), thresholds)
		logger.Info("ioscan semantic classifier enabled", "model", model, "fail_closed", classifierFailClosed)
	}
}
